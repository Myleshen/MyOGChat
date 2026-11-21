package calendar

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
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
	others    []string // calendars listed before calendar (for fallback tests)
	objects   map[string]*caldav.CalendarObject
	etagSeq   int

	// Discovery counters: a PROPFIND for the principal or the home set.
	principalCalls int
	homeSetCalls   int
}

func newMemBackend() *memBackend {
	return &memBackend{
		principal: "/user/",
		home:      "/user/calendars/",
		calendar:  "/user/calendars/cal/",
		objects:   map[string]*caldav.CalendarObject{},
	}
}

func (m *memBackend) CurrentUserPrincipal(context.Context) (string, error) {
	m.mu.Lock()
	m.principalCalls++
	m.mu.Unlock()
	return m.principal, nil
}

func (m *memBackend) CalendarHomeSetPath(context.Context) (string, error) {
	m.mu.Lock()
	m.homeSetCalls++
	m.mu.Unlock()
	return m.home, nil
}

func (m *memBackend) discoveryCalls() (principal, homeSet int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.principalCalls, m.homeSetCalls
}
func (m *memBackend) CreateCalendar(context.Context, *caldav.Calendar) error {
	return nil
}
func (m *memBackend) ListCalendars(context.Context) ([]caldav.Calendar, error) {
	out := make([]caldav.Calendar, 0, len(m.others)+1)
	for _, p := range m.others {
		out = append(out, caldav.Calendar{Path: p, Name: p})
	}
	return append(out, caldav.Calendar{Path: m.calendar, Name: "Calendar"}), nil
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

// requestLog records "METHOD /path" for every request a fake server sees.
type requestLog struct {
	mu   sync.Mutex
	reqs []string
}

func (l *requestLog) add(r *http.Request) {
	l.mu.Lock()
	l.reqs = append(l.reqs, r.Method+" "+r.URL.Path)
	l.mu.Unlock()
}

func (l *requestLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.reqs...)
}

func (l *requestLog) first() string {
	if all := l.all(); len(all) > 0 {
		return all[0]
	}
	return ""
}

// newLoggingServer serves the memBackend and records request paths. GETs are
// 404s (no well-known redirect) unless redirect is set, in which case
// GET /.well-known/caldav redirects there.
func newLoggingServer(t *testing.T, mb *memBackend, redirect string) (*httptest.Server, *requestLog) {
	t.Helper()
	h := &caldav.Handler{Backend: mb}
	log := &requestLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.add(r)
		if r.Method == http.MethodGet {
			if redirect != "" && r.URL.Path == "/.well-known/caldav" {
				http.Redirect(w, r, redirect, http.StatusMovedPermanently)
				return
			}
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, log
}

// newDiscoveryServer serves the memBackend without a /.well-known/caldav
// redirect, so discovery exercises the principal/home-set PROPFINDs directly.
func newDiscoveryServer(t *testing.T, mb *memBackend) *httptest.Server {
	t.Helper()
	srv, _ := newLoggingServer(t, mb, "")
	return srv
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

// TestCalDAVCachedHomeSetSkipsDiscovery proves that a backend built from the
// stored collection + home set performs no discovery requests at all and reads
// that collection.
func TestCalDAVCachedHomeSetSkipsDiscovery(t *testing.T) {
	mb := newMemBackend()
	srv := newDiscoveryServer(t, mb)
	b, err := newCalDAV(Config{
		Kind: KindCalDAV, Endpoint: srv.URL, Username: "me@example.com", Password: "app-password",
		CalendarPath: mb.calendar, HomeSet: mb.home,
	})
	if err != nil {
		t.Fatalf("newCalDAV: %v", err)
	}
	ctx := context.Background()
	start := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)

	created, err := b.Create(ctx, Event{Title: "Cached", Start: start, End: start.Add(time.Hour)})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(created.ResourcePath, mb.calendar) {
		t.Fatalf("event stored at %q, want collection %q", created.ResourcePath, mb.calendar)
	}
	events, err := b.List(ctx, ListQuery{From: start.Add(-time.Hour), To: start.Add(time.Hour)})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(events) != 1 || events[0].Title != "Cached" {
		t.Fatalf("List = %+v, want 1 Cached", events)
	}
	if principal, homeSet := mb.discoveryCalls(); principal != 0 || homeSet != 0 {
		t.Fatalf("cached backend rediscovered: principal=%d home-set=%d", principal, homeSet)
	}
}

// TestCalDAVCalendarIDIsCollectionPath proves the shared CalendarID field is
// used as the CalDAV collection, not whichever calendar the server lists first.
func TestCalDAVCalendarIDIsCollectionPath(t *testing.T) {
	mb := newMemBackend()
	mb.others = []string{"/user/calendars/other/"}
	srv := newDiscoveryServer(t, mb)
	b, err := newCalDAV(Config{
		Kind: KindCalDAV, Endpoint: srv.URL, Username: "me@example.com",
		Password: "app-password", CalendarID: mb.calendar,
	})
	if err != nil {
		t.Fatalf("newCalDAV: %v", err)
	}
	if b.calendarPath != mb.calendar {
		t.Fatalf("calendarPath = %q, want %q", b.calendarPath, mb.calendar)
	}

	ctx := context.Background()
	start := time.Date(2026, 4, 2, 9, 0, 0, 0, time.UTC)
	created, err := b.Create(ctx, Event{Title: "Default target", Start: start, End: start.Add(time.Hour)})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(created.ResourcePath, mb.calendar) {
		t.Fatalf("event stored at %q, want collection %q", created.ResourcePath, mb.calendar)
	}
	// Event operations need no discovery, but listing calendars does: that
	// discovery must not fall back to the first listed calendar.
	cals, err := b.Calendars(ctx)
	if err != nil {
		t.Fatalf("Calendars: %v", err)
	}
	if len(cals) != 2 {
		t.Fatalf("Calendars = %+v, want 2", cals)
	}
	if _, homeSet := mb.discoveryCalls(); homeSet == 0 {
		t.Error("expected discovery when the home set is not cached")
	}
	if b.calendarPath != mb.calendar {
		t.Errorf("discovery fell back to %q, want %q", b.calendarPath, mb.calendar)
	}
}

// TestCalDAVWellKnownRedirectToPathKeepsSlash proves that a RFC 6764 redirect
// onto a collection path (https://host/user/) keeps the trailing slash, so the
// resolved endpoint addresses the collection and discovery returns calendars.
func TestCalDAVWellKnownRedirectToPathKeepsSlash(t *testing.T) {
	mb := newMemBackend()
	srv, reqs := newLoggingServer(t, mb, "/user/")

	disc, err := Discover(context.Background(), Config{
		Kind: KindCalDAV, Endpoint: srv.URL, Username: "me@example.com", Password: "app-password",
	})
	if err != nil {
		t.Fatalf("Discover: %v (requests: %v)", err, reqs.all())
	}
	if len(disc.Calendars) == 0 {
		t.Fatalf("no calendars discovered after redirect to /user/ (requests: %v)", reqs.all())
	}
	if want := srv.URL + "/user/"; disc.Endpoint != want {
		t.Errorf("discovered endpoint = %q, want %q", disc.Endpoint, want)
	}
	if !slices.Contains(reqs.all(), "PROPFIND /user/") {
		t.Errorf("requests = %v, want a follow-up PROPFIND on /user/", reqs.all())
	}
}

// TestCalDAVEndpointPathTrailingSlash proves an endpoint that already carries a
// collection path (with or without its trailing slash) is used as the client
// base, with the collection slash kept either way.
func TestCalDAVEndpointPathTrailingSlash(t *testing.T) {
	for _, tc := range []struct{ name, endpoint string }{
		{"with slash", "/user/"},
		{"without slash", "/user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mb := newMemBackend()
			srv, reqs := newLoggingServer(t, mb, "")
			b, err := newCalDAV(Config{
				Kind: KindCalDAV, Endpoint: srv.URL + tc.endpoint, Username: "me@example.com",
				Password: "app-password", CalendarPath: mb.calendar, HomeSet: mb.home,
			})
			if err != nil {
				t.Fatalf("newCalDAV: %v", err)
			}
			if want := srv.URL + "/user/"; b.endpoint != want {
				t.Errorf("endpoint = %q, want %q", b.endpoint, want)
			}
			if b.base.Path != "/user/" {
				t.Errorf("base path = %q, want /user/", b.base.Path)
			}

			start := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
			created, err := b.Create(context.Background(), Event{Title: "Located", Start: start, End: start.Add(time.Hour)})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if want := "PUT /user/calendars/cal/" + created.UID + ".ics"; reqs.first() != want {
				t.Errorf("first request = %q, want %q", reqs.first(), want)
			}
		})
	}
}

// TestCalDAVWellKnownRedirectToHostRoot is the iCloud shape: a redirect to the
// server root must still discover calendars.
func TestCalDAVWellKnownRedirectToHostRoot(t *testing.T) {
	mb := newMemBackend()
	srv, reqs := newLoggingServer(t, mb, "/")

	disc, err := Discover(context.Background(), Config{
		Kind: KindCalDAV, Endpoint: srv.URL, Username: "me@example.com", Password: "app-password",
	})
	if err != nil {
		t.Fatalf("Discover: %v (requests: %v)", err, reqs.all())
	}
	if len(disc.Calendars) == 0 {
		t.Fatalf("no calendars discovered after root redirect (requests: %v)", reqs.all())
	}
	if want := srv.URL + "/"; disc.Endpoint != want {
		t.Errorf("discovered endpoint = %q, want %q", disc.Endpoint, want)
	}
}

func TestCalendarExists(t *testing.T) {
	cals := []Calendar{{ID: "/user/calendars/work/"}, {ID: "feed-label"}}
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"/user/calendars/work/", true},
		{"/user/calendars/work", true}, // trailing slash optional
		{" feed-label ", true},
		{"/user/calendars/other/", false},
		{"", false},
	} {
		if got := CalendarExists(cals, tc.id); got != tc.want {
			t.Errorf("CalendarExists(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}
