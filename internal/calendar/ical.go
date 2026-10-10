package calendar

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/emersion/go-ical"
)

const prodID = "-//MyOGChat//Calendar 3.3//EN"

// eventToCalendar builds a single-VEVENT iCalendar object for ev.
func eventToCalendar(ev Event) (*ical.Calendar, error) {
	if strings.TrimSpace(ev.UID) == "" {
		return nil, fmt.Errorf("event uid is required")
	}
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Props.SetText(ical.PropProductID, prodID)

	e := ical.NewEvent()
	e.Props.SetText(ical.PropUID, ev.UID)
	e.Props.SetDateTime(ical.PropDateTimeStamp, time.Now().UTC())
	if ev.Title != "" {
		e.Props.SetText(ical.PropSummary, ev.Title)
	}
	if ev.Description != "" {
		e.Props.SetText(ical.PropDescription, ev.Description)
	}
	if ev.Location != "" {
		e.Props.SetText(ical.PropLocation, ev.Location)
	}

	loc := time.UTC
	if !ev.AllDay && ev.TimeZone != "" {
		loc = loadLocation(ev.TimeZone)
	}
	if ev.AllDay {
		e.Props.SetDate(ical.PropDateTimeStart, ev.Start)
		e.Props.SetDate(ical.PropDateTimeEnd, ev.End)
	} else {
		e.Props.SetDateTime(ical.PropDateTimeStart, ev.Start.In(loc))
		e.Props.SetDateTime(ical.PropDateTimeEnd, ev.End.In(loc))
	}

	for _, entry := range ev.Recurrence {
		if err := applyRecurrenceLine(e, entry); err != nil {
			return nil, err
		}
	}

	for _, addr := range ev.Attendees {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(addr), "mailto:") {
			addr = "mailto:" + addr
		}
		p := ical.NewProp(ical.PropAttendee)
		p.SetValueType(ical.ValueCalendarAddress)
		p.Value = addr
		e.Props.Add(p)
	}

	for _, r := range ev.Reminders {
		action := strings.TrimSpace(r.Action)
		if action == "" {
			action = "DISPLAY"
		}
		alarm := ical.NewComponent(ical.CompAlarm)
		alarm.Props.SetText(ical.PropAction, strings.ToUpper(action))
		trig := ical.NewProp(ical.PropTrigger)
		trig.SetDuration(-time.Duration(r.Minutes) * time.Minute)
		alarm.Props.Set(trig)
		if strings.EqualFold(action, "DISPLAY") {
			alarm.Props.SetText(ical.PropDescription, ev.Title)
		}
		e.Children = append(e.Children, alarm)
	}

	cal.Children = []*ical.Component{e.Component}
	return cal, nil
}

// applyRecurrenceLine applies one recurrence entry to a VEVENT. Entries may be
// a bare RRULE value ("FREQ=WEEKLY") or prefixed ("RRULE:…", "RDATE:…",
// "EXDATE:…").
func applyRecurrenceLine(e *ical.Event, entry string) error {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return nil
	}
	name := ical.PropRecurrenceRule
	value := entry
	if i := strings.IndexByte(entry, ':'); i > 0 {
		candidate := strings.ToUpper(entry[:i])
		switch candidate {
		case ical.PropRecurrenceRule, ical.PropRecurrenceDates, ical.PropExceptionDates:
			name = candidate
			value = entry[i+1:]
		}
	}
	switch name {
	case ical.PropRecurrenceRule:
		p := ical.NewProp(ical.PropRecurrenceRule)
		p.SetValueType(ical.ValueRecurrence)
		p.Value = strings.ToUpper(value)
		e.Props.Set(p) // at most one RRULE
	case ical.PropRecurrenceDates, ical.PropExceptionDates:
		for _, part := range strings.Split(value, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			p := ical.NewProp(name)
			if len(part) == len("20060102") {
				p.SetValueType(ical.ValueDate)
			} else {
				p.SetValueType(ical.ValueDateTime)
			}
			p.Value = part
			e.Props.Add(p)
		}
	}
	return nil
}

// calendarToEvents converts every VEVENT in cal into normalized Events. It does
// not expand recurrences; callers that need instances use expand().
func calendarToEvents(cal *ical.Calendar, calendarID string) []Event {
	out := make([]Event, 0)
	for _, ev := range cal.Events() {
		parsed, ok := eventFromComponent(ev.Component, calendarID)
		if ok {
			out = append(out, parsed)
		}
	}
	return out
}

// eventFromComponent converts one VEVENT component.
func eventFromComponent(comp *ical.Component, calendarID string) (Event, bool) {
	uid, _ := comp.Props.Text(ical.PropUID)
	if strings.TrimSpace(uid) == "" {
		return Event{}, false
	}
	ev := Event{UID: uid, CalendarID: calendarID}
	ev.Title, _ = comp.Props.Text(ical.PropSummary)
	ev.Description, _ = comp.Props.Text(ical.PropDescription)
	ev.Location, _ = comp.Props.Text(ical.PropLocation)

	startProp := comp.Props.Get(ical.PropDateTimeStart)
	if startProp == nil {
		return Event{}, false
	}
	allDay := startProp.ValueType() == ical.ValueDate
	ev.AllDay = allDay
	tz := startProp.Params.Get(ical.PropTimezoneID)
	loc := time.UTC
	if !allDay {
		loc = loadLocation(tz)
		ev.TimeZone = normalizeTZ(loc)
		if tz != "" && loadLocation(tz) != time.UTC {
			ev.TimeZone = tz
		}
	}
	start, err := startProp.DateTime(loc)
	if err != nil {
		return Event{}, false
	}
	ev.Start = start
	if end, err := (&ical.Event{Component: comp}).DateTimeEnd(loc); err == nil {
		ev.End = end
	}
	if ev.End.IsZero() {
		if allDay {
			ev.End = ev.Start.Add(24 * time.Hour)
		} else {
			ev.End = ev.Start
		}
	}

	for _, name := range []string{ical.PropRecurrenceRule, ical.PropRecurrenceDates, ical.PropExceptionDates} {
		for _, p := range comp.Props.Values(name) {
			if name == ical.PropRecurrenceRule {
				ev.Recurrence = append(ev.Recurrence, "RRULE:"+p.Value)
			} else {
				ev.Recurrence = append(ev.Recurrence, name+":"+p.Value)
			}
		}
	}

	for _, p := range comp.Props.Values(ical.PropAttendee) {
		if addr := attendeeEmail(p.Value); addr != "" {
			ev.Attendees = append(ev.Attendees, addr)
		}
	}

	for _, child := range comp.Children {
		if child.Name != ical.CompAlarm {
			continue
		}
		if r, ok := reminderFromAlarm(child, ev.Start); ok {
			ev.Reminders = append(ev.Reminders, r)
		}
	}
	return ev, true
}

func attendeeEmail(v string) string {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(strings.ToLower(v), "mailto:") {
		v = v[len("mailto:"):]
	}
	return v
}

func reminderFromAlarm(alarm *ical.Component, start time.Time) (Reminder, bool) {
	action, _ := alarm.Props.Text(ical.PropAction)
	trig := alarm.Props.Get(ical.PropTrigger)
	if trig == nil {
		return Reminder{}, false
	}
	var mins int
	if trig.ValueType() == ical.ValueDateTime {
		t, err := trig.DateTime(time.UTC)
		if err != nil {
			return Reminder{}, false
		}
		mins = int(start.Sub(t).Minutes())
	} else {
		dur, err := trig.Duration()
		if err != nil {
			return Reminder{}, false
		}
		mins = int(-dur.Minutes())
	}
	return Reminder{Minutes: mins, Action: action}, true
}

// parseICS decodes an iCalendar byte stream and returns its events (unexpanded).
func parseICS(data []byte) (*ical.Calendar, error) {
	cal, err := ical.NewDecoder(bytes.NewReader(data)).Decode()
	if err != nil {
		return nil, fmt.Errorf("parse iCalendar: %w", err)
	}
	return cal, nil
}

// expand returns events overlapping [from, to). Recurring events are expanded
// client-side (used by the ICS backend, which has no server-side expand);
// non-recurring events are included when their span overlaps the window.
func expand(events []Event, from, to time.Time) []Event {
	out := make([]Event, 0, len(events))
	for _, ev := range events {
		if !hasRRule(ev.Recurrence) {
			if overlaps(ev, from, to) {
				out = append(out, ev)
			}
			continue
		}
		loc := time.UTC
		if ev.TimeZone != "" {
			loc = loadLocation(ev.TimeZone)
		}
		comp, err := eventComponentFor(ev)
		if err != nil {
			if overlaps(ev, from, to) {
				out = append(out, ev)
			}
			continue
		}
		set, err := comp.RecurrenceSet(loc)
		if err != nil || set == nil {
			if overlaps(ev, from, to) {
				out = append(out, ev)
			}
			continue
		}
		dur := ev.End.Sub(ev.Start)
		for _, s := range set.Between(from, to, true) {
			inst := ev
			inst.Start = s
			inst.End = s.Add(dur)
			// Instances share the master UID (standard iCalendar); the start
			// time disambiguates them in the listing.
			out = append(out, inst)
		}
	}
	return out
}

func hasRRule(recurrence []string) bool {
	for _, r := range recurrence {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(r)), "RRULE") {
			return true
		}
	}
	return false
}

func overlaps(ev Event, from, to time.Time) bool {
	return ev.Start.Before(to) && (ev.End.After(from) || ev.End.Equal(from))
}

// eventComponentFor rebuilds a VEVENT component from a normalized event so its
// recurrence set can be expanded.
func eventComponentFor(ev Event) (*ical.Event, error) {
	cal, err := eventToCalendar(ev)
	if err != nil {
		return nil, err
	}
	events := cal.Events()
	if len(events) == 0 {
		return nil, fmt.Errorf("no VEVENT built")
	}
	return &events[0], nil
}
