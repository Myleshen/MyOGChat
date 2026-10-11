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

// mailWriteRequest is the create/update payload for a mailbox (5.11): the
// connection fields plus a label and the per-account toggles.
type mailWriteRequest struct {
	mailRequest
	Label     string `json:"label"`
	Enabled   *bool  `json:"enabled"`
	IsDefault *bool  `json:"is_default"`
	SkipTest  bool   `json:"skip_test"`
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

// mailAccountView renders one mailbox for the API (never secret_enc).
func mailAccountView(a store.MailAccount) map[string]any {
	return map[string]any{
		"id":         a.ID,
		"label":      a.Label,
		"provider":   a.Provider,
		"email":      a.Email,
		"imap_host":  a.Host,
		"imap_port":  a.Port,
		"imap_tls":   a.TLS,
		"enabled":    a.Enabled,
		"is_default": a.IsDefault,
		"created_at": a.CreatedAt,
		"updated_at": a.UpdatedAt,
	}
}

// ---------- plural endpoints (canonical, 5.11) ----------

// handleListMyMailAccounts returns every mailbox the caller has connected plus
// the provider presets and the per-user cap.
func (s *Server) handleListMyMailAccounts(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	rows, err := s.st.ListMailAccounts(r.Context(), u.ID)
	if err != nil {
		s.log.Error("list mail accounts", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load mailboxes")
		return
	}
	accounts := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		accounts = append(accounts, mailAccountView(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":     accounts,
		"connected":    len(accounts) > 0,
		"max_accounts": store.MaxMailAccounts,
		"presets":      mail.Presets(),
	})
}

// handleCreateMyMailAccount connects a new mailbox (tested unless skip_test).
func (s *Server) handleCreateMyMailAccount(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	var req mailWriteRequest
	if err := decodeJSON(w, r, &req, 16*1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	count, err := s.st.CountMailAccounts(r.Context(), u.ID)
	if err != nil {
		s.log.Error("count mail accounts", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load mailboxes")
		return
	}
	if count >= store.MaxMailAccounts {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("at most %d mailboxes are supported", store.MaxMailAccounts))
		return
	}
	acct, err := resolveMailAccount(req.mailRequest)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !req.SkipTest {
		if !testMailAccountHTTP(w, r, acct) {
			return
		}
	}
	enc, err := s.encryptMailCredential(acct.Password)
	if err != nil {
		s.log.Error("encrypt mail credential", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to store credential")
		return
	}
	now := time.Now().UnixMilli()
	label := strings.TrimSpace(req.Label)
	if label == "" {
		label = acct.Email
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	row := &store.MailAccount{
		UserID: u.ID, Label: label, Provider: acct.Provider, Email: acct.Email,
		Host: acct.Host, Port: acct.Port, TLS: acct.TLS, Enabled: enabled,
		IsDefault: req.IsDefault != nil && *req.IsDefault,
		SecretEnc: enc, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.st.CreateMailAccount(r.Context(), row); err != nil {
		s.log.Error("create mail account", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to save mailbox")
		return
	}
	writeJSON(w, http.StatusCreated, mailAccountView(*row))
}

// handlePatchMyMailAccount updates one mailbox: metadata toggles always apply;
// any connection field triggers a re-validation (and a test unless skip_test).
// An empty password keeps the stored credential.
func (s *Server) handlePatchMyMailAccount(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	existing, err := s.st.GetMailAccountByID(r.Context(), u.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "mailbox not found")
		return
	} else if err != nil {
		s.log.Error("get mail account", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load mailbox")
		return
	}
	var req mailWriteRequest
	if err := decodeJSON(w, r, &req, 16*1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	now := time.Now().UnixMilli()
	row := existing

	if connFieldsPresent(req.mailRequest) {
		pass := req.Password
		if strings.TrimSpace(pass) == "" {
			p, derr := s.decryptMailPassword(existing.SecretEnc)
			if derr != nil {
				s.log.Warn("decrypt mail credential", "error", derr)
				writeError(w, http.StatusBadRequest, "enter the app-specific password")
				return
			}
			pass = p
		}
		acct, rerr := resolveMailAccount(mailRequest{
			Provider: req.Provider, Email: req.Email, Password: pass,
			Host: req.Host, Port: req.Port, TLS: req.TLS,
		})
		if rerr != nil {
			writeError(w, http.StatusBadRequest, rerr.Error())
			return
		}
		if !req.SkipTest {
			if !testMailAccountHTTP(w, r, acct) {
				return
			}
		}
		enc, eerr := s.encryptMailCredential(acct.Password)
		if eerr != nil {
			s.log.Error("encrypt mail credential", "error", eerr)
			writeError(w, http.StatusInternalServerError, "failed to store credential")
			return
		}
		row.Provider, row.Email, row.Host, row.Port, row.TLS = acct.Provider, acct.Email, acct.Host, acct.Port, acct.TLS
		row.SecretEnc = enc
	}
	if strings.TrimSpace(req.Label) != "" {
		row.Label = strings.TrimSpace(req.Label)
	}
	if req.Enabled != nil {
		row.Enabled = *req.Enabled
	}
	row.UpdatedAt = now
	if err := s.st.UpdateMailAccount(r.Context(), &row); err != nil {
		s.log.Error("update mail account", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to save mailbox")
		return
	}
	if req.IsDefault != nil && *req.IsDefault && !existing.IsDefault {
		if err := s.st.SetDefaultMailAccount(r.Context(), u.ID, row.ID, now); err != nil {
			s.log.Error("set default mail account", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to set the default mailbox")
			return
		}
		row.IsDefault = true
	}
	writeJSON(w, http.StatusOK, mailAccountView(row))
}

// handleDeleteMyMailAccount disconnects one mailbox (promoting a new default if
// the deleted one was the default).
func (s *Server) handleDeleteMyMailAccount(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	err := s.st.DeleteMailAccount(r.Context(), u.ID, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "mailbox not found")
		return
	} else if err != nil {
		s.log.Error("delete mail account", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to disconnect mailbox")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTestMyMailAccountByID tests a stored mailbox without changing it.
func (s *Server) handleTestMyMailAccountByID(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	acct, err := s.mailAccountFor(r.Context(), u.ID, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "mailbox not found")
		return
	} else if err != nil {
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

// ---------- singular endpoints (thin aliases to the default mailbox) ----------

// handleGetMyMail reports the default mailbox's status and the provider presets.
func (s *Server) handleGetMyMail(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	s.writeMailStatus(w, r, u.ID)
}

// writeMailStatus emits the legacy GET /api/me/mail shape for the default mailbox.
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
		out["id"] = row.ID
		out["label"] = row.Label
		out["is_default"] = row.IsDefault
	}
	writeJSON(w, http.StatusOK, out)
}

// handlePutMyMail connects (or replaces) the default mailbox.
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
		if !testMailAccountHTTP(w, r, acct) {
			return
		}
	}
	enc, err := s.encryptMailCredential(acct.Password)
	if err != nil {
		s.log.Error("encrypt mail credential", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to store credential")
		return
	}
	now := time.Now().UnixMilli()
	existing, gerr := s.st.GetMailAccount(r.Context(), u.ID)
	switch {
	case gerr == nil:
		existing.Provider, existing.Email = acct.Provider, acct.Email
		existing.Host, existing.Port, existing.TLS = acct.Host, acct.Port, acct.TLS
		existing.SecretEnc, existing.UpdatedAt = enc, now
		if strings.TrimSpace(existing.Label) == "" {
			existing.Label = acct.Email
		}
		if err := s.st.UpdateMailAccount(r.Context(), &existing); err != nil {
			s.log.Error("update mail account", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to save mailbox")
			return
		}
	case errors.Is(gerr, store.ErrNotFound):
		row := &store.MailAccount{
			UserID: u.ID, Label: acct.Email, Provider: acct.Provider, Email: acct.Email,
			Host: acct.Host, Port: acct.Port, TLS: acct.TLS, Enabled: true,
			SecretEnc: enc, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.st.CreateMailAccount(r.Context(), row); err != nil {
			s.log.Error("create mail account", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to save mailbox")
			return
		}
	default:
		s.log.Error("get mail account", "error", gerr)
		writeError(w, http.StatusInternalServerError, "failed to load mailbox")
		return
	}
	s.writeMailStatus(w, r, u.ID)
}

// handlePatchMyMail updates the default mailbox's email-tools toggle.
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
	row, err := s.st.GetMailAccount(r.Context(), u.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no mailbox connected")
		return
	} else if err != nil {
		s.log.Error("get mail account", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load mailbox")
		return
	}
	if req.Enabled != nil {
		if err := s.st.SetMailAccountEnabled(r.Context(), u.ID, row.ID, *req.Enabled, time.Now().UnixMilli()); err != nil {
			s.log.Error("set mail enabled", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to update mailbox")
			return
		}
	}
	s.writeMailStatus(w, r, u.ID)
}

// handleDeleteMyMail disconnects the default mailbox.
func (s *Server) handleDeleteMyMail(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	row, err := s.st.GetMailAccount(r.Context(), u.ID)
	if errors.Is(err, store.ErrNotFound) {
		w.WriteHeader(http.StatusNoContent)
		return
	} else if err != nil {
		s.log.Error("get mail account", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load mailbox")
		return
	}
	if err := s.st.DeleteMailAccount(r.Context(), u.ID, row.ID); err != nil {
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
	if !testMailAccountHTTP(w, r, acct) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- internals ----------

// connFieldsPresent reports whether a write request carries connection fields
// (so a PATCH knows to re-validate the mailbox rather than only toggle it).
func connFieldsPresent(req mailRequest) bool {
	return strings.TrimSpace(req.Provider) != "" || strings.TrimSpace(req.Email) != "" ||
		strings.TrimSpace(req.Password) != "" || strings.TrimSpace(req.Host) != "" ||
		req.Port != 0 || strings.TrimSpace(req.TLS) != ""
}

// testMailAccountHTTP dials the account and, on failure, writes a 400. It
// reports whether the connection succeeded.
func testMailAccountHTTP(w http.ResponseWriter, r *http.Request, acct mail.Account) bool {
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	if err := mail.TestConnection(ctx, acct); err != nil {
		writeError(w, http.StatusBadRequest, "connection failed: "+err.Error())
		return false
	}
	return true
}

// encryptMailCredential encrypts an app password into the stored blob.
func (s *Server) encryptMailCredential(password string) (string, error) {
	blob, err := json.Marshal(mailCredential{AuthType: "password", Password: password})
	if err != nil {
		return "", err
	}
	return secret.Encrypt(s.encKey, blob)
}

// decryptMailPassword returns the plaintext app password from a stored blob.
func (s *Server) decryptMailPassword(secretEnc string) (string, error) {
	raw, err := secret.Decrypt(s.encKey, secretEnc)
	if err != nil {
		return "", err
	}
	var cred mailCredential
	if err := json.Unmarshal(raw, &cred); err != nil {
		return "", err
	}
	return cred.Password, nil
}

// mailAccount loads and decrypts the caller's default mailbox when it is
// connected and enabled. It never exposes the password to callers other than the
// dialer.
func (s *Server) mailAccount(ctx context.Context, userID string) (mail.Account, bool) {
	acct, err := s.mailAccountFor(ctx, userID, "")
	if err != nil {
		return mail.Account{}, false
	}
	return acct, true
}

// mailAccountFor resolves a mailbox reference ("" = default), decrypts it, and
// requires it to be enabled. A missing/unknown ref is store.ErrNotFound.
func (s *Server) mailAccountFor(ctx context.Context, userID, ref string) (mail.Account, error) {
	if strings.TrimSpace(userID) == "" {
		return mail.Account{}, store.ErrNotFound
	}
	row, err := s.st.ResolveMailAccount(ctx, userID, ref)
	if err != nil {
		return mail.Account{}, err
	}
	if !row.Enabled {
		return mail.Account{}, fmt.Errorf("mailbox %q is disabled", mailAccountName(row))
	}
	pass, err := s.decryptMailPassword(row.SecretEnc)
	if err != nil {
		s.log.Warn("decode mail credential", "error", err)
		return mail.Account{}, errors.New("mailbox credential is unreadable")
	}
	if strings.TrimSpace(pass) == "" {
		return mail.Account{}, errors.New("mailbox credential is empty")
	}
	return mail.Account{
		Provider: row.Provider, Email: row.Email, Host: row.Host,
		Port: row.Port, TLS: row.TLS, Password: pass,
	}, nil
}

// mailAccountName is a human label for a mailbox in error messages.
func mailAccountName(row store.MailAccount) string {
	if strings.TrimSpace(row.Label) != "" {
		return row.Label
	}
	return row.Email
}

// listMailAccounts returns the caller's mailboxes (all of them, newest/oldest
// order preserved) for tool registration and listing.
func (s *Server) listMailAccounts(ctx context.Context, userID string) ([]store.MailAccount, error) {
	return s.st.ListMailAccounts(ctx, userID)
}

// mailToolAccounts converts the enabled mailboxes into the model-facing listing.
func (s *Server) mailToolAccounts(ctx context.Context, userID string) ([]tools.EmailAccount, error) {
	rows, err := s.st.ListMailAccounts(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]tools.EmailAccount, 0, len(rows))
	for _, row := range rows {
		if !row.Enabled {
			continue
		}
		out = append(out, tools.EmailAccount{ID: row.ID, Label: mailAccountName(row), Email: row.Email, Default: row.IsDefault})
	}
	return out, nil
}

// mailConnector returns a per-request connector that opens a fresh read-only
// session for each email tool call, resolving an optional account reference
// ("" = the default mailbox) at call time. ok is false when no enabled mailbox
// exists.
func (s *Server) mailConnector(ctx context.Context, userID string) (tools.EmailConnector, bool) {
	accounts, err := s.mailToolAccounts(ctx, userID)
	if err != nil || len(accounts) == 0 {
		return nil, false
	}
	return func(ctx context.Context, ref string) (tools.EmailConn, error) {
		acct, err := s.mailAccountFor(ctx, userID, ref)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				if strings.TrimSpace(ref) == "" {
					return nil, errors.New("no mailbox is connected")
				}
				return nil, fmt.Errorf("no mailbox matches %q (use email_accounts to list them)", ref)
			}
			return nil, err
		}
		return mail.Dial(ctx, acct)
	}, true
}
