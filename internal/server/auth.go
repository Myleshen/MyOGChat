package server

import (
	"net/http"

	"myogchat/internal/auth"
)

// handleMe returns the signed-in user plus the server-wide tool and agent
// switches. They are otherwise only readable from /api/settings (admin-only),
// which left everyone else showing a composer chip that silently behaved like
// a normal chat.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	out := auth.PublicUser(u)
	out["tools_enabled"] = s.toolsEnabled(r.Context())
	out["agent_enabled"] = s.agentEnabled(r.Context())
	// Never the text: the SPA only needs to know whether to show a hint (§24).
	out["memory_set"] = memorySet(u.Memory)
	writeJSON(w, http.StatusOK, out)
}
