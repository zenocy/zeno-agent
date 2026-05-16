package synth

import (
	"math"
	"testing"
	"time"

	"github.com/zenocy/zeno-v2/internal/projection"
)

// All times in this test live in UTC for clarity; Weekday/LocalHour come
// straight from now.Weekday()/now.Hour().
var testNow = time.Date(2026, 4, 28, 10, 0, 0, 0, time.UTC) // Tue 10:00 UTC

func mkEvent(uid, title, tag string, start time.Time, dur time.Duration) projection.CalendarEvent {
	// Default to one synthetic attendee — V2.3.0 P3 swapped attendeeCount
	// from a tag-based approximation to the real ATTENDEE list, so the
	// "default attended" semantics this test scaffold relies on now requires
	// at least one entry in Attendees.
	return mkEventWithAttendees(uid, title, tag, start, dur, []string{"attendee@test"})
}

func mkEventWithAttendees(uid, title, tag string, start time.Time, dur time.Duration, attendees []string) projection.CalendarEvent {
	return projection.CalendarEvent{
		UID:       uid,
		Title:     title,
		Tag:       tag,
		Start:     start,
		End:       start.Add(dur),
		Attendees: attendees,
	}
}

func TestBuildStateInputs_EmptyCalendar(t *testing.T) {
	in := BuildStateInputs(testNow, nil, nil, nil)
	if in.NextMeetingMinutes != -1 {
		t.Errorf("NextMeetingMinutes = %d, want -1", in.NextMeetingMinutes)
	}
	if in.NextMeetingAttendees != 0 {
		t.Errorf("NextMeetingAttendees = %d, want 0", in.NextMeetingAttendees)
	}
	if in.UnbookedBlockHours < 5.99 || in.UnbookedBlockHours > 6.01 {
		t.Errorf("UnbookedBlockHours = %f, want ~6.0", in.UnbookedBlockHours)
	}
	if in.Weekday != time.Tuesday {
		t.Errorf("Weekday = %s, want Tuesday", in.Weekday)
	}
	if in.LocalHour != 10 {
		t.Errorf("LocalHour = %d, want 10", in.LocalHour)
	}
	if in.HasInjectSignal {
		t.Errorf("HasInjectSignal = true, want false")
	}
}

func TestBuildStateInputs_NextMeetingInHorizon(t *testing.T) {
	cal := []projection.CalendarEvent{
		mkEvent("e1", "Series B review", "work", testNow.Add(90*time.Minute), 60*time.Minute),
	}
	in := BuildStateInputs(testNow, cal, nil, nil)
	if in.NextMeetingMinutes != 90 {
		t.Errorf("NextMeetingMinutes = %d, want 90", in.NextMeetingMinutes)
	}
	if in.NextMeetingAttendees != 1 {
		t.Errorf("NextMeetingAttendees = %d, want 1 (default attended)", in.NextMeetingAttendees)
	}
}

func TestBuildStateInputs_BlockEventDoesNotCountAsNextMeeting(t *testing.T) {
	cal := []projection.CalendarEvent{
		mkEvent("focus", "Deep work", "focus", testNow.Add(30*time.Minute), 2*time.Hour),
		mkEvent("e1", "Standup", "work", testNow.Add(150*time.Minute), 30*time.Minute),
	}
	in := BuildStateInputs(testNow, cal, nil, nil)
	if in.NextMeetingMinutes != 150 {
		t.Errorf("NextMeetingMinutes = %d, want 150 (focus event skipped)", in.NextMeetingMinutes)
	}
}

func TestBuildStateInputs_NearestMeetingWins(t *testing.T) {
	cal := []projection.CalendarEvent{
		mkEvent("e2", "afternoon", "work", testNow.Add(180*time.Minute), 60*time.Minute),
		mkEvent("e1", "soonest", "work", testNow.Add(15*time.Minute), 30*time.Minute),
	}
	in := BuildStateInputs(testNow, cal, nil, nil)
	if in.NextMeetingMinutes != 15 {
		t.Errorf("NextMeetingMinutes = %d, want 15 (nearest wins after sort)", in.NextMeetingMinutes)
	}
}

func TestBuildStateInputs_MeetingOutsideHorizonIgnored(t *testing.T) {
	cal := []projection.CalendarEvent{
		// 7 hours out — past the 6h horizon
		mkEvent("e1", "later today", "work", testNow.Add(7*time.Hour), 30*time.Minute),
	}
	in := BuildStateInputs(testNow, cal, nil, nil)
	if in.NextMeetingMinutes != -1 {
		t.Errorf("NextMeetingMinutes = %d, want -1 (meeting outside horizon)", in.NextMeetingMinutes)
	}
}

func TestBuildStateInputs_UnbookedBlockHours_FullyOpen(t *testing.T) {
	in := BuildStateInputs(testNow, nil, nil, nil)
	// 6h horizon, no events — unbooked = 6h
	if math.Abs(in.UnbookedBlockHours-6.0) > 0.01 {
		t.Errorf("UnbookedBlockHours = %f, want ~6.0", in.UnbookedBlockHours)
	}
}

func TestBuildStateInputs_UnbookedBlockHours_OneMidEvent(t *testing.T) {
	// Event from now+2h to now+3h splits the 6h window into [now,+2h] and [+3h,+6h].
	// Largest contiguous gap is 3h.
	cal := []projection.CalendarEvent{
		mkEvent("e1", "noon meeting", "work", testNow.Add(2*time.Hour), 1*time.Hour),
	}
	in := BuildStateInputs(testNow, cal, nil, nil)
	if math.Abs(in.UnbookedBlockHours-3.0) > 0.01 {
		t.Errorf("UnbookedBlockHours = %f, want ~3.0", in.UnbookedBlockHours)
	}
}

func TestBuildStateInputs_UnbookedBlockHours_BlockCounts(t *testing.T) {
	// Block-tagged event still consumes time (only its impact on
	// "next meeting" is suppressed; deep-work signal is meant to honor
	// real protected blocks).
	cal := []projection.CalendarEvent{
		mkEvent("focus", "Focus", "focus", testNow.Add(1*time.Hour), 4*time.Hour),
	}
	in := BuildStateInputs(testNow, cal, nil, nil)
	// Largest gap is the first hour — [now, +1h] = 1h.
	if math.Abs(in.UnbookedBlockHours-1.0) > 0.01 {
		t.Errorf("UnbookedBlockHours = %f, want ~1.0 (block consumes time)", in.UnbookedBlockHours)
	}
}

func TestBuildStateInputs_UnbookedBlockHours_OverlappingEvents(t *testing.T) {
	// Two overlapping events from +1h to +3h and +2h to +4h. The cursor
	// should advance to +4h after both. Gap = [now, +1h] = 1h, then
	// [+4h, +6h] = 2h. Largest = 2h.
	cal := []projection.CalendarEvent{
		mkEvent("e1", "first", "work", testNow.Add(1*time.Hour), 2*time.Hour),
		mkEvent("e2", "overlap", "work", testNow.Add(2*time.Hour), 2*time.Hour),
	}
	in := BuildStateInputs(testNow, cal, nil, nil)
	if math.Abs(in.UnbookedBlockHours-2.0) > 0.01 {
		t.Errorf("UnbookedBlockHours = %f, want ~2.0", in.UnbookedBlockHours)
	}
}

func TestBuildStateInputs_PastEventIgnored(t *testing.T) {
	// Event ending 30 min ago is irrelevant.
	cal := []projection.CalendarEvent{
		mkEvent("past", "earlier", "work", testNow.Add(-2*time.Hour), 1*time.Hour),
	}
	in := BuildStateInputs(testNow, cal, nil, nil)
	if math.Abs(in.UnbookedBlockHours-6.0) > 0.01 {
		t.Errorf("UnbookedBlockHours = %f, want ~6.0 (past event filtered)", in.UnbookedBlockHours)
	}
	if in.NextMeetingMinutes != -1 {
		t.Errorf("NextMeetingMinutes = %d, want -1", in.NextMeetingMinutes)
	}
}

func TestBuildStateInputs_InjectSignalPropagated(t *testing.T) {
	sig := &InjectSignal{Kind: "email", Subject: "test", At: testNow}
	in := BuildStateInputs(testNow, nil, nil, sig)
	if !in.HasInjectSignal {
		t.Errorf("HasInjectSignal = false, want true")
	}
	if in.InjectSignalKind != "email" {
		t.Errorf("InjectSignalKind = %q, want email", in.InjectSignalKind)
	}
}

// TestBuildStateInputs_HonorsCallerTZ pins the contract the runner relies on:
// when the caller hands BuildStateInputs a now already converted to the user's
// timezone, Weekday/LocalHour reflect that zone — and DetectState's local-hour
// rules (EndOfDayHour) fire on the user's clock, not UTC. Regression guard for
// the "morning cards at 3pm" bug where runner.detectState passed UTC.
func TestBuildStateInputs_HonorsCallerTZ(t *testing.T) {
	// 14:00 UTC == 17:00 in UTC+3 — past DeepWorkLatestHour=16, so the
	// deep_work rule no longer applies and the next rule (LocalHour ≥
	// EndOfDayHour=16) fires.
	nowUTC := time.Date(2026, 4, 28, 14, 0, 0, 0, time.UTC) // Tue
	tz := time.FixedZone("test+3", 3*3600)

	in := BuildStateInputs(nowUTC.In(tz), nil, nil, nil)
	if in.LocalHour != 17 {
		t.Errorf("LocalHour = %d, want 17 (UTC 14:00 in UTC+3)", in.LocalHour)
	}
	if in.Weekday != time.Tuesday {
		t.Errorf("Weekday = %s, want Tuesday", in.Weekday)
	}
	if got := DetectState(in); got != StateEndOfDay {
		t.Errorf("DetectState = %s, want %s (16:00 local must trip end_of_day)", got, StateEndOfDay)
	}

	// Without the conversion, the same UTC now would read LocalHour=14 and
	// land on deep_work (or morning_calm), not end_of_day — guard against
	// regressing the call site.
	rawIn := BuildStateInputs(nowUTC, nil, nil, nil)
	if rawIn.LocalHour != 14 {
		t.Fatalf("test scaffold broken: raw UTC now should produce LocalHour=14, got %d", rawIn.LocalHour)
	}
	if got := DetectState(rawIn); got == StateEndOfDay {
		t.Fatalf("test scaffold broken: raw UTC LocalHour=14 should not trip end_of_day")
	}
}
