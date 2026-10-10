package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
