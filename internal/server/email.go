package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"myogchat/internal/mail"
	"myogchat/internal/secret"
	"myogchat/internal/store"
	"myogchat/internal/tools"
)

// mailCredential is the encrypted-at-rest blob for one mailbox (feature 3.2).
// The auth_type field leaves room for OAuth2 later without a schema change.
type mailCredential struct {
	AuthType string `json:"auth_type"`
	Password string `json:"password"`
}

// mailRequest is the connect payload from the Settings UI.
type mailRequest struct {
	Provider string `json:"provider"`
	Email    string `json:"email"`
	Password string `json:"password"`
	Host     string `json:"imap_host"`
	Port     int    `json:"imap_port"`
	TLS      string `json:"imap_tls"`
}

// resolveMailAccount turns a request into a validated connection config,
// applying the provider preset when the user did not override the host/port.
func resolveMailAccount(req mailRequest) (mail.Account, error) {
	provider := strings.TrimSpace(req.Provider)
	if provider == "" {
		provider = "custom"
	}
	preset, ok := mail.LookupProvider(provider)
	if !ok {
		return mail.Account{}, fmt.Errorf("unknown provider %q", provider)
	}
	host := strings.TrimSpace(req.Host)
	port := req.Port
	tlsMode := strings.TrimSpace(req.TLS)
	if preset.ID != "custom" {
		if host == "" {
			host = preset.Host
		}
		if port == 0 {
			port = preset.Port
		}
		if tlsMode == "" {
			tlsMode = preset.TLS
		}
	}
	if tlsMode == "" {
		tlsMode = mail.TLSImplicit
	}
	if port == 0 {
		port = 993
	}
	acct := mail.Account{
		Provider: preset.ID,
		Email:    strings.TrimSpace(req.Email),
		Host:     host,
		Port:     port,
		TLS:      tlsMode,
		Password: req.Password,
	}
	if err := acct.Validate(); err != nil {
		return mail.Account{}, err
	}
	return acct, nil
}

// handleGetMyMail reports the caller's mailbox status and the provider presets.
// It never returns the app password.
func (s *Server) handleGetMyMail(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	s.writeMailStatus(w, r, u.ID)
}

// writeMailStatus emits the GET /api/me/mail shape.
func (s *Server) writeMailStatus(w http.ResponseWriter, r *http.Request, userID string) {
	out := map[string]any{
		"connected": false,
		"enabled":   false,
		"presets":   mail.Presets(),
	}
	row, err := s.st.GetMailAccount(r.Context(), userID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// not connected
	case err != nil:
		s.log.Error("get mail account", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load mailbox")
		return
	default:
		out["connected"] = true
		out["enabled"] = row.Enabled
		out["provider"] = row.Provider
		out["email"] = row.Email
		out["imap_host"] = row.Host
		out["imap_port"] = row.Port
		out["imap_tls"] = row.TLS
	}
	writeJSON(w, http.StatusOK, out)
}

// handlePutMyMail connects (or replaces) the caller's mailbox. The connection is
// tested by default before the encrypted credential is stored; skip_test:true
// stores it without a live check.
func (s *Server) handlePutMyMail(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	var req struct {
		mailRequest
		SkipTest bool `json:"skip_test"`
	}
	if err := decodeJSON(w, r, &req, 16*1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	acct, err := resolveMailAccount(req.mailRequest)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !req.SkipTest {
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		if err := mail.TestConnection(ctx, acct); err != nil {
			writeError(w, http.StatusBadRequest, "connection failed: "+err.Error())
			return
		}
	}
	blob, err := json.Marshal(mailCredential{AuthType: "password", Password: acct.Password})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to encode credential")
		return
	}
	enc, err := secret.Encrypt(s.encKey, blob)
	if err != nil {
		s.log.Error("encrypt mail credential", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to store credential")
		return
	}
	now := time.Now().UnixMilli()
	createdAt := now
	enabled := true
	if existing, gerr := s.st.GetMailAccount(r.Context(), u.ID); gerr == nil {
		createdAt = existing.CreatedAt
		enabled = existing.Enabled
	}
	row := &store.MailAccount{
		UserID: u.ID, Provider: acct.Provider, Email: acct.Email,
		Host: acct.Host, Port: acct.Port, TLS: acct.TLS, Enabled: enabled,
		SecretEnc: enc, CreatedAt: createdAt, UpdatedAt: now,
	}
	if err := s.st.SetMailAccount(r.Context(), row); err != nil {
		s.log.Error("store mail account", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to save mailbox")
		return
	}
	s.writeMailStatus(w, r, u.ID)
}

// handlePatchMyMail updates the per-user email-tools toggle.
func (s *Server) handlePatchMyMail(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decodeJSON(w, r, &req, 4*1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if _, err := s.st.GetMailAccount(r.Context(), u.ID); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no mailbox connected")
		return
	} else if err != nil {
		s.log.Error("get mail account", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load mailbox")
		return
	}
	if req.Enabled != nil {
		if err := s.st.SetMailAccountEnabled(r.Context(), u.ID, *req.Enabled, time.Now().UnixMilli()); err != nil {
			s.log.Error("set mail enabled", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to update mailbox")
			return
		}
	}
	s.writeMailStatus(w, r, u.ID)
}

// handleDeleteMyMail disconnects the caller's mailbox (deletes the credential).
func (s *Server) handleDeleteMyMail(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	if err := s.st.DeleteMailAccount(r.Context(), u.ID); err != nil {
		s.log.Error("delete mail account", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to disconnect mailbox")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTestMyMail validates a mailbox configuration without storing it.
func (s *Server) handleTestMyMail(w http.ResponseWriter, r *http.Request) {
	if _, ok := currentUser(w, r); !ok {
		return
	}
	var req mailRequest
	if err := decodeJSON(w, r, &req, 16*1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	acct, err := resolveMailAccount(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	if err := mail.TestConnection(ctx, acct); err != nil {
		writeError(w, http.StatusBadRequest, "connection failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// mailAccount loads and decrypts a user's mailbox when it is connected and
// enabled. It never exposes the password to callers other than the dialer.
func (s *Server) mailAccount(ctx context.Context, userID string) (mail.Account, bool) {
	if strings.TrimSpace(userID) == "" {
		return mail.Account{}, false
	}
	row, err := s.st.GetMailAccount(ctx, userID)
	if err != nil || !row.Enabled {
		return mail.Account{}, false
	}
	raw, err := secret.Decrypt(s.encKey, row.SecretEnc)
	if err != nil {
		s.log.Warn("decrypt mail credential", "error", err)
		return mail.Account{}, false
	}
	var cred mailCredential
	if err := json.Unmarshal(raw, &cred); err != nil {
		s.log.Warn("decode mail credential", "error", err)
		return mail.Account{}, false
	}
	if strings.TrimSpace(cred.Password) == "" {
		return mail.Account{}, false
	}
	return mail.Account{
		Provider: row.Provider, Email: row.Email, Host: row.Host,
		Port: row.Port, TLS: row.TLS, Password: cred.Password,
	}, true
}

// mailConnector returns a per-request connector that opens a fresh read-only
// session for each email tool call. ok is false when no enabled mailbox exists.
func (s *Server) mailConnector(ctx context.Context, userID string) (tools.EmailConnector, bool) {
	acct, ok := s.mailAccount(ctx, userID)
	if !ok {
		return nil, false
	}
	return func(ctx context.Context) (tools.EmailConn, error) {
		return mail.Dial(ctx, acct)
	}, true
}
