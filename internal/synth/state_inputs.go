package synth

import (
	"sort"
	"time"

	"github.com/zenocy/zeno-v2/internal/projection"
)

// StateInputs is the typed surface DetectState reads. Every field is
// derivable from current projections + the clock + (Phase 3) an inject
// signal — no I/O, no LLM call.
type StateInputs struct {
	// NextMeetingMinutes is the count of minutes from now to the start of
	// the next attended meeting in the next 6h window. -1 means "no meeting
	// in horizon" — used by DetectState to gate StatePreMeeting.
	NextMeetingMinutes int

	// NextMeetingAttendees is the attendee count of that nearest meeting.
	// 0 when NextMeetingMinutes == -1.
	NextMeetingAttendees int

	// UnbookedBlockHours is the largest contiguous unbooked stretch in the
	// next 6h, in hours. Floors to fractional hours so 2h59m reads as ~2.98.
	UnbookedBlockHours float64

	// Weekday is the local-tz weekday at synth time.
	Weekday time.Weekday

	// LocalHour is the local-tz hour (0..23) at synth time.
	LocalHour int

	// HasInjectSignal is set only by Phase 3's inject pipeline. Morning synth
	// runs always pass false here — inject is its own Runner.
	HasInjectSignal bool

	// InjectSignalKind names the inject path that fired ("email" |
	// "calendar_move"). Empty when HasInjectSignal is false.
	InjectSignalKind string
}

// InjectSignal is the cross-package marker for "a high-signal mid-day event
// arrived" — produced by internal/sensor/inject_detector.go in Phase 3 and
// consumed by Phase 3's inject synth. Phase 0 forward-declared it; Phase 3
// fills in the producer.
type InjectSignal struct {
	Kind       string    // "email" | "calendar_move" | "calendar_soon" | "stock_breach" | "stock_move" | "thread_reply"
	Subject    string    // human-readable subject line for the inject card
	EvidenceID string    // event-log ID of the underlying observation
	At         time.Time // when the signal was detected

	// EntityKey is the V2.x continuity anchor for the entity this signal is
	// about ("thread:...", "cal:...", "ticker:..."). When non-empty and Mode
	// is "update", the inject persist path uses it as the card ID so the
	// reactive card refreshes the existing morning card in place instead of
	// appending a duplicate. Empty → legacy append-only behavior.
	EntityKey string
	// Mode is "update" when the signal's entity already has a card today (so
	// the reactive synth should refresh it) or "append" (the default) for a
	// brand-new inject card. Empty is treated as "append".
	Mode string
}

// Inject modes.
const (
	InjectModeAppend = "append"
	InjectModeUpdate = "update"
)

// stateInputHorizon is the look-ahead window for next-meeting and
// unbooked-block computations.
const stateInputHorizon = 6 * time.Hour

// stateInputAttendeeFloor — calendar events with strictly fewer attendees
// than this are treated as personal/block events: they consume calendar
// real estate (so they do break a deep-work block), but they don't qualify
// as "the next meeting" for the pre_meeting rule.
//
// Set to 1: an event with at least one attendee on the user's calendar can
// be a meeting. (The user counts.) The pre_meeting rule additionally
// requires PreMeetingMinAttendees so a 1-attendee event still falls
// through to morning_calm at the rule layer.
const stateInputAttendeeFloor = 1

// BuildStateInputs constructs the typed inputs from current projections.
// Pure; no I/O. now is the synth pass timestamp; cal is today's calendar
// projection (may include past events — they're filtered out); threads is
// the open-email projection (not used in V2.3 detector but pinned for
// V2.3.1 weighting); signal is the optional Phase 3 inject signal.
//
// Behavior:
//   - NextMeetingMinutes / NextMeetingAttendees: nearest upcoming event in
//     [now, now+6h] with at least 1 attendee. -1 / 0 if none.
//   - UnbookedBlockHours: largest contiguous gap in [now, now+6h], with all
//     events (including blocks/personal) consuming calendar real estate.
//   - Weekday / LocalHour: from now (time.Now() carries the loc).
func BuildStateInputs(now time.Time, cal []projection.CalendarEvent, threads []projection.Thread, signal *InjectSignal) StateInputs {
	_ = threads // V2.3.1 may use threads for VIP weighting; pinned for shape stability.

	in := StateInputs{
		NextMeetingMinutes:   -1,
		NextMeetingAttendees: 0,
		Weekday:              now.Weekday(),
		LocalHour:            now.Hour(),
	}
	if signal != nil {
		in.HasInjectSignal = true
		in.InjectSignalKind = signal.Kind
	}

	horizonEnd := now.Add(stateInputHorizon)

	// Filter to events that intersect the horizon, sort by start.
	relevant := make([]projection.CalendarEvent, 0, len(cal))
	for _, ev := range cal {
		if ev.End.Before(now) || ev.Start.After(horizonEnd) || ev.Start.Equal(horizonEnd) {
			continue
		}
		relevant = append(relevant, ev)
	}
	sort.SliceStable(relevant, func(i, j int) bool {
		return relevant[i].Start.Before(relevant[j].Start)
	})

	// NextMeetingMinutes: nearest upcoming event with attendees.
	for _, ev := range relevant {
		if ev.Start.Before(now) {
			continue // event already started; doesn't count as "next" — allow-pm-language
		}
		attendees := attendeeCount(ev)
		if attendees < stateInputAttendeeFloor {
			continue
		}
		in.NextMeetingMinutes = int(ev.Start.Sub(now).Minutes())
		in.NextMeetingAttendees = attendees
		break
	}

	// UnbookedBlockHours: largest contiguous gap in [now, horizonEnd] given
	// the relevant events. All events consume time (blocks count too).
	in.UnbookedBlockHours = largestUnbookedHours(now, horizonEnd, relevant)

	return in
}

// attendeeCount returns the count the detector should use for an event.
//
// V2.3.0 P3 plumbed real ATTENDEE data through the calendar pipeline, so
// this is now just a thin wrapper around len(ev.Attendees) plus the
// long-standing rule that block/focus/personal-tagged events never count
// as a meeting (they consume calendar time for the unbooked-block
// computation but never trigger pre_meeting). The Phase 1 carry-forward
// item — replacing tag inference with real counts — is closed by this
// change. Events from older event-log payloads (pre-P3) that lack
// attendees fall through with count 0, matching the "no meeting in
// horizon" semantics for legacy data.
func attendeeCount(ev projection.CalendarEvent) int {
	switch ev.Tag {
	case "block", "focus", "personal":
		return 0
	}
	return len(ev.Attendees)
}

// largestUnbookedHours returns the largest contiguous gap in [start, end]
// given a sorted slice of events that intersect the window. Events
// overlapping start are clamped to start; events overlapping end are
// clamped to end. Returns 0 when the window is fully booked.
func largestUnbookedHours(start, end time.Time, events []projection.CalendarEvent) float64 {
	if !end.After(start) {
		return 0
	}
	cursor := start
	var best time.Duration
	for _, ev := range events {
		evStart := ev.Start
		if evStart.Before(cursor) {
			evStart = cursor
		}
		if evStart.After(end) {
			evStart = end
		}
		evEnd := ev.End
		if evEnd.After(end) {
			evEnd = end
		}
		if gap := evStart.Sub(cursor); gap > best {
			best = gap
		}
		if evEnd.After(cursor) {
			cursor = evEnd
		}
	}
	if tail := end.Sub(cursor); tail > best {
		best = tail
	}
	return best.Hours()
}
