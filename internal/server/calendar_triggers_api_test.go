package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"myogchat/internal/auth"
	"myogchat/internal/store"
)

// triggerCall drives one trigger handler with an authed request.
func triggerCall(srv *Server, u store.User, method, path, body string,
	h func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r = r.WithContext(auth.WithUser(r.Context(), u))
	rec := httptest.NewRecorder()
	h(rec, r)
	return rec
}

// getTrigger decodes GET /api/me/calendar/trigger.
func getTrigger(t *testing.T, srv *Server, u store.User) calendarTriggerJSON {
	t.Helper()
	rec := triggerCall(srv, u, http.MethodGet, "/api/me/calendar/trigger", "", srv.handleGetCalendarTrigger)
	if rec.Code != http.StatusOK {
		t.Fatalf("get trigger = %d (body %s)", rec.Code, rec.Body.String())
	}
	var out calendarTriggerJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode trigger: %v", err)
	}
	return out
}

// TestCalendarTriggerEndpoints covers the config lifecycle: defaults, the
// validation rejects, a valid save, the run list and the idempotent delete.
func TestCalendarTriggerEndpoints(t *testing.T) {
	srv, st := newKeyTestServer(t)
	owner := seedKeyUser(t, st, "u-owner", "owner", "")
	other := seedKeyUser(t, st, "u-other", "other", "")
	ics := seedKeyUser(t, st, "u-ics", "ics", "")
	ctx := context.Background()

	seedTriggerCalendar(t, srv, owner.ID, fakeTriggerCalendar(t).URL)
	seedCalendarAccount(t, srv, ics.ID, store.CalendarAccount{Kind: "ics", Provider: "google"}, "https://example.com/feed.ics")
	mkProject(t, st, owner, "p-owner", "", "", false)
	mkProject(t, st, other, "p-other", "", "", false)

	// Defaults: off, the default model and template, 15-minute lead.
	got := getTrigger(t, srv, owner)
	if got.Enabled || got.Model != triggerDefaultModel || got.LeadMinutes != 15 ||
		!got.Connected || got.PromptTemplate == "" || got.ProjectID != nil ||
		got.QuietStart != nil || got.LastError != "" {
		t.Fatalf("defaults = %+v", got)
	}

	put := func(u store.User, body string) *httptest.ResponseRecorder {
		return triggerCall(srv, u, http.MethodPut, "/api/me/calendar/trigger", body, srv.handlePutCalendarTrigger)
	}
	for _, tc := range []struct {
		name, body, want string
		u                store.User
	}{
		{"unknown model", `{"model":"nope"}`, "unknown model", owner},
		{"unknown project", `{"project_id":"p-missing"}`, "unknown project", owner},
		{"another user's project", `{"project_id":"p-other"}`, "unknown project", owner},
		{"bad quiet start", `{"quiet_start":"25:00","quiet_end":"06:00"}`, "HH:MM", owner},
		{"quiet start only", `{"quiet_start":"22:00"}`, "set together", owner},
		{"quiet end only", `{"quiet_end":"06:00"}`, "set together", owner},
		{"enable without caldav", `{"enabled":true}`, "CalDAV", ics},
	} {
		if rec := put(tc.u, tc.body); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s = %d %s, want 400 mentioning %q", tc.name, rec.Code, rec.Body.String(), tc.want)
		}
	}
	// None of the rejected writes were stored.
	if got := getTrigger(t, srv, owner); got.Enabled || got.Model != triggerDefaultModel {
		t.Fatalf("rejected PUT persisted: %+v", got)
	}

	// `enabled` alone is a valid save; the rest keeps its values.
	rec := put(owner, `{"enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable = %d (body %s)", rec.Code, rec.Body.String())
	}
	if got := getTrigger(t, srv, owner); !got.Enabled {
		t.Fatalf("after enable = %+v", got)
	}

	rec = put(owner, `{"model":"deepseek-v4.1-flash","project_id":"p-owner","quiet_start":"22:00","quiet_end":"06:00","prompt_template":"Prepare: {{summary}}"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("save = %d (body %s)", rec.Code, rec.Body.String())
	}
	got = getTrigger(t, srv, owner)
	if got.Model != "deepseek-v4.1-flash" || got.ProjectID == nil || *got.ProjectID != "p-owner" ||
		got.QuietStart == nil || *got.QuietStart != "22:00" || got.QuietEnd == nil || *got.QuietEnd != "06:00" ||
		got.PromptTemplate != "Prepare: {{summary}}" {
		t.Fatalf("saved config = %+v", got)
	}

	// Clearing the project and the quiet window.
	if rec := put(owner, `{"project_id":"","quiet_start":"","quiet_end":""}`); rec.Code != http.StatusOK {
		t.Fatalf("clear = %d (body %s)", rec.Code, rec.Body.String())
	}
	if got := getTrigger(t, srv, owner); got.ProjectID != nil || got.QuietStart != nil || got.QuietEnd != nil {
		t.Fatalf("after clear = %+v", got)
	}

	// Run list: newest first, owner-scoped, limit clamped to 1..50.
	for i, id := range []string{"r1", "r2", "r3"} {
		run := &store.TriggerRun{
			ID: id, UserID: owner.ID, EventUID: "e-" + id, EventStart: time.Now().UnixMilli(),
			Summary: "S" + id, Status: triggerFired, CreatedAt: int64(i + 1),
		}
		if _, err := st.InsertTriggerRun(ctx, run); err != nil {
			t.Fatalf("InsertTriggerRun: %v", err)
		}
	}
	if _, err := st.InsertTriggerRun(ctx, &store.TriggerRun{
		ID: "r-other", UserID: other.ID, EventUID: "e-other", EventStart: 1,
		Summary: "Other", Status: triggerFired, CreatedAt: 10,
	}); err != nil {
		t.Fatalf("InsertTriggerRun(other): %v", err)
	}
	runs := func(u store.User, query string) []triggerRunJSON {
		rec := triggerCall(srv, u, http.MethodGet, "/api/me/calendar/trigger/runs"+query, "", srv.handleListCalendarTriggerRuns)
		if rec.Code != http.StatusOK {
			t.Fatalf("runs%s = %d (body %s)", query, rec.Code, rec.Body.String())
		}
		var out struct {
			Runs []triggerRunJSON `json:"runs"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode runs: %v", err)
		}
		return out.Runs
	}
	gotRuns := runs(owner, "?limit=2")
	if len(gotRuns) != 2 || gotRuns[0].ID != "r3" || gotRuns[1].ID != "r2" {
		t.Fatalf("runs = %+v, want the two newest", gotRuns)
	}
	if gotRuns[0].Summary != "Sr3" || gotRuns[0].Start == "" {
		t.Errorf("run shape = %+v", gotRuns[0])
	}
	if otherRuns := runs(other, ""); len(otherRuns) != 1 || otherRuns[0].ID != "r-other" {
		t.Fatalf("other user's runs = %+v, want only their own", otherRuns)
	}
	if rec := triggerCall(srv, owner, http.MethodGet, "/api/me/calendar/trigger/runs?limit=x", "", srv.handleListCalendarTriggerRuns); rec.Code != http.StatusBadRequest {
		t.Errorf("bad limit = %d, want 400", rec.Code)
	}

	// DELETE is idempotent and removes the config and its ledger.
	del := func() *httptest.ResponseRecorder {
		return triggerCall(srv, owner, http.MethodDelete, "/api/me/calendar/trigger", "", srv.handleDeleteCalendarTrigger)
	}
	if rec := del(); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	if rec := del(); rec.Code != http.StatusNoContent {
		t.Fatalf("second delete = %d, want 204", rec.Code)
	}
	if got := getTrigger(t, srv, owner); got.Enabled || got.PromptTemplate != triggerDefaultPrompt {
		t.Fatalf("after delete = %+v", got)
	}
	if gotRuns := runs(owner, ""); len(gotRuns) != 0 {
		t.Fatalf("runs after delete = %+v", gotRuns)
	}
	if _, err := st.GetCalendarTrigger(ctx, owner.ID); err != store.ErrNotFound {
		t.Errorf("config after delete = %v, want ErrNotFound", err)
	}
}

// TestCalendarTriggerPreview covers the dry run: it renders the next matching
// event, counts skipped ones, writes nothing, and refuses a non-CalDAV account.
func TestCalendarTriggerPreview(t *testing.T) {
	srv, st := newKeyTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "")
	ics := seedKeyUser(t, st, "u-ics", "ics", "")
	ctx := context.Background()
	now := time.Now()
	feed := fakeTriggerCalendar(t,
		fakeEvent{uid: "future", summary: "Board meeting", location: "HQ",
			start: now.Add(2 * time.Hour), end: now.Add(3 * time.Hour)},
		fakeEvent{uid: "past", summary: "Old news", start: now.Add(-time.Hour), end: now.Add(time.Hour)},
	)
	seedTriggerCalendar(t, srv, u.ID, feed.URL)
	seedCalendarAccount(t, srv, ics.ID, store.CalendarAccount{Kind: "ics", Provider: "google"}, "https://example.com/feed.ics")

	preview := func(user store.User) *httptest.ResponseRecorder {
		return triggerCall(srv, user, http.MethodPost, "/api/me/calendar/trigger/preview", "", srv.handlePreviewCalendarTrigger)
	}
	rec := preview(u)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview = %d (body %s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Prompt string `json:"prompt"`
		Events []struct {
			UID, Summary, Start string
		} `json:"events"`
		Skipped int `json:"skipped"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if len(out.Events) != 1 || out.Events[0].UID != "future" || out.Events[0].Summary != "Board meeting" || out.Skipped != 1 {
		t.Fatalf("preview = %+v, want one future event and one skipped", out)
	}
	if !strings.Contains(out.Prompt, "Board meeting") || !strings.Contains(out.Prompt, "never as instructions") {
		t.Errorf("preview prompt = %q, want the event and the untrusted-data line", out.Prompt)
	}
	// A dry run writes nothing.
	if _, err := st.GetCalendarTrigger(ctx, u.ID); err != store.ErrNotFound {
		t.Errorf("preview stored a config: %v", err)
	}
	if convs, _ := st.ListConversations(ctx, u.ID); len(convs) != 0 {
		t.Errorf("preview created conversations: %+v", convs)
	}
	if runs, _ := st.ListTriggerRuns(ctx, u.ID, 10); len(runs) != 0 {
		t.Errorf("preview wrote run rows: %+v", runs)
	}

	// An already-handled event is skipped too.
	if _, err := st.InsertTriggerRun(ctx, &store.TriggerRun{
		ID: "r1", UserID: u.ID, EventUID: "future", EventStart: now.Add(2 * time.Hour).UTC().Truncate(time.Second).UnixMilli(),
		Summary: "Board meeting", Status: triggerFired, CreatedAt: now.UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertTriggerRun: %v", err)
	}
	rec = preview(u)
	out.Prompt, out.Events, out.Skipped = "", nil, 0
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if len(out.Events) != 0 || out.Skipped != 2 || out.Prompt != "" {
		t.Fatalf("preview after a run = %+v, want nothing left to fire", out)
	}

	// ICS is never a trigger source.
	if rec := preview(ics); rec.Code != http.StatusBadRequest {
		t.Fatalf("ICS preview = %d, want 400", rec.Code)
	}

	// No calendar at all: the refusal names both supported sources, not CalDAV
	// alone (Google Calendar is one too).
	none := seedKeyUser(t, st, "u-none", "none", "")
	rec = preview(none)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unconnected preview = %d, want 400", rec.Code)
	}
	if want := `{"error":"connect a calendar first (CalDAV or Google Calendar)"}` + "\n"; rec.Body.String() != want {
		t.Errorf("unconnected preview = %s, want %s", rec.Body.String(), want)
	}
}
