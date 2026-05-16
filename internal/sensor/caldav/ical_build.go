package caldav

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/google/uuid"
)

// EventSpec is the input to BuildEventICS — everything needed to mint
// a fresh VEVENT for `add_event` / `block_calendar`. UID is generated
// when empty.
type EventSpec struct {
	UID         string
	Title       string
	Start       time.Time
	End         time.Time
	Location    string
	Description string
	// Categories is optional; if non-empty rendered as CATEGORIES so
	// the V2.5 personal-tag projection picks it up. Pass "personal"
	// for blocks that should surface as src=personal cards next time.
	Categories []string
}

// BuildEventICS renders spec as a complete VCALENDAR + VEVENT
// document. Returns the iCal text plus the UID (the same as
// spec.UID, or the generated one). Used for both create
// (PutEvent) and brand-new add_event flows.
func BuildEventICS(spec EventSpec) (ics, uid string, err error) {
	if spec.Title == "" {
		return "", "", fmt.Errorf("ical: title is required")
	}
	if spec.End.Before(spec.Start) || spec.End.Equal(spec.Start) {
		return "", "", fmt.Errorf("ical: end must be after start")
	}
	uid = spec.UID
	if uid == "" {
		uid = uuid.NewString() + "@zeno"
	}

	event := ical.NewEvent()
	event.Props.SetText(ical.PropUID, uid)
	event.Props.SetDateTime(ical.PropDateTimeStamp, time.Now().UTC())
	event.Props.SetText(ical.PropSummary, spec.Title)
	event.Props.SetDateTime(ical.PropDateTimeStart, spec.Start)
	event.Props.SetDateTime(ical.PropDateTimeEnd, spec.End)
	if spec.Location != "" {
		event.Props.SetText(ical.PropLocation, spec.Location)
	}
	if spec.Description != "" {
		event.Props.SetText(ical.PropDescription, spec.Description)
	}
	if len(spec.Categories) > 0 {
		event.Props.SetText(ical.PropCategories, strings.Join(spec.Categories, ","))
	}

	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Props.SetText(ical.PropProductID, "-//Zeno V2.8//EN")
	cal.Children = append(cal.Children, event.Component)

	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		return "", "", fmt.Errorf("ical encode: %w", err)
	}
	return buf.String(), uid, nil
}

// PartStat is one of ACCEPTED / DECLINED / TENTATIVE.
type PartStat string

const (
	PartStatAccepted  PartStat = "ACCEPTED"
	PartStatDeclined  PartStat = "DECLINED"
	PartStatTentative PartStat = "TENTATIVE"
)

// SetAttendeePartStat parses sourceICS, mutates the ATTENDEE line whose
// CAL-ADDRESS (mailto:...) matches userMailto (case-insensitive), and
// returns the re-encoded iCal. If no matching ATTENDEE is found a new
// one is appended — required for invites the server delivered without
// the user already as a participant.
//
// status is the new PARTSTAT value (use the PartStat constants).
func SetAttendeePartStat(sourceICS, userMailto string, status PartStat) (string, error) {
	if sourceICS == "" {
		return "", fmt.Errorf("ical: empty source")
	}
	if userMailto == "" {
		return "", fmt.Errorf("ical: userMailto is required")
	}
	if !strings.HasPrefix(strings.ToLower(userMailto), "mailto:") {
		userMailto = "mailto:" + strings.TrimSpace(userMailto)
	}

	dec := ical.NewDecoder(strings.NewReader(sourceICS))
	cal, err := dec.Decode()
	if err != nil {
		return "", fmt.Errorf("ical decode: %w", err)
	}

	want := strings.ToLower(strings.TrimSpace(userMailto))
	for _, ev := range cal.Events() {
		props := ev.Props.Values(ical.PropAttendee)
		matched := false
		for i := range props {
			val := strings.ToLower(strings.TrimSpace(props[i].Value))
			if val == want {
				if props[i].Params == nil {
					props[i].Params = ical.Params{}
				}
				props[i].Params.Set("PARTSTAT", string(status))
				matched = true
				break
			}
		}
		if matched {
			ev.Props[ical.PropAttendee] = props
			continue
		}
		// No matching ATTENDEE — append one. The CAL-ADDRESS goes in
		// the property's Value; PARTSTAT is a parameter.
		newAttendee := ical.Prop{
			Name:   ical.PropAttendee,
			Params: ical.Params{},
			Value:  userMailto,
		}
		newAttendee.Params.Set("PARTSTAT", string(status))
		ev.Props[ical.PropAttendee] = append(props, newAttendee)
	}

	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		return "", fmt.Errorf("ical encode: %w", err)
	}
	return buf.String(), nil
}

// MutateEventTimes parses sourceICS, replaces the first VEVENT's
// DTSTART and (when newEnd is non-zero) DTEND properties with the
// supplied values, and returns the re-encoded iCalendar text.
//
// Used by the V2.8.1 reschedule_event Executor: the Executor reads
// the existing event, builds a new ICS via this helper, then PUTs
// with If-Match against the previous ETag.
func MutateEventTimes(sourceICS string, newStart, newEnd time.Time) (string, error) {
	if sourceICS == "" {
		return "", fmt.Errorf("ical: empty source")
	}
	if newStart.IsZero() {
		return "", fmt.Errorf("ical: newStart is required")
	}

	dec := ical.NewDecoder(strings.NewReader(sourceICS))
	cal, err := dec.Decode()
	if err != nil {
		return "", fmt.Errorf("ical decode: %w", err)
	}
	events := cal.Events()
	if len(events) == 0 {
		return "", fmt.Errorf("ical: no VEVENT")
	}
	ev := events[0]
	ev.Props.SetDateTime(ical.PropDateTimeStart, newStart.UTC())
	if !newEnd.IsZero() {
		ev.Props.SetDateTime(ical.PropDateTimeEnd, newEnd.UTC())
	}
	// DTSTAMP refresh signals to the server that this is a real edit
	// (some servers reject PUTs whose DTSTAMP didn't move).
	ev.Props.SetDateTime(ical.PropDateTimeStamp, time.Now().UTC())

	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		return "", fmt.Errorf("ical encode: %w", err)
	}
	return buf.String(), nil
}

// EventSummary parses sourceICS and returns the first event's title +
// time range. Used by RSVP executors to build a confirmation toast and
// by the modal preview for add_event.
func EventSummary(sourceICS string) (title string, start, end time.Time, err error) {
	dec := ical.NewDecoder(strings.NewReader(sourceICS))
	cal, decErr := dec.Decode()
	if decErr != nil {
		return "", time.Time{}, time.Time{}, fmt.Errorf("ical decode: %w", decErr)
	}
	events := cal.Events()
	if len(events) == 0 {
		return "", time.Time{}, time.Time{}, fmt.Errorf("ical: no VEVENT")
	}
	ev := events[0]
	title, _ = ev.Props.Text(ical.PropSummary)
	if v, e := ev.Props.DateTime(ical.PropDateTimeStart, time.UTC); e == nil {
		start = v
	}
	if v, e := ev.Props.DateTime(ical.PropDateTimeEnd, time.UTC); e == nil {
		end = v
	}
	return
}
