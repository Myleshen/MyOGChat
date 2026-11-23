package server

import (
	"net/http"
	"strings"
	"time"
)

// maxUserMemoryRunes caps the per-user global memory (6.8). It is re-sent on
// every turn of every chat, so it bounds that extra input: at the cap it costs
// roughly 2k tokens per turn, cheaper than a project memory because it is one
// block. Unlike project memory nothing ever writes it automatically.
const maxUserMemoryRunes = 8000

// memoryPayload is the shape of GET/PUT /api/me/memory (§24).
type memoryPayload struct {
	Memory    string `json:"memory"`
	Limit     int    `json:"limit"`
	UpdatedAt *int64 `json:"updated_at"`
}

// handleGetMyMemory returns the caller's own "about me" memory (§24).
func (s *Server) handleGetMyMemory(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, memoryPayload{
		Memory: u.Memory, Limit: maxUserMemoryRunes, UpdatedAt: u.MemoryUpdatedAt,
	})
}

// handlePutMyMemory replaces the caller's memory; an empty string clears it.
// The cap counts runes, not bytes, like project memory (§8.2).
func (s *Server) handlePutMyMemory(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	var req struct {
		Memory string `json:"memory"`
	}
	if err := decodeJSON(w, r, &req, 64*1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len([]rune(req.Memory)) > maxUserMemoryRunes {
		writeError(w, http.StatusBadRequest, "memory exceeds 8000 characters")
		return
	}
	now := time.Now().UnixMilli()
	if err := s.st.SetUserMemory(r.Context(), u.ID, req.Memory, now); err != nil {
		s.log.Error("store user memory", "user", u.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to save memory")
		return
	}
	out := memoryPayload{Memory: req.Memory, Limit: maxUserMemoryRunes}
	if req.Memory != "" {
		out.UpdatedAt = &now
	}
	writeJSON(w, http.StatusOK, out)
}

// memorySet reports whether the user has a usable global memory. A whitespace
// only value is not injected, so it does not count as set.
func memorySet(memory string) bool {
	return strings.TrimSpace(memory) != ""
}
