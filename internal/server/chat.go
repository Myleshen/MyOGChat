package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"myogchat/internal/extract"
	"myogchat/internal/models"
	"myogchat/internal/sandbox"
	"myogchat/internal/store"
	"myogchat/internal/upstream"
)

const (
	// maxContentBytes caps a single user message at 100 KB (contract §3).
	maxContentBytes = 100 * 1024
	// titleModel is used for the one-shot auto-title call (contract §3).
	titleModel = "longcat-2.5-preview-free"

	// Vision image input (Phase 5.1): the SPA sends up to a few inline image
	// data URIs with a message when the conversation's model can see images.
	// The caps bound the request body and the upstream payload.
	maxVisionImages        = 4
	maxVisionImageURIBytes = 8 << 20  // one data URI, encoded string length
	maxVisionTotalURIBytes = 16 << 20 // all data URIs in one message
	// maxMessageBodyBytes caps the message POST body (content + inline images).
	maxMessageBodyBytes = maxVisionTotalURIBytes + 2<<20
)

// SSE payload shapes (contract §3 wire format).
type deltaEvent struct {
	Delta string `json:"delta"`
}

type usageEvent struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	CachedTokens     int64 `json:"cached_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
	CostMicros       int64 `json:"cost_micros"`
	DurationMs       int64 `json:"duration_ms"`
}

type doneEvent struct {
	MessageID string `json:"message_id"`
	Title     string `json:"title,omitempty"` // only when newly set
}

type errorEvent struct {
	Message string `json:"message"`
}

// toolEvent surfaces tool activity to the client (Phase 2.4; extended 4.1 with
// the full call so the UI can show inputs/outputs collapsibly).
type toolEvent struct {
	Name       string `json:"name"`
	Status     string `json:"status"` // running | done | error
	Summary    string `json:"summary,omitempty"`
	Args       string `json:"args,omitempty"`
	Output     string `json:"output,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
}

// Size caps for persisted/streamed tool activity (Phase 4.1). These bound the
// messages.tools row; the model always receives the uncapped tool result.
const (
	maxToolArgsBytes   = 16 * 1024
	maxToolOutputBytes = 64 * 1024
	maxToolErrorBytes  = 4 * 1024
)

// replySpec describes one detached reply job (Phase 5.7). For a new message the
// user content/attachments/images are carried; for a regenerate the handler has
// already validated the request and the trailing reply is replaced.
type replySpec struct {
	conv   store.Conversation
	user   store.User
	apiKey string

	userContent string             // post: the just-sent user message
	userAtts    []store.Attachment // post: attachment metadata + folded text
	images      []string           // post: inline vision image data URIs
	regenerate  bool               // regenerate instead of appending a user message
}

// streamAgent runs the upstream agent loop, allowing tests to inject a stream.
func (s *Server) streamAgent(ctx context.Context, p upstream.Params, exec upstream.ExecuteTool) <-chan upstream.Event {
	if s.streamAgentFn != nil {
		return s.streamAgentFn(ctx, p, exec)
	}
	return s.up.StreamAgent(ctx, p, exec)
}

// startReply reserves a job, launches the detached agent loop, and relays the
// job's SSE frames to the calling client. The job is not tied to the request:
// if the client disconnects the relay returns and the job keeps running.
func (s *Server) startReply(w http.ResponseWriter, r *http.Request, spec replySpec) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	j, err := s.hub().start(spec.conv.ID, spec.user.ID, spec.conv.Model)
	if err != nil {
		status := http.StatusTooManyRequests
		if errors.Is(err, errJobRunning) {
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	// Subscribe before launching the job so no early frame (e.g. meta) is lost.
	sub := j.subscribe()
	go s.runReply(j, spec)
	relayJob(w, r, flusher, j, sub)
}

// handlePostMessage validates a new user message and starts a detached reply.
func (s *Server) handlePostMessage(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	conv, ok := s.getConversation(w, r, u.ID)
	if !ok {
		return
	}
	var req struct {
		Content       string   `json:"content"`
		AttachmentIDs []string `json:"attachment_ids"`
		Images        []string `json:"images"`
	}
	if err := decodeJSON(w, r, &req, maxMessageBodyBytes); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.Content) > maxContentBytes {
		writeError(w, http.StatusBadRequest, "content exceeds 100 KB")
		return
	}
	// Inline images are only meaningful for a vision-capable model. For any other
	// model they are ignored (the folded OCR text is used), so image caps must
	// never reject a message the model cannot even see (5.1 hardening).
	m, ok := models.ByID(conv.Model)
	if !ok {
		writeError(w, http.StatusInternalServerError, "conversation has unknown model")
		return
	}
	var images []string
	if m.Vision {
		cleaned, cerr := cleanVisionImages(req.Images)
		if cerr != nil {
			writeError(w, http.StatusBadRequest, cerr.Error())
			return
		}
		images = cleaned
	}
	atts, err := s.loadAttachments(r.Context(), u.ID, req.AttachmentIDs)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "unknown attachment")
		return
	}
	var notReady *attachmentNotReadyError
	if errors.As(err, &notReady) {
		writeError(w, http.StatusConflict, notReady.msg)
		return
	}
	if err != nil {
		s.log.Error("load attachments", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load attachments")
		return
	}
	// A message may carry only an image or an attachment, with no text (asking
	// "what's in this image?" needs no prompt).
	if strings.TrimSpace(req.Content) == "" && len(images) == 0 && len(atts) == 0 {
		writeError(w, http.StatusBadRequest, "content is required")
		return
	}
	apiKey, err := s.resolveKey(r.Context(), u)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Quota is enforced before the user message is persisted, so a rejection
	// leaves no orphan row (2.11).
	if err := s.checkQuota(r.Context(), u); err != nil {
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	s.startReply(w, r, replySpec{
		conv:        conv,
		user:        u,
		apiKey:      apiKey,
		userContent: req.Content,
		userAtts:    atts,
		images:      images,
	})
}

// handleRegenerate replaces the trailing assistant reply (§8.5).
func (s *Server) handleRegenerate(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	conv, ok := s.getConversation(w, r, u.ID)
	if !ok {
		return
	}
	hasUser, err := s.st.HasUserMessage(r.Context(), conv.ID)
	if err != nil {
		s.log.Error("check user message", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load conversation")
		return
	}
	if !hasUser {
		writeError(w, http.StatusBadRequest, "conversation has no user message to regenerate")
		return
	}
	apiKey, err := s.resolveKey(r.Context(), u)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Quota is enforced before the trailing assistant row is deleted (2.11).
	if err := s.checkQuota(r.Context(), u); err != nil {
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	// The detached job deletes the trailing reply and re-folds the last user
	// message's attachments (2.3) under its own context.
	s.startReply(w, r, replySpec{conv: conv, user: u, apiKey: apiKey, regenerate: true})
}

// runReply owns the detached agent loop and every DB write for one reply
// (Phase 5.7). It runs on the job's background context — never r.Context() — so
// a client disconnect, reload, or tab close cannot interrupt it. Progress is
// published on the job (live SSE + the generation snapshot).
func (s *Server) runReply(j *genJob, spec replySpec) {
	defer s.hub().complete(j)
	ctx := j.ctx
	conv := spec.conv
	u := spec.user
	apiKey := spec.apiKey

	m, ok := models.ByID(conv.Model)
	if !ok {
		j.finish(jobError, "", "conversation has unknown model")
		return
	}

	atts := spec.userAtts
	if spec.regenerate {
		if _, err := s.st.DeleteAssistantAfterLastUser(ctx, conv.ID); err != nil {
			s.log.Error("delete trailing assistant reply", "error", err)
			j.finish(jobError, "", "failed to prepare regenerate")
			return
		}
		atts = s.attachmentsFromLastUser(ctx, u.ID, conv.ID)
	} else {
		userMsg := store.Message{
			ID:             newUUID(),
			ConversationID: conv.ID,
			Role:           "user",
			Content:        spec.userContent,
			Attachments:    attachmentMeta(atts),
			CreatedAt:      time.Now().UnixMilli(),
		}
		if err := s.st.InsertMessage(ctx, &userMsg); err != nil {
			s.log.Error("insert user message", "error", err)
			j.finish(jobError, "", "failed to save message")
			return
		}
	}

	history, err := s.st.ListMessages(ctx, conv.ID)
	if err != nil {
		s.log.Error("load history", "error", err)
		j.finish(jobError, "", "failed to load history")
		return
	}
	msgs := make([]upstream.Message, 0, len(history))
	for _, hm := range history {
		msgs = append(msgs, upstream.Message{Role: hm.Role, Content: hm.Content})
	}
	// Vision input (Phase 5.1): when the model can see images, attach the inline
	// data URIs to the just-sent user message and drop the redundant OCR text of
	// image attachments (the model reads them directly). Otherwise fall back to
	// the folded OCR text exactly as before.
	context := foldAttachments(atts)
	if m.Vision && len(spec.images) > 0 {
		attachImagesToLastUser(msgs, spec.images)
		context = foldAttachments(nonImageAttachments(atts))
	}
	instructions := s.projectInstructions(ctx, conv.ProjectID, u.ID)

	// Build the per-request tool set before composing the instructions: a
	// registered email_digest appends the fixed summarization standard (5.9) to
	// the system prompt (the project prompt stays first, so it wins). Images
	// generated by generate_image (4.3b) and files produced by the code sandbox
	// (5.8) are collected here and attached to the assistant message below.
	var generatedImages []store.GeneratedImage
	var generatedArtifacts []store.ArtifactRef
	reg := s.toolRegistryWith(ctx, u, func(img store.GeneratedImage) {
		generatedImages = append(generatedImages, img)
	}, func(a sandbox.Artifact) {
		ref, err := s.storeArtifact(ctx, u.ID, conv.ID, a)
		if err != nil {
			s.log.Error("store sandbox artifact", "name", a.Name, "error", err)
			return
		}
		generatedArtifacts = append(generatedArtifacts, ref)
	})
	if _, ok := reg.Get("email_digest"); ok {
		instructions = joinSystem(instructions, emailDigestStandardWithDate(time.Now(), time.Local))
	}

	asstID := newUUID()
	j.meta(asstID)

	effort := ""
	if conv.Effort != nil {
		effort = *conv.Effort
	}
	// Tool calling runs the same agentic code path even when the registry is
	// empty. The generated images/artifacts were collected above.
	events := s.streamAgent(ctx, upstream.Params{
		Dialect:      m.Dialect,
		Model:        conv.Model,
		Effort:       effort,
		SessionID:    conv.ID,
		APIKey:       apiKey,
		Messages:     msgs,
		Instructions: instructions,
		Context:      context,
		Tools:        upstreamToolSpecs(reg.Specs()),
	}, s.toolExecutor(reg))

	var content, reasoning strings.Builder
	var tools []store.ToolActivity
	var usage *upstream.Usage
	var costMicros int64
	var durationMs int64
	var firstTokenAt time.Time
	startedAt := time.Now()

stream:
	for {
		select {
		case <-ctx.Done():
			// Stopped (explicit stop or shutdown); no partial reply is persisted.
			j.finish(jobStopped, "", "")
			return
		case ev, open := <-events:
			if !open {
				if ctx.Err() != nil {
					j.finish(jobStopped, "", "")
					return
				}
				j.finish(jobError, "", "upstream stream ended unexpectedly")
				return
			}
			switch ev.Kind {
			case upstream.EventReasoning:
				if firstTokenAt.IsZero() {
					firstTokenAt = time.Now()
				}
				reasoning.WriteString(ev.Delta)
				j.addReasoning(ev.Delta)
			case upstream.EventText:
				if firstTokenAt.IsZero() {
					firstTokenAt = time.Now()
				}
				content.WriteString(ev.Delta)
				j.addText(ev.Delta)
			case upstream.EventUsage:
				if ev.Usage == nil {
					j.finish(jobError, "", "upstream sent empty usage")
					return
				}
				usage = ev.Usage
				// Duration measured from the first streamed token to completion,
				// so tokens/sec reflects generation speed (§8.4).
				from := startedAt
				if !firstTokenAt.IsZero() {
					from = firstTokenAt
				}
				durationMs = time.Since(from).Milliseconds()
				costMicros = models.CostMicros(m, usage.PromptTokens, usage.CompletionTokens, usage.CachedTokens, time.Now())
				j.addUsage(usageEvent{
					PromptTokens:     usage.PromptTokens,
					CompletionTokens: usage.CompletionTokens,
					CachedTokens:     usage.CachedTokens,
					ReasoningTokens:  usage.ReasoningTokens,
					CostMicros:       costMicros,
					DurationMs:       durationMs,
				})
				break stream
			case upstream.EventError:
				msg := "upstream error"
				if ev.Err != nil {
					msg = ev.Err.Error()
				}
				s.log.Error("upstream stream failed", "model", conv.Model, "error", msg)
				j.finish(jobError, "", msg)
				return
			case upstream.EventTool:
				if ev.Tool != nil {
					// Collect the same activity we stream so it survives a
					// reload (2.10/4.1).
					tools = mergeToolEvent(tools, ev.Tool)
					j.addTool(ev.Tool)
				}
			}
		}
	}

	if usage == nil {
		j.finish(jobError, "", "upstream stream ended without usage")
		return
	}
	reasoningTokens := usage.ReasoningTokens
	model := conv.Model
	asstMsg := store.Message{
		ID:               asstID,
		ConversationID:   conv.ID,
		Role:             "assistant",
		Content:          content.String(),
		Model:            &model,
		PromptTokens:     &usage.PromptTokens,
		CompletionTokens: &usage.CompletionTokens,
		CachedTokens:     &usage.CachedTokens,
		CostMicros:       &costMicros,
		ReasoningTokens:  &reasoningTokens,
		DurationMs:       &durationMs,
		Tools:            tools,
		Images:           generatedImages,
		Artifacts:        generatedArtifacts,
		CreatedAt:        time.Now().UnixMilli(),
	}
	if reasoningStr := reasoning.String(); reasoningStr != "" {
		asstMsg.Reasoning = &reasoningStr
	}
	if err := s.st.InsertMessage(ctx, &asstMsg); err != nil {
		s.log.Error("insert assistant message", "error", err)
		j.finish(jobError, "", "failed to save message")
		return
	}
	if err := s.st.TouchConversation(ctx, conv.ID, time.Now().UnixMilli()); err != nil {
		s.log.Error("bump conversation updated_at", "error", err)
	}

	title := ""
	if conv.Title == "" {
		firstUser := ""
		for _, hm := range history {
			if hm.Role == "user" {
				firstUser = hm.Content
				break
			}
		}
		title = s.autoTitle(ctx, conv.ID, apiKey, firstUser, content.String())
		if title != "" {
			if err := s.st.SetTitle(ctx, conv.ID, title); err != nil {
				s.log.Error("persist auto-title", "error", err)
				title = ""
			}
		}
	}

	j.finish(jobDone, title, "")
}

// attachmentsFromLastUser reloads the attachment texts referenced by the most
// recent user message so a regenerate folds the same context (2.3). Missing
// attachments (e.g. deleted after sending) are skipped.
func (s *Server) attachmentsFromLastUser(ctx context.Context, userID, convID string) []store.Attachment {
	msgs, err := s.st.ListMessages(ctx, convID)
	if err != nil {
		s.log.Warn("load messages for attachments", "error", err)
		return nil
	}
	var ids []string
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			for _, f := range msgs[i].Attachments {
				ids = append(ids, f.ID)
			}
			break
		}
	}
	atts := make([]store.Attachment, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if seen[id] || len(atts) >= maxAttachmentsPerMessage {
			continue
		}
		seen[id] = true
		a, err := s.st.GetAttachment(ctx, id, userID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			s.log.Warn("load attachment for regenerate", "id", id, "error", err)
			continue
		}
		atts = append(atts, a)
	}
	return atts
}

// cleanVisionImages validates the inline image data URIs sent with a message
// (Phase 5.1). Each must be a base64 image data URI within the size budget, and
// the set must fit maxVisionTotalURIBytes. Empty entries are skipped.
func cleanVisionImages(images []string) ([]string, error) {
	if len(images) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(images))
	total := 0
	for _, raw := range images {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if len(out) >= maxVisionImages {
			return nil, fmt.Errorf("at most %d images per message", maxVisionImages)
		}
		if !isImageDataURI(s) {
			return nil, errors.New("invalid image attachment")
		}
		if len(s) > maxVisionImageURIBytes {
			return nil, errors.New("an image is too large")
		}
		total += len(s)
		if total > maxVisionTotalURIBytes {
			return nil, errors.New("attached images are too large in total")
		}
		out = append(out, s)
	}
	return out, nil
}

// isImageDataURI reports whether s is a base64-encoded **raster** image data
// URI, the only shape both upstream dialects accept for image parts. SVG is
// rejected (it is a script container, not something the models read), and an
// empty payload is rejected.
func isImageDataURI(s string) bool {
	const prefix = "data:image/"
	if !strings.HasPrefix(s, prefix) {
		return false
	}
	i := strings.Index(s, ";base64,")
	if i < 0 {
		return false
	}
	switch sub := s[len(prefix):i]; sub {
	case "png", "jpeg", "jpg", "gif", "webp", "bmp":
	default:
		return false
	}
	return strings.TrimSpace(s[i+len(";base64,"):]) != ""
}

// attachImagesToLastUser sets the inline images on the most recent user message
// so the model receives them as multimodal content (Phase 5.1). The slice is
// mutated in place; a conversation with no user message is left untouched.
func attachImagesToLastUser(msgs []upstream.Message, images []string) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			msgs[i].Images = images
			return
		}
	}
}

// nonImageAttachments drops image attachments before folding their text into
// the request context: a vision-capable model sees the image itself, so the OCR
// text would be redundant (Phase 5.1).
func nonImageAttachments(atts []store.Attachment) []store.Attachment {
	var out []store.Attachment
	for _, a := range atts {
		if extract.IsImage(a.Name, a.Mime) {
			continue
		}
		out = append(out, a)
	}
	return out
}

// projectInstructions returns the system prompt of a conversation's project, or
// "" when the chat is unfiled (§8.6).
func (s *Server) projectInstructions(ctx context.Context, projectID *string, userID string) string {
	if projectID == nil {
		return ""
	}
	p, err := s.st.GetProject(ctx, *projectID, userID)
	if err != nil {
		// A missing project must not break the chat; log and continue.
		s.log.Warn("project instructions unavailable", "project", *projectID, "error", err)
		return ""
	}
	return p.Instructions
}

// autoTitle makes one non-streaming call to generate a short title, falling
// back to the first 40 chars of the first user message on any failure.
func (s *Server) autoTitle(ctx context.Context, convID, apiKey, firstUser, firstAssistant string) string {
	prompt := "Write a 3-6 word chat title for this conversation. Reply with the title only, no quotes, no punctuation at the end.\n\n" +
		"User: " + truncateRunes(firstUser, 500) + "\n" +
		"Assistant: " + truncateRunes(firstAssistant, 500)
	// The free title model is a reasoning model, so the cap must leave room
	// for reasoning tokens plus the actual title after them.
	t, err := s.up.ChatOnce(ctx, apiKey, convID, titleModel,
		[]upstream.Message{{Role: "user", Content: prompt}}, 200)
	if err != nil {
		s.log.Warn("auto-title call failed, using fallback", "error", err)
		return fallbackTitle(firstUser)
	}
	if t = sanitizeTitle(t); t == "" {
		return fallbackTitle(firstUser)
	}
	return t
}

// sanitizeTitle reduces an LLM title to one line of at most 80 chars.
func sanitizeTitle(s string) string {
	s = oneLine(s)
	s = strings.Trim(s, `"'“”‘’ `)
	return truncateRunes(strings.TrimSpace(s), 80)
}

// fallbackTitle is the first 40 chars of the first user message, one line.
func fallbackTitle(firstUser string) string {
	return truncateRunes(strings.TrimSpace(oneLine(firstUser)), 40)
}

func oneLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// cappedTool converts an upstream tool event into a persistable/streamable
// store.ToolActivity, applying UTF-8-safe size caps so the SSE payload and the
// stored row are identical.
func cappedTool(t *upstream.ToolActivity) store.ToolActivity {
	args, argsTrunc := truncateToolField(t.Args, maxToolArgsBytes)
	output, outTrunc := truncateToolField(t.Output, maxToolOutputBytes)
	return store.ToolActivity{
		Name:       t.Name,
		Status:     t.Status,
		Summary:    t.Summary,
		Args:       args,
		Output:     output,
		Error:      firstNBytes(t.Error, maxToolErrorBytes),
		DurationMs: t.DurationMs,
		Truncated:  argsTrunc || outTrunc,
	}
}

// truncateToolField cuts s to at most max bytes on a UTF-8 boundary and reports
// whether it was truncated. A visible marker is appended when cut.
func truncateToolField(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n… [truncated]", true
}

// firstNBytes is like firstN but UTF-8-safe and byte-counted.
func firstNBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// mergeToolEvent folds one upstream tool activity into the persisted list,
// mirroring the SPA's updateToolActivity (2.10/4.1): a "running" event starts an
// entry; a terminal event closes the most recent matching running entry, else
// appends. Tools run sequentially (agent.go), so there is never true
// concurrency to reconcile.
func mergeToolEvent(list []store.ToolActivity, t *upstream.ToolActivity) []store.ToolActivity {
	entry := cappedTool(t)
	if entry.Status == upstream.ToolRunning {
		return append(list, entry)
	}
	for i := len(list) - 1; i >= 0; i-- {
		if list[i].Name == entry.Name && list[i].Status == upstream.ToolRunning {
			// Keep the args captured when the call started if the terminal
			// event omits them, and carry the truncation flag forward.
			if entry.Args == "" {
				entry.Args = list[i].Args
			}
			entry.Truncated = entry.Truncated || list[i].Truncated
			list[i] = entry
			return list
		}
	}
	return append(list, entry)
}
