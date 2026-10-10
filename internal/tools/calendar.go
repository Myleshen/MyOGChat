package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"myogchat/internal/calendar"
)

// calendarMaxList caps how many events a single calendar_list call returns.
const calendarMaxList = 100

// parseWhen accepts an RFC3339 timestamp or a bare YYYY-MM-DD date (all-day).
func parseWhen(s string) (time.Time, bool, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false, errors.New("a date/time is required")
	}
	if len(s) == len("2006-01-02") && s[4] == '-' && s[7] == '-' {
		t, err := time.ParseInLocation("2006-01-02", s, time.UTC)
		if err != nil {
			return time.Time{}, false, err
		}
		return t, true, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("expected an RFC3339 timestamp or YYYY-MM-DD date, got %q", s)
	}
	return t, false, nil
}

// formatEvent renders one event as a compact line for the model.
func formatEvent(ev calendar.Event) string {
	var when string
	if ev.AllDay {
		when = ev.Start.Format("2006-01-02") + " (all day)"
	} else {
		tz := ev.TimeZone
		if tz == "" {
			tz = "UTC"
		}
		when = ev.Start.Format("2006-01-02 15:04") + "–" + ev.End.Format("15:04") + " " + tz
	}
	var b strings.Builder
	b.WriteString("- ")
	b.WriteString(when)
	b.WriteString("  ")
	b.WriteString(strings.TrimSpace(ev.Title))
	if ev.Location != "" {
		b.WriteString(" @ ")
		b.WriteString(ev.Location)
	}
	if len(ev.Recurrence) > 0 {
		b.WriteString(" [recurring]")
	}
	if len(ev.Attendees) > 0 {
		b.WriteString(" [attendees: ")
		b.WriteString(strings.Join(ev.Attendees, ", "))
		b.WriteString("]")
	}
	b.WriteString("  (uid: ")
	b.WriteString(ev.UID)
	b.WriteString(")")
	return b.String()
}

// CalendarListTool lists events in a date range.
type CalendarListTool struct{ b calendar.Backend }

// NewCalendarListTool wraps b as calendar_list.
func NewCalendarListTool(b calendar.Backend) *CalendarListTool { return &CalendarListTool{b: b} }

func (t *CalendarListTool) Name() string { return "calendar_list" }

func (t *CalendarListTool) Description() string {
	return "List calendar events in a time range. Use this to answer questions " +
		"about the user's schedule, find events, or look up an event's uid before " +
		"updating or deleting it."
}

func (t *CalendarListTool) Parameters() json.RawMessage {
	return schemaObj(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"from": map[string]any{
				"type":        "string",
				"description": "Start of the range: an RFC3339 timestamp or a YYYY-MM-DD date.",
			},
			"to": map[string]any{
				"type":        "string",
				"description": "End of the range: an RFC3339 timestamp or a YYYY-MM-DD date.",
			},
			"query": map[string]any{
				"type":        "string",
				"description": "Optional case-insensitive text filter over title, description and location.",
			},
			"calendar_id": map[string]any{
				"type":        "string",
				"description": "Optional calendar/collection id. Defaults to the connected calendar.",
			},
			"max": map[string]any{
				"type":        "integer",
				"description": "Maximum number of events to return (default 25, max 100).",
			},
		},
		"required": []string{"from", "to"},
	})
}

func (t *CalendarListTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		From       string `json:"from"`
		To         string `json:"to"`
		Query      string `json:"query"`
		CalendarID string `json:"calendar_id"`
		Max        int    `json:"max"`
	}
	if err := DecodeArgs(args, &in); err != nil {
		return "", err
	}
	from, _, err := parseWhen(in.From)
	if err != nil {
		return "", fmt.Errorf("from: %w", err)
	}
	to, _, err := parseWhen(in.To)
	if err != nil {
		return "", fmt.Errorf("to: %w", err)
	}
	if !to.After(from) {
		return "", errors.New("to must be after from")
	}
	max := in.Max
	if max <= 0 {
		max = 25
	}
	if max > calendarMaxList {
		max = calendarMaxList
	}
	events, err := t.b.List(ctx, calendar.ListQuery{
		From:       from,
		To:         to,
		Query:      in.Query,
		CalendarID: in.CalendarID,
		Max:        max,
	})
	if err != nil {
		return "", err
	}
	if len(events) == 0 {
		return "No events in that range.", nil
	}
	lines := make([]string, 0, len(events)+1)
	lines = append(lines, fmt.Sprintf("%d event(s):", len(events)))
	for _, ev := range events {
		lines = append(lines, formatEvent(ev))
	}
	return strings.Join(lines, "\n"), nil
}

// CalendarCreateTool creates a new event.
type CalendarCreateTool struct{ b calendar.Backend }

// NewCalendarCreateTool wraps b as calendar_create.
func NewCalendarCreateTool(b calendar.Backend) *CalendarCreateTool { return &CalendarCreateTool{b: b} }

func (t *CalendarCreateTool) Name() string { return "calendar_create" }

func (t *CalendarCreateTool) Description() string {
	return "Create a calendar event. Not supported on read-only (ICS) calendars."
}

func (t *CalendarCreateTool) Parameters() json.RawMessage {
	return schemaObj(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title":       map[string]any{"type": "string", "description": "Event title."},
			"start":       map[string]any{"type": "string", "description": "Start: RFC3339 timestamp or YYYY-MM-DD for an all-day event."},
			"end":         map[string]any{"type": "string", "description": "End: RFC3339 timestamp or YYYY-MM-DD (exclusive) for an all-day event."},
			"description": map[string]any{"type": "string"},
			"location":    map[string]any{"type": "string"},
			"all_day":     map[string]any{"type": "boolean", "description": "Treat start/end as dates (defaults to true when both are YYYY-MM-DD)."},
			"timezone":    map[string]any{"type": "string", "description": "IANA timezone for timed events, e.g. Europe/London."},
			"attendees":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Attendee email addresses."},
			"recurrence":  map[string]any{"type": "string", "description": "An RRULE, e.g. FREQ=WEEKLY;BYDAY=MO."},
			"reminders_minutes": map[string]any{
				"type": "array", "items": map[string]any{"type": "integer"},
				"description": "Minutes before the start to remind (e.g. [10]).",
			},
			"calendar_id": map[string]any{"type": "string", "description": "Optional target calendar/collection id."},
		},
		"required": []string{"title", "start", "end"},
	})
}

func (t *CalendarCreateTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	if !t.b.Writable() {
		return "", calendar.ErrReadOnly
	}
	var in struct {
		Title       string   `json:"title"`
		Start       string   `json:"start"`
		End         string   `json:"end"`
		Description string   `json:"description"`
		Location    string   `json:"location"`
		AllDay      bool     `json:"all_day"`
		TimeZone    string   `json:"timezone"`
		Attendees   []string `json:"attendees"`
		Recurrence  string   `json:"recurrence"`
		Reminders   []int    `json:"reminders_minutes"`
		CalendarID  string   `json:"calendar_id"`
	}
	if err := DecodeArgs(args, &in); err != nil {
		return "", err
	}
	if strings.TrimSpace(in.Title) == "" {
		return "", errors.New("title is required")
	}
	start, startAllDay, err := parseWhen(in.Start)
	if err != nil {
		return "", fmt.Errorf("start: %w", err)
	}
	end, endAllDay, err := parseWhen(in.End)
	if err != nil {
		return "", fmt.Errorf("end: %w", err)
	}
	allDay := in.AllDay || (startAllDay && endAllDay)
	if allDay {
		start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
		end = time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, time.UTC)
		if !end.After(start) {
			end = start.Add(24 * time.Hour)
		}
	} else if !end.After(start) {
		return "", errors.New("end must be after start")
	}
	ev := calendar.Event{
		Title:       strings.TrimSpace(in.Title),
		Description: in.Description,
		Location:    in.Location,
		Start:       start,
		End:         end,
		TimeZone:    strings.TrimSpace(in.TimeZone),
		AllDay:      allDay,
		Attendees:   cleanStrings(in.Attendees),
		Reminders:   remindersFromMinutes(in.Reminders),
		CalendarID:  in.CalendarID,
	}
	if r := strings.TrimSpace(in.Recurrence); r != "" {
		ev.Recurrence = []string{r}
	}
	created, err := t.b.Create(ctx, ev)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Created event %q (uid %s): %s", created.Title, created.UID, formatEvent(created)), nil
}

// CalendarUpdateTool updates an existing event (whole series).
type CalendarUpdateTool struct{ b calendar.Backend }

// NewCalendarUpdateTool wraps b as calendar_update.
func NewCalendarUpdateTool(b calendar.Backend) *CalendarUpdateTool { return &CalendarUpdateTool{b: b} }

func (t *CalendarUpdateTool) Name() string { return "calendar_update" }

func (t *CalendarUpdateTool) Description() string {
	return "Update an existing calendar event by uid (edits the whole series). " +
		"Not supported on read-only (ICS) calendars."
}

func (t *CalendarUpdateTool) Parameters() json.RawMessage {
	return schemaObj(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"uid":         map[string]any{"type": "string", "description": "The uid of the event to update (from calendar_list)."},
			"title":       map[string]any{"type": "string"},
			"start":       map[string]any{"type": "string"},
			"end":         map[string]any{"type": "string"},
			"description": map[string]any{"type": "string"},
			"location":    map[string]any{"type": "string"},
			"all_day":     map[string]any{"type": "boolean"},
			"timezone":    map[string]any{"type": "string"},
			"attendees":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"recurrence":  map[string]any{"type": "string", "description": "New RRULE, or \"\" to clear recurrence."},
			"reminders_minutes": map[string]any{
				"type": "array", "items": map[string]any{"type": "integer"},
			},
			"calendar_id": map[string]any{"type": "string"},
		},
		"required": []string{"uid"},
	})
}

func (t *CalendarUpdateTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	if !t.b.Writable() {
		return "", calendar.ErrReadOnly
	}
	var in struct {
		UID         string    `json:"uid"`
		Title       *string   `json:"title"`
		Start       *string   `json:"start"`
		End         *string   `json:"end"`
		Description *string   `json:"description"`
		Location    *string   `json:"location"`
		AllDay      *bool     `json:"all_day"`
		TimeZone    *string   `json:"timezone"`
		Attendees   *[]string `json:"attendees"`
		Recurrence  *string   `json:"recurrence"`
		Reminders   *[]int    `json:"reminders_minutes"`
		CalendarID  string    `json:"calendar_id"`
	}
	if err := DecodeArgs(args, &in); err != nil {
		return "", err
	}
	uid := strings.TrimSpace(in.UID)
	if uid == "" {
		return "", errors.New("uid is required")
	}
	ev, err := t.b.Get(ctx, uid, in.CalendarID)
	if err != nil {
		return "", err
	}
	if in.Title != nil {
		ev.Title = strings.TrimSpace(*in.Title)
	}
	if in.Description != nil {
		ev.Description = *in.Description
	}
	if in.Location != nil {
		ev.Location = *in.Location
	}
	if in.TimeZone != nil {
		ev.TimeZone = strings.TrimSpace(*in.TimeZone)
	}
	if in.AllDay != nil {
		ev.AllDay = *in.AllDay
	}
	if in.Start != nil {
		start, _, err := parseWhen(*in.Start)
		if err != nil {
			return "", fmt.Errorf("start: %w", err)
		}
		ev.Start = start
	}
	if in.End != nil {
		end, _, err := parseWhen(*in.End)
		if err != nil {
			return "", fmt.Errorf("end: %w", err)
		}
		ev.End = end
	}
	if in.Attendees != nil {
		ev.Attendees = cleanStrings(*in.Attendees)
	}
	if in.Reminders != nil {
		ev.Reminders = remindersFromMinutes(*in.Reminders)
	}
	if in.Recurrence != nil {
		if r := strings.TrimSpace(*in.Recurrence); r == "" {
			ev.Recurrence = nil
		} else {
			ev.Recurrence = []string{r}
		}
	}
	if ev.AllDay {
		ev.Start = time.Date(ev.Start.Year(), ev.Start.Month(), ev.Start.Day(), 0, 0, 0, 0, time.UTC)
		ev.End = time.Date(ev.End.Year(), ev.End.Month(), ev.End.Day(), 0, 0, 0, 0, time.UTC)
	}
	if !ev.End.After(ev.Start) {
		return "", errors.New("end must be after start")
	}
	updated, err := t.b.Update(ctx, ev)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Updated event %q (uid %s): %s", updated.Title, updated.UID, formatEvent(updated)), nil
}

// CalendarDeleteTool deletes an event.
type CalendarDeleteTool struct{ b calendar.Backend }

// NewCalendarDeleteTool wraps b as calendar_delete.
func NewCalendarDeleteTool(b calendar.Backend) *CalendarDeleteTool { return &CalendarDeleteTool{b: b} }

func (t *CalendarDeleteTool) Name() string { return "calendar_delete" }

func (t *CalendarDeleteTool) Description() string {
	return "Delete a calendar event by uid. Not supported on read-only (ICS) calendars."
}

func (t *CalendarDeleteTool) Parameters() json.RawMessage {
	return schemaObj(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"uid":         map[string]any{"type": "string", "description": "The uid of the event to delete."},
			"calendar_id": map[string]any{"type": "string", "description": "Optional calendar/collection id."},
		},
		"required": []string{"uid"},
	})
}

func (t *CalendarDeleteTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	if !t.b.Writable() {
		return "", calendar.ErrReadOnly
	}
	var in struct {
		UID        string `json:"uid"`
		CalendarID string `json:"calendar_id"`
	}
	if err := DecodeArgs(args, &in); err != nil {
		return "", err
	}
	uid := strings.TrimSpace(in.UID)
	if uid == "" {
		return "", errors.New("uid is required")
	}
	// Resolve the ETag first so CalDAV deletes are conditional.
	etag := ""
	if ev, err := t.b.Get(ctx, uid, in.CalendarID); err == nil {
		etag = ev.ETag
	}
	if err := t.b.Delete(ctx, uid, in.CalendarID, etag); err != nil {
		return "", err
	}
	return fmt.Sprintf("Deleted event %s.", uid), nil
}

// cleanStrings trims and drops empty entries.
func cleanStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func remindersFromMinutes(mins []int) []calendar.Reminder {
	out := make([]calendar.Reminder, 0, len(mins))
	for _, m := range mins {
		out = append(out, calendar.Reminder{Minutes: m, Action: "DISPLAY"})
	}
	return out
}
