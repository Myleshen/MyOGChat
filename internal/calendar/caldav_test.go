package calendar

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/caldav"
)

// memBackend is a minimal in-memory CalDAV backend used to exercise the client
// against a real caldav.Handler (protocol round-trip, not a hand-rolled mock).
type memBackend struct {
	mu        sync.Mutex
	principal string
	home      string
	calendar  string
	objects   map[string]*caldav.CalendarObject
	etagSeq   int
}

func newMemBackend() *memBackend {
	return &memBackend{
		principal: "/user/",
		home:      "/user/calendars/",
		calendar:  "/user/calendars/cal/",
		objects:   map[string]*caldav.CalendarObject{},
	}
}

func (m *memBackend) CurrentUserPrincipal(context.Context) (string, error) { return m.principal, nil }
func (m *memBackend) CalendarHomeSetPath(context.Context) (string, error)  { return m.home, nil }
func (m *memBackend) CreateCalendar(context.Context, *caldav.Calendar) error {
	return nil
}
func (m *memBackend) ListCalendars(context.Context) ([]caldav.Calendar, error) {
	return []caldav.Calendar{{Path: m.calendar, Name: "Calendar"}}, nil
}
func (m *memBackend) GetCalendar(_ context.Context, path string) (*caldav.Calendar, error) {
	if path == m.calendar {
		return &caldav.Calendar{Path: m.calendar, Name: "Calendar"}, nil
	}
	return nil, webdav.NewHTTPError(http.StatusNotFound, fmt.Errorf("no calendar %s", path))
}
func (m *memBackend) GetCalendarObject(_ context.Context, path string, _ *caldav.CalendarCompRequest) (*caldav.CalendarObject, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.objects[path]
	if !ok {
		return nil, webdav.NewHTTPError(http.StatusNotFound, fmt.Errorf("no object %s", path))
	}
	return obj, nil
}
func (m *memBackend) ListCalendarObjects(_ context.Context, path string, _ *caldav.CalendarCompRequest) ([]caldav.CalendarObject, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []caldav.CalendarObject{}
	for p, obj := range m.objects {
		if strings.HasPrefix(p, path) {
			out = append(out, *obj)
		}
	}
	return out, nil
}
func (m *memBackend) QueryCalendarObjects(_ context.Context, path string, query *caldav.CalendarQuery) ([]caldav.CalendarObject, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	uid := ""
	for _, cf := range query.CompFilter.Comps {
		for _, pf := range cf.Props {
			if pf.Name == "UID" && pf.TextMatch != nil {
				uid = pf.TextMatch.Text
			}
		}
	}
	out := []caldav.CalendarObject{}
	for p, obj := range m.objects {
		if !strings.HasPrefix(p, path) {
			continue
		}
		if uid != "" && !objectHasUID(*obj, uid) {
			continue
		}
		out = append(out, *obj)
	}
	return out, nil
}
func (m *memBackend) PutCalendarObject(_ context.Context, path string, cal *ical.Calendar, opts *caldav.PutCalendarObjectOptions) (*caldav.CalendarObject, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, exists := m.objects[path]
	etag := ""
	if exists {
		etag = existing.ETag
	}
	if opts != nil {
		if opts.IfNoneMatch.IsSet() {
			if ok, _ := opts.IfNoneMatch.MatchETag(etag); ok {
				return nil, webdav.NewHTTPError(http.StatusPreconditionFailed, fmt.Errorf("If-None-Match failed"))
			}
		}
		if opts.IfMatch.IsSet() {
			if ok, _ := opts.IfMatch.MatchETag(etag); !ok {
				return nil, webdav.NewHTTPError(http.StatusPreconditionFailed, fmt.Errorf("If-Match failed"))
			}
		}
	}
	m.etagSeq++
	obj := &caldav.CalendarObject{Path: path, Data: cal, ETag: fmt.Sprintf("etag-%d", m.etagSeq)}
	m.objects[path] = obj
	return obj, nil
}
func (m *memBackend) DeleteCalendarObject(_ context.Context, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objects[path]; !ok {
		return webdav.NewHTTPError(http.StatusNotFound, fmt.Errorf("no object %s", path))
	}
	delete(m.objects, path)
	return nil
}

func newTestServer(t *testing.T) (*caldavBackend, *memBackend) {
	t.Helper()
	mb := newMemBackend()
	srv := httptest.NewServer(&caldav.Handler{Backend: mb})
	t.Cleanup(srv.Close)
	b, err := newCalDAV(Config{
		Kind:         KindCalDAV,
		Endpoint:     srv.URL,
		Username:     "me@example.com",
		Password:     "app-password",
		CalendarPath: mb.calendar,
	})
	if err != nil {
		t.Fatalf("newCalDAV: %v", err)
	}
	return b, mb
}

func TestCalDAVRoundTrip(t *testing.T) {
	b, mb := newTestServer(t)
	ctx := context.Background()

	start := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	created, err := b.Create(ctx, Event{
		Title:       "Standup",
		Description: "Daily sync",
		Location:    "Room 1",
		Start:       start,
		End:         start.Add(30 * time.Minute),
		TimeZone:    "UTC",
		Attendees:   []string{"a@example.com"},
		Reminders:   []Reminder{{Minutes: 10, Action: "DISPLAY"}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.UID == "" || created.ETag == "" {
		t.Fatalf("created event missing uid/etag: %+v", created)
	}
	if _, ok := mb.objects[created.ResourcePath]; !ok {
		t.Fatalf("object not stored at %s", created.ResourcePath)
	}

	// List in range.
	events, err := b.List(ctx, ListQuery{From: start.Add(-time.Hour), To: start.Add(time.Hour)})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(events) != 1 || events[0].Title != "Standup" {
		t.Fatalf("List = %+v, want 1 Standup", events)
	}
	if events[0].ETag == "" {
		t.Errorf("listed event has no ETag")
	}
	if len(events[0].Attendees) != 1 || events[0].Attendees[0] != "a@example.com" {
		t.Errorf("attendees = %v", events[0].Attendees)
	}
	if len(events[0].Reminders) != 1 || events[0].Reminders[0].Minutes != 10 {
		t.Errorf("reminders = %v", events[0].Reminders)
	}

	// Update (whole series).
	events[0].Title = "Standup (renamed)"
	events[0].Location = "Room 2"
	updated, err := b.Update(ctx, events[0])
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.ETag == events[0].ETag {
		t.Errorf("update did not refresh ETag")
	}
	got, err := b.List(ctx, ListQuery{From: start.Add(-time.Hour), To: start.Add(time.Hour)})
	if err != nil {
		t.Fatalf("List after update: %v", err)
	}
	if len(got) != 1 || got[0].Title != "Standup (renamed)" || got[0].Location != "Room 2" {
		t.Fatalf("after update = %+v", got)
	}

	// Delete.
	if err := b.Delete(ctx, created.UID, "", got[0].ETag); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	after, err := b.List(ctx, ListQuery{From: start.Add(-time.Hour), To: start.Add(time.Hour)})
	if err != nil {
		t.Fatalf("List after delete: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("after delete = %+v, want empty", after)
	}
}

func TestCalDAVUpdateConflict(t *testing.T) {
	b, _ := newTestServer(t)
	ctx := context.Background()
	start := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	created, err := b.Create(ctx, Event{Title: "A", Start: start, End: start.Add(time.Hour)})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Stale ETag must be rejected with a clear conflict error.
	stale := created
	stale.ETag = "etag-stale"
	stale.Title = "B"
	if _, err := b.Update(ctx, stale); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("stale update err = %v, want conflict", err)
	}
}

func TestCalDAVAllDay(t *testing.T) {
	b, _ := newTestServer(t)
	ctx := context.Background()
	day := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	created, err := b.Create(ctx, Event{Title: "Holiday", AllDay: true, Start: day, End: day.Add(24 * time.Hour)})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	events, err := b.List(ctx, ListQuery{From: day.Add(-24 * time.Hour), To: day.Add(48 * time.Hour)})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(events) != 1 || !events[0].AllDay {
		t.Fatalf("all-day round-trip = %+v", events)
	}
	if !events[0].Start.Equal(day) {
		t.Errorf("all-day start = %v, want %v", events[0].Start, day)
	}
	_ = created
}

func TestCalDAVRejectsUnknownKind(t *testing.T) {
	if _, err := New(Config{Kind: "bogus"}); err == nil {
		t.Fatal("expected error for unknown kind")
	}
}
