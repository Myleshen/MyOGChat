package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"myogchat/internal/calendar"
	"myogchat/internal/store"
	"myogchat/internal/upstream"
)

// fakeEvent is one event the fake CalDAV feed serves.
type fakeEvent struct {
	uid, summary, location, description string
	start, end                          time.Time
}

// fakeTriggerCalendar serves a calendar-query REPORT whose calendar-data holds
// events, so the scheduler can be driven through a real CalDAV round trip with
// no network and no discovery (the seeded account carries collection + home).
func fakeTriggerCalendar(t *testing.T, events ...fakeEvent) *httptest.Server {
	t.Helper()
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>`)
	b.WriteString(`<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">`)
	for _, ev := range events {
		ical := "BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:-//test//EN\nBEGIN:VEVENT\n"
		ical += "UID:" + ev.uid + "\n"
		ical += "SUMMARY:" + ev.summary + "\n"
		ical += "DTSTART:" + ev.start.UTC().Truncate(time.Second).Format("20060102T150405Z") + "\n"
		ical += "DTEND:" + ev.end.UTC().Truncate(time.Second).Format("20060102T150405Z") + "\n"
		if ev.location != "" {
			ical += "LOCATION:" + ev.location + "\n"
		}
		if ev.description != "" {
			ical += "DESCRIPTION:" + ev.description + "\n"
		}
		ical += "END:VEVENT\nEND:VCALENDAR\n"
		fmt.Fprintf(&b, `<D:response><D:href>/user/calendars/cal/%s.ics</D:href><D:propstat><D:prop>`+
			`<D:getetag>"e-%s"</D:getetag><C:calendar-data>%s</C:calendar-data>`+
			`</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`, ev.uid, ev.uid, ical)
	}
	b.WriteString(`</D:multistatus>`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "REPORT" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = io.WriteString(w, b.String())
	}))
	t.Cleanup(srv.Close)
	return srv
}

// seedTriggerCalendar stores a CalDAV account pointing at the fake feed.
func seedTriggerCalendar(t *testing.T, srv *Server, userID, baseURL string) {
	t.Helper()
	seedCalendarAccount(t, srv, userID, store.CalendarAccount{
		Kind: "caldav", Provider: "apple", Endpoint: baseURL,
		Username: "me@example.com", CalendarID: "/user/calendars/cal/",
		HomeSet: "/user/calendars/",
	}, "pw")
}

// seedTrigger stores an enabled trigger config for u.
func seedTrigger(t *testing.T, st *store.Store, u store.User, quietStart, quietEnd *string) {
	t.Helper()
	tr := store.CalendarTrigger{
		UserID: u.ID, Enabled: true, Model: triggerDefaultModel,
		QuietStart: quietStart, QuietEnd: quietEnd, UpdatedAt: 1,
	}
	if err := st.UpsertCalendarTrigger(context.Background(), &tr); err != nil {
		t.Fatalf("UpsertCalendarTrigger: %v", err)
	}
}

// triggerRuns lists a user's runs through the store.
func triggerRuns(t *testing.T, st *store.Store, userID string) []store.TriggerRun {
	t.Helper()
	runs, err := st.ListTriggerRuns(context.Background(), userID, 50)
	if err != nil {
		t.Fatalf("ListTriggerRuns: %v", err)
	}
	return runs
}

// runStatuses maps summary → status for the runs a tick recorded.
func runStatuses(runs []store.TriggerRun) map[string]string {
	out := make(map[string]string, len(runs))
	for _, r := range runs {
		out[r.Summary] = r.Status
	}
	return out
}

// TestJobStartRefusedWhileShuttingDown proves a send cannot start a job once
// ShutdownJobs has returned (and maps to 503 over HTTP).
func TestJobStartRefusedWhileShuttingDown(t *testing.T) {
	srv, st := newJobTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "key")
	mkConv(t, st, u, "c1")

	srv.ShutdownJobs(context.Background())

	if _, err := srv.hub().start("c1", u.ID, "glm-5.3-flash", false); !errors.Is(err, errShuttingDown) {
		t.Fatalf("start after shutdown = %v, want errShuttingDown", err)
	}
	rec := postMessage(srv, u, "c1", "hi")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("post after shutdown = %d, want 503 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "shutting down") {
		t.Errorf("body = %s, want the shutdown reason", rec.Body.String())
	}

	// The agent path (and its queue) refuses too: a queued send would never start.
	if err := st.InsertConversation(context.Background(), &store.Conversation{
		ID: "c-agent", UserID: &u.ID, Title: "t", Model: "glm-5.3-flash",
		AgentMode: true, CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertConversation(agent): %v", err)
	}
	if rec := postMessage(srv, u, "c-agent", "hi"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("agent post after shutdown = %d, want 503 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestJobStartExclusiveRefusesAndNeverQueues proves the scheduler's start path
// refuses a busy user instead of queueing, while an ordinary agent send still
// queues.
func TestJobStartExclusiveRefusesAndNeverQueues(t *testing.T) {
	h := newJobHub()

	j, err := h.startAgentExclusive("c1", "u1", "glm-5.3-flash")
	if err != nil {
		t.Fatalf("first exclusive start: %v", err)
	}
	if _, err := h.startAgentExclusive("c2", "u1", "glm-5.3-flash"); !errors.Is(err, errAgentBusy) {
		t.Fatalf("start while agent busy = %v, want errAgentBusy", err)
	}
	if h.queuedPosition("c2") != 0 {
		t.Fatal("exclusive start must never queue")
	}

	// An ordinary agent send still waits in the per-user FIFO.
	j2, pos, err := h.startAgent("c2", "u1", "glm-5.3-flash", replySpec{})
	if err != nil || j2 != nil || pos != 1 {
		t.Fatalf("ordinary startAgent = (%v, %d, %v), want a queue entry at 1", j2, pos, err)
	}

	// Draining refuses both paths.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	h.shutdown(ctx)
	if _, err := h.startAgentExclusive("c3", "u1", "glm-5.3-flash"); !errors.Is(err, errShuttingDown) {
		t.Fatalf("exclusive start while closing = %v, want errShuttingDown", err)
	}
	_ = j
}

// TestInQuietWindow covers the plain and the midnight-crossing window.
func TestInQuietWindow(t *testing.T) {
	quiet := func(start, end string) (*string, *string) { return &start, &end }
	at := func(hour, minute int) time.Time {
		return time.Date(2026, 5, 15, hour, minute, 0, 0, time.Local)
	}

	for _, tc := range []struct {
		name       string
		start, end string
		now        time.Time
		want       bool
	}{
		{"overnight inside", "22:00", "06:00", at(23, 30), true},
		{"overnight just before end", "22:00", "06:00", at(5, 59), true},
		{"overnight at end", "22:00", "06:00", at(6, 0), false},
		{"overnight midday", "22:00", "06:00", at(12, 0), false},
		{"overnight at start", "22:00", "06:00", at(22, 0), true},
		{"same day inside", "13:00", "14:00", at(13, 30), true},
		{"same day at end", "13:00", "14:00", at(14, 0), false},
		{"same day outside", "13:00", "14:00", at(12, 59), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, e := quiet(tc.start, tc.end)
			if got := inQuietWindow(tc.now, s, e); got != tc.want {
				t.Errorf("inQuietWindow(%s, %s–%s) = %v, want %v",
					tc.now.Format("15:04"), tc.start, tc.end, got, tc.want)
			}
		})
	}
	// Equal bounds and missing bounds are no window, not all-day quiet.
	s, e := quiet("60", "60")
	if inQuietWindow(at(12, 0), s, e) {
		t.Error("equal/degenerate bounds must not be a quiet window")
	}
	if inQuietWindow(at(12, 0), nil, nil) {
		t.Error("unset quiet hours must not be a quiet window")
	}
}

// TestRenderTriggerPrompt covers token substitution, the unknown-token rule and
// the built-in default.
func TestRenderTriggerPrompt(t *testing.T) {
	start := time.Date(2026, 5, 15, 9, 0, 0, 0, time.UTC)
	ev := calendar.Event{
		UID: "e1", Title: "Sprint review", Location: "Room 3",
		Description: "Demo the release", Start: start, End: start.Add(time.Hour),
	}
	out := renderTriggerPrompt(
		"{{summary}}|{{start}}|{{end}}|{{location}}|{{description}}|{{unknown}}", ev)
	parts := strings.Split(out, "|")
	if parts[0] != "Sprint review" || parts[3] != "Room 3" || parts[4] != "Demo the release" {
		t.Errorf("rendered = %q", out)
	}
	wantStart := start.In(time.Local).Format(time.RFC3339)
	if parts[1] != wantStart || parts[2] != start.Add(time.Hour).In(time.Local).Format(time.RFC3339) {
		t.Errorf("dates = %q, %q; want %q and +1h", parts[1], parts[2], wantStart)
	}
	if parts[5] != "{{unknown}}" {
		t.Errorf("unknown token = %q, want it left as-is", parts[5])
	}

	def := renderTriggerPrompt("", ev)
	for _, want := range []string{"Sprint review", wantStart} {
		if !strings.Contains(def, want) {
			t.Errorf("default prompt missing %q:\n%s", want, def)
		}
	}
}

// TestCalendarTriggerTickFiresOnceAndDedupes drives two ticks against a fake
// calendar holding one due event and one already-started event: exactly one
// conversation, one fired run and one skipped_late run, and the second tick
// adds nothing.
func TestCalendarTriggerTickFiresOnceAndDedupes(t *testing.T) {
	srv, st := newKeyTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "key")
	now := time.Now()
	feed := fakeTriggerCalendar(t,
		fakeEvent{uid: "due", summary: "Sprint review", start: now.Add(10 * time.Minute), end: now.Add(40 * time.Minute)},
		fakeEvent{uid: "late", summary: "Dentist", start: now.Add(-30 * time.Minute), end: now.Add(10 * time.Minute)},
	)
	seedTriggerCalendar(t, srv, u.ID, feed.URL)
	seedTrigger(t, st, u, nil, nil)
	srv.streamAgentFn = fixedStream(
		upstream.Event{Kind: upstream.EventText, Delta: "here is your brief"},
		upstream.Event{Kind: upstream.EventUsage, Usage: &upstream.Usage{PromptTokens: 3, CompletionTokens: 2}},
	)

	ctx := context.Background()
	srv.calendarTriggerTick(ctx)

	runs := triggerRuns(t, st, u.ID)
	if len(runs) != 2 {
		t.Fatalf("runs = %+v, want the due event and the late one", runs)
	}
	got := runStatuses(runs)
	if got["Sprint review"] != triggerFired || got["Dentist"] != triggerSkippedLate {
		t.Fatalf("statuses = %v, want fired + skipped_late", got)
	}

	convs, err := st.ListConversations(ctx, u.ID)
	if err != nil {
		t.Fatalf("ListConversations: %v", err)
	}
	if len(convs) != 1 {
		t.Fatalf("conversations = %+v, want exactly one", convs)
	}
	conv := convs[0]
	if conv.Title != "Calendar: Sprint review" || !conv.AgentMode {
		t.Fatalf("conversation = %+v, want the trigger title and agent mode", conv)
	}
	var fired store.TriggerRun
	for _, r := range runs {
		if r.Status == triggerFired {
			fired = r
		}
	}
	if fired.ConversationID == nil || *fired.ConversationID != conv.ID {
		t.Fatalf("fired run conversation = %v, want %s", fired.ConversationID, conv.ID)
	}
	// The prompt is written before the job starts, so it exists immediately.
	msgs, err := st.ListMessages(ctx, conv.ID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) == 0 || msgs[0].Role != "user" || !strings.Contains(msgs[0].Content, "Sprint review") {
		t.Fatalf("messages = %+v, want the rendered prompt", msgs)
	}
	waitFor(t, func() bool {
		msgs, err := st.ListMessages(ctx, conv.ID)
		return err == nil && len(msgs) == 2 && msgs[1].Role == "assistant" && msgs[1].Content == "here is your brief"
	})

	// The second tick sees the same events: the dedupe key makes both no-ops.
	srv.calendarTriggerTick(ctx)
	if runs := triggerRuns(t, st, u.ID); len(runs) != 2 {
		t.Fatalf("runs after second tick = %+v, want no new rows", runs)
	}
	if convs, err := st.ListConversations(ctx, u.ID); err != nil || len(convs) != 1 {
		t.Fatalf("conversations after second tick = %+v (%v), want one", convs, err)
	}
}

// TestCalendarTriggerSkips covers the quiet, quota and busy outcomes in one
// tick each: a skipped event creates a ledger row but never a conversation.
func TestCalendarTriggerSkips(t *testing.T) {
	dueEvent := func(now time.Time) fakeEvent {
		return fakeEvent{uid: "due-" + now.Format("150405"), summary: "Standup",
			start: now.Add(10 * time.Minute), end: now.Add(40 * time.Minute)}
	}

	t.Run("quiet hours", func(t *testing.T) {
		srv, st := newKeyTestServer(t)
		u := seedKeyUser(t, st, "u1", "alice", "key")
		now := time.Now()
		// A one-hour window around now, built from local clock times.
		start := now.Add(-time.Hour).Format("15:04")
		end := now.Add(time.Hour).Format("15:04")
		seedTriggerCalendar(t, srv, u.ID, fakeTriggerCalendar(t, dueEvent(now)).URL)
		seedTrigger(t, st, u, &start, &end)

		srv.calendarTriggerTick(context.Background())
		runs := triggerRuns(t, st, u.ID)
		if len(runs) != 1 || runs[0].Status != triggerSkippedQuiet {
			t.Fatalf("runs = %+v, want one skipped_quiet", runs)
		}
		if convs, _ := st.ListConversations(context.Background(), u.ID); len(convs) != 0 {
			t.Fatalf("quiet hours created %+v", convs)
		}
	})

	t.Run("quota", func(t *testing.T) {
		srv, st := newKeyTestServer(t)
		u := seedKeyUser(t, st, "u1", "alice", "key")
		if err := st.SetUserLimits(context.Background(), u.ID, int64ptr(1), nil, time.Now().UnixMilli()); err != nil {
			t.Fatalf("SetUserLimits: %v", err)
		}
		mkConv(t, st, u, "seed")
		seedServerUsage(t, st, "m1", "seed", "glm-5.3-flash", time.Now().UnixMilli(), 10, 10, 0)
		now := time.Now()
		seedTriggerCalendar(t, srv, u.ID, fakeTriggerCalendar(t, dueEvent(now)).URL)
		seedTrigger(t, st, u, nil, nil)

		srv.calendarTriggerTick(context.Background())
		runs := triggerRuns(t, st, u.ID)
		if len(runs) != 1 || runs[0].Status != triggerSkippedQuota {
			t.Fatalf("runs = %+v, want one skipped_quota", runs)
		}
		if !strings.Contains(runs[0].Reason, "daily token limit") {
			t.Errorf("reason = %q, want the quota message", runs[0].Reason)
		}
	})

	t.Run("agent busy", func(t *testing.T) {
		srv, st := newKeyTestServer(t)
		u := seedKeyUser(t, st, "u1", "alice", "key")
		now := time.Now()
		seedTriggerCalendar(t, srv, u.ID, fakeTriggerCalendar(t, dueEvent(now)).URL)
		seedTrigger(t, st, u, nil, nil)
		busy, err := srv.hub().startAgentExclusive("owner-chat", u.ID, "glm-5.3-flash")
		if err != nil {
			t.Fatalf("occupy the agent slot: %v", err)
		}
		t.Cleanup(func() {
			busy.cancel()
			srv.hub().complete(busy)
		})

		srv.calendarTriggerTick(context.Background())
		runs := triggerRuns(t, st, u.ID)
		if len(runs) != 1 || runs[0].Status != triggerSkippedBusy {
			t.Fatalf("runs = %+v, want one skipped_busy", runs)
		}
		if !strings.Contains(runs[0].Reason, "already running") {
			t.Errorf("reason = %q, want the busy reason", runs[0].Reason)
		}
	})
}

// TestCalendarTriggerEventEligibility covers the firing window itself: an event
// whose trigger time is an hour ahead is left for a later tick, one 11 minutes
// past its window is late.
func TestCalendarTriggerEventEligibility(t *testing.T) {
	srv, st := newKeyTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "key")
	now := time.Now()
	feed := fakeTriggerCalendar(t,
		// Fire time is 45 min away: not due, and no ledger row may exist yet.
		fakeEvent{uid: "future", summary: "Future", start: now.Add(time.Hour), end: now.Add(2 * time.Hour)},
		// Fire time passed 11 min ago, event still 4 min away: too late.
		fakeEvent{uid: "stale", summary: "Stale", start: now.Add(4 * time.Minute), end: now.Add(30 * time.Minute)},
	)
	seedTriggerCalendar(t, srv, u.ID, feed.URL)
	seedTrigger(t, st, u, nil, nil)

	srv.calendarTriggerTick(context.Background())
	statuses := runStatuses(triggerRuns(t, st, u.ID))
	if _, ok := statuses["Future"]; ok {
		t.Errorf("a not-yet-due event was recorded: %v", statuses)
	}
	if statuses["Stale"] != triggerSkippedLate {
		t.Errorf("stale event status = %q, want skipped_late", statuses["Stale"])
	}
}

// TestTriggeredReplyHasNoChatToolsAndNoMemory proves the two sandbox exceptions:
// a triggered reply's tool set omits list_chats/read_chat, and a successful
// triggered reply never calls the project-memory model.
func TestTriggeredReplyHasNoChatToolsAndNoMemory(t *testing.T) {
	ctx := context.Background()
	srv, st := newKeyTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "key")
	mkProject(t, st, u, "p1", "Be terse.", "", true)

	var toolNames atomic.Value
	toolNames.Store([]string{})
	srv.streamAgentFn = func(_ context.Context, p upstream.Params, _ upstream.ExecuteTool, _ upstream.AgentOptions) <-chan upstream.Event {
		names := make([]string, 0, len(p.Tools))
		for _, t := range p.Tools {
			names = append(names, t.Function.Name)
		}
		toolNames.Store(names)
		out := make(chan upstream.Event, 2)
		out <- upstream.Event{Kind: upstream.EventText, Delta: "brief"}
		out <- upstream.Event{Kind: upstream.EventUsage, Usage: &upstream.Usage{PromptTokens: 1, CompletionTokens: 1}}
		close(out)
		return out
	}
	var memoryCalls int32
	srv.chatOnceFn = func(context.Context, string, string, string, []upstream.Message, int) (string, error) {
		atomic.AddInt32(&memoryCalls, 1)
		return "merged", nil
	}

	// A triggered reply in a project chat.
	mkProjectConv(t, st, u, "c-trig", "p1")
	conv, err := st.GetConversation(ctx, "c-trig", u.ID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	j, err := srv.hub().startAgentExclusive(conv.ID, u.ID, conv.Model)
	if err != nil {
		t.Fatalf("startAgentExclusive: %v", err)
	}
	srv.runReply(j, replySpec{conv: conv, user: u, apiKey: "key", userSaved: true, triggered: true, userContent: "prepare"})
	if j.snapshot().Status != jobDone {
		t.Fatalf("triggered reply status = %q, want done", j.snapshot().Status)
	}
	names := toolNames.Load().([]string)
	has := func(name string) bool {
		for _, n := range names {
			if n == name {
				return true
			}
		}
		return false
	}
	if has("list_chats") || has("read_chat") {
		t.Errorf("triggered reply tool set = %v, must not contain chat reads", names)
	}
	if !has("fetch_url") {
		t.Errorf("triggered reply tool set = %v, want ordinary tools intact", names)
	}
	time.Sleep(50 * time.Millisecond)
	if n := atomic.LoadInt32(&memoryCalls); n != 0 {
		t.Fatalf("triggered reply made %d project-memory calls, want none", n)
	}

	// The same project chat with an ordinary reply keeps both.
	mkProjectConv(t, st, u, "c-normal", "p1")
	normal, _ := st.GetConversation(ctx, "c-normal", u.ID)
	j2, err := srv.hub().startAgentExclusive(normal.ID, u.ID, normal.Model)
	if err != nil {
		t.Fatalf("startAgentExclusive (normal): %v", err)
	}
	srv.runReply(j2, replySpec{conv: normal, user: u, apiKey: "key", userSaved: true, userContent: "hello"})
	names = toolNames.Load().([]string)
	if !has("list_chats") || !has("read_chat") {
		t.Errorf("ordinary reply tool set = %v, want the chat tools", names)
	}
	waitFor(t, func() bool { return atomic.LoadInt32(&memoryCalls) == 1 })
}
