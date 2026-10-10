package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"myogchat/internal/store"
	"myogchat/internal/tools"
)

// linkwardenStatusFor reports the effective Linkwarden configuration for the
// caller (2.8): the resolved instance URL, whether it is usable, and a
// non-secret hint of the token. The URL/token come from the caller's own stored
// instance or the optional server env fallback.
func (s *Server) linkwardenStatusFor(ctx context.Context, u store.User) (rawURL string, configured bool, hint string) {
	url, token, ok := s.linkwardenConfigFor(ctx, u)
	if !ok {
		return "", false, ""
	}
	return url, true, keyHint(token)
}

// handleGetMyLinkwarden reports the caller's Linkwarden status (all users).
func (s *Server) handleGetMyLinkwarden(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	rawURL, configured, hint := s.linkwardenStatusFor(r.Context(), u)
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": configured,
		"url":        rawURL,
		"token_hint": hint,
	})
}

// handlePutMyLinkwarden sets or replaces the caller's own Linkwarden instance
// (2.8). The URL + token are validated with a lightweight authenticated request
// before the (encrypted) token is stored. An omitted/empty token keeps the
// caller's stored token.
func (s *Server) handlePutMyLinkwarden(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	var req struct {
		URL   string  `json:"url"`
		Token *string `json:"token"`
	}
	if err := decodeJSON(w, r, &req, 8*1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	rawURL := strings.TrimSpace(req.URL)
	if rawURL == "" {
		writeError(w, http.StatusBadRequest, "url is required")
		return
	}
	// A provided non-empty token wins; otherwise reuse the caller's stored one.
	token := ""
	if req.Token != nil && strings.TrimSpace(*req.Token) != "" {
		token = strings.TrimSpace(*req.Token)
	} else if _, stored, ok := s.userLinkwardenConfig(u); ok {
		token = stored
	}
	if token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}
	normalized, err := tools.NormalizeBaseURL(rawURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	client, err := tools.NewLinkwardenClient(normalized, token)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := client.Check(ctx); err != nil {
		writeError(w, http.StatusBadRequest, "could not connect to Linkwarden: "+err.Error())
		return
	}
	if err := s.setUserLinkwarden(r.Context(), u, normalized, &token, time.Now().UnixMilli()); err != nil {
		s.log.Error("set user linkwarden", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to store Linkwarden settings")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": true,
		"url":        normalized,
		"token_hint": keyHint(token),
	})
}

// handleDeleteMyLinkwarden clears the caller's own Linkwarden instance (2.8).
// The server env fallback (if any) is unaffected.
func (s *Server) handleDeleteMyLinkwarden(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	if err := s.st.SetUserLinkwarden(r.Context(), u.ID, "", nil, time.Now().UnixMilli()); err != nil {
		s.log.Error("delete user linkwarden", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to clear Linkwarden settings")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
