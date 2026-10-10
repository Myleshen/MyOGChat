package calendar

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/caldav"
)

// caldavTimeout bounds one CalDAV operation.
const caldavTimeout = 45 * time.Second

// caldavBackend talks CalDAV with HTTP Basic auth (app-specific password).
type caldavBackend struct {
	username     string
	password     string
	endpoint     string // normalized base URL
	calendarPath string // default collection path
	homeSet      string // cached calendar-home-set

	base   *url.URL
	hc     *http.Client
	client *caldav.Client
}

// newCalDAV validates the endpoint and builds a client. No network I/O happens
// here; discovery is lazy.
func newCalDAV(cfg Config) (*caldavBackend, error) {
	raw := strings.TrimSpace(cfg.Endpoint)
	if raw == "" {
		return nil, fmt.Errorf("calendar server url is required")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	base, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid calendar server url: %w", err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("unsupported calendar server url scheme %q", base.Scheme)
	}
	if base.Host == "" {
		return nil, fmt.Errorf("calendar server url has no host")
	}
	base.Path = strings.TrimRight(base.Path, "/")
	base.RawQuery = ""
	base.Fragment = ""
	endpoint := base.String()

	hc := &http.Client{Timeout: caldavTimeout}
	authClient := webdav.HTTPClientWithBasicAuth(hc, cfg.Username, cfg.Password)
	client, err := caldav.NewClient(authClient, endpoint)
	if err != nil {
		return nil, fmt.Errorf("caldav client: %w", err)
	}
	b := &caldavBackend{
		username:     cfg.Username,
		password:     cfg.Password,
		endpoint:     endpoint,
		calendarPath: normalizeCollectionPath(cfg.CalendarPath),
		homeSet:      normalizeCollectionPath(cfg.HomeSet),
		base:         base,
		hc:           hc,
		client:       client,
	}
	return b, nil
}

// Writable reports true: CalDAV supports create/update/delete.
func (b *caldavBackend) Writable() bool { return true }

// normalizeCollectionPath ensures a non-empty collection path ends in "/".
func normalizeCollectionPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// resolveEndpoint follows the RFC 6764 /.well-known/caldav redirect to find the
// real CalDAV context URL (iCloud redirects to the pNN partition host). On any
// failure the original endpoint is used.
func (b *caldavBackend) resolveEndpoint(ctx context.Context) string {
	wk := *b.base
	wk.Path = strings.TrimRight(wk.Path, "/") + "/.well-known/caldav"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wk.String(), nil)
	if err != nil {
		return b.endpoint
	}
	req.Header.Set("User-Agent", "myogchat/1.0")
	noRedirect := &http.Client{
		Timeout:       caldavTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		return b.endpoint
	}
	defer resp.Body.Close()
	if resp.StatusCode < 300 || resp.StatusCode > 399 {
		return b.endpoint
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		return b.endpoint
	}
	u, err := url.Parse(loc)
	if err != nil {
		return b.endpoint
	}
	resolved := b.base.ResolveReference(u)
	resolved.RawQuery = ""
	resolved.Fragment = ""
	return strings.TrimRight(resolved.String(), "/")
}

// discover resolves the principal, calendar-home-set, and available calendars,
// caching them on the backend. It is a no-op when already resolved.
func (b *caldavBackend) discover(ctx context.Context) error {
	if b.homeSet != "" && b.calendarPath != "" {
		return nil
	}
	// Re-point the client at the well-known context URL if one exists.
	if resolved := b.resolveEndpoint(ctx); resolved != b.endpoint {
		b.endpoint = resolved
		if u, err := url.Parse(resolved); err == nil {
			b.base = u
		}
		client, err := caldav.NewClient(webdav.HTTPClientWithBasicAuth(b.hc, b.username, b.password), resolved)
		if err != nil {
			return err
		}
		b.client = client
	}
	principal, err := b.client.FindCurrentUserPrincipal(ctx)
	if err != nil {
		return fmt.Errorf("caldav: find principal: %w", err)
	}
	home, err := b.client.FindCalendarHomeSet(ctx, principal)
	if err != nil {
		return fmt.Errorf("caldav: find calendar home set: %w", err)
	}
	b.homeSet = normalizeCollectionPath(home)
	cals, err := b.client.FindCalendars(ctx, home)
	if err != nil {
		return fmt.Errorf("caldav: list calendars: %w", err)
	}
	if b.calendarPath == "" {
		if len(cals) == 0 {
			return fmt.Errorf("caldav: no calendars found")
		}
		b.calendarPath = normalizeCollectionPath(cals[0].Path)
	}
	return nil
}

// collection returns the collection path to operate on (the query override or
// the account default), discovering it if necessary.
func (b *caldavBackend) collection(ctx context.Context, override string) (string, error) {
	if override != "" {
		return normalizeCollectionPath(override), nil
	}
	if b.calendarPath != "" {
		return b.calendarPath, nil
	}
	if err := b.discover(ctx); err != nil {
		return "", err
	}
	return b.calendarPath, nil
}

func (b *caldavBackend) Calendars(ctx context.Context) ([]Calendar, error) {
	ctx, cancel := context.WithTimeout(ctx, caldavTimeout)
	defer cancel()
	if err := b.discover(ctx); err != nil {
		return nil, err
	}
	return b.listCalendars(ctx)
}

func (b *caldavBackend) listCalendars(ctx context.Context) ([]Calendar, error) {
	cals, err := b.client.FindCalendars(ctx, b.homeSet)
	if err != nil {
		return nil, fmt.Errorf("caldav: list calendars: %w", err)
	}
	out := make([]Calendar, 0, len(cals))
	for _, c := range cals {
		out = append(out, Calendar{ID: c.Path, Name: c.Name, Description: c.Description, Writable: true})
	}
	return out, nil
}

// discoverAll runs discovery and returns the resolved endpoint, home set and
// calendars (used when connecting an account).
func (b *caldavBackend) discoverAll(ctx context.Context) (Discovery, error) {
	ctx, cancel := context.WithTimeout(ctx, caldavTimeout)
	defer cancel()
	if err := b.discover(ctx); err != nil {
		return Discovery{}, err
	}
	cals, err := b.listCalendars(ctx)
	if err != nil {
		return Discovery{}, err
	}
	return Discovery{
		Endpoint:   b.endpoint,
		HomeSet:    b.homeSet,
		CalendarID: b.calendarPath,
		Calendars:  cals,
	}, nil
}

func (b *caldavBackend) List(ctx context.Context, q ListQuery) ([]Event, error) {
	ctx, cancel := context.WithTimeout(ctx, caldavTimeout)
	defer cancel()
	path, err := b.collection(ctx, q.CalendarID)
	if err != nil {
		return nil, err
	}
	from, to := q.From, q.To
	if from.IsZero() || to.IsZero() || !to.After(from) {
		from = time.Now().AddDate(-1, 0, 0)
		to = time.Now().AddDate(1, 0, 0)
	}
	objs, err := b.query(ctx, path, from, to)
	if err != nil {
		return nil, err
	}
	events := make([]Event, 0, len(objs))
	for _, obj := range objs {
		for _, ev := range calendarToEvents(obj.Data, path) {
			ev.ETag = obj.ETag
			ev.ResourcePath = obj.Path
			events = append(events, ev)
		}
	}
	return applyQuery(events, q), nil
}

// query runs a calendar-query REPORT over a time range with server-side
// recurrence expansion.
func (b *caldavBackend) query(ctx context.Context, path string, from, to time.Time) ([]caldav.CalendarObject, error) {
	cq := &caldav.CalendarQuery{
		CompRequest: caldav.CalendarCompRequest{
			Name:     "VEVENT",
			AllProps: true,
			AllComps: true,
			Expand:   &caldav.CalendarExpandRequest{Start: from, End: to},
		},
		CompFilter: caldav.CompFilter{
			Name: "VCALENDAR",
			Comps: []caldav.CompFilter{
				{Name: "VEVENT", Start: from, End: to},
			},
		},
	}
	return b.client.QueryCalendar(ctx, path, cq)
}

// Get returns one event by UID (whole series).
func (b *caldavBackend) Get(ctx context.Context, uid, calendarID string) (Event, error) {
	ctx, cancel := context.WithTimeout(ctx, caldavTimeout)
	defer cancel()
	path, err := b.collection(ctx, calendarID)
	if err != nil {
		return Event{}, err
	}
	obj, err := b.findObject(ctx, path, uid)
	if err != nil {
		return Event{}, err
	}
	for _, ev := range calendarToEvents(obj.Data, path) {
		if ev.UID == uid {
			ev.ETag = obj.ETag
			ev.ResourcePath = obj.Path
			return ev, nil
		}
	}
	return Event{}, fmt.Errorf("event %q not found", uid)
}

// findObject locates a calendar object by UID, returning its path and ETag.
func (b *caldavBackend) findObject(ctx context.Context, path, uid string) (caldav.CalendarObject, error) {
	// Preferred: a UID prop-filter (cheap, exact).
	cq := &caldav.CalendarQuery{
		CompRequest: caldav.CalendarCompRequest{Name: "VEVENT", AllProps: true, AllComps: true},
		CompFilter: caldav.CompFilter{
			Name: "VCALENDAR",
			Comps: []caldav.CompFilter{
				{Name: "VEVENT", Props: []caldav.PropFilter{{Name: "UID", TextMatch: &caldav.TextMatch{Text: uid}}}},
			},
		},
	}
	if objs, err := b.client.QueryCalendar(ctx, path, cq); err == nil {
		for _, obj := range objs {
			if objectHasUID(obj, uid) {
				return obj, nil
			}
		}
	}
	// Fallback: a wide time-range scan.
	objs, err := b.query(ctx, path, time.Now().AddDate(-10, 0, 0), time.Now().AddDate(10, 0, 0))
	if err != nil {
		return caldav.CalendarObject{}, err
	}
	for _, obj := range objs {
		if objectHasUID(obj, uid) {
			return obj, nil
		}
	}
	return caldav.CalendarObject{}, fmt.Errorf("event %q not found", uid)
}

func objectHasUID(obj caldav.CalendarObject, uid string) bool {
	if obj.Data == nil {
		return false
	}
	for _, ev := range obj.Data.Events() {
		if u, _ := ev.Props.Text(ical.PropUID); u == uid {
			return true
		}
	}
	return false
}

func (b *caldavBackend) Create(ctx context.Context, ev Event) (Event, error) {
	ctx, cancel := context.WithTimeout(ctx, caldavTimeout)
	defer cancel()
	path, err := b.collection(ctx, ev.CalendarID)
	if err != nil {
		return Event{}, err
	}
	if strings.TrimSpace(ev.UID) == "" {
		ev.UID = newUID()
	}
	cal, err := eventToCalendar(ev)
	if err != nil {
		return Event{}, err
	}
	objPath := path + url.PathEscape(ev.UID) + ".ics"
	resp, err := b.raw(ctx, http.MethodPut, objPath, cal, map[string]string{
		"If-None-Match": "*",
		"Content-Type":  ical.MIMEType,
	})
	if err != nil {
		return Event{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Event{}, fmt.Errorf("caldav create failed: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	ev.ETag = unquoteETag(resp.Header.Get("ETag"))
	ev.ResourcePath = objPath
	if loc := resp.Header.Get("Location"); loc != "" {
		if u, err := url.Parse(loc); err == nil {
			ev.ResourcePath = u.Path
		}
	}
	ev.CalendarID = path
	return ev, nil
}

func (b *caldavBackend) Update(ctx context.Context, ev Event) (Event, error) {
	ctx, cancel := context.WithTimeout(ctx, caldavTimeout)
	defer cancel()
	if strings.TrimSpace(ev.UID) == "" {
		return Event{}, fmt.Errorf("event uid is required")
	}
	path, err := b.collection(ctx, ev.CalendarID)
	if err != nil {
		return Event{}, err
	}
	objPath := ev.ResourcePath
	etag := ev.ETag
	if objPath == "" || etag == "" {
		obj, err := b.findObject(ctx, path, ev.UID)
		if err != nil {
			return Event{}, err
		}
		objPath = obj.Path
		etag = obj.ETag
	}
	cal, err := eventToCalendar(ev)
	if err != nil {
		return Event{}, err
	}
	headers := map[string]string{"Content-Type": ical.MIMEType}
	if etag != "" {
		headers["If-Match"] = quoteETag(etag)
	}
	resp, err := b.raw(ctx, http.MethodPut, objPath, cal, headers)
	if err != nil {
		return Event{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusPreconditionFailed {
		return Event{}, fmt.Errorf("caldav update conflict: the event changed on the server (HTTP 412)")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Event{}, fmt.Errorf("caldav update failed: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if newTag := unquoteETag(resp.Header.Get("ETag")); newTag != "" {
		ev.ETag = newTag
	} else {
		ev.ETag = etag
	}
	ev.ResourcePath = objPath
	ev.CalendarID = path
	return ev, nil
}

func (b *caldavBackend) Delete(ctx context.Context, uid, calendarID, etag string) error {
	ctx, cancel := context.WithTimeout(ctx, caldavTimeout)
	defer cancel()
	if strings.TrimSpace(uid) == "" {
		return fmt.Errorf("event uid is required")
	}
	path, err := b.collection(ctx, calendarID)
	if err != nil {
		return err
	}
	objPath := ""
	if etag != "" {
		// The caller already knows the resource; assume the conventional path.
		objPath = path + url.PathEscape(uid) + ".ics"
	} else {
		obj, err := b.findObject(ctx, path, uid)
		if err != nil {
			return err
		}
		objPath = obj.Path
		etag = obj.ETag
	}
	headers := map[string]string{}
	if etag != "" {
		headers["If-Match"] = quoteETag(etag)
	}
	resp, err := b.raw(ctx, http.MethodDelete, objPath, nil, headers)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("event %q not found", uid)
	}
	if resp.StatusCode == http.StatusPreconditionFailed {
		return fmt.Errorf("caldav delete conflict: the event changed on the server (HTTP 412)")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("caldav delete failed: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// raw issues an authenticated CalDAV request against an absolute collection or
// object path.
func (b *caldavBackend) raw(ctx context.Context, method, path string, cal *ical.Calendar, headers map[string]string) (*http.Response, error) {
	u := *b.base
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	u.Path = path
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	var body io.Reader
	if cal != nil {
		var buf bytes.Buffer
		if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
			return nil, fmt.Errorf("encode iCalendar: %w", err)
		}
		body = &buf
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(b.username, b.password)
	req.Header.Set("User-Agent", "myogchat/1.0")
	req.Header.Set("Accept", ical.MIMEType)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return b.hc.Do(req)
}

func unquoteETag(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if s, err := strconv.Unquote(v); err == nil {
		return s
	}
	return v
}

func quoteETag(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if strings.HasPrefix(v, "\"") {
		return v
	}
	return strconv.Quote(v)
}

// newUID returns a random RFC 4122 version-4 UUID.
func newUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is unrecoverable; fall back to a time-based id.
		return fmt.Sprintf("evt-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
