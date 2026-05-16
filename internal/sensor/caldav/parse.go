package caldav

import (
	"fmt"
	"strings"
	"time"

	"github.com/emersion/go-ical"
)

// Event is the canonical decoded form of a single VEVENT.
//
// Attendees holds human-readable names — CN= when present, otherwise the
// local-part of the mailto: address. LastModified is LAST-MODIFIED in UTC
// (zero when the source omitted it). Both are V2.3.0 P3 additions feeding
// the inject detector's calendar-move path.
type Event struct {
	UID          string
	Title        string
	Location     string
	Tag          string
	Start        time.Time
	End          time.Time
	Attendees    []string
	LastModified time.Time
}

// ParseVEVENT parses a single ICS payload (as returned by ListEvents) into
// a canonical Event. For recurring events, only the first instance is
// returned — RRULE expansion is deferred to Phase 2.
func ParseVEVENT(ics string, loc *time.Location) (Event, error) {
	if loc == nil {
		loc = time.UTC
	}
	dec := ical.NewDecoder(strings.NewReader(ics))
	cal, err := dec.Decode()
	if err != nil {
		return Event{}, fmt.Errorf("decode ics: %w", err)
	}
	events := cal.Events()
	if len(events) == 0 {
		return Event{}, fmt.Errorf("no VEVENT in ICS")
	}
	v := events[0]

	uid, err := v.Props.Text(ical.PropUID)
	if err != nil || uid == "" {
		return Event{}, fmt.Errorf("missing UID")
	}

	start, err := v.DateTimeStart(loc)
	if err != nil || start.IsZero() {
		return Event{}, fmt.Errorf("missing or invalid DTSTART")
	}
	end, err := v.DateTimeEnd(loc)
	if err != nil {
		end = time.Time{}
	}
	if end.IsZero() {
		// All-day event with no DTEND: treat as 24h.
		end = start.Add(24 * time.Hour)
	}

	title, _ := v.Props.Text(ical.PropSummary)
	location, _ := v.Props.Text(ical.PropLocation)

	tag := deriveTag(v)

	return Event{
		UID:          uid,
		Title:        title,
		Location:     location,
		Tag:          tag,
		Start:        start,
		End:          end,
		Attendees:    extractAttendees(v),
		LastModified: extractLastModified(v),
	}, nil
}

// extractAttendees returns the human-readable display name for every
// ATTENDEE on the VEVENT. Preference order: CN parameter, then the
// local-part of the mailto: URI in the property value. Empty/duplicate
// entries are dropped. The result is stable in source order.
func extractAttendees(e ical.Event) []string {
	props := e.Props[ical.PropAttendee]
	if len(props) == 0 {
		return nil
	}
	out := make([]string, 0, len(props))
	seen := map[string]bool{}
	for _, p := range props {
		name := strings.TrimSpace(p.Params.Get(ical.ParamCommonName))
		if name == "" {
			val := strings.TrimSpace(p.Value)
			val = strings.TrimPrefix(strings.ToLower(val), "mailto:")
			if at := strings.IndexByte(val, '@'); at > 0 {
				name = val[:at]
			} else {
				name = val
			}
		}
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, name)
	}
	return out
}

// extractLastModified parses the LAST-MODIFIED property if present. RFC 5545
// says LAST-MODIFIED is always UTC; on parse failure we return zero so the
// downstream detector treats the event as "not recently modified".
func extractLastModified(e ical.Event) time.Time {
	prop := e.Props.Get(ical.PropLastModified)
	if prop == nil {
		return time.Time{}
	}
	t, err := prop.DateTime(time.UTC)
	if err != nil {
		return time.Time{}
	}
	return t
}

// deriveTag picks a category label for the event. Order of preference:
//  1. CATEGORIES (first entry, lowercased)
//  2. X-APPLE-CALENDAR-COLOR mapped to "work" (red/orange) or "personal"
//     (blue/green/yellow). The mapping is heuristic; missing colors return "".
func deriveTag(e ical.Event) string {
	if prop := e.Props.Get(ical.PropCategories); prop != nil {
		cats, err := prop.TextList()
		if err == nil {
			for _, c := range cats {
				c = strings.TrimSpace(strings.ToLower(c))
				if c != "" {
					return c
				}
			}
		}
	}
	if prop := e.Props.Get("X-APPLE-CALENDAR-COLOR"); prop != nil {
		return colorToTag(strings.ToUpper(strings.TrimSpace(prop.Value)))
	}
	return ""
}

// colorToTag maps Apple's hex calendar colors to either "work" or "personal".
func colorToTag(hex string) string {
	hex = strings.TrimPrefix(hex, "#")
	if len(hex) < 6 {
		return ""
	}
	hex = hex[:6]
	switch hex {
	case "FF2D55", "FF3B30", "FF9500", "FF6B00":
		return "work" // reds and oranges
	case "34C759", "30D158", "00C7BE":
		return "personal" // greens and teals
	case "007AFF", "5856D6", "AF52DE":
		return "personal" // blues and purples
	case "FFCC00", "FFD60A":
		return "personal" // yellows
	default:
		return ""
	}
}
