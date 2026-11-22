package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"myogchat/internal/auth"
	"myogchat/internal/calendar"
	"myogchat/internal/secret"
	"myogchat/internal/store"
)

// fakeGoogleOAuth serves the OAuth token endpoint, the revoke endpoint and the
// Calendar API (calendarList + events.list) on one host.
type fakeGoogleOAuth struct {
	srv *httptest.Server

	mu         sync.Mutex
	tokenForms []url.Values
	revoked    []string
	apiPaths   []string
	authHeader []string

	calendarsBody string
	eventsBody    string
	tokenStatus   int
	tokenBody     string
}

func newFakeGoogleOAuth(t *testing.T) *fakeGoogleOAuth {
	t.Helper()
	f := &fakeGoogleOAuth{
		calendarsBody: `{"items":[{"id":"me@example.com","summary":"Me","primary":true},
		                          {"id":"team@group.calendar.google.com","summary":"Team"}]}`,
		eventsBody:  `{"items":[]}`,
		tokenStatus: http.StatusOK,
		tokenBody:   `{"access_token":"at-1","refresh_token":"rt-1","expires_in":3600,"token_type":"Bearer","scope":"` + calendar.GoogleReadOnlyScope + `"}`,
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			_ = r.ParseForm()
			f.mu.Lock()
			f.tokenForms = append(f.tokenForms, r.PostForm)
			status, body := f.tokenStatus, f.tokenBody
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		case r.URL.Path == "/revoke":
			_ = r.ParseForm()
			f.mu.Lock()
			f.revoked = append(f.revoked, r.PostForm.Get("token"))
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			f.mu.Lock()
			f.apiPaths = append(f.apiPaths, r.URL.Path)
			f.authHeader = append(f.authHeader, r.Header.Get("Authorization"))
			cal, events := f.calendarsBody, f.eventsBody
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if strings.HasSuffix(r.URL.Path, "/calendarList") {
				_, _ = io.WriteString(w, cal)
				return
			}
			_, _ = io.WriteString(w, events)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// config returns a GoogleCalendar pointed at the fakes.
func (f *fakeGoogleOAuth) config() GoogleCalendar {
	return GoogleCalendar{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		tokenURL:     f.srv.URL + "/token",
		revokeURL:    f.srv.URL + "/revoke",
		apiBase:      f.srv.URL,
	}
}

// newGoogleTestServer builds a server with a session-signing auth (needed for
// the OAuth state) and the given Google config.
func newGoogleTestServer(t *testing.T, g GoogleCalendar) (*Server, *store.Store) {
	t.Helper()
	srv, st := newKeyTestServer(t)
	srv.auth = auth.New(st, []byte("test-session-secret"))
	srv.google = g.withDefaults()
	return srv, st
}

// googleCall drives one handler with an authed request against target (whose
// host becomes r.Host). X-Forwarded-Proto mirrors the Cloudflare tunnel, which
// is how the app is actually reached.
func googleCall(srv *Server, u store.User, target string, h func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	r = r.WithContext(auth.WithUser(r.Context(), u))
	rec := httptest.NewRecorder()
	h(rec, r)
	return rec
}

// seedCalendarAccountRow stores an account and returns the stored row.
func seedCalendarAccountRow(t *testing.T, srv *Server, userID string, acc store.CalendarAccount, rawSecret string) store.CalendarAccount {
	t.Helper()
	seedCalendarAccount(t, srv, userID, acc, rawSecret)
	got, err := srv.st.GetCalendarAccount(context.Background(), userID)
	if err != nil {
		t.Fatalf("GetCalendarAccount: %v", err)
	}
	return got
}

// seedGoogleAccount stores an encrypted Google token row.
func seedGoogleAccount(t *testing.T, srv *Server, userID string, tok calendar.Token) store.CalendarAccount {
	t.Helper()
	raw, err := json.Marshal(tok)
	if err != nil {
		t.Fatalf("marshal token: %v", err)
	}
	return seedCalendarAccountRow(t, srv, userID, store.CalendarAccount{
		Kind: "google", Provider: "google", CalendarID: "primary",
	}, string(raw))
}

// startState drives the start endpoint and returns the state Google would echo.
func startState(t *testing.T, srv *Server, u store.User) (string, url.URL) {
	t.Helper()
	rec := googleCall(srv, u, "http://myogchat.myleshenp.dev/api/me/calendar/google/start", srv.handleGoogleCalendarStart)
	if rec.Code != http.StatusFound {
		t.Fatalf("start = %d (body %s)", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	return loc.Query().Get("state"), *loc
}

func TestGoogleCalendarStartRedirect(t *testing.T) {
	f := newFakeGoogleOAuth(t)
	srv, st := newGoogleTestServer(t, f.config())
	u := seedKeyUser(t, st, "u1", "alice", "")

	state, loc := startState(t, srv, u)
	if loc.Scheme != "https" || loc.Host != "accounts.google.com" || loc.Path != "/o/oauth2/v2/auth" {
		t.Fatalf("auth URL = %s", loc.String())
	}
	q := loc.Query()
	for k, want := range map[string]string{
		"client_id":              "client-id",
		"redirect_uri":           "https://myogchat.myleshenp.dev/api/me/calendar/google/callback",
		"response_type":          "code",
		"scope":                  "https://www.googleapis.com/auth/calendar.readonly",
		"access_type":            "offline",
		"prompt":                 "consent",
		"include_granted_scopes": "true",
	} {
		if got := q.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if state == "" {
		t.Fatal("no state in the auth URL")
	}
	// Only the readonly scope is ever requested.
	if got := q.Get("scope"); strings.Contains(got, "calendar.events") || got != calendar.GoogleReadOnlyScope {
		t.Errorf("scope = %q, want exactly %q", got, calendar.GoogleReadOnlyScope)
	}
	// The state round-trips for its own user and carries a ~10 minute expiry.
	if err := srv.verifyGoogleState(state, u.ID); err != nil {
		t.Fatalf("verifyGoogleState: %v", err)
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.SplitN(state, ".", 2)[0])
	if err != nil {
		t.Fatalf("decode state payload: %v", err)
	}
	parts := strings.Split(string(payload), "|")
	if len(parts) != 3 {
		t.Fatalf("state payload = %q, want userID|expiry|nonce", payload)
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		t.Fatalf("state expiry: %v", err)
	}
	if d := time.Until(time.Unix(exp, 0)); d < 9*time.Minute || d > 11*time.Minute {
		t.Errorf("state lifetime = %s, want ~10m", d)
	}
	// Nothing was stored and no token call happened yet.
	if _, err := srv.st.GetCalendarAccount(context.Background(), u.ID); err == nil {
		t.Error("start stored an account")
	}
	if len(f.tokenForms) != 0 {
		t.Errorf("token calls = %d, want 0", len(f.tokenForms))
	}
}

func TestGoogleCalendarStartPublicURLOverride(t *testing.T) {
	f := newFakeGoogleOAuth(t)
	g := f.config()
	g.PublicURL = "https://example.test/"
	srv, st := newGoogleTestServer(t, g)
	u := seedKeyUser(t, st, "u1", "alice", "")

	_, loc := startState(t, srv, u)
	if got := loc.Query().Get("redirect_uri"); got != "https://example.test/api/me/calendar/google/callback" {
		t.Errorf("redirect_uri = %q", got)
	}
}

func TestGoogleCalendarStartUnavailable(t *testing.T) {
	srv, st := newGoogleTestServer(t, GoogleCalendar{})
	u := seedKeyUser(t, st, "u1", "alice", "")

	rec := googleCall(srv, u, "http://myogchat.myleshenp.dev/api/me/calendar/google/start", srv.handleGoogleCalendarStart)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("start = %d, want 404 when unconfigured", rec.Code)
	}
	status := googleCall(srv, u, "http://myogchat.myleshenp.dev/api/me/calendar", srv.handleGetCalendar)
	if status.Code != http.StatusOK {
		t.Fatalf("status = %d", status.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(status.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if body["connected"] != false || body["google_available"] != false {
		t.Errorf("status = %v, want connected=false google_available=false", body)
	}
}

func TestGoogleCalendarStateRejects(t *testing.T) {
	f := newFakeGoogleOAuth(t)
	srv, st := newGoogleTestServer(t, f.config())
	u := seedKeyUser(t, st, "u1", "alice", "")
	otherStranger := seedKeyUser(t, st, "u2", "bob", "")
	state, _ := startState(t, srv, u)

	tampered := state[:len(state)-2] + "aa"
	expiredRaw := otherStranger.ID + "|" + strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10) + "|bm9uY2U"
	expiredPayload := base64.RawURLEncoding.EncodeToString([]byte(expiredRaw))
	expired := expiredPayload + "." + srv.googleStateMAC(expiredPayload)

	for _, tc := range []struct{ name, state, user, want string }{
		{"tampered", tampered, u.ID, "signature"},
		{"expired", expired, otherStranger.ID, "expired"},
		{"another user's state", state, otherStranger.ID, "another account"},
		{"garbage", "not-a-state", u.ID, "malformed"},
	} {
		rec := googleCall(srv, store.User{ID: tc.user}, "http://myogchat.myleshenp.dev/api/me/calendar/google/callback?state="+url.QueryEscape(tc.state)+"&code=x", srv.handleGoogleCalendarCallback)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", tc.name, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("%s: Content-Type = %q, want HTML (the callback lands in a browser tab)", tc.name, ct)
		}
		if !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: body = %q, want %q", tc.name, rec.Body.String(), tc.want)
		}
	}
	if len(f.tokenForms) != 0 {
		t.Errorf("a bad state still hit the token endpoint %d times", len(f.tokenForms))
	}
}

func TestGoogleCalendarCallbackStoresAccount(t *testing.T) {
	f := newFakeGoogleOAuth(t)
	srv, st := newGoogleTestServer(t, f.config())
	u := seedKeyUser(t, st, "u1", "alice", "")
	state, _ := startState(t, srv, u)

	target := "http://myogchat.myleshenp.dev/api/me/calendar/google/callback?state=" +
		url.QueryEscape(state) + "&code=auth-code-1"
	rec := googleCall(srv, u, target, srv.handleGoogleCalendarCallback)
	if rec.Code != http.StatusFound {
		t.Fatalf("callback = %d (body %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/#/settings" {
		t.Errorf("callback redirect = %q, want /#/settings", got)
	}

	// The code exchange carried the right form.
	if len(f.tokenForms) != 1 {
		t.Fatalf("token calls = %d, want 1", len(f.tokenForms))
	}
	form := f.tokenForms[0]
	for k, want := range map[string]string{
		"grant_type":    "authorization_code",
		"code":          "auth-code-1",
		"client_id":     "client-id",
		"client_secret": "client-secret",
		"redirect_uri":  "https://myogchat.myleshenp.dev/api/me/calendar/google/callback",
	} {
		if got := form.Get(k); got != want {
			t.Errorf("token form %s = %q, want %q", k, got, want)
		}
	}
	// The calendar list proved the token works.
	if len(f.apiPaths) != 1 || f.apiPaths[0] != "/users/me/calendarList" {
		t.Fatalf("api paths = %v, want [/users/me/calendarList]", f.apiPaths)
	}
	if f.authHeader[0] != "Bearer at-1" {
		t.Errorf("Authorization = %q, want Bearer at-1", f.authHeader[0])
	}

	acc, err := srv.st.GetCalendarAccount(context.Background(), u.ID)
	if err != nil {
		t.Fatalf("GetCalendarAccount: %v", err)
	}
	if acc.Kind != "google" || acc.Provider != "google" || acc.CalendarID != "primary" {
		t.Errorf("account = %+v", acc)
	}
	if acc.Endpoint != "" || acc.Username != "" || acc.HomeSet != "" {
		t.Errorf("google account carries CalDAV fields: %+v", acc)
	}
	raw, err := secret.Decrypt(srv.encKey, acc.CredentialEnc)
	if err != nil {
		t.Fatalf("decrypt credential: %v", err)
	}
	var tok calendar.Token
	if err := json.Unmarshal(raw, &tok); err != nil {
		t.Fatalf("credential is not token JSON: %v (%s)", err, raw)
	}
	if tok.AccessToken != "at-1" || tok.RefreshToken != "rt-1" || tok.Scope != calendar.GoogleReadOnlyScope {
		t.Errorf("stored token = %+v", tok)
	}
	if tok.Expiry.Before(time.Now().Add(30 * time.Minute)) {
		t.Errorf("stored expiry = %s, want ~1h ahead", tok.Expiry)
	}

	// Re-running the flow replaces the row instead of adding one.
	state2, _ := startState(t, srv, u)
	rec = googleCall(srv, u, "http://myogchat.myleshenp.dev/api/me/calendar/google/callback?state="+url.QueryEscape(state2)+"&code=auth-code-2", srv.handleGoogleCalendarCallback)
	if rec.Code != http.StatusFound {
		t.Fatalf("second callback = %d", rec.Code)
	}
	again, err := srv.st.GetCalendarAccount(context.Background(), u.ID)
	if err != nil {
		t.Fatalf("GetCalendarAccount: %v", err)
	}
	if again.ID != acc.ID || again.CreatedAt != acc.CreatedAt {
		t.Errorf("reconnect changed the row identity: %+v vs %+v", again, acc)
	}
}

func TestGoogleCalendarCallbackErrors(t *testing.T) {
	f := newFakeGoogleOAuth(t)
	srv, st := newGoogleTestServer(t, f.config())
	u := seedKeyUser(t, st, "u1", "alice", "")
	state, _ := startState(t, srv, u)

	// The user declined the consent screen.
	rec := googleCall(srv, u, "http://myogchat.myleshenp.dev/api/me/calendar/google/callback?state="+
		url.QueryEscape(state)+"&error=access_denied&error_description=The+user+denied+access", srv.handleGoogleCalendarCallback)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "did not grant access") ||
		!strings.Contains(rec.Body.String(), "The user denied access") {
		t.Fatalf("access_denied = %d %s", rec.Code, rec.Body.String())
	}
	if _, err := srv.st.GetCalendarAccount(context.Background(), u.ID); err == nil {
		t.Error("access_denied stored an account")
	}

	// Google rejects the code exchange.
	f.tokenStatus = http.StatusBadRequest
	f.tokenBody = `{"error":"invalid_grant","error_description":"Bad Request"}`
	state2, _ := startState(t, srv, u)
	rec = googleCall(srv, u, "http://myogchat.myleshenp.dev/api/me/calendar/google/callback?state="+url.QueryEscape(state2)+"&code=bad", srv.handleGoogleCalendarCallback)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "Bad Request") {
		t.Fatalf("bad code = %d %s", rec.Code, rec.Body.String())
	}
	if _, err := srv.st.GetCalendarAccount(context.Background(), u.ID); err == nil {
		t.Error("a failed exchange stored an account")
	}

	// No code and no error: a malformed callback.
	state3, _ := startState(t, srv, u)
	rec = googleCall(srv, u, "http://myogchat.myleshenp.dev/api/me/calendar/google/callback?state="+url.QueryEscape(state3), srv.handleGoogleCalendarCallback)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "no authorization code") {
		t.Fatalf("no code = %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want HTML", ct)
	}
}

func TestGoogleCalendarRevokesOnDisconnect(t *testing.T) {
	f := newFakeGoogleOAuth(t)
	srv, st := newGoogleTestServer(t, f.config())
	u := seedKeyUser(t, st, "u1", "alice", "")
	seedGoogleAccount(t, srv, u.ID, calendar.Token{
		AccessToken: "at-1", RefreshToken: "rt-1", Expiry: time.Now().Add(time.Hour),
	})

	rec := googleCall(srv, u, "http://myogchat.myleshenp.dev/api/me/calendar", srv.handleDeleteCalendar)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d (body %s)", rec.Code, rec.Body.String())
	}
	if len(f.revoked) != 1 || f.revoked[0] != "rt-1" {
		t.Fatalf("revoked = %v, want [rt-1]", f.revoked)
	}
	if _, err := srv.st.GetCalendarAccount(context.Background(), u.ID); err == nil {
		t.Error("account row survived the disconnect")
	}

	// An ICS account has no token to revoke.
	ics := seedKeyUser(t, st, "u2", "bob", "")
	seedCalendarAccount(t, srv, ics.ID, store.CalendarAccount{Kind: "ics", Provider: "google"}, "https://example.com/feed.ics")
	if rec := googleCall(srv, ics, "http://myogchat.myleshenp.dev/api/me/calendar", srv.handleDeleteCalendar); rec.Code != http.StatusNoContent {
		t.Fatalf("ics delete = %d", rec.Code)
	}
	if len(f.revoked) != 1 {
		t.Errorf("revoked = %v after an ICS disconnect, want no new call", f.revoked)
	}
}

func TestGoogleCalendarListAndPatchDefault(t *testing.T) {
	f := newFakeGoogleOAuth(t)
	srv, st := newGoogleTestServer(t, f.config())
	u := seedKeyUser(t, st, "u1", "alice", "")
	seedGoogleAccount(t, srv, u.ID, calendar.Token{
		AccessToken: "at-1", RefreshToken: "rt-1", Expiry: time.Now().Add(time.Hour),
	})

	rec := googleCall(srv, u, "http://myogchat.myleshenp.dev/api/me/calendar/calendars", srv.handleListCalendars)
	if rec.Code != http.StatusOK {
		t.Fatalf("calendars = %d (body %s)", rec.Code, rec.Body.String())
	}
	for _, want := range []string{`"default":"primary"`, `"id":"primary"`, `"id":"team@group.calendar.google.com"`, `"writable":false`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("calendars body lacks %s: %s", want, rec.Body.String())
		}
	}

	patch := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPatch, "http://myogchat.myleshenp.dev/api/me/calendar", strings.NewReader(body))
		r = r.WithContext(auth.WithUser(r.Context(), u))
		rec := httptest.NewRecorder()
		srv.handlePatchCalendar(rec, r)
		return rec
	}
	if rec := patch(`{"calendar_id":"team@group.calendar.google.com"}`); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"calendar_id":"team@group.calendar.google.com"`) {
		t.Fatalf("patch = %d %s", rec.Code, rec.Body.String())
	}
	acc, err := srv.st.GetCalendarAccount(context.Background(), u.ID)
	if err != nil || acc.CalendarID != "team@group.calendar.google.com" {
		t.Fatalf("stored default = %q, %v", acc.CalendarID, err)
	}
	if rec := patch(`{"calendar_id":"nope@example.com"}`); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "unknown calendar") {
		t.Errorf("unknown patch = %d %s", rec.Code, rec.Body.String())
	}
}

func TestGoogleCalendarSaveRefreshedToken(t *testing.T) {
	f := newFakeGoogleOAuth(t)
	srv, st := newGoogleTestServer(t, f.config())
	u := seedKeyUser(t, st, "u1", "alice", "")
	acc := seedGoogleAccount(t, srv, u.ID, calendar.Token{
		AccessToken: "old", RefreshToken: "rt-1", Expiry: time.Now().Add(time.Hour),
	})

	fresh := calendar.Token{AccessToken: "new", RefreshToken: "rt-2", Expiry: time.Now().Add(2 * time.Hour)}
	if err := srv.saveGoogleToken(context.Background(), u.ID, acc.ID, fresh); err != nil {
		t.Fatalf("saveGoogleToken: %v", err)
	}
	stored, err := srv.st.GetCalendarAccount(context.Background(), u.ID)
	if err != nil {
		t.Fatalf("GetCalendarAccount: %v", err)
	}
	raw, err := secret.Decrypt(srv.encKey, stored.CredentialEnc)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	var tok calendar.Token
	if err := json.Unmarshal(raw, &tok); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if tok.AccessToken != "new" || tok.RefreshToken != "rt-2" {
		t.Errorf("stored token = %+v, want the refreshed one", tok)
	}
	if stored.CalendarID != "primary" || stored.Kind != "google" {
		t.Errorf("refresh rewrote unrelated columns: %+v", stored)
	}

	// A row that was replaced in the meantime keeps whatever it now holds.
	if _, err := srv.st.DeleteCalendarAccount(context.Background(), u.ID); err != nil {
		t.Fatalf("DeleteCalendarAccount: %v", err)
	}
	ics := seedCalendarAccountRow(t, srv, u.ID, store.CalendarAccount{Kind: "ics", Provider: "google"}, "https://example.com/feed.ics")
	if err := srv.saveGoogleToken(context.Background(), u.ID, acc.ID, fresh); err != nil {
		t.Fatalf("saveGoogleToken (stale row): %v", err)
	}
	kept, err := srv.st.GetCalendarAccount(context.Background(), u.ID)
	if err != nil {
		t.Fatalf("GetCalendarAccount: %v", err)
	}
	if kept.ID != ics.ID || kept.Kind != "ics" {
		t.Errorf("a stale Google token overwrote the row: %+v", kept)
	}
}

func TestGoogleCalendarCrossUserIsolation(t *testing.T) {
	f := newFakeGoogleOAuth(t)
	srv, st := newGoogleTestServer(t, f.config())
	owner := seedKeyUser(t, st, "u-owner", "owner", "")
	other := seedKeyUser(t, st, "u-other", "other", "")
	seedGoogleAccount(t, srv, owner.ID, calendar.Token{
		AccessToken: "at-owner", RefreshToken: "rt-owner", Expiry: time.Now().Add(time.Hour),
	})

	for _, h := range []struct {
		name string
		path string
		fn   func(http.ResponseWriter, *http.Request)
	}{
		{"status", "/api/me/calendar", srv.handleGetCalendar},
		{"calendars", "/api/me/calendar/calendars", srv.handleListCalendars},
	} {
		rec := googleCall(srv, other, "http://myogchat.myleshenp.dev"+h.path, h.fn)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"connected":false`) {
			t.Errorf("%s for another user = %d %s, want connected:false", h.name, rec.Code, rec.Body.String())
		}
	}
	// PATCH (switch default calendar) has nothing to switch.
	patch := httptest.NewRequest(http.MethodPatch, "http://myogchat.myleshenp.dev/api/me/calendar", strings.NewReader(`{"calendar_id":"team"}`))
	patch = patch.WithContext(auth.WithUser(patch.Context(), other))
	prec := httptest.NewRecorder()
	srv.handlePatchCalendar(prec, patch)
	if prec.Code != http.StatusNotFound {
		t.Errorf("patch for another user = %d, want 404", prec.Code)
	}
	// A client cannot create a Google account with PUT.
	put := httptest.NewRequest(http.MethodPut, "http://myogchat.myleshenp.dev/api/me/calendar", strings.NewReader(`{"kind":"google"}`))
	put = put.WithContext(auth.WithUser(put.Context(), other))
	prec = httptest.NewRecorder()
	srv.handlePutCalendar(prec, put)
	if prec.Code != http.StatusBadRequest || !strings.Contains(prec.Body.String(), "sign-in") {
		t.Errorf("PUT kind=google = %d %s, want 400", prec.Code, prec.Body.String())
	}
	// The owner is untouched by any of it.
	acc, err := srv.st.GetCalendarAccount(context.Background(), owner.ID)
	if err != nil || acc.Kind != "google" {
		t.Errorf("owner account = %+v, %v", acc, err)
	}
}

func TestGoogleCalendarTriggersGate(t *testing.T) {
	f := newFakeGoogleOAuth(t)
	srv, st := newGoogleTestServer(t, f.config())
	u := seedKeyUser(t, st, "u1", "alice", "")
	acc := seedGoogleAccount(t, srv, u.ID, calendar.Token{
		AccessToken: "at-1", RefreshToken: "rt-1", Expiry: time.Now().Add(time.Hour),
	})

	// The view reports the account as connected and PUT enabled succeeds.
	view := getTrigger(t, srv, u)
	if !view.Connected {
		t.Fatal("Connected = false for a Google account")
	}
	rec := triggerCall(srv, u, http.MethodPut, "/api/me/calendar/trigger", `{"enabled":true}`, srv.handlePutCalendarTrigger)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"enabled":true`) {
		t.Fatalf("enable trigger = %d %s", rec.Code, rec.Body.String())
	}

	// The scheduler reads Google events (the gate lets it through).
	f.apiPaths = nil
	srv.runCalendarTrigger(context.Background(), store.CalendarTrigger{
		UserID: u.ID, Enabled: true, Model: triggerDefaultModel, Account: &acc,
	}, time.Now())
	if len(f.apiPaths) != 1 || f.apiPaths[0] != "/calendars/primary/events" {
		t.Fatalf("trigger api paths = %v, want one events.list", f.apiPaths)
	}

	// ICS stays excluded, even though the account is otherwise valid.
	ics := seedKeyUser(t, st, "u2", "bob", "")
	icsAcc := seedCalendarAccountRow(t, srv, ics.ID, store.CalendarAccount{Kind: "ics", Provider: "google"}, "https://example.com/feed.ics")
	f.apiPaths = nil
	srv.runCalendarTrigger(context.Background(), store.CalendarTrigger{
		UserID: ics.ID, Enabled: true, Model: triggerDefaultModel, Account: &icsAcc,
	}, time.Now())
	if len(f.apiPaths) != 0 {
		t.Errorf("an ICS account reached the Google API: %v", f.apiPaths)
	}
	if got := getTrigger(t, srv, ics); got.Connected {
		t.Error("Connected = true for an ICS account")
	}
	if rec := triggerCall(srv, ics, http.MethodPut, "/api/me/calendar/trigger", `{"enabled":true}`, srv.handlePutCalendarTrigger); rec.Code != http.StatusBadRequest {
		t.Errorf("enabling a trigger on ICS = %d, want 400", rec.Code)
	}

	// The kinds that can drive a trigger, asserted directly.
	for kind, want := range map[string]bool{"caldav": true, "google": true, "ics": false, "": false} {
		if got := triggerCalendarKind(kind); got != want {
			t.Errorf("triggerCalendarKind(%q) = %v, want %v", kind, got, want)
		}
	}
}
