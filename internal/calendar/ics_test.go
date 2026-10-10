package calendar

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const sampleICS = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//Test//EN
BEGIN:VEVENT
UID:timed-1
SUMMARY:Team sync
DESCRIPTION:Weekly planning
LOCATION:Room 1
DTSTART:20260302T090000Z
DTEND:20260302T093000Z
END:VEVENT
BEGIN:VEVENT
UID:allday-1
SUMMARY:Holiday
DTSTART;VALUE=DATE:20260501
DTEND;VALUE=DATE:20260502
END:VEVENT
BEGIN:VEVENT
UID:weekly-1
SUMMARY:Weekly standup
DTSTART:20260302T120000Z
DTEND:20260302T130000Z
RRULE:FREQ=WEEKLY;COUNT=4
END:VEVENT
END:VCALENDAR
`

func newICSTestServer(t *testing.T, body string) *icsBackend {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/calendar")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	b, err := newICS(Config{Kind: KindICS, URL: srv.URL + "/basic.ics"})
	if err != nil {
		t.Fatalf("newICS: %v", err)
	}
	return b
}

func TestICSListAndExpand(t *testing.T) {
	b := newICSTestServer(t, sampleICS)
	ctx := context.Background()

	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	events, err := b.List(ctx, ListQuery{From: from, To: to})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// timed-1 (once) + weekly-1 expanded to 4 instances = 5.
	if len(events) != 5 {
		t.Fatalf("List = %d events, want 5: %+v", len(events), events)
	}
	byTitle := map[string]int{}
	for _, ev := range events {
		byTitle[ev.Title]++
	}
	if byTitle["Team sync"] != 1 || byTitle["Weekly standup"] != 4 {
		t.Fatalf("expansion counts = %v", byTitle)
	}

	// Query filter.
	filtered, err := b.List(ctx, ListQuery{From: from, To: to, Query: "team"})
	if err != nil {
		t.Fatalf("List query: %v", err)
	}
	if len(filtered) != 1 || filtered[0].Title != "Team sync" {
		t.Fatalf("query filter = %+v", filtered)
	}

	// Max cap.
	capped, err := b.List(ctx, ListQuery{From: from, To: to, Max: 2})
	if err != nil {
		t.Fatalf("List max: %v", err)
	}
	if len(capped) != 2 {
		t.Fatalf("max cap = %d, want 2", len(capped))
	}

	// All-day window.
	allday, err := b.List(ctx, ListQuery{
		From: time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("List allday: %v", err)
	}
	if len(allday) != 1 || !allday[0].AllDay || allday[0].Title != "Holiday" {
		t.Fatalf("all-day = %+v", allday)
	}
}

func TestICSIsReadOnly(t *testing.T) {
	b := newICSTestServer(t, sampleICS)
	ctx := context.Background()
	if b.Writable() {
		t.Fatal("ICS backend must be read-only")
	}
	if _, err := b.Create(ctx, Event{Title: "x"}); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Create err = %v, want ErrReadOnly", err)
	}
	if _, err := b.Update(ctx, Event{UID: "x"}); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Update err = %v, want ErrReadOnly", err)
	}
	if err := b.Delete(ctx, "x", "", ""); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Delete err = %v, want ErrReadOnly", err)
	}
}

func TestICSHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()
	b, err := newICS(Config{Kind: KindICS, URL: srv.URL})
	if err != nil {
		t.Fatalf("newICS: %v", err)
	}
	if _, err := b.List(context.Background(), ListQuery{
		From: time.Now(), To: time.Now().Add(time.Hour),
	}); err == nil {
		t.Fatal("expected HTTP error")
	}
}

func TestNewRejectsBadICSURL(t *testing.T) {
	if _, err := newICS(Config{Kind: KindICS, URL: "ftp://example.com/x.ics"}); err == nil {
		t.Fatal("expected scheme error")
	}
	if _, err := newICS(Config{Kind: KindICS, URL: ""}); err == nil {
		t.Fatal("expected required-url error")
	}
}
