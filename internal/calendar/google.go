package calendar

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Google's OAuth + Calendar endpoints and the one scope we ask for (3.3).
const (
	// GoogleAuthURL is the consent screen.
	GoogleAuthURL = "https://accounts.google.com/o/oauth2/v2/auth"
	// GoogleTokenURL exchanges a code and refreshes tokens.
	GoogleTokenURL = "https://oauth2.googleapis.com/token"
	// GoogleRevokeURL revokes an access or refresh token.
	GoogleRevokeURL = "https://oauth2.googleapis.com/revoke"
	// GoogleAPIBase is the Calendar v3 API root.
	GoogleAPIBase = "https://www.googleapis.com/calendar/v3"
	// GoogleReadOnlyScope is the only scope requested: no write access at all.
	GoogleReadOnlyScope = "https://www.googleapis.com/auth/calendar.readonly"
)

const (
	// googleTimeout bounds one Calendar API call.
	googleTimeout = 30 * time.Second
	// googleTokenTimeout bounds one token/revoke call.
	googleTokenTimeout = 15 * time.Second
	// googleRefreshSkew refreshes this long before the access token expires.
	googleRefreshSkew = 2 * time.Minute
	// googleMaxPages caps one List (250 events per page).
	//
	// shortcut: 10 pages per list; widen if a single window can hold more.
	googleMaxPages = 10
	// googleMaxBytes caps one API response body.
	googleMaxBytes = 8 << 20
)

// Token is the stored Google OAuth credential. It is encrypted into
// calendar_accounts.credential and never leaves the server.
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	Scope        string    `json:"scope,omitempty"`
	Expiry       time.Time `json:"expiry"` // RFC3339
}

// googleBackend reads Google calendars through the Calendar v3 API with a
// stored OAuth token. Read-only: the granted scope allows nothing else.
type googleBackend struct {
	clientID     string
	clientSecret string
	token        Token
	saveToken    func(Token) error // persists a refreshed token; may be nil
	calendarID   string            // default calendar id ("primary" by default)

	tokenURL string
	apiBase  string
	hc       *http.Client
	now      func() time.Time
}

// newGoogle validates the stored token and returns a read-only backend. No
// network I/O happens here.
func newGoogle(cfg Config) (*googleBackend, error) {
	if strings.TrimSpace(cfg.Token.AccessToken) == "" && strings.TrimSpace(cfg.Token.RefreshToken) == "" {
		return nil, fmt.Errorf("google calendar: no token stored — connect Google Calendar")
	}
	b := &googleBackend{
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		token:        cfg.Token,
		saveToken:    cfg.SaveToken,
		// CalendarPath is the per-request override (PATCH /api/me/calendar);
		// CalendarID is the account default.
		calendarID: strings.TrimSpace(cfg.CalendarPath),
		tokenURL:   cfg.TokenURL,
		apiBase:    cfg.APIBase,
		hc:         cfg.HTTPClient,
		now:        time.Now,
	}
	if b.calendarID == "" {
		b.calendarID = strings.TrimSpace(cfg.CalendarID)
	}
	if b.calendarID == "" {
		b.calendarID = "primary"
	}
	if b.tokenURL == "" {
		b.tokenURL = GoogleTokenURL
	}
	if b.apiBase == "" {
		b.apiBase = GoogleAPIBase
	}
	if b.hc == nil {
		b.hc = &http.Client{Timeout: googleTimeout}
	}
	return b, nil
}

// Writable always reports false: the readonly scope grants no writes.
func (b *googleBackend) Writable() bool { return false }

// calendar returns the calendar id to operate on: the per-call override or the
// account default.
func (b *googleBackend) calendar(override string) string {
	if id := strings.TrimSpace(override); id != "" {
		return id
	}
	return b.calendarID
}

func (b *googleBackend) Calendars(ctx context.Context) ([]Calendar, error) {
	ctx, cancel := context.WithTimeout(ctx, googleTimeout)
	defer cancel()
	var body googleCalendarList
	if err := b.getJSON(ctx, "/users/me/calendarList", nil, &body); err != nil {
		return nil, err
	}
	out := make([]Calendar, 0, len(body.Items))
	for _, it := range body.Items {
		id := it.ID
		// Google accepts "primary" wherever a calendar id goes, and the primary
		// calendar's real id is the account's email address. Store the stable
		// alias so the default survives an address change.
		if it.Primary {
			id = "primary"
		}
		if id == "" {
			continue
		}
		out = append(out, Calendar{ID: id, Name: it.Summary, Description: it.Description, Writable: false})
	}
	return out, nil
}

func (b *googleBackend) List(ctx context.Context, q ListQuery) ([]Event, error) {
	ctx, cancel := context.WithTimeout(ctx, googleTimeout)
	defer cancel()
	from, to := q.From, q.To
	if from.IsZero() || to.IsZero() || !to.After(from) {
		from = time.Now().AddDate(-1, 0, 0)
		to = time.Now().AddDate(1, 0, 0)
	}
	params := url.Values{
		"timeMin":      {from.Format(time.RFC3339)},
		"timeMax":      {to.Format(time.RFC3339)},
		"singleEvents": {"true"},
		"orderBy":      {"startTime"},
		"showDeleted":  {"false"},
		"maxResults":   {"250"},
	}
	events, err := b.listEvents(ctx, b.calendar(q.CalendarID), params)
	if err != nil {
		return nil, err
	}
	return applyQuery(events, q), nil
}

// listEvents pages through events.list. Recurring events arrive expanded
// (singleEvents=true), so no recurrence handling is needed here.
func (b *googleBackend) listEvents(ctx context.Context, calendarID string, params url.Values) ([]Event, error) {
	cal := url.PathEscape(calendarID)
	var out []Event
	pageToken := ""
	for page := 0; page < googleMaxPages; page++ {
		p := url.Values{}
		for k, v := range params {
			p[k] = v
		}
		if pageToken != "" {
			p.Set("pageToken", pageToken)
		}
		var body googleEventList
		if err := b.getJSON(ctx, "/calendars/"+cal+"/events", p, &body); err != nil {
			return nil, err
		}
		for _, it := range body.Items {
			ev, ok := googleToEvent(it, calendarID)
			if ok {
				out = append(out, ev)
			}
		}
		pageToken = body.NextPageToken
		if pageToken == "" {
			break
		}
	}
	return out, nil
}

// Get resolves one event by its iCalendar UID.
//
// shortcut: iCalUID only; an event Google never gave an iCalUID to is not
// found. Add a window scan if that ever matters.
func (b *googleBackend) Get(ctx context.Context, uid, calendarID string) (Event, error) {
	ctx, cancel := context.WithTimeout(ctx, googleTimeout)
	defer cancel()
	if strings.TrimSpace(uid) == "" {
		return Event{}, fmt.Errorf("event uid is required")
	}
	events, err := b.listEvents(ctx, b.calendar(calendarID), url.Values{
		"iCalUID":      {uid},
		"singleEvents": {"true"},
		"maxResults":   {"1"},
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

func (b *googleBackend) Create(context.Context, Event) (Event, error) { return Event{}, ErrReadOnly }
func (b *googleBackend) Update(context.Context, Event) (Event, error) { return Event{}, ErrReadOnly }
func (b *googleBackend) Delete(context.Context, string, string, string) error {
	return ErrReadOnly
}

// getJSON issues an authenticated GET and decodes the JSON body.
func (b *googleBackend) getJSON(ctx context.Context, path string, q url.Values, v any) error {
	access, err := b.accessToken(ctx)
	if err != nil {
		return err
	}
	target := strings.TrimRight(b.apiBase, "/") + path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "myogchat/1.0")
	resp, err := b.hc.Do(req)
	if err != nil {
		return fmt.Errorf("google calendar: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, googleMaxBytes))
	if err != nil {
		return fmt.Errorf("google calendar: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("google calendar: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(truncateBody(body)))
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("google calendar: decode response: %w", err)
	}
	return nil
}

// accessToken returns a usable access token, refreshing at most once per call.
func (b *googleBackend) accessToken(ctx context.Context) (string, error) {
	if strings.TrimSpace(b.token.AccessToken) != "" && b.now().Add(googleRefreshSkew).Before(b.token.Expiry) {
		return b.token.AccessToken, nil
	}
	if strings.TrimSpace(b.token.RefreshToken) == "" {
		return "", fmt.Errorf("google calendar: the access token expired and no refresh token is stored — reconnect Google Calendar")
	}
	return b.refresh(ctx)
}

// refresh trades the stored refresh token for a new access token and persists
// the result through saveToken, so the DB row never goes stale.
func (b *googleBackend) refresh(ctx context.Context) (string, error) {
	tr, err := postToken(ctx, b.hc, b.tokenURL, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {b.token.RefreshToken},
		"client_id":     {b.clientID},
		"client_secret": {b.clientSecret},
	})
	if err != nil {
		return "", err
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("google calendar: token refresh returned no access token — reconnect Google Calendar")
	}
	tok := b.token
	tok.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		tok.RefreshToken = tr.RefreshToken
	}
	if tr.TokenType != "" {
		tok.TokenType = tr.TokenType
	}
	if tr.Scope != "" {
		tok.Scope = tr.Scope
	}
	tok.Expiry = b.now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	if b.saveToken != nil {
		if err := b.saveToken(tok); err != nil {
			return "", fmt.Errorf("google calendar: persist refreshed token: %w", err)
		}
	}
	b.token = tok
	return tok.AccessToken, nil
}

// ExchangeCode trades an OAuth authorization code for a token.
func ExchangeCode(ctx context.Context, hc *http.Client, tokenURL, clientID, clientSecret, redirectURI, code string) (Token, error) {
	tr, err := postToken(ctx, hc, tokenURL, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"redirect_uri":  {redirectURI},
	})
	if err != nil {
		return Token{}, err
	}
	if tr.AccessToken == "" {
		return Token{}, fmt.Errorf("google calendar: the token endpoint returned no access token")
	}
	if tr.RefreshToken == "" {
		return Token{}, fmt.Errorf("google calendar: no refresh token was returned — remove MyOGChat at myaccount.google.com/permissions and connect again")
	}
	return Token{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		TokenType:    tr.TokenType,
		Scope:        tr.Scope,
		Expiry:       time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second),
	}, nil
}

// RevokeToken asks Google to revoke an access or refresh token.
func RevokeToken(ctx context.Context, hc *http.Client, revokeURL, token string) error {
	if strings.TrimSpace(revokeURL) == "" {
		revokeURL = GoogleRevokeURL
	}
	ctx, cancel := context.WithTimeout(ctx, googleTokenTimeout)
	defer cancel()
	if hc == nil {
		hc = &http.Client{Timeout: googleTokenTimeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, revokeURL,
		strings.NewReader(url.Values{"token": {token}}.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "myogchat/1.0")
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("google calendar: revoke request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("google calendar: revoke HTTP %d", resp.StatusCode)
	}
	return nil
}

// tokenResponse is the OAuth token endpoint's JSON body (success or error).
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// postToken posts an OAuth form body and maps Google's error shape to a plain
// error.
func postToken(ctx context.Context, hc *http.Client, tokenURL string, form url.Values) (tokenResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, googleTokenTimeout)
	defer cancel()
	if hc == nil {
		hc = &http.Client{Timeout: googleTokenTimeout}
	}
	if strings.TrimSpace(tokenURL) == "" {
		tokenURL = GoogleTokenURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "myogchat/1.0")
	resp, err := hc.Do(req)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("google calendar: token request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return tokenResponse{}, fmt.Errorf("google calendar: read token response: %w", err)
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return tokenResponse{}, fmt.Errorf("google calendar: token endpoint HTTP %d: %s", resp.StatusCode, strings.TrimSpace(truncateBody(body)))
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 || tr.Error != "" {
		msg := strings.TrimSpace(tr.ErrorDesc)
		if msg == "" {
			msg = strings.TrimSpace(tr.Error)
		}
		if msg == "" {
			msg = "HTTP " + strconv.Itoa(resp.StatusCode)
		}
		return tokenResponse{}, fmt.Errorf("google calendar: %s", msg)
	}
	return tr, nil
}

// googleCalendarList is calendarList.list (only the fields we map).
type googleCalendarList struct {
	Items []struct {
		ID          string `json:"id"`
		Summary     string `json:"summary"`
		Description string `json:"description"`
		Primary     bool   `json:"primary"`
	} `json:"items"`
}

// googleEventList is events.list.
type googleEventList struct {
	Items         []googleEventItem `json:"items"`
	NextPageToken string            `json:"nextPageToken"`
}

type googleEventItem struct {
	ID          string     `json:"id"`
	ICalUID     string     `json:"iCalUID"`
	Status      string     `json:"status"`
	Summary     string     `json:"summary"`
	Description string     `json:"description"`
	Location    string     `json:"location"`
	Start       googleWhen `json:"start"`
	End         googleWhen `json:"end"`
	Attendees   []struct {
		Email string `json:"email"`
	} `json:"attendees"`
}

// googleWhen is an event's start or end: date (all-day) or dateTime (timed).
type googleWhen struct {
	Date     string `json:"date"`
	DateTime string `json:"dateTime"`
	TimeZone string `json:"timeZone"`
}

// parse returns the timestamp, whether it is an all-day date, and its zone.
func (w googleWhen) parse() (time.Time, bool, string, bool) {
	if w.Date != "" {
		t, err := time.Parse("2006-01-02", w.Date)
		if err != nil {
			return time.Time{}, false, "", false
		}
		return t, true, "", true
	}
	if w.DateTime == "" {
		return time.Time{}, false, "", false
	}
	t, err := time.Parse(time.RFC3339, w.DateTime)
	if err != nil {
		return time.Time{}, false, "", false
	}
	// Render in the event's own IANA zone when Google supplies one.
	if w.TimeZone != "" {
		t = t.In(loadLocation(w.TimeZone))
	}
	return t, false, w.TimeZone, true
}

// googleToEvent maps one API event to the normalized shape. Cancelled events
// and events with an unparsable start are dropped.
func googleToEvent(it googleEventItem, calendarID string) (Event, bool) {
	if it.Status == "cancelled" {
		return Event{}, false
	}
	start, allDay, zone, ok := it.Start.parse()
	if !ok {
		return Event{}, false
	}
	ev := Event{
		UID:        it.ICalUID,
		CalendarID: calendarID,
		Title:      it.Summary,
		Start:      start,
		AllDay:     allDay,
	}
	if ev.UID == "" {
		ev.UID = it.ID
	}
	if it.Description != "" {
		ev.Description = it.Description
	}
	if it.Location != "" {
		ev.Location = it.Location
	}
	if !allDay {
		ev.TimeZone = zone
	}
	if end, _, _, ok := it.End.parse(); ok {
		ev.End = end
	}
	for _, a := range it.Attendees {
		if a.Email != "" {
			ev.Attendees = append(ev.Attendees, a.Email)
		}
	}
	return ev, true
}

// truncateBody keeps an error body short enough for a log line or an HTTP
// error message.
func truncateBody(body []byte) string {
	const max = 512
	s := string(body)
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
