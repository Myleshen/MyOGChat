package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"myogchat/internal/auth"
	"myogchat/internal/calendar"
	"myogchat/internal/secret"
	"myogchat/internal/store"
)

const serverTestICS = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//T//EN
BEGIN:VEVENT
UID:srv-1
SUMMARY:Server event
DTSTART:20260302T090000Z
DTEND:20260302T093000Z
END:VEVENT
END:VCALENDAR
`

// seedCalendarAccount stores an encrypted calendar account for a user.
func seedCalendarAccount(t *testing.T, srv *Server, userID string, acc store.CalendarAccount, rawSecret string) {
	t.Helper()
	enc, err := secret.Encrypt(srv.encKey, []byte(rawSecret))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	acc.ID = "cal-" + userID
	acc.UserID = userID
	acc.CredentialEnc = enc
	acc.CreatedAt, acc.UpdatedAt = 1, 1
	if err := srv.st.UpsertCalendarAccount(context.Background(), &acc); err != nil {
		t.Fatalf("UpsertCalendarAccount: %v", err)
	}
}

func TestCalendarBackendNoAccount(t *testing.T) {
	srv, st := newKeyTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "")
	if _, ok := srv.calendarBackend(context.Background(), u.ID); ok {
		t.Fatal("expected no backend without an account")
	}
	// The calendar tools must not be registered.
	reg := srv.toolRegistry(context.Background(), u)
	for _, name := range []string{"calendar_list", "calendar_create", "calendar_update", "calendar_delete"} {
		if _, ok := reg.Get(name); ok {
			t.Errorf("%s registered without a connected calendar", name)
		}
	}
}

func TestCalendarBackendICSAndTools(t *testing.T) {
	srv, st := newKeyTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "")

	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/calendar")
		_, _ = w.Write([]byte(serverTestICS))
	}))
	t.Cleanup(feed.Close)

	seedCalendarAccount(t, srv, u.ID, store.CalendarAccount{Kind: "ics", Provider: "google"}, feed.URL+"/basic.ics")

	b, ok := srv.calendarBackend(context.Background(), u.ID)
	if !ok {
		t.Fatal("expected an ICS backend")
	}
	if b.Writable() {
		t.Error("ICS backend must be read-only")
	}

	reg := srv.toolRegistry(context.Background(), u)
	for _, name := range []string{"calendar_list", "calendar_create", "calendar_update", "calendar_delete"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("%s not registered with a connected calendar", name)
		}
	}
	// A write tool must reject an ICS calendar with a clear error.
	create, _ := reg.Get("calendar_create")
	if _, err := create.Run(context.Background(), json.RawMessage(`{"title":"x","start":"2026-03-02T09:00:00Z","end":"2026-03-02T10:00:00Z"}`)); !errors.Is(err, calendar.ErrReadOnly) {
		t.Errorf("create on ICS = %v, want ErrReadOnly", err)
	}
	// A read tool works.
	list, _ := reg.Get("calendar_list")
	if _, err := list.Run(context.Background(), json.RawMessage(`{"from":"2026-03-01","to":"2026-03-03"}`)); err != nil {
		t.Errorf("list on ICS: %v", err)
	}
}

func TestCalendarBackendCalDAVBuilds(t *testing.T) {
	srv, st := newKeyTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "")
	// No network: newCalDAV does not connect. Writable() must be true.
	seedCalendarAccount(t, srv, u.ID, store.CalendarAccount{
		Kind: "caldav", Provider: "apple", Endpoint: "https://caldav.icloud.com",
		Username: "alice@example.com", CalendarID: "/123/calendars/home/",
	}, "app-password")

	b, ok := srv.calendarBackend(context.Background(), u.ID)
	if !ok {
		t.Fatal("expected a CalDAV backend")
	}
	if !b.Writable() {
		t.Error("CalDAV backend must be writable")
	}
}

func TestCalendarStatusJSONHidesSecret(t *testing.T) {
	acc := store.CalendarAccount{
		ID: "c1", Kind: "ics", Provider: "google",
		CredentialEnc: "super-secret-ciphertext", CalendarID: "label",
	}
	out := calendarStatusJSON(acc)
	if _, ok := out["credential"]; ok {
		t.Fatal("status must not expose the credential")
	}
	blob, _ := json.Marshal(out)
	if strings.Contains(string(blob), "super-secret-ciphertext") {
		t.Fatalf("status leaked the secret: %s", blob)
	}
	if out["connected"] != true || out["can_write"] != false {
		t.Fatalf("status = %v", out)
	}
}

// fakeCalendar is one collection the fake CalDAV server lists.
type fakeCalendar struct{ path, name string }

// fakeCalDAVServer records how many discovery (depth 0) and calendar-list
// (depth 1) PROPFINDs the client made.
type fakeCalDAVServer struct {
	URL                string
	discovery, listing int
}

// fakeCalDAV serves the discovery PROPFINDs go-webdav issues when connecting or
// listing an account; event requests are never made in these tests.
func fakeCalDAV(t *testing.T, cals ...fakeCalendar) *fakeCalDAVServer {
	t.Helper()
	var list strings.Builder
	for _, c := range cals {
		fmt.Fprintf(&list, `<D:response><D:href>%s</D:href><D:propstat><D:prop>`+
			`<D:resourcetype><D:collection/><C:calendar/></D:resourcetype>`+
			`<D:displayname>%s</D:displayname>`+
			`<C:calendar-description>%s description</C:calendar-description>`+
			`</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`, c.path, c.name, c.name)
	}
	const discovery = `<?xml version="1.0" encoding="utf-8"?>` +
		`<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">` +
		`<D:response><D:href>/</D:href><D:propstat><D:prop>` +
		`<D:current-user-principal><D:href>/user/</D:href></D:current-user-principal>` +
		`<C:calendar-home-set><D:href>/user/calendars/</D:href></C:calendar-home-set>` +
		`</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>` +
		`</D:multistatus>`
	listXML := `<?xml version="1.0" encoding="utf-8"?>` +
		`<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">` +
		list.String() + `</D:multistatus>`
	fake := &fakeCalDAVServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PROPFIND" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
		w.WriteHeader(http.StatusMultiStatus)
		if r.Header.Get("Depth") == "1" {
			fake.listing++
			_, _ = io.WriteString(w, listXML)
			return
		}
		fake.discovery++
		_, _ = io.WriteString(w, discovery)
	}))
	t.Cleanup(srv.Close)
	fake.URL = srv.URL
	return fake
}

// TestCalendarPutValidatesAndCaches proves PUT rejects an unknown collection,
// and otherwise stores the canonical collection plus the discovery cache.
func TestCalendarPutValidatesAndCaches(t *testing.T) {
	srv, st := newKeyTestServer(t)
	u := seedKeyUser(t, st, "u-cal1", "cal1", "")
	base := fakeCalDAV(t,
		fakeCalendar{"/user/calendars/personal/", "Personal"},
		fakeCalendar{"/user/calendars/work/", "Work"},
	)
	put := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/api/me/calendar", strings.NewReader(body))
		req = req.WithContext(auth.WithUser(req.Context(), u))
		srv.handlePutCalendar(rec, req)
		return rec
	}

	rec := put(`{"kind":"caldav","endpoint":"` + base.URL + `","username":"me@example.com","password":"pw","calendar_id":"/user/calendars/nope/"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unknown calendar") {
		t.Fatalf("unknown calendar: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := st.GetCalendarAccount(context.Background(), u.ID); err != store.ErrNotFound {
		t.Fatalf("account stored despite an invalid calendar: %v", err)
	}

	rec = put(`{"kind":"caldav","endpoint":"` + base.URL + `","username":"me@example.com","password":"pw","calendar_id":"/user/calendars/work"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"calendars":[`) {
		t.Errorf("put response missing calendars: %s", rec.Body.String())
	}
	acc, err := st.GetCalendarAccount(context.Background(), u.ID)
	if err != nil {
		t.Fatalf("GetCalendarAccount: %v", err)
	}
	if acc.CalendarID != "/user/calendars/work/" {
		t.Errorf("calendar_id = %q, want canonical /user/calendars/work/", acc.CalendarID)
	}
	if acc.HomeSet != "/user/calendars/" {
		t.Errorf("home_set = %q, want /user/calendars/", acc.HomeSet)
	}
	if acc.Endpoint != base.URL {
		t.Errorf("endpoint = %q, want %q", acc.Endpoint, base.URL)
	}
}

// TestCalendarPatchSwitchesDefault proves PATCH validates the collection,
// switches the default and refreshes the cache without touching the credential.
func TestCalendarPatchSwitchesDefault(t *testing.T) {
	srv, st := newKeyTestServer(t)
	u := seedKeyUser(t, st, "u-cal2", "cal2", "")
	base := fakeCalDAV(t,
		fakeCalendar{"/user/calendars/personal/", "Personal"},
		fakeCalendar{"/user/calendars/work/", "Work"},
	)
	// An account from before the cache existed: no home_set stored.
	seedCalendarAccount(t, srv, u.ID, store.CalendarAccount{
		Kind: "caldav", Provider: "apple", Endpoint: base.URL,
		Username: "me@example.com", CalendarID: "/user/calendars/personal/",
	}, "pw")
	before, err := st.GetCalendarAccount(context.Background(), u.ID)
	if err != nil {
		t.Fatalf("GetCalendarAccount: %v", err)
	}

	patch := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPatch, "/api/me/calendar", strings.NewReader(body))
		req = req.WithContext(auth.WithUser(req.Context(), u))
		srv.handlePatchCalendar(rec, req)
		return rec
	}

	rec := patch(`{"calendar_id":"/user/calendars/work/"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["calendar_id"] != "/user/calendars/work/" {
		t.Errorf("response calendar_id = %v", out["calendar_id"])
	}
	acc, err := st.GetCalendarAccount(context.Background(), u.ID)
	if err != nil {
		t.Fatalf("GetCalendarAccount: %v", err)
	}
	if acc.CalendarID != "/user/calendars/work/" || acc.HomeSet != "/user/calendars/" {
		t.Errorf("stored account = %+v", acc)
	}
	if acc.CredentialEnc != before.CredentialEnc {
		t.Error("PATCH must not rewrite the credential")
	}

	rec = patch(`{"calendar_id":"/user/calendars/nope/"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unknown calendar") {
		t.Fatalf("unknown calendar: %d %s", rec.Code, rec.Body.String())
	}
	if acc, _ := st.GetCalendarAccount(context.Background(), u.ID); acc.CalendarID != "/user/calendars/work/" {
		t.Errorf("rejected PATCH changed the default: %q", acc.CalendarID)
	}
}

// TestCalendarPatchErrors covers the not-connected and ICS cases.
func TestCalendarPatchErrors(t *testing.T) {
	srv, st := newKeyTestServer(t)
	u := seedKeyUser(t, st, "u-cal3", "cal3", "")
	patch := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPatch, "/api/me/calendar", strings.NewReader(`{"calendar_id":"x"}`))
		req = req.WithContext(auth.WithUser(req.Context(), u))
		srv.handlePatchCalendar(rec, req)
		return rec
	}

	if rec := patch(); rec.Code != http.StatusNotFound {
		t.Fatalf("patch without an account: %d, want 404", rec.Code)
	}
	seedCalendarAccount(t, srv, u.ID, store.CalendarAccount{Kind: "ics", Provider: "google", CalendarID: "feed"}, "https://example.com/feed.ics")
	rec := patch()
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "single calendar") {
		t.Fatalf("patch on ICS: %d %s", rec.Code, rec.Body.String())
	}
}

type calendarListJSON struct {
	Connected bool   `json:"connected"`
	Default   string `json:"default"`
	Calendars []struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Writable    bool   `json:"writable"`
	} `json:"calendars"`
}

// TestCalendarListCalendars proves /calendars lists the account's collections
// for the owner only, and the single synthetic entry for ICS.
func TestCalendarListCalendars(t *testing.T) {
	srv, st := newKeyTestServer(t)
	owner := seedKeyUser(t, st, "u-cal4", "cal4", "")
	other := seedKeyUser(t, st, "u-cal5", "cal5", "")
	ics := seedKeyUser(t, st, "u-cal6", "cal6", "")
	base := fakeCalDAV(t,
		fakeCalendar{"/user/calendars/personal/", "Personal"},
		fakeCalendar{"/user/calendars/work/", "Work"},
	)
	seedCalendarAccount(t, srv, owner.ID, store.CalendarAccount{
		Kind: "caldav", Endpoint: base.URL, Username: "me@example.com",
		CalendarID: "/user/calendars/work/", HomeSet: "/user/calendars/",
	}, "pw")
	seedCalendarAccount(t, srv, ics.ID, store.CalendarAccount{Kind: "ics", Provider: "google", CalendarID: "work-feed"}, "https://example.com/feed.ics")

	list := func(u store.User) (int, calendarListJSON) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/me/calendar/calendars", nil)
		req = req.WithContext(auth.WithUser(req.Context(), u))
		srv.handleListCalendars(rec, req)
		var out calendarListJSON
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	code, got := list(owner)
	if code != http.StatusOK {
		t.Fatalf("list: %d", code)
	}
	if !got.Connected || got.Default != "/user/calendars/work/" {
		t.Fatalf("list = %+v", got)
	}
	if len(got.Calendars) != 2 {
		t.Fatalf("calendars = %+v, want 2", got.Calendars)
	}
	// A cached home set means listing costs one PROPFIND and no discovery.
	if base.discovery != 0 || base.listing != 1 {
		t.Errorf("cached listing did %d discovery / %d list requests, want 0 / 1", base.discovery, base.listing)
	}
	if got.Calendars[1].ID != "/user/calendars/work/" || got.Calendars[1].Name != "Work" ||
		got.Calendars[1].Description != "Work description" || !got.Calendars[1].Writable {
		t.Errorf("calendar entry = %+v", got.Calendars[1])
	}

	// Another user's account is never visible.
	code, got = list(other)
	if code != http.StatusOK || got.Connected || len(got.Calendars) != 0 {
		t.Fatalf("cross-user list = %d %+v", code, got)
	}

	// ICS exposes its single feed under the stored label.
	code, got = list(ics)
	if code != http.StatusOK || !got.Connected || got.Default != "work-feed" {
		t.Fatalf("ics list = %d %+v", code, got)
	}
	if len(got.Calendars) != 1 || got.Calendars[0].ID != "work-feed" || got.Calendars[0].Writable {
		t.Errorf("ics calendars = %+v", got.Calendars)
	}
}
