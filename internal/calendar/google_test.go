package calendar

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordedReq is one Calendar API request the fake server saw.
type recordedReq struct {
	path  string
	query url.Values
	auth  string
}

// fakeGoogle is a fake OAuth token endpoint plus a fake Calendar v3 API. Every
// test asserts on the wire shape (path, query, headers, form), so the fakes
// record what they were sent.
type fakeGoogle struct {
	api   *httptest.Server
	token *httptest.Server

	mu         sync.Mutex
	apiReqs    []recordedReq
	tokenForms []url.Values

	calendarsJSON string
	pages         map[string]string // pageToken ("" = first page) → events.list body
	tokenJSON     string
	tokenStatus   int
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	t.Helper()
	f := &fakeGoogle{
		tokenStatus: http.StatusOK,
		tokenJSON:   `{"access_token":"fresh-token","expires_in":3600,"token_type":"Bearer","scope":"` + GoogleReadOnlyScope + `"}`,
	}
	f.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.apiReqs = append(f.apiReqs, recordedReq{path: r.URL.Path, query: r.URL.Query(), auth: r.Header.Get("Authorization")})
		cals, pages := f.calendarsJSON, f.pages
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/calendarList") {
			_, _ = io.WriteString(w, cals)
			return
		}
		if body, ok := pages[r.URL.Query().Get("pageToken")]; ok {
			_, _ = io.WriteString(w, body)
			return
		}
		_, _ = io.WriteString(w, `{"items":[]}`)
	}))
	f.token = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("token endpoint: %v", err)
		}
		f.mu.Lock()
		f.tokenForms = append(f.tokenForms, r.PostForm)
		status, body := f.tokenStatus, f.tokenJSON
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(func() {
		f.api.Close()
		f.token.Close()
	})
	return f
}

// backend wires a googleBackend to the fakes.
func (f *fakeGoogle) backend(t *testing.T, tok Token, save func(Token) error) *googleBackend {
	t.Helper()
	b, err := newGoogle(Config{
		Kind:         KindGoogle,
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		Token:        tok,
		TokenURL:     f.token.URL,
		APIBase:      f.api.URL,
		HTTPClient:   f.api.Client(),
	})
	if err != nil {
		t.Fatalf("newGoogle: %v", err)
	}
	b.saveToken = save
	return b
}

// liveToken is a token that needs no refresh.
func liveToken() Token {
	return Token{AccessToken: "access-token", RefreshToken: "refresh-token",
		Expiry: time.Now().Add(time.Hour), TokenType: "Bearer", Scope: GoogleReadOnlyScope}
}

func TestGoogleCalendars(t *testing.T) {
	f := newFakeGoogle(t)
	f.calendarsJSON = `{"items":[
	  {"id":"me@example.com","summary":"Me","description":"Personal","primary":true,"accessRole":"owner"},
	  {"id":"team@group.calendar.google.com","summary":"Team"},
	  {"id":"","summary":"no id"}
	]}`
	b := f.backend(t, liveToken(), nil)

	cals, err := b.Calendars(context.Background())
	if err != nil {
		t.Fatalf("Calendars: %v", err)
	}
	// The primary calendar is stored by its stable alias, which Google accepts
	// as a calendar id everywhere.
	want := []Calendar{
		{ID: "primary", Name: "Me", Description: "Personal", Writable: false},
		{ID: "team@group.calendar.google.com", Name: "Team", Writable: false},
	}
	if !reflect.DeepEqual(cals, want) {
		t.Fatalf("Calendars = %+v, want %+v", cals, want)
	}
	if len(f.apiReqs) != 1 {
		t.Fatalf("api requests = %d, want 1", len(f.apiReqs))
	}
	if got := f.apiReqs[0].path; got != "/users/me/calendarList" {
		t.Errorf("path = %q, want /users/me/calendarList", got)
	}
	if got := f.apiReqs[0].auth; got != "Bearer access-token" {
		t.Errorf("Authorization = %q, want Bearer access-token", got)
	}
}

func TestGoogleListWireShapeAndMapping(t *testing.T) {
	f := newFakeGoogle(t)
	f.pages = map[string]string{"": `{"items":[
	  {"id":"ev-timed","iCalUID":"uid-timed@google.com","summary":"Team sync",
	   "description":"Weekly planning","location":"Room 1","status":"confirmed",
	   "start":{"dateTime":"2026-03-02T09:00:00-08:00","timeZone":"America/Los_Angeles"},
	   "end":{"dateTime":"2026-03-02T09:30:00-08:00","timeZone":"America/Los_Angeles"},
	   "attendees":[{"email":"a@example.com"},{"email":"b@example.com"},{"email":""}]},
	  {"id":"ev-allday","summary":"Holiday","status":"confirmed",
	   "start":{"date":"2026-05-01"},"end":{"date":"2026-05-02"}},
	  {"id":"ev-cancelled","status":"cancelled","start":{"date":"2026-05-03"},"end":{"date":"2026-05-04"}}
	]}`}
	b := f.backend(t, liveToken(), nil)

	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	events, err := b.List(context.Background(), ListQuery{From: from, To: to})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	// Wire shape: the query the Calendar API actually received.
	if len(f.apiReqs) != 1 {
		t.Fatalf("api requests = %d, want 1", len(f.apiReqs))
	}
	req := f.apiReqs[0]
	if req.path != "/calendars/primary/events" {
		t.Errorf("path = %q, want /calendars/primary/events", req.path)
	}
	if req.auth != "Bearer access-token" {
		t.Errorf("Authorization = %q", req.auth)
	}
	for k, want := range map[string]string{
		"timeMin":      "2026-03-01T00:00:00Z",
		"timeMax":      "2026-04-01T00:00:00Z",
		"singleEvents": "true",
		"orderBy":      "startTime",
		"showDeleted":  "false",
		"maxResults":   "250",
	} {
		if got := req.query.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if req.query.Has("pageToken") {
		t.Errorf("first page sent a pageToken: %q", req.query.Get("pageToken"))
	}

	// Mapping: one timed, one all-day; the cancelled event is dropped.
	if len(events) != 2 {
		t.Fatalf("events = %d (%+v), want 2", len(events), events)
	}
	timed := events[0]
	if timed.UID != "uid-timed@google.com" || timed.Title != "Team sync" ||
		timed.Description != "Weekly planning" || timed.Location != "Room 1" ||
		timed.AllDay || timed.TimeZone != "America/Los_Angeles" {
		t.Errorf("timed event = %+v", timed)
	}
	wantStart := time.Date(2026, 3, 2, 17, 0, 0, 0, time.UTC)
	if !timed.Start.Equal(wantStart) || !timed.End.Equal(wantStart.Add(30*time.Minute)) {
		t.Errorf("timed window = %s..%s, want %s..%s", timed.Start, timed.End, wantStart, wantStart.Add(30*time.Minute))
	}
	if !reflect.DeepEqual(timed.Attendees, []string{"a@example.com", "b@example.com"}) {
		t.Errorf("attendees = %v", timed.Attendees)
	}
	if timed.ETag != "" {
		t.Errorf("etag = %q, want empty", timed.ETag)
	}

	allDay := events[1]
	if allDay.UID != "ev-allday" || !allDay.AllDay || allDay.TimeZone != "" {
		t.Errorf("all-day event = %+v", allDay)
	}
	if !allDay.Start.Equal(time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)) ||
		!allDay.End.Equal(time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("all-day window = %s..%s", allDay.Start, allDay.End)
	}
}

func TestGoogleListFollowsPageToken(t *testing.T) {
	f := newFakeGoogle(t)
	f.pages = map[string]string{
		"":       `{"items":[{"id":"e1","summary":"One","start":{"date":"2026-03-02"},"end":{"date":"2026-03-03"}}],"nextPageToken":"page-2"}`,
		"page-2": `{"items":[{"id":"e2","summary":"Two","start":{"date":"2026-03-04"},"end":{"date":"2026-03-05"}}]}`,
	}
	b := f.backend(t, liveToken(), nil)
	events, err := b.List(context.Background(), ListQuery{
		From: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(events) != 2 || events[0].UID != "e1" || events[1].UID != "e2" {
		t.Fatalf("events = %+v", events)
	}
	if len(f.apiReqs) != 2 {
		t.Fatalf("api requests = %d, want 2", len(f.apiReqs))
	}
	if got := f.apiReqs[1].query.Get("pageToken"); got != "page-2" {
		t.Errorf("second page pageToken = %q, want page-2", got)
	}
	// Every page keeps the same filter and auth.
	if f.apiReqs[1].query.Get("singleEvents") != "true" || f.apiReqs[1].auth != "Bearer access-token" {
		t.Errorf("second page request = %+v", f.apiReqs[1])
	}
}

func TestGoogleRefreshesNearExpiryOnceAndPersists(t *testing.T) {
	f := newFakeGoogle(t)
	f.calendarsJSON = `{"items":[{"id":"me@example.com","summary":"Me","primary":true}]}`
	var saved []Token
	tok := Token{AccessToken: "stale-token", RefreshToken: "refresh-1",
		Expiry: time.Now().Add(30 * time.Second)} // inside the 2-minute skew
	b := f.backend(t, tok, func(t Token) error {
		saved = append(saved, t)
		return nil
	})

	ctx := context.Background()
	if _, err := b.Calendars(ctx); err != nil {
		t.Fatalf("Calendars: %v", err)
	}
	if len(f.tokenForms) != 1 {
		t.Fatalf("token calls = %d, want exactly 1", len(f.tokenForms))
	}
	form := f.tokenForms[0]
	for k, want := range map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": "refresh-1",
		"client_id":     "client-id",
		"client_secret": "client-secret",
	} {
		if got := form.Get(k); got != want {
			t.Errorf("token form %s = %q, want %q", k, got, want)
		}
	}
	if got := f.apiReqs[0].auth; got != "Bearer fresh-token" {
		t.Errorf("Authorization = %q, want the refreshed token", got)
	}
	if len(saved) != 1 {
		t.Fatalf("SaveToken calls = %d, want 1", len(saved))
	}
	if saved[0].AccessToken != "fresh-token" || saved[0].RefreshToken != "refresh-1" {
		t.Errorf("saved token = %+v", saved[0])
	}
	if !saved[0].Expiry.After(time.Now().Add(30 * time.Minute)) {
		t.Errorf("saved expiry = %s, want ~1h ahead", saved[0].Expiry)
	}

	// A second call reuses the refreshed token: no second refresh.
	if _, err := b.Calendars(ctx); err != nil {
		t.Fatalf("Calendars (second): %v", err)
	}
	if len(f.tokenForms) != 1 {
		t.Errorf("token calls after second read = %d, want 1", len(f.tokenForms))
	}
	if got := f.apiReqs[1].auth; got != "Bearer fresh-token" {
		t.Errorf("second Authorization = %q", got)
	}
}

func TestGoogleRefreshRejected(t *testing.T) {
	f := newFakeGoogle(t)
	f.tokenStatus = http.StatusBadRequest
	f.tokenJSON = `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`
	called := false
	b := f.backend(t, Token{AccessToken: "stale", RefreshToken: "refresh-1", Expiry: time.Now().Add(-time.Hour)},
		func(Token) error { called = true; return nil })

	_, err := b.Calendars(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Token has been expired or revoked.") {
		t.Fatalf("Calendars error = %v, want Google's invalid_grant message", err)
	}
	if called {
		t.Error("SaveToken called after a failed refresh")
	}
	if len(f.apiReqs) != 0 {
		t.Errorf("calendar API called %d times after a failed refresh, want 0", len(f.apiReqs))
	}
}

func TestGoogleExpiredWithoutRefreshToken(t *testing.T) {
	f := newFakeGoogle(t)
	b := f.backend(t, Token{AccessToken: "stale", Expiry: time.Now().Add(-time.Hour)}, nil)
	_, err := b.Calendars(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no refresh token") {
		t.Fatalf("Calendars error = %v, want a reconnect hint", err)
	}
	if len(f.tokenForms) != 0 {
		t.Errorf("token calls = %d, want 0", len(f.tokenForms))
	}
}

func TestGoogleReadOnly(t *testing.T) {
	f := newFakeGoogle(t)
	b := f.backend(t, liveToken(), nil)
	ctx := context.Background()
	if b.Writable() {
		t.Fatal("Writable() = true, want false")
	}
	ev := Event{UID: "x", Title: "x"}
	if _, err := b.Create(ctx, ev); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Create = %v, want ErrReadOnly", err)
	}
	if _, err := b.Update(ctx, ev); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Update = %v, want ErrReadOnly", err)
	}
	if err := b.Delete(ctx, "x", "", ""); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Delete = %v, want ErrReadOnly", err)
	}
	if !strings.Contains(ErrReadOnly.Error(), "read-only") {
		t.Errorf("ErrReadOnly = %q, want a clear read-only message", ErrReadOnly)
	}
	if len(f.apiReqs) != 0 {
		t.Errorf("write attempt hit the API %d times, want 0", len(f.apiReqs))
	}
}

func TestGoogleTokenJSONShape(t *testing.T) {
	// The credential stored in calendar_accounts is this JSON (encrypted).
	tok := Token{AccessToken: "a", RefreshToken: "r", TokenType: "Bearer",
		Scope: GoogleReadOnlyScope, Expiry: time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)}
	raw, err := json.Marshal(tok)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	for _, k := range []string{"access_token", "refresh_token", "expiry", "token_type", "scope"} {
		if _, ok := back[k]; !ok {
			t.Errorf("token JSON lacks %q: %s", k, raw)
		}
	}
	if got := back["expiry"]; got != "2026-03-02T09:00:00Z" {
		t.Errorf("expiry = %v, want RFC3339", got)
	}
}
