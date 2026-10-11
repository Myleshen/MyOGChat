package server

// Message edit & resend (§18). POST /api/conversations/{id}/messages/{messageId}/edit
// rewrites one user message in place and deletes everything after it, so the
// client can stream a fresh reply with the existing regenerate endpoint. The
// two-step design (edit, then regenerate) reuses the SSE path untouched.

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"myogchat/internal/store"
)

// handleEditMessage rewrites a user message and truncates the conversation
// after it. Owner-scoped: a message that is not the caller's, not a user
// message, or not in this conversation returns 404.
func (s *Server) handleEditMessage(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	conv, ok := s.getConversation(w, r, u.ID)
	if !ok {
		return
	}
	var req struct {
		Content string `json:"content"`
	}
	if err := decodeJSON(w, r, &req, 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.Content) > maxContentBytes {
		writeError(w, http.StatusBadRequest, "content exceeds 100 KB")
		return
	}
	msgID := r.PathValue("messageId")
	content := strings.TrimSpace(req.Content)
	if content == "" {
		writeError(w, http.StatusBadRequest, "content is required")
		return
	}
	if err := s.st.EditUserMessageAndTruncate(r.Context(), conv.ID, msgID, u.ID, content); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "message not found")
			return
		}
		s.log.Error("edit message", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to edit message")
		return
	}
	if err := s.st.TouchConversation(r.Context(), conv.ID, time.Now().UnixMilli()); err != nil {
		s.log.Error("bump conversation updated_at", "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
