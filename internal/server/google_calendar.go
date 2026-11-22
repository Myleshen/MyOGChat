package server

// Read-only Google Calendar via OAuth (feature 3.3). The owner registers one
// Google OAuth client; every user then connects their own calendar with a
// consent screen. Only the calendar.readonly scope is requested, so the backend
// has no write path. Access tokens are refreshed through the calendar package
// and re-encrypted into the account row, so a stored refresh token never goes
// stale.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"myogchat/internal/calendar"
	"myogchat/internal/secret"
	"myogchat/internal/store"
)

// googleCallbackPath is the OAuth redirect path. It must match the redirect URI
// registered on the Google client exactly.
const googleCallbackPath = "/api/me/calendar/google/callback"

const (
	// googleStateTTL is how long a sign-in link stays valid.
	googleStateTTL = 10 * time.Minute
	// googleOAuthTimeout bounds the token, API and revoke calls this file makes.
	googleOAuthTimeout = 15 * time.Second
)

// GoogleCalendar configures the OAuth client for the read-only Google Calendar
// backend. It is enabled only when both ClientID and ClientSecret are set.
type GoogleCalendar struct {
	ClientID     string
	ClientSecret string
	PublicURL    string // optional redirect base override (MYOGCHAT_PUBLIC_URL)

	// Overridable endpoints and client for tests; empty means Google's.
	tokenURL  string
	revokeURL string
	apiBase   string
	hc        *http.Client
}

// Available reports whether an OAuth client is configured.
func (g GoogleCalendar) Available() bool {
	return strings.TrimSpace(g.ClientID) != "" && strings.TrimSpace(g.ClientSecret) != ""
}

// withDefaults fills in Google's endpoints and a bounded HTTP client.
func (g GoogleCalendar) withDefaults() GoogleCalendar {
	if g.tokenURL == "" {
		g.tokenURL = calendar.GoogleTokenURL
	}
	if g.revokeURL == "" {
		g.revokeURL = calendar.GoogleRevokeURL
	}
	if g.apiBase == "" {
		g.apiBase = calendar.GoogleAPIBase
	}
	if g.hc == nil {
		g.hc = &http.Client{Timeout: googleOAuthTimeout}
	}
	return g
}

// googleRedirectURI is the absolute callback URL Google sends the browser back
// to. Behind the Cloudflare tunnel the request carries X-Forwarded-Proto: https
// and Host: myogchat.myleshenp.dev, so deriving it yields the URI registered on
// the client. MYOGCHAT_PUBLIC_URL overrides the derivation.
func (s *Server) googleRedirectURI(r *http.Request) string {
	if base := strings.TrimSpace(s.google.PublicURL); base != "" {
		return strings.TrimRight(base, "/") + googleCallbackPath
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := forwardedValue(r.Header.Get("X-Forwarded-Proto")); p != "" {
		scheme = p
	}
	host := r.Host
	if h := forwardedValue(r.Header.Get("X-Forwarded-Host")); h != "" {
		host = h
	}
	return scheme + "://" + host + googleCallbackPath
}

// forwardedValue takes the first value of a comma-separated proxy header.
func forwardedValue(v string) string {
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	return strings.ToLower(strings.TrimSpace(v))
}

// ---------- stateless OAuth state ----------

// newGoogleState signs "userID|unixExpiry|nonce" for the consent redirect. It is
// stateless (no DB row) and unforgeable without the session secret.
func (s *Server) newGoogleState(userID string) (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	raw := userID + "|" +
		strconv.FormatInt(time.Now().Add(googleStateTTL).Unix(), 10) + "|" +
		base64.RawURLEncoding.EncodeToString(nonce)
	payload := base64.RawURLEncoding.EncodeToString([]byte(raw))
	return payload + "." + s.googleStateMAC(payload), nil
}

// googleStateMAC is base64url(HMAC-SHA256(payload, session secret)).
func (s *Server) googleStateMAC(payload string) string {
	mac := hmac.New(sha256.New, s.auth.SigningKey())
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verifyGoogleState checks the signature, the expiry and that the state belongs
// to the signed-in user.
func (s *Server) verifyGoogleState(state, userID string) error {
	i := strings.LastIndexByte(state, '.')
	if i <= 0 || i+1 >= len(state) {
		return errors.New("the sign-in link is malformed")
	}
	payload, sig := state[:i], state[i+1:]
	if !hmac.Equal([]byte(sig), []byte(s.googleStateMAC(payload))) {
		return errors.New("the sign-in link signature does not match")
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return errors.New("the sign-in link is malformed")
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 3 {
		return errors.New("the sign-in link is malformed")
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return errors.New("the sign-in link is malformed")
	}
	if time.Now().Unix() > exp {
		return errors.New("the sign-in link has expired — start again")
	}
	if parts[0] != userID {
		return errors.New("the sign-in link belongs to another account")
	}
	return nil
}

// ---------- handlers ----------

// handleGoogleCalendarStart redirects the browser to Google's consent screen.
// 404 when no OAuth client is configured, so the SPA's button is the only
// entry point.
func (s *Server) handleGoogleCalendarStart(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	if !s.google.Available() {
		writeError(w, http.StatusNotFound, "Google Calendar is not configured on this server")
		return
	}
	state, err := s.newGoogleState(u.ID)
	if err != nil {
		s.log.Error("google calendar: build oauth state", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to start the Google sign-in")
		return
	}
	q := url.Values{
		"client_id":              {s.google.ClientID},
		"redirect_uri":           {s.googleRedirectURI(r)},
		"response_type":          {"code"},
		"scope":                  {calendar.GoogleReadOnlyScope},
		"access_type":            {"offline"},
		"prompt":                 {"consent"},
		"include_granted_scopes": {"true"},
		"state":                  {state},
	}
	http.Redirect(w, r, calendar.GoogleAuthURL+"?"+q.Encode(), http.StatusFound)
}

// handleGoogleCalendarCallback finishes the consent flow: it verifies the state,
// exchanges the code, proves the token works by listing calendars, stores the
// encrypted token and returns the browser to Settings.
func (s *Server) handleGoogleCalendarCallback(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	g := s.google.withDefaults()
	if !g.Available() {
		renderGoogleOutcome(w, http.StatusNotFound, "Google Calendar is not configured", "This server has no Google OAuth client.")
		return
	}
	if err := s.verifyGoogleState(r.URL.Query().Get("state"), u.ID); err != nil {
		renderGoogleOutcome(w, http.StatusBadRequest, "Sign-in could not be verified", err.Error())
		return
	}
	q := r.URL.Query()
	if e := strings.TrimSpace(q.Get("error")); e != "" {
		detail := strings.TrimSpace(q.Get("error_description"))
		if detail == "" {
			detail = e
		}
		renderGoogleOutcome(w, http.StatusBadRequest, "Google did not grant access", detail)
		return
	}
	code := strings.TrimSpace(q.Get("code"))
	if code == "" {
		renderGoogleOutcome(w, http.StatusBadRequest, "Sign-in failed", "Google returned no authorization code.")
		return
	}

	ctx := r.Context()
	tok, err := calendar.ExchangeCode(ctx, g.hc, g.tokenURL, g.ClientID, g.ClientSecret, s.googleRedirectURI(r), code)
	if err != nil {
		s.log.Warn("google calendar: exchange code", "error", err)
		renderGoogleOutcome(w, http.StatusBadGateway, "Google sign-in failed", err.Error())
		return
	}
	cfg := calendar.Config{
		Kind:         calendar.KindGoogle,
		ClientID:     g.ClientID,
		ClientSecret: g.ClientSecret,
		Token:        tok,
		CalendarID:   "primary",
		TokenURL:     g.tokenURL,
		APIBase:      g.apiBase,
		HTTPClient:   g.hc,
	}
	b, err := calendar.New(cfg)
	if err != nil {
		s.log.Error("google calendar: build backend", "error", err)
		renderGoogleOutcome(w, http.StatusInternalServerError, "Google Calendar could not be connected", err.Error())
		return
	}
	// Listing calendars both proves the token works and locates the primary.
	cals, err := b.Calendars(ctx)
	if err != nil {
		s.log.Warn("google calendar: list calendars", "error", err)
		renderGoogleOutcome(w, http.StatusBadGateway, "Google Calendar could not be read", err.Error())
		return
	}
	calendarID := "primary"
	if !calendar.CalendarExists(cals, calendarID) {
		if len(cals) == 0 {
			renderGoogleOutcome(w, http.StatusBadGateway, "Google Calendar could not be connected", "This Google account has no calendars.")
			return
		}
		calendarID = cals[0].ID
	}
	raw, err := json.Marshal(tok)
	if err != nil {
		s.log.Error("google calendar: encode token", "error", err)
		renderGoogleOutcome(w, http.StatusInternalServerError, "Google Calendar could not be connected", "The token could not be encoded.")
		return
	}
	enc, err := secret.Encrypt(s.encKey, raw)
	if err != nil {
		s.log.Error("google calendar: encrypt token", "error", err)
		renderGoogleOutcome(w, http.StatusInternalServerError, "Google Calendar could not be connected", "The token could not be stored.")
		return
	}
	now := time.Now().UnixMilli()
	acc := store.CalendarAccount{
		ID:            newUUID(),
		UserID:        u.ID,
		Kind:          string(calendar.KindGoogle),
		Provider:      "google",
		CalendarID:    calendarID,
		CredentialEnc: enc,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if existing, err := s.st.GetCalendarAccount(ctx, u.ID); err == nil {
		acc.ID = existing.ID
		acc.CreatedAt = existing.CreatedAt
	}
	if err := s.st.UpsertCalendarAccount(ctx, &acc); err != nil {
		s.log.Error("google calendar: store account", "error", err)
		renderGoogleOutcome(w, http.StatusInternalServerError, "Google Calendar could not be connected", "The account could not be saved.")
		return
	}
	http.Redirect(w, r, "/#/settings", http.StatusFound)
}

// saveGoogleToken persists a refreshed token into the caller's account row. A
// row that vanished or changed kind mid-request is left alone: the read that
// triggered the refresh already succeeded, and the new owner of the row does not
// want a Google token in it.
func (s *Server) saveGoogleToken(ctx context.Context, userID, accID string, tok calendar.Token) error {
	raw, err := json.Marshal(tok)
	if err != nil {
		return err
	}
	enc, err := secret.Encrypt(s.encKey, raw)
	if err != nil {
		return err
	}
	acc, err := s.st.GetCalendarAccount(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if acc.Kind != string(calendar.KindGoogle) || acc.ID != accID {
		return nil
	}
	acc.CredentialEnc = enc
	acc.UpdatedAt = time.Now().UnixMilli()
	return s.st.UpsertCalendarAccount(ctx, &acc)
}

// revokeGoogleToken asks Google to revoke the stored token, best-effort: the
// row is deleted whether or not revocation succeeds.
func (s *Server) revokeGoogleToken(ctx context.Context, acc store.CalendarAccount) {
	raw, err := secret.Decrypt(s.encKey, acc.CredentialEnc)
	if err != nil {
		return
	}
	var tok calendar.Token
	if err := json.Unmarshal(raw, &tok); err != nil {
		return
	}
	token := tok.RefreshToken
	if token == "" {
		token = tok.AccessToken
	}
	if token == "" {
		return
	}
	g := s.google.withDefaults()
	if err := calendar.RevokeToken(ctx, g.hc, g.revokeURL, token); err != nil {
		s.log.Warn("google calendar: revoke token", "error", err)
	}
}

// renderGoogleOutcome writes a small plain HTML page for the OAuth redirect,
// which lands in a browser tab and must never show raw JSON.
func renderGoogleOutcome(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s — MyOGChat</title>
<style>
  body { margin: 0; min-height: 100vh; display: grid; place-items: center;
         background: #0b0b0c; color: #e7e7e7;
         font: 16px/1.5 ui-monospace, SFMono-Regular, Menlo, monospace; }
  main { max-width: 34rem; padding: 2rem; }
  h1 { font-size: 1.15rem; margin: 0 0 0.75rem; }
  p { margin: 0 0 1rem; color: #b9b9b9; }
  a { color: #7fb2ff; }
</style>
</head>
<body>
<main>
  <h1>%s</h1>
  <p>%s</p>
  <p><a href="/#/settings">Back to Settings</a></p>
</main>
</body>
</html>
`, html.EscapeString(title), html.EscapeString(title), html.EscapeString(detail))
}
