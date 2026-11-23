package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"myogchat/internal/extract"
	"myogchat/internal/models"
	"myogchat/internal/sandbox"
	"myogchat/internal/store"
	"myogchat/internal/tools"
	"myogchat/internal/upstream"
)

const (
	// maxContentBytes caps a single user message at 100 KB (contract §3).
	maxContentBytes = 100 * 1024
	// Title model for the one-shot auto-title call: a cheap Go model, billed to
	// the caller's own key (contract §3). Measured against the live API: this one
	// returns a title at 64 tokens every time, while hy3 spent 256 tokens and
	// returned finish_reason=length with no content and no reasoning, and
	// longcat-2.0 emitted only reasoning.
	titleModel = "glm-5.3-flash"
	// titleMaxTokens is comfortable headroom for a short title.
	titleMaxTokens = 64
	// titleRetryTokens is the last attempt when a model returns finish_reason
	// length with no text. Only a failing title pays for the second call.
	titleRetryTokens = 256
	// titleTimeout bounds the detached auto-title call. Nothing waits on it, so
	// it only stops a stray call running on — it never caps the reply.
	titleTimeout = 30 * time.Second

	// Rolling project memory (6.2): the same cheap model as auto-title, on a
	// detached, non-streaming call of its own. The token budget fits the model's
	// reasoning plus an ≤32000-character memory.
	memoryModel     = "mimo-v2.6-flash"
	memoryMaxTokens = 4000
	memoryTimeout   = 30 * time.Second
	// User turn and reply are truncated to these byte caps before they go into
	// the memory prompt, so one long turn cannot dominate the call.
	memoryUserBytes  = 2048
	memoryReplyBytes = 6144

	// turnMaxTokens is the output budget for one chat turn, sent to every
	// dialect that takes an explicit budget (responses, anthropic, google).
	// The chat-completions dialect sends no cap, as it always has.
	turnMaxTokens = 8192

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
	// userSaved marks a message already persisted by the caller (agent sends
	// insert before queueing, so a queued message is visible immediately).
	userSaved bool
	// triggered marks a reply started by the calendar scheduler (6.0): it gets
	// no chat-reference tools and never writes project memory.
	triggered bool
}

// agentWorkspaceMinFree is the free-disk headroom required to start an agent job.
const agentWorkspaceMinFree = 1 << 30 // 1 GiB

// streamAgent runs the upstream agent loop, allowing tests to inject a stream.
func (s *Server) streamAgent(ctx context.Context, p upstream.Params, exec upstream.ExecuteTool, opts upstream.AgentOptions) <-chan upstream.Event {
	if s.streamAgentFn != nil {
		return s.streamAgentFn(ctx, p, exec, opts)
	}
	return s.up.StreamAgent(ctx, p, exec, opts)
}

// agentMode reports whether a conversation's sends run as agent jobs: the
// per-conversation flag plus the admin kill switches. An agent loop exists to
// run tools, so tools off disables it too. When either switch is off the chat
// keeps working as an ordinary chat (nothing is refused or lost).
func (s *Server) agentMode(ctx context.Context, conv store.Conversation) bool {
	return conv.AgentMode && s.agentEnabled(ctx) && s.toolsEnabled(ctx)
}

// agentDiskOK reports whether the sandbox filesystem has room for an agent
// workspace. It fails open: an unreadable path never blocks a send.
func (s *Server) agentDiskOK() bool {
	if s.sbx == nil {
		return true
	}
	free := s.freeBytes
	if free == nil {
		free = sandbox.FreeBytes
	}
	n, err := free(s.sbx.Config().WorkDir)
	if err != nil {
		s.log.Warn("agent disk check failed, allowing", "error", err)
		return true
	}
	return n >= agentWorkspaceMinFree
}

// agentWorkspace binds a conversation to its persistent sandbox workspace.
// A nil result means "use the ephemeral tools".
func (s *Server) agentWorkspace(userID, convID string) *tools.Workspace {
	if s.sbx == nil || !safePathComponent(userID) || !safePathComponent(convID) {
		return nil
	}
	cfg := s.sbx.Config()
	root := filepath.Join(cfg.WorkDir, sandbox.AgentWorkspaceDirName)
	return &tools.Workspace{
		Path:       filepath.Join(root, userID, convID),
		QuotaDir:   filepath.Join(root, userID),
		QuotaBytes: cfg.AgentQuotaBytes,
		Timeout:    agentToolTimeout,
	}
}

// safePathComponent rejects anything that could escape a parent directory when
// used as a single path element.
func safePathComponent(s string) bool {
	return s != "" && s != "." && s != ".." && filepath.Base(s) == s &&
		!strings.ContainsAny(s, `/\`)
}

// insertUserMessage persists the just-sent user message (with attachment
// metadata). Agent sends insert before the job runs so a queued message is
// visible immediately; normal replies insert inside runReply.
func (s *Server) insertUserMessage(ctx context.Context, spec *replySpec) error {
	return s.st.InsertMessage(ctx, &store.Message{
		ID:             newUUID(),
		ConversationID: spec.conv.ID,
		Role:           "user",
		Content:        spec.userContent,
		Attachments:    attachmentMeta(spec.userAtts),
		CreatedAt:      time.Now().UnixMilli(),
	})
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
	j, err := s.hub().start(spec.conv.ID, spec.user.ID, spec.conv.Model, s.agentMode(r.Context(), spec.conv))
	if err != nil {
		status := http.StatusTooManyRequests
		if errors.Is(err, errJobRunning) {
			status = http.StatusConflict
		}
		if errors.Is(err, errShuttingDown) {
			status = http.StatusServiceUnavailable
		}
		writeError(w, status, err.Error())
		return
	}
	// Subscribe before launching the job so no early frame (e.g. meta) is lost.
	sub := j.subscribe()
	go s.runReply(j, spec)
	relayJob(w, r, flusher, j, sub)
}

// startAgentReply starts an agent-mode reply, or queues it when the user already
// has an agent job running and answers 202 with the queue position. The user
// message is persisted here (before the job runs) so a queued send is visible.
func (s *Server) startAgentReply(w http.ResponseWriter, r *http.Request, spec replySpec) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	if !s.agentDiskOK() {
		writeError(w, http.StatusInsufficientStorage, "not enough disk space on the server to start an agent workspace")
		return
	}
	// The job (or its queue entry) carries no API key: it is resolved when the
	// job actually starts, so a queued send never holds a key for an hour.
	spec.userSaved = true
	spec.apiKey = ""
	j, pos, err := s.hub().startAgent(spec.conv.ID, spec.user.ID, spec.conv.Model, spec)
	if err != nil {
		status := http.StatusTooManyRequests
		if errors.Is(err, errJobRunning) {
			status = http.StatusConflict
		}
		if errors.Is(err, errShuttingDown) {
			status = http.StatusServiceUnavailable
		}
		writeError(w, status, err.Error())
		return
	}
	if err := s.insertUserMessage(r.Context(), &spec); err != nil {
		if j != nil {
			j.cancel()
			s.hub().complete(j)
		}
		s.hub().removeQueued(spec.conv.ID)
		s.log.Error("insert user message", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to save message")
		return
	}
	if j == nil {
		writeJSON(w, http.StatusAccepted, map[string]any{"queued": true, "position": pos})
		return
	}
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
	// never reject a message the model cannot even see (5.1 hardening). A custom
	// provider model is chat-only and never vision-capable.
	m, prov, err := s.resolveModel(r.Context(), u, conv.Model)
	if err != nil {
		s.log.Error("resolve conversation model", "model", conv.Model, "error", err)
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
	// A custom provider carries its own key; only an OpenCode model needs the
	// user's OpenCode key here.
	apiKey := ""
	if prov == nil {
		apiKey, err = s.resolveKey(r.Context(), u)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	// Quota is enforced before the user message is persisted, so a rejection
	// leaves no orphan row (2.11).
	if err := s.checkQuota(r.Context(), u); err != nil {
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	spec := replySpec{
		conv:        conv,
		user:        u,
		apiKey:      apiKey,
		userContent: req.Content,
		userAtts:    atts,
		images:      images,
	}
	if s.agentMode(r.Context(), conv) {
		s.startAgentReply(w, r, spec)
		return
	}
	s.startReply(w, r, spec)
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
	_, prov, err := s.resolveModel(r.Context(), u, conv.Model)
	if err != nil {
		s.log.Error("resolve conversation model", "model", conv.Model, "error", err)
		writeError(w, http.StatusInternalServerError, "conversation has unknown model")
		return
	}
	apiKey := ""
	if prov == nil {
		apiKey, err = s.resolveKey(r.Context(), u)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
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
	began := time.Now()
	defer func() {
		// Every terminal failure gets one WARN line so the next incident is
		// diagnosable from the journal (no stack, one line per failed job).
		if msg := j.failure(); msg != "" {
			s.log.Warn("reply job failed", "conversation", j.convID, "agent", j.agent,
				"elapsed", time.Since(began).Round(time.Second), "error", msg)
		}
		if next, nextSpec, ok := s.hub().complete(j); ok {
			go s.runReply(next, nextSpec)
		}
		// Web Push (6.6): a client that was not watching gets a notification.
		s.notifyReply(j, spec.conv)
	}()
	ctx := j.ctx
	conv := spec.conv
	u := spec.user
	agent := j.agent

	// Resolve the model first: a custom provider carries its own key, so an
	// OpenCode key is only required for an OpenCode model.
	m, prov, err := s.resolveModel(ctx, u, conv.Model)
	if err != nil {
		j.finish(jobError, "", "conversation has unknown model")
		return
	}
	apiKey := spec.apiKey
	// helperKey is the OpenCode key used by the internal helpers (auto-title,
	// project memory). It stays empty for a custom chat when the user has no
	// OpenCode key, and those helpers then fall back or are skipped.
	helperKey := spec.apiKey
	if prov != nil {
		apiKey = prov.apiKey
		if k, kerr := s.resolveKey(ctx, u); kerr == nil {
			helperKey = k
		} else {
			helperKey = ""
		}
	} else if apiKey == "" {
		// A queued agent send resolves the key when it actually starts, never
		// when it was enqueued.
		k, kerr := s.resolveKey(ctx, u)
		if kerr != nil {
			j.finish(jobError, "", kerr.Error())
			return
		}
		apiKey = k
		helperKey = k
	}

	atts := spec.userAtts
	switch {
	case spec.regenerate:
		if _, err := s.st.DeleteAssistantAfterLastUser(ctx, conv.ID); err != nil {
			s.log.Error("delete trailing assistant reply", "error", err)
			j.finish(jobError, "", "failed to prepare regenerate")
			return
		}
		atts = s.attachmentsFromLastUser(ctx, u.ID, conv.ID)
	case !spec.userSaved:
		if err := s.insertUserMessage(ctx, &spec); err != nil {
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
	// Auto-title runs in parallel with the reply: start it as soon as the first
	// user message is known, so a slow title can never delay `done`.
	if conv.Title == "" {
		if firstUser := firstUserMessage(history); firstUser != "" {
			j.titleCh = make(chan string, 1)
			go s.generateTitle(ctx, j.titleCh, conv.ID, helperKey, firstUser)
		}
	}
	msgs := make([]upstream.Message, 0, len(history))
	for _, hm := range history {
		m := upstream.Message{ID: hm.ID, Role: hm.Role, Content: hm.Content}
		for _, t := range hm.Tools {
			m.Tools = append(m.Tools, upstream.ToolResult{
				Name:             t.Name,
				Arguments:        t.Args,
				Output:           t.Output,
				Error:            t.Error,
				Status:           t.Status,
				ThoughtSignature: t.ThoughtSignature,
			})
		}
		msgs = append(msgs, m)
	}
	// Vision input (Phase 5.1): when the model can see images, attach the inline
	// data URIs to the just-sent user message and drop the redundant OCR text of
	// image attachments (the model reads them directly). Otherwise fall back to
	// the folded OCR text exactly as before.
	chatCtx := foldAttachments(atts)
	if m.Vision && len(spec.images) > 0 {
		attachImagesToLastUser(msgs, spec.images)
		chatCtx = foldAttachments(nonImageAttachments(atts))
	}
	instructions := s.projectInstructions(ctx, conv.ProjectID, u.ID)

	// Build the per-request tool set before composing the instructions: a
	// registered email_digest appends the fixed summarization standard (5.9) to
	// the system prompt (the project prompt stays first, so it wins). Images
	// generated by generate_image (4.3b) and files produced by the code sandbox
	// (5.8) are collected here and attached to the assistant message below. An
	// agent-mode chat gets the persistent-workspace sandbox tools instead.
	var generatedImages []store.GeneratedImage
	var generatedArtifacts []store.ArtifactRef
	var ws *tools.Workspace
	if agent {
		ws = s.agentWorkspace(u.ID, conv.ID)
	}
	reg := s.toolRegistryWithWorkspace(ctx, u, func(img store.GeneratedImage) {
		generatedImages = append(generatedImages, img)
	}, func(a sandbox.Artifact) {
		ref, err := s.storeArtifact(ctx, u.ID, conv.ID, a)
		if err != nil {
			s.log.Error("store sandbox artifact", "name", a.Name, "error", err)
			return
		}
		generatedArtifacts = append(generatedArtifacts, ref)
	}, ws)
	// A calendar-triggered reply is sandboxed away from the owner's other
	// chats: the event text is third-party data, so it cannot browse them.
	if spec.triggered {
		reg.Remove("list_chats")
		reg.Remove("read_chat")
	}
	if _, ok := reg.Get("email_digest"); ok {
		instructions = joinSystem(instructions, emailDigestStandardWithDate(time.Now(), time.Local))
	}

	asstID := newUUID()
	j.meta(asstID)

	effort := ""
	if conv.Effort != nil {
		effort = *conv.Effort
	}
	// Agent mode gets a much larger loop and per-call budget, plus step
	// progress for the generation snapshot.
	opts := upstream.AgentOptions{}
	toolTimeout := time.Duration(0)
	if agent {
		opts = upstream.AgentOptions{
			MaxIterations: agentMaxSteps,
			CallTimeout:   agentCallTimeout,
			OnStep:        j.setStep,
		}
		toolTimeout = agentToolTimeout
	}
	// Tool calling runs the same agentic code path even when the registry is
	// empty. The generated images/artifacts were collected above.
	// A custom provider's wire model id is the vendor id alone (the "<slug>/"
	// prefix is only the local selector) and its base URL replaces OpenCode's.
	wireModel, baseURL := conv.Model, ""
	if prov != nil {
		wireModel, baseURL = prov.model, prov.baseURL
	}
	events := s.streamAgent(ctx, upstream.Params{
		Dialect:      m.Dialect,
		Model:        wireModel,
		Effort:       effort,
		SessionID:    conv.ID,
		APIKey:       apiKey,
		BaseURL:      baseURL,
		Messages:     msgs,
		Instructions: instructions,
		Context:      chatCtx,
		Tools:        upstreamToolSpecs(reg.Specs()),
		MaxTokens:    turnMaxTokens,
	}, s.toolExecutor(reg, toolTimeout), opts)

	var content, reasoning strings.Builder
	var tools []store.ToolActivity
	var usage *upstream.Usage
	var costMicros int64
	var durationMs int64
	var firstTokenAt time.Time
	startedAt := time.Now()

	// persistPartialReply stores what the job produced so far. Only agent mode
	// does: a long agent run is expensive and its work must survive a stop,
	// shutdown or failure. A normal chat keeps the historical discard
	// behaviour, so a failed reply never leaves a junk half-message behind. It
	// reports whether a partial was actually written (6.6 notifies only then).
	persistPartialReply := func() bool {
		if !agent {
			return false
		}
		return s.persistPartial(ctx, conv, asstID, content.String(), reasoning.String(),
			tools, generatedImages, generatedArtifacts, startedAt)
	}
	// stop ends the job on an explicit stop or shutdown. fail ends it on a
	// terminal error but still reports the real message to the client.
	stop := func() {
		if persistPartialReply() {
			j.setPersisted()
		}
		j.finish(jobStopped, "", "")
	}
	fail := func(msg string) {
		if persistPartialReply() {
			j.setPersisted()
		}
		j.finish(jobError, "", msg)
	}
	// cancelled ends a job whose context is done: a budget running out is a
	// failure, an explicit stop or shutdown is not.
	cancelled := func() {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			fail("reply timed out")
		} else {
			stop()
		}
	}

stream:
	for {
		select {
		case <-ctx.Done():
			cancelled()
			return
		case ev, open := <-events:
			if !open {
				if ctx.Err() != nil {
					cancelled()
					return
				}
				fail("upstream stream ended unexpectedly")
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
					fail("upstream sent empty usage")
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
				fail(msg)
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
		fail("upstream stream ended without usage")
		return
	}
	reasoningTokens := usage.ReasoningTokens
	model := conv.Model
	asstMsg := store.Message{
		ID:               asstID,
		ConversationID:   conv.ID,
		Role:             "assistant",
		Content:          upstream.SanitizeDSML(content.String()),
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

	// Rolling project memory (6.2) runs off the reply's critical path, exactly
	// like the title: nothing below waits on it, so it can never delay or fail
	// `done`. Only a successful reply in a project chat is worth remembering —
	// and a calendar-triggered reply is event data, not the owner's own turn.
	if conv.ProjectID != nil && !spec.triggered {
		userTurn := spec.userContent
		if strings.TrimSpace(userTurn) == "" {
			userTurn = lastUserMessage(history)
		}
		if strings.TrimSpace(userTurn) != "" {
			go s.updateProjectMemory(ctx, u.ID, conv.ID, *conv.ProjectID, helperKey, userTurn, asstMsg.Content)
		}
	}

	// Reap the title if it beat the reply. Otherwise `done` goes out now and the
	// goroutine's SetTitle lands in the background; the next list refresh shows
	// it. Either way nothing here waits on the title.
	title := ""
	if j.titleCh != nil {
		select {
		case title = <-j.titleCh:
		default:
		}
	}

	j.finish(jobDone, title, "")
}

// firstUserMessage returns a conversation's first user message, the seed for
// the auto-title prompt.
func firstUserMessage(history []store.Message) string {
	for _, hm := range history {
		if hm.Role == "user" {
			return hm.Content
		}
	}
	return ""
}

// lastUserMessage returns a conversation's most recent user message, the turn
// that a regenerate reply belongs to (a post carries its own userContent).
func lastUserMessage(history []store.Message) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "user" {
			return history[i].Content
		}
	}
	return ""
}

// updateProjectMemory asks the cheap model to merge this turn into the project's
// durable memory and stores the result. It is best effort by design: every
// failure is logged and dropped, and a compare-and-set write means a memory the
// user edited during the call is never clobbered.
func (s *Server) updateProjectMemory(ctx context.Context, userID, convID, projectID, apiKey, userTurn, reply string) {
	ctx = context.WithoutCancel(ctx)
	if s.chatOnceFn == nil || apiKey == "" {
		return
	}
	p, err := s.st.GetProject(ctx, projectID, userID)
	if err != nil {
		s.log.Warn("project memory unavailable", "project", projectID, "error", err)
		return
	}
	if !p.MemoryAuto {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, memoryTimeout)
	defer cancel()
	out, err := s.chatOnceFn(ctx, apiKey, convID, memoryModel,
		[]upstream.Message{{Role: "user", Content: memoryPrompt(p.Memory, userTurn, reply)}}, memoryMaxTokens)
	if err != nil {
		s.log.Warn("project memory call failed", "project", projectID, "error", err)
		return
	}
	memory := truncateRunes(strings.TrimSpace(out), maxProjectMemoryRunes)
	if memory == "" {
		return
	}
	wrote, err := s.st.SetProjectMemoryIfUnchanged(ctx, projectID, userID, p.Memory, memory)
	if err != nil {
		s.log.Error("persist project memory", "project", projectID, "error", err)
		return
	}
	if !wrote {
		// The user edited the memory while the call ran; their text wins.
		s.log.Debug("project memory changed by user, dropping update", "project", projectID)
	}
}

// memoryPrompt is the single memory-merge prompt: current memory plus the
// truncated exchange, asking for the updated memory alone.
func memoryPrompt(memory, userTurn, reply string) string {
	if strings.TrimSpace(memory) == "" {
		memory = "(empty)"
	}
	return "You maintain the durable memory of a project workspace. Merge this exchange into the existing memory.\n\n" +
		"Rules:\n" +
		"- Keep only durable facts that will still matter in future chats: decisions, preferences, names, versions, constraints and ongoing work.\n" +
		"- Merge duplicates and compress older detail first, rather than dropping structure; drop pleasantries and one-off details.\n" +
		"- Reply with the updated memory only: plain text, no preamble, no code fences.\n" +
		fmt.Sprintf("- Keep it under %d characters.\n\n", maxProjectMemoryRunes) +
		"Existing memory:\n" + memory + "\n\n" +
		"Exchange:\nUser:\n" + firstNBytes(userTurn, memoryUserBytes) +
		"\n\nAssistant:\n" + firstNBytes(reply, memoryReplyBytes)
}

// generateTitle runs the one-shot auto-title call off the reply's critical path
// and persists the result itself. The context is detached from the job so a
// cancelled reply cannot lose a title already in flight, and the result channel
// is buffered so the send succeeds even after the job is gone.
func (s *Server) generateTitle(ctx context.Context, out chan<- string, convID, apiKey, firstUser string) {
	ctx = context.WithoutCancel(ctx)
	title := s.autoTitle(ctx, convID, apiKey, firstUser)
	if title == "" {
		return
	}
	wrote, err := s.st.SetTitleIfEmpty(ctx, convID, title)
	if err != nil {
		s.log.Error("persist auto-title", "error", err)
		return
	}
	if !wrote {
		// The user named the conversation while this call was running.
		return
	}
	out <- title
}

// persistPartial stores what a stopped or failed agent reply produced so far:
// the partial content/reasoning/tools plus any collected images and artifacts.
// No usage is recorded because the stream never reported it. An empty reply is
// never stored, and the write uses a context detached from the cancelled job.
// It reports whether a row was written.
func (s *Server) persistPartial(ctx context.Context, conv store.Conversation, id, content, reasoning string,
	tools []store.ToolActivity, images []store.GeneratedImage, artifacts []store.ArtifactRef, startedAt time.Time) bool {
	content = upstream.SanitizeDSML(content)
	if strings.TrimSpace(content) == "" && strings.TrimSpace(reasoning) == "" {
		return false
	}
	now := time.Now()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	model := conv.Model
	durationMs := now.Sub(startedAt).Milliseconds()
	msg := store.Message{
		ID:             id,
		ConversationID: conv.ID,
		Role:           "assistant",
		Content:        content,
		Model:          &model,
		DurationMs:     &durationMs,
		Tools:          tools,
		Images:         images,
		Artifacts:      artifacts,
		CreatedAt:      now.UnixMilli(),
	}
	if reasoning != "" {
		msg.Reasoning = &reasoning
	}
	if err := s.st.InsertMessage(ctx, &msg); err != nil {
		s.log.Error("insert partial assistant message", "error", err)
		return false
	}
	if err := s.st.TouchConversation(ctx, conv.ID, msg.CreatedAt); err != nil {
		s.log.Error("bump conversation updated_at", "error", err)
	}
	return true
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

// projectInstructions returns the system prompt of a conversation's project:
// the user-authored instructions plus, when set, the rolling memory block, or
// "" when the chat is unfiled (§8.6). Both dialects take this one string.
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
	if strings.TrimSpace(p.Memory) == "" {
		return p.Instructions
	}
	return joinSystem(p.Instructions, "## Project memory\n"+p.Memory)
}

// autoTitle makes one non-streaming call on the caller's key to generate a short
// title, falling back to the first 40 chars of the first user message on any
// failure. Only the first 400 chars of that message are sent, so a title costs a
// fraction of a cent.
func (s *Server) autoTitle(ctx context.Context, convID, apiKey, firstUser string) string {
	if s.chatOnceFn == nil || apiKey == "" {
		// Tests may build a Server without New, and a custom-provider chat may
		// have no OpenCode key: the prompt fallback costs nothing.
		return fallbackTitle(firstUser)
	}
	ctx, cancel := context.WithTimeout(ctx, titleTimeout)
	defer cancel()
	prompt := "Write a 3-6 word chat title for this conversation. Reply with the title only, no quotes, no punctuation at the end.\n\n" +
		"User: " + truncateRunes(firstUser, 400)
	// Some models answer finish_reason=length with no text (their output lands in
	// a field we do not surface), so a title gets one retry with a much larger
	// budget before falling back to the truncated prompt.
	var lastErr error
	for _, mt := range []int{titleMaxTokens, titleRetryTokens} {
		t, err := s.chatOnceFn(ctx, apiKey, convID, titleModel,
			[]upstream.Message{{Role: "user", Content: prompt}}, mt)
		if err != nil {
			lastErr = err
			continue
		}
		if t = sanitizeTitle(t); t != "" {
			return t
		}
		lastErr = errors.New("empty completion")
	}
	if lastErr != nil {
		s.log.Warn("auto-title call failed, using fallback", "error", lastErr)
	}
	return fallbackTitle(firstUser)
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
		Name:             t.Name,
		Status:           t.Status,
		Summary:          t.Summary,
		Args:             args,
		Output:           output,
		Error:            firstNBytes(t.Error, maxToolErrorBytes),
		DurationMs:       t.DurationMs,
		Truncated:        argsTrunc || outTrunc,
		ThoughtSignature: t.ThoughtSignature,
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
// entry; a terminal event closes the first matching running entry, else appends.
// Tools run sequentially in call order (agent.go), so the first running entry
// with a matching name is the one this terminal event belongs to; matching the
// last would mis-assign the result when the same tool is called twice in a row.
func mergeToolEvent(list []store.ToolActivity, t *upstream.ToolActivity) []store.ToolActivity {
	entry := cappedTool(t)
	if entry.Status == upstream.ToolRunning {
		return append(list, entry)
	}
	for i := 0; i < len(list); i++ {
		if list[i].Name == entry.Name && list[i].Status == upstream.ToolRunning {
			// Keep the args captured when the call started if the terminal
			// event omits them, carry the truncation flag forward, and keep
			// Google's provider metadata for the history replay.
			if entry.Args == "" {
				entry.Args = list[i].Args
			}
			if entry.ThoughtSignature == "" {
				entry.ThoughtSignature = list[i].ThoughtSignature
			}
			entry.Truncated = entry.Truncated || list[i].Truncated
			list[i] = entry
			return list
		}
	}
	return append(list, entry)
}
