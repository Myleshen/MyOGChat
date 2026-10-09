package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"myogchat/internal/models"
	"myogchat/internal/store"
	"myogchat/internal/upstream"
)

const (
	// maxContentBytes caps a single user message at 100 KB (contract §3).
	maxContentBytes = 100 * 1024
	// titleModel is used for the one-shot auto-title call (contract §3).
	titleModel = "longcat-2.5-preview-free"
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

// toolEvent surfaces tool activity to the client (Phase 2.4).
type toolEvent struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // running | done | error
	Summary string `json:"summary,omitempty"`
}

// handlePostMessage appends a user message and streams the assistant reply.
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
	}
	if err := decodeJSON(w, r, &req, 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		writeError(w, http.StatusBadRequest, "content is required")
		return
	}
	if len(req.Content) > maxContentBytes {
		writeError(w, http.StatusBadRequest, "content exceeds 100 KB")
		return
	}
	atts, err := s.loadAttachments(r.Context(), u.ID, req.AttachmentIDs)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "unknown attachment")
		return
	}
	if err != nil {
		s.log.Error("load attachments", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load attachments")
		return
	}
	apiKey, err := s.resolveKey(r.Context(), u)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	userMsg := store.Message{
		ID:             newUUID(),
		ConversationID: conv.ID,
		Role:           "user",
		Content:        req.Content,
		Attachments:    attachmentMeta(atts),
		CreatedAt:      time.Now().UnixMilli(),
	}
	if err := s.st.InsertMessage(r.Context(), &userMsg); err != nil {
		s.log.Error("insert user message", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to save message")
		return
	}
	s.streamReply(w, r, conv, u.ID, apiKey, atts)
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
	if _, err := s.st.DeleteAssistantAfterLastUser(r.Context(), conv.ID); err != nil {
		s.log.Error("delete trailing assistant reply", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to prepare regenerate")
		return
	}
	// Re-fold any attachments the last user message carried (2.3).
	atts := s.attachmentsFromLastUser(r.Context(), u.ID, conv.ID)
	s.streamReply(w, r, conv, u.ID, apiKey, atts)
}

// streamReply streams one assistant reply for conv over SSE and persists it.
func (s *Server) streamReply(w http.ResponseWriter, r *http.Request, conv store.Conversation, userID, apiKey string, atts []store.Attachment) {
	ctx := r.Context()
	m, ok := models.ByID(conv.Model)
	if !ok {
		writeError(w, http.StatusInternalServerError, "conversation has unknown model")
		return
	}
	history, err := s.st.ListMessages(ctx, conv.ID)
	if err != nil {
		s.log.Error("load history", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load history")
		return
	}
	msgs := make([]upstream.Message, 0, len(history))
	for _, hm := range history {
		msgs = append(msgs, upstream.Message{Role: hm.Role, Content: hm.Content})
	}
	instructions := s.projectInstructions(ctx, conv.ProjectID, userID)

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	asstID := newUUID()
	writeSSE(w, flusher, "meta", map[string]string{"message_id": asstID, "model": conv.Model})

	effort := ""
	if conv.Effort != nil {
		effort = *conv.Effort
	}
	// Build the per-request tool set (Phase 2.4). An empty registry disables
	// tool calling but still runs the same agentic code path.
	reg := s.toolRegistry(ctx)
	events := s.up.StreamAgent(ctx, upstream.Params{
		Dialect:      m.Dialect,
		Model:        conv.Model,
		Effort:       effort,
		SessionID:    conv.ID,
		APIKey:       apiKey,
		Messages:     msgs,
		Instructions: instructions,
		Context:      foldAttachments(atts),
		Tools:        upstreamToolSpecs(reg.Specs()),
	}, s.toolExecutor(reg))

	var content, reasoning strings.Builder
	var usage *upstream.Usage
	var costMicros int64
	var durationMs int64
	var firstTokenAt time.Time
	startedAt := time.Now()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

stream:
	for {
		select {
		case ev, open := <-events:
			if !open {
				writeSSE(w, flusher, "error", errorEvent{Message: "upstream stream ended unexpectedly"})
				return
			}
			switch ev.Kind {
			case upstream.EventReasoning:
				if firstTokenAt.IsZero() {
					firstTokenAt = time.Now()
				}
				reasoning.WriteString(ev.Delta)
				writeSSE(w, flusher, "reasoning", deltaEvent{Delta: ev.Delta})
			case upstream.EventText:
				if firstTokenAt.IsZero() {
					firstTokenAt = time.Now()
				}
				content.WriteString(ev.Delta)
				writeSSE(w, flusher, "text", deltaEvent{Delta: ev.Delta})
			case upstream.EventUsage:
				if ev.Usage == nil {
					writeSSE(w, flusher, "error", errorEvent{Message: "upstream sent empty usage"})
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
				writeSSE(w, flusher, "usage", usageEvent{
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
				writeSSE(w, flusher, "error", errorEvent{Message: msg})
				return
			case upstream.EventTool:
				if ev.Tool != nil {
					writeSSE(w, flusher, "tool", toolEvent{
						Name:    ev.Tool.Name,
						Status:  ev.Tool.Status,
						Summary: ev.Tool.Summary,
					})
				}
			}
		case <-ticker.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err == nil {
				flusher.Flush()
			}
		case <-ctx.Done():
			// Client disconnected; the derived upstream request is canceled.
			return
		}
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
		CreatedAt:        time.Now().UnixMilli(),
	}
	if reasoningStr := reasoning.String(); reasoningStr != "" {
		asstMsg.Reasoning = &reasoningStr
	}
	if err := s.st.InsertMessage(ctx, &asstMsg); err != nil {
		s.log.Error("insert assistant message", "error", err)
		writeSSE(w, flusher, "error", errorEvent{Message: "failed to save message"})
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

	writeSSE(w, flusher, "done", doneEvent{MessageID: asstID, Title: title})
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

// writeSSE writes one SSE frame and flushes it immediately.
func writeSSE(w io.Writer, f http.Flusher, event string, data any) {
	b, err := json.Marshal(data)
	if err != nil {
		return
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err == nil {
		f.Flush()
	}
}
