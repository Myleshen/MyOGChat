// Package calendar provides a provider-agnostic calendar backend for feature
// 3.3. It normalizes two very different wire protocols behind one Event model:
//
//   - CalDAV (Apple/iCloud, Zoho, self-hosted Radicale/Nextcloud) via
//     github.com/emersion/go-webdav + github.com/emersion/go-ical, using HTTP
//     Basic auth with an app-specific password. Read/write.
//   - Google's per-calendar "secret address" ICS URL — a plain unauthenticated
//     GET of an iCalendar feed. Read-only (owner decision: no OAuth/consent).
//
// The package is deliberately free of server/store types so it can be unit
// tested in isolation, exactly like internal/tools.
package calendar

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Kind discriminates the credential/backend type stored per user.
type Kind string

const (
	// KindCalDAV is a CalDAV collection (Apple/iCloud, Zoho, Radicale, …).
	KindCalDAV Kind = "caldav"
	// KindICS is a read-only iCalendar feed URL (Google secret address).
	KindICS Kind = "ics"
)

// ErrReadOnly is returned by write operations on a read-only backend.
var ErrReadOnly = errors.New("this calendar is read-only (ICS feed) — creating, updating and deleting events is not supported")

// Reminder is a normalized VALARM: minutes before the event start, plus the
// alarm action (e.g. DISPLAY).
type Reminder struct {
	Minutes int    `json:"minutes"`
	Action  string `json:"action,omitempty"`
}

// Event is the normalized, provider-agnostic event shape.
type Event struct {
	UID         string     `json:"uid"`
	CalendarID  string     `json:"calendar_id,omitempty"`
	Title       string     `json:"title"`
	Description string     `json:"description,omitempty"`
	Location    string     `json:"location,omitempty"`
	Start       time.Time  `json:"start"`
	End         time.Time  `json:"end"`
	TimeZone    string     `json:"tz,omitempty"` // IANA name (empty for all-day)
	AllDay      bool       `json:"all_day"`
	Recurrence  []string   `json:"recurrence,omitempty"` // raw RRULE/RDATE/EXDATE
	Attendees   []string   `json:"attendees,omitempty"`  // email addresses
	Reminders   []Reminder `json:"reminders,omitempty"`
	ETag        string     `json:"etag,omitempty"` // CalDAV concurrency token

	// ResourcePath is the CalDAV calendar-object path (server-internal). It is
	// not part of the model-facing shape but lets update/delete address the
	// exact resource without re-discovering it.
	ResourcePath string `json:"-"`
}

// Calendar is a calendar collection (CalDAV) or a named feed (ICS).
type Calendar struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Writable    bool   `json:"writable"`
}

// ListQuery bounds a list operation. From/To are inclusive/exclusive; Query is
// an optional case-insensitive text filter over title/description/location.
type ListQuery struct {
	From       time.Time
	To         time.Time
	Query      string
	CalendarID string
	Max        int
}

// Backend is a calendar the tools can talk to.
type Backend interface {
	// List returns events overlapping [From, To).
	List(ctx context.Context, q ListQuery) ([]Event, error)
	// Get returns one event by UID.
	Get(ctx context.Context, uid, calendarID string) (Event, error)
	// Create stores a new event and returns it (with UID/ETag populated).
	Create(ctx context.Context, ev Event) (Event, error)
	// Update replaces an existing event (whole series; v1 does not edit single
	// instances) and returns the stored event.
	Update(ctx context.Context, ev Event) (Event, error)
	// Delete removes an event by UID.
	Delete(ctx context.Context, uid, calendarID, etag string) error
	// Calendars lists the collections available to this account.
	Calendars(ctx context.Context) ([]Calendar, error)
	// Writable reports whether Create/Update/Delete are supported.
	Writable() bool
}

// Config builds a backend from stored credentials.
type Config struct {
	Kind Kind

	// CalDAV.
	Endpoint     string // server base URL, e.g. https://caldav.icloud.com
	Username     string // Apple ID / Zoho email (not secret)
	Password     string // app-specific password (secret)
	CalendarPath string // default collection path (optional; discovered if empty)
	HomeSet      string // cached calendar-home-set (optional)

	// ICS.
	URL string // secret ICS feed URL (secret)

	// Shared.
	CalendarID string // default calendar id/name for tools
}

// New builds the backend for cfg. It performs no network I/O.
func New(cfg Config) (Backend, error) {
	switch cfg.Kind {
	case KindCalDAV:
		return newCalDAV(cfg)
	case KindICS:
		return newICS(cfg)
	default:
		return nil, fmt.Errorf("unknown calendar kind %q", cfg.Kind)
	}
}

// Discovery is the result of validating/connecting a calendar account. The
// resolved Endpoint and default CalendarID are cached so later calls skip
// discovery (iCloud redirects to a per-account partition host).
type Discovery struct {
	Endpoint   string
	HomeSet    string
	CalendarID string
	Calendars  []Calendar
}

// Discover validates cfg by connecting to the backend and, for CalDAV,
// resolving the principal, home set, and calendars. For ICS it fetches and
// parses the feed. It is used when a user connects an account.
func Discover(ctx context.Context, cfg Config) (Discovery, error) {
	b, err := New(cfg)
	if err != nil {
		return Discovery{}, err
	}
	switch v := b.(type) {
	case *caldavBackend:
		return v.discoverAll(ctx)
	case *icsBackend:
		cals, err := v.Calendars(ctx)
		if err != nil {
			return Discovery{}, err
		}
		// A cheap read validates that the feed parses.
		now := time.Now()
		if _, err := v.List(ctx, ListQuery{From: now.AddDate(0, -1, 0), To: now.AddDate(0, 1, 0)}); err != nil {
			return Discovery{}, err
		}
		return Discovery{CalendarID: v.calendarID, Calendars: cals}, nil
	default:
		return Discovery{}, fmt.Errorf("unknown calendar backend")
	}
}

// loadLocation resolves an IANA timezone name, falling back to UTC for unknown
// or non-IANA ids (iCloud sometimes returns Windows-style or
// /freeassociation.sourceforge.net/… TZIDs).
func loadLocation(tz string) *time.Location {
	tz = strings.TrimSpace(tz)
	if tz == "" || tz == "UTC" {
		return time.UTC
	}
	// Some clients prefix TZIDs with "/" or a vendor path; try the tail.
	candidates := []string{tz}
	if i := strings.LastIndex(tz, "/"); i >= 0 && i+1 < len(tz) {
		candidates = append(candidates, tz[i+1:])
	}
	for _, c := range candidates {
		if loc, err := time.LoadLocation(c); err == nil {
			return loc
		}
	}
	return time.UTC
}

// normalizeTZ returns an IANA timezone name for a location, defaulting to UTC.
func normalizeTZ(loc *time.Location) string {
	if loc == nil || loc == time.UTC {
		return "UTC"
	}
	return loc.String()
}
