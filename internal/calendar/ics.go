package calendar

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// icsTimeout bounds one ICS feed fetch.
const icsTimeout = 30 * time.Second

// icsMaxBytes caps a feed download (Google feeds can be large).
const icsMaxBytes = 8 << 20

// icsBackend reads a read-only iCalendar feed (Google's per-calendar "secret
// address"). The URL is the credential and is never logged.
type icsBackend struct {
	url        string
	calendarID string
	hc         *http.Client
}

// newICS validates the feed URL and returns a read-only backend.
func newICS(cfg Config) (*icsBackend, error) {
	raw := strings.TrimSpace(cfg.URL)
	if raw == "" {
		return nil, fmt.Errorf("calendar ICS url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid calendar ICS url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("calendar ICS url must be http(s)")
	}
	if u.Host == "" {
		return nil, fmt.Errorf("calendar ICS url has no host")
	}
	id := strings.TrimSpace(cfg.CalendarID)
	if id == "" {
		id = u.Host
	}
	return &icsBackend{url: raw, calendarID: id, hc: &http.Client{Timeout: icsTimeout}}, nil
}

// Writable always reports false: an ICS feed has no write path.
func (b *icsBackend) Writable() bool { return false }

// Calendars returns the single logical feed.
func (b *icsBackend) Calendars(context.Context) ([]Calendar, error) {
	return []Calendar{{ID: b.calendarID, Name: b.calendarID, Writable: false}}, nil
}

func (b *icsBackend) List(ctx context.Context, q ListQuery) ([]Event, error) {
	ctx, cancel := context.WithTimeout(ctx, icsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/calendar, text/plain;q=0.9, */*;q=0.5")
	req.Header.Set("User-Agent", "myogchat/1.0")
	resp, err := b.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch calendar feed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("fetch calendar feed: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, icsMaxBytes))
	if err != nil {
		return nil, fmt.Errorf("read calendar feed: %w", err)
	}
	cal, err := parseICS(body)
	if err != nil {
		return nil, err
	}
	events := calendarToEvents(cal, b.calendarID)

	from, to := q.From, q.To
	if from.IsZero() || to.IsZero() || !to.After(from) {
		// No usable window: return the raw feed (unexpanded) instead of nothing.
		return applyQuery(events, q), nil
	}
	expanded := expand(events, from, to)
	return applyQuery(expanded, q), nil
}

// Get returns one event by UID from the feed (read-only).
func (b *icsBackend) Get(ctx context.Context, uid, _ string) (Event, error) {
	ctx, cancel := context.WithTimeout(ctx, icsTimeout)
	defer cancel()
	events, err := b.List(ctx, ListQuery{
		From: time.Now().AddDate(-5, 0, 0),
		To:   time.Now().AddDate(5, 0, 0),
	})
	if err != nil {
		return Event{}, err
	}
	for _, ev := range events {
		if ev.UID == uid {
			return ev, nil
		}
	}
	return Event{}, fmt.Errorf("event %q not found", uid)
}

func (b *icsBackend) Create(context.Context, Event) (Event, error) { return Event{}, ErrReadOnly }
func (b *icsBackend) Update(context.Context, Event) (Event, error) { return Event{}, ErrReadOnly }
func (b *icsBackend) Delete(context.Context, string, string, string) error {
	return ErrReadOnly
}

// applyQuery filters by the optional text query and caps the result count.
func applyQuery(events []Event, q ListQuery) []Event {
	needle := strings.ToLower(strings.TrimSpace(q.Query))
	out := events
	if needle != "" {
		filtered := make([]Event, 0, len(events))
		for _, ev := range events {
			if strings.Contains(strings.ToLower(ev.Title), needle) ||
				strings.Contains(strings.ToLower(ev.Description), needle) ||
				strings.Contains(strings.ToLower(ev.Location), needle) {
				filtered = append(filtered, ev)
			}
		}
		out = filtered
	}
	if q.Max > 0 && len(out) > q.Max {
		out = out[:q.Max]
	}
	return out
}
