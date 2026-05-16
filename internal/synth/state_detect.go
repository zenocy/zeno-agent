package synth

import "time"

// Detector thresholds. Hardcoded for V2.3.0; configurable via config.yaml
// in V2.3.x once we see auto-defaults fail. Decisions live in
// doc/TODO.md::V2.3.0 decisions.
const (
	// PreMeetingMaxMinutes — a meeting within this many minutes from now
	// trips StatePreMeeting (provided attendee count meets the floor).
	PreMeetingMaxMinutes = 120

	// PreMeetingMinAttendees — a "real meeting" floor. Excludes 1:1s with
	// the user's calendar showing themselves as a single attendee, and
	// excludes solo blocks/personal events that already have 0 attendees.
	PreMeetingMinAttendees = 3

	// DeepWorkMinHours — a contiguous unbooked stretch must be at least this
	// many hours in the next 6h horizon to trip StateDeepWork.
	DeepWorkMinHours = 3.0

	// DeepWorkEarliestHour / DeepWorkLatestHour — local-tz hours that gate
	// deep_work to "during working day". Outside this window, the day is
	// either too early (still morning_calm) or too late (end_of_day).
	DeepWorkEarliestHour = 9
	DeepWorkLatestHour   = 16

	// EndOfDayHour — local-tz hour at which the register flips to
	// end_of_day regardless of meetings or blocks.
	EndOfDayHour = 16

	// FridayEndOfDayHour — Friday-afternoon shift to retro framing.
	FridayEndOfDayHour = 14
)

// DetectState maps StateInputs onto the active register. Priority order
// (earlier rules dominate later ones) IS the design — see
// doc/v2.3/Phase1.md.
//
// Priority:
//
//  1. Inject signal present → message_inject (set only by Phase 3's inject
//     pipeline; morning runner always passes nil).
//  2. Next meeting within PreMeetingMaxMinutes with at least
//     PreMeetingMinAttendees attendees → pre_meeting.
//  3. Weekday, local hour in [DeepWorkEarliestHour, DeepWorkLatestHour],
//     ≥ DeepWorkMinHours unbooked → deep_work.
//  4. Local hour ≥ EndOfDayHour, OR Friday and local hour ≥
//     FridayEndOfDayHour → end_of_day.
//  5. default → morning_calm.
func DetectState(in StateInputs) State {
	if in.HasInjectSignal && in.InjectSignalKind != "" {
		return StateMessageInject
	}
	if in.NextMeetingMinutes >= 0 &&
		in.NextMeetingMinutes <= PreMeetingMaxMinutes &&
		in.NextMeetingAttendees >= PreMeetingMinAttendees {
		return StatePreMeeting
	}
	isWeekday := in.Weekday >= time.Monday && in.Weekday <= time.Friday
	if isWeekday &&
		in.UnbookedBlockHours >= DeepWorkMinHours &&
		in.LocalHour >= DeepWorkEarliestHour &&
		in.LocalHour <= DeepWorkLatestHour {
		return StateDeepWork
	}
	if in.LocalHour >= EndOfDayHour ||
		(in.Weekday == time.Friday && in.LocalHour >= FridayEndOfDayHour) {
		return StateEndOfDay
	}
	return StateMorningCalm
}
