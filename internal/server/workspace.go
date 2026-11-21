package server

// Agent-mode workspace management (per user). The directories are owned by the
// sandbox runner (<WorkDir>/agent/<userID>/<convID>); these endpoints only list
// and remove them, so a user can reclaim disk space without shell access.

import (
	"net/http"
	"os"
	"path/filepath"

	"myogchat/internal/sandbox"
)

// agentWorkspaceRoot is <Sandbox.WorkDir>/agent, or "" when no sandbox exists.
func (s *Server) agentWorkspaceRoot() string {
	if s.sbx == nil {
		return ""
	}
	return filepath.Join(s.sbx.Config().WorkDir, sandbox.AgentWorkspaceDirName)
}

// agentWorkspaceDir returns the absolute workspace directory of one
// conversation, or "" when the path cannot be built safely.
func (s *Server) agentWorkspaceDir(userID, convID string) string {
	root := s.agentWorkspaceRoot()
	if root == "" || !safePathComponent(userID) || !safePathComponent(convID) {
		return ""
	}
	return filepath.Join(root, userID, convID)
}

// agentWorkspaceInfo is one row of GET /api/me/agent/workspaces.
type agentWorkspaceInfo struct {
	ConversationID string `json:"conversation_id"`
	Title          string `json:"title"`
	Bytes          int64  `json:"bytes"`
	UpdatedAt      int64  `json:"updated_at"` // Unix milliseconds (dir mtime)
}

// handleListAgentWorkspaces lists the caller's agent-mode conversations whose
// workspace directory exists on disk, newest first.
func (s *Server) handleListAgentWorkspaces(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	out := make([]agentWorkspaceInfo, 0)
	root := s.agentWorkspaceRoot()
	if root == "" {
		writeJSON(w, http.StatusOK, out)
		return
	}
	convs, err := s.st.ListAgentConversations(r.Context(), u.ID)
	if err != nil {
		s.log.Error("list agent conversations", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list workspaces")
		return
	}
	for _, c := range convs {
		dir := s.agentWorkspaceDir(u.ID, c.ID)
		if dir == "" {
			continue
		}
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue
		}
		size, err := sandbox.DirSize(dir)
		if err != nil {
			s.log.Warn("measure agent workspace", "conversation", c.ID, "error", err)
			size = 0
		}
		out = append(out, agentWorkspaceInfo{
			ConversationID: c.ID,
			Title:          c.Title,
			Bytes:          size,
			UpdatedAt:      info.ModTime().UnixMilli(),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleDeleteAgentWorkspace removes one conversation's workspace (owner only).
// It is idempotent: a conversation without a directory still answers 204.
func (s *Server) handleDeleteAgentWorkspace(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	conv, ok := s.getConversation(w, r, u.ID)
	if !ok {
		return
	}
	dir := s.agentWorkspaceDir(u.ID, conv.ID)
	if dir == "" {
		writeError(w, http.StatusBadRequest, "invalid conversation")
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		s.log.Error("delete agent workspace", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to delete workspace")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
