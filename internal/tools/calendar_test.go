package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"myogchat/internal/calendar"
)

// fakeCal is an in-memory writable backend for tool-level tests.
type fakeCal struct {
	events map[string]calendar.Event
}

func newFakeCal() *fakeCal { return &fakeCal{events: map[string]calendar.Event{}} }

func (f *fakeCal) List(_ context.Context, q calendar.ListQuery) ([]calendar.Event, error) {
	out := []calendar.Event{}
	for _, ev := range f.events {
		if ev.Start.Before(q.To) && !ev.End.Before(q.From) {
			out = append(out, ev)
		}
	}
	return out, nil
}

func (f *fakeCal) Get(_ context.Context, uid, _ string) (calendar.Event, error) {
	ev, ok := f.events[uid]
	if !ok {
		return calendar.Event{}, errors.New("not found")
	}
	return ev, nil
}

func (f *fakeCal) Create(_ context.Context, ev calendar.Event) (calendar.Event, error) {
	if ev.UID == "" {
		ev.UID = "uid-created"
	}
	ev.ETag = "etag-1"
	f.events[ev.UID] = ev
	return ev, nil
}

func (f *fakeCal) Update(_ context.Context, ev calendar.Event) (calendar.Event, error) {
	f.events[ev.UID] = ev
	return ev, nil
}

func (f *fakeCal) Delete(_ context.Context, uid, _, _ string) error {
	delete(f.events, uid)
	return nil
}

func (f *fakeCal) Calendars(context.Context) ([]calendar.Calendar, error) {
	return []calendar.Calendar{{ID: "primary", Writable: true}}, nil
}
func (f *fakeCal) Writable() bool { return true }

func TestCalendarToolRegistryShape(t *testing.T) {
	f := newFakeCal()
	tools := []Tool{
		NewCalendarListTool(f), NewCalendarCreateTool(f),
		NewCalendarUpdateTool(f), NewCalendarDeleteTool(f),
	}
	want := []string{"calendar_list", "calendar_create", "calendar_update", "calendar_delete"}
	for i, tl := range tools {
		if tl.Name() != want[i] {
			t.Errorf("tool[%d].Name() = %q, want %q", i, tl.Name(), want[i])
		}
		var schema map[string]any
		if err := json.Unmarshal(tl.Parameters(), &schema); err != nil {
			t.Errorf("%s schema: %v", tl.Name(), err)
		}
	}
}

func TestCalendarCreateUpdateDelete(t *testing.T) {
	f := newFakeCal()
	ctx := context.Background()

	create := NewCalendarCreateTool(f)
	out, err := create.Run(ctx, json.RawMessage(`{
		"title":"Dentist","start":"2026-03-02T09:00:00Z","end":"2026-03-02T09:30:00Z",
		"location":"Clinic","attendees":["a@example.com"],"reminders_minutes":[15],
		"recurrence":"FREQ=WEEKLY;COUNT=2"
	}`))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.Contains(out, "Dentist") || !strings.Contains(out, "uid-created") {
		t.Fatalf("create output = %q", out)
	}
	ev := f.events["uid-created"]
	if ev.Location != "Clinic" || len(ev.Attendees) != 1 || len(ev.Reminders) != 1 || len(ev.Recurrence) != 1 {
		t.Fatalf("created event = %+v", ev)
	}

	list := NewCalendarListTool(f)
	out, err = list.Run(ctx, json.RawMessage(`{"from":"2026-03-01","to":"2026-03-05"}`))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "Dentist") || !strings.Contains(out, "recurring") {
		t.Fatalf("list output = %q", out)
	}

	update := NewCalendarUpdateTool(f)
	out, err = update.Run(ctx, json.RawMessage(`{"uid":"uid-created","title":"Dentist (moved)","location":"New clinic"}`))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !strings.Contains(out, "Dentist (moved)") {
		t.Fatalf("update output = %q", out)
	}
	if f.events["uid-created"].Location != "New clinic" || f.events["uid-created"].Title != "Dentist (moved)" {
		t.Fatalf("updated event = %+v", f.events["uid-created"])
	}

	del := NewCalendarDeleteTool(f)
	out, err = del.Run(ctx, json.RawMessage(`{"uid":"uid-created"}`))
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := f.events["uid-created"]; ok {
		t.Fatalf("event not deleted: %q", out)
	}
}

func TestCalendarAllDayCreate(t *testing.T) {
	f := newFakeCal()
	create := NewCalendarCreateTool(f)
	if _, err := create.Run(context.Background(), json.RawMessage(`{
		"title":"Holiday","start":"2026-05-01","end":"2026-05-02"
	}`)); err != nil {
		t.Fatalf("create: %v", err)
	}
	ev := f.events["uid-created"]
	if !ev.AllDay || ev.Start.Format("2006-01-02") != "2026-05-01" {
		t.Fatalf("all-day event = %+v", ev)
	}
}

func TestCalendarListValidation(t *testing.T) {
	list := NewCalendarListTool(newFakeCal())
	if _, err := list.Run(context.Background(), json.RawMessage(`{"from":"2026-03-05","to":"2026-03-01"}`)); err == nil {
		t.Fatal("expected to-after-from error")
	}
	if _, err := list.Run(context.Background(), json.RawMessage(`{"from":"nonsense","to":"2026-03-01"}`)); err == nil {
		t.Fatal("expected parse error")
	}
}

// ICS-backed tools must reject every write with a clear read-only error.
func TestCalendarToolsRejectICSWrite(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/calendar")
		_, _ = w.Write([]byte("BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:-//T//EN\nBEGIN:VEVENT\nUID:x\nSUMMARY:X\nDTSTART:20260302T090000Z\nDTEND:20260302T093000Z\nEND:VEVENT\nEND:VCALENDAR\n"))
	}))
	defer srv.Close()
	b, err := calendar.New(calendar.Config{Kind: calendar.KindICS, URL: srv.URL})
	if err != nil {
		t.Fatalf("calendar.New: %v", err)
	}
	ctx := context.Background()
	if _, err := NewCalendarCreateTool(b).Run(ctx, json.RawMessage(`{"title":"x","start":"2026-03-02T09:00:00Z","end":"2026-03-02T10:00:00Z"}`)); !errors.Is(err, calendar.ErrReadOnly) {
		t.Errorf("create err = %v, want ErrReadOnly", err)
	}
	if _, err := NewCalendarUpdateTool(b).Run(ctx, json.RawMessage(`{"uid":"x","title":"y"}`)); !errors.Is(err, calendar.ErrReadOnly) {
		t.Errorf("update err = %v, want ErrReadOnly", err)
	}
	if _, err := NewCalendarDeleteTool(b).Run(ctx, json.RawMessage(`{"uid":"x"}`)); !errors.Is(err, calendar.ErrReadOnly) {
		t.Errorf("delete err = %v, want ErrReadOnly", err)
	}
	// Reads still work.
	out, err := NewCalendarListTool(b).Run(ctx, json.RawMessage(`{"from":"2026-03-01","to":"2026-03-03"}`))
	if err != nil || !strings.Contains(out, "X") {
		t.Fatalf("list = %q, %v", out, err)
	}
}

func TestParseWhen(t *testing.T) {
	if _, allDay, err := parseWhen("2026-05-01"); err != nil || !allDay {
		t.Errorf("date: allDay=%v err=%v", allDay, err)
	}
	tm, allDay, err := parseWhen("2026-05-01T10:00:00+01:00")
	if err != nil || allDay || !tm.Equal(time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("rfc3339: %v allDay=%v err=%v", tm, allDay, err)
	}
	if _, _, err := parseWhen("not-a-date"); err == nil {
		t.Error("expected error")
	}
}
