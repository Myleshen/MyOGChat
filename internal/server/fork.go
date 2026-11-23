package server

// "Continue from here": copy a conversation's transcript up to one message into
// a new chat the caller owns. No upstream call, no title generation, no agent
// workspace — the copied transcript is context, not files.

import (
	"errors"
	"net/http"
	"time"

	"myogchat/internal/store"
)

// forkMessageLimit caps how many messages one fork may copy, so a fork stays
// cheap to load.
const forkMessageLimit = 5000

// forkTitleSuffix marks a forked chat (em dash, so it reads as a label).
const forkTitleSuffix = " — fork"

// forkTitle marks the source title as a fork, truncating the base so the whole
// title still fits the cap PATCH enforces. An untitled source gets a readable
// base rather than a lone marker.
func forkTitle(title string) string {
	if title == "" {
		title = "Untitled"
	}
	return truncateRunes(title, titleMaxRunes-len([]rune(forkTitleSuffix))) + forkTitleSuffix
}

// handleForkConversation copies messages up to and including
// {"message_id"} into a fresh conversation owned by the caller.
func (s *Server) handleForkConversation(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	conv, ok := s.getConversation(w, r, u.ID)
	if !ok {
		return
	}
	var req struct {
		MessageID string `json:"message_id"`
	}
	if err := decodeJSON(w, r, &req, 4*1024); err != nil || req.MessageID == "" {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	now := time.Now().UnixMilli()
	userID := u.ID
	fork := store.Conversation{
		ID:        newUUID(),
		UserID:    &userID,
		Title:     forkTitle(conv.Title),
		Model:     conv.Model,
		Effort:    conv.Effort,
		ProjectID: conv.ProjectID,
		AgentMode: conv.AgentMode,
		CreatedAt: now,
		UpdatedAt: now,
	}
	n, err := s.st.ForkConversation(r.Context(), conv.ID, req.MessageID, &fork, newUUID, forkMessageLimit)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// A message outside this conversation is indistinguishable from a
		// missing one, so nothing leaks about other conversations.
		writeError(w, http.StatusNotFound, "message not found")
		return
	case errors.Is(err, store.ErrForkTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "conversation too large to fork")
		return
	case err != nil:
		s.log.Error("fork conversation", "conversation", conv.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to fork conversation")
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		ID           string `json:"id"`
		Title        string `json:"title"`
		MessageCount int    `json:"message_count"`
	}{fork.ID, fork.Title, n})
}
