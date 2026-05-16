package projection

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/clock"
	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/log/logtest"
)

func mustTZ(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	require.NoError(t, err)
	return loc
}

func calPayload(uid, title string, start, end time.Time) any {
	return map[string]any{
		"uid": uid, "title": title, "location": "", "tag": "",
		"start": start, "end": end,
	}
}

func newCfg(now time.Time, tz *time.Location) Config {
	return Config{
		TZ:                    tz,
		LookbackDays:          14,
		RunWindowMinMinutes:   45,
		RunWindowMaxWindKmh:   25,
		RunWindowEarliestHour: 6,
		RunWindowLatestHour:   20,
		OpenThreadsMax:        20,
		Now:                   func() time.Time { return now },
	}
}

func TestTodaysCalendar_HappyPath(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 8, 0, 0, 0, tz)
	cfg := newCfg(now, tz)

	mem := logtest.NewMemReader()
	for i, h := range []int{14, 9, 11} {
		s := time.Date(2026, 4, 25, h, 0, 0, 0, tz)
		mem.AppendEvent(logtest.MakeEvent(log.KindCalEventSeen, "caldav",
			now.Add(time.Duration(i)*time.Minute),
			calPayload(fmtUID("today", i), fmtUID("Today", i), s, s.Add(30*time.Minute))))
	}
	// One yesterday, one tomorrow.
	mem.AppendEvent(logtest.MakeEvent(log.KindCalEventSeen, "caldav", now,
		calPayload("y", "Yesterday", time.Date(2026, 4, 24, 10, 0, 0, 0, tz), time.Date(2026, 4, 24, 11, 0, 0, 0, tz))))
	mem.AppendEvent(logtest.MakeEvent(log.KindCalEventSeen, "caldav", now,
		calPayload("tm", "Tomorrow", time.Date(2026, 4, 26, 10, 0, 0, 0, tz), time.Date(2026, 4, 26, 11, 0, 0, 0, tz))))

	got, err := TodaysCalendar{Cfg: cfg}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Len(t, got, 3)
	for i := 1; i < len(got); i++ {
		require.True(t, got[i].Start.After(got[i-1].Start), "sorted by start ascending")
	}
}

func fmtUID(prefix string, i int) string { return prefix + "-" + string(rune('0'+i)) }

func TestTodaysCalendar_LatestWins(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 8, 0, 0, 0, tz)

	mem := logtest.NewMemReader()
	original := time.Date(2026, 4, 25, 14, 0, 0, 0, tz)
	mem.AppendEvent(logtest.MakeEvent(log.KindCalEventSeen, "caldav",
		now.Add(-time.Hour),
		calPayload("e1", "Original title", original, original.Add(time.Hour))))
	mem.AppendEvent(logtest.MakeEvent(log.KindCalEventChanged, "caldav",
		now,
		calPayload("e1", "Updated title", original, original.Add(time.Hour))))

	got, err := TodaysCalendar{Cfg: newCfg(now, tz)}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "Updated title", got[0].Title)
}

func TestTodaysCalendar_AllDayEvent(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 8, 0, 0, 0, tz)

	mem := logtest.NewMemReader()
	dayStart := time.Date(2026, 4, 25, 0, 0, 0, 0, tz)
	mem.AppendEvent(logtest.MakeEvent(log.KindCalEventSeen, "caldav", now,
		calPayload("a", "All day", dayStart, dayStart.Add(24*time.Hour))))

	got, err := TodaysCalendar{Cfg: newCfg(now, tz)}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Len(t, got, 1)
}

func calSnapshotPayload(uids []string) any {
	return map[string]any{"uids": uids}
}

func TestTodaysCalendar_NoSnapshot_IncludesAll(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 8, 0, 0, 0, tz)

	mem := logtest.NewMemReader()
	for i, h := range []int{9, 14, 18} {
		s := time.Date(2026, 4, 25, h, 0, 0, 0, tz)
		mem.AppendEvent(logtest.MakeEvent(log.KindCalEventSeen, "caldav",
			now.Add(time.Duration(i)*time.Minute),
			calPayload(fmtUID("e", i), fmtUID("Event", i), s, s.Add(30*time.Minute))))
	}

	got, err := TodaysCalendar{Cfg: newCfg(now, tz)}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Len(t, got, 3, "no snapshot → first-deploy compat: include all seen")
}

func TestTodaysCalendar_SnapshotFiltersDeleted(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 8, 0, 0, 0, tz)

	mem := logtest.NewMemReader()
	for i, h := range []int{9, 14, 18} {
		s := time.Date(2026, 4, 25, h, 0, 0, 0, tz)
		mem.AppendEvent(logtest.MakeEvent(log.KindCalEventSeen, "caldav",
			now.Add(time.Duration(i)*time.Minute),
			calPayload(fmtUID("e", i), fmtUID("Event", i), s, s.Add(30*time.Minute))))
	}
	// Server now lists only e-0 and e-2 (e-1 was deleted in the user's UI).
	mem.AppendEvent(logtest.MakeEvent(log.KindCalListSnapshot, "caldav",
		now.Add(5*time.Minute), calSnapshotPayload([]string{"e-0", "e-2"})))

	got, err := TodaysCalendar{Cfg: newCfg(now, tz)}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Len(t, got, 2)
	titles := []string{got[0].Title, got[1].Title}
	require.NotContains(t, titles, "Event-1", "deleted UID must drop out")
}

func TestTodaysCalendar_LatestSnapshotWins(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 8, 0, 0, 0, tz)

	mem := logtest.NewMemReader()
	for i, h := range []int{9, 14} {
		s := time.Date(2026, 4, 25, h, 0, 0, 0, tz)
		mem.AppendEvent(logtest.MakeEvent(log.KindCalEventSeen, "caldav", now,
			calPayload(fmtUID("e", i), fmtUID("Event", i), s, s.Add(30*time.Minute))))
	}
	// Older snapshot says both present; newer drops e-1.
	mem.AppendEvent(logtest.MakeEvent(log.KindCalListSnapshot, "caldav",
		now.Add(-2*time.Hour), calSnapshotPayload([]string{"e-0", "e-1"})))
	mem.AppendEvent(logtest.MakeEvent(log.KindCalListSnapshot, "caldav",
		now.Add(-1*time.Hour), calSnapshotPayload([]string{"e-0"})))

	got, err := TodaysCalendar{Cfg: newCfg(now, tz)}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "Event-0", got[0].Title)
}

func TestTodaysCalendar_EmptyLog(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	mem := logtest.NewMemReader()
	got, err := TodaysCalendar{Cfg: newCfg(time.Now().In(tz), tz)}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got)
}

func TestTodaysCalendar_OverlapsMidnight(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 8, 0, 0, 0, tz)

	mem := logtest.NewMemReader()
	// 23:00 → 01:00 next day
	start := time.Date(2026, 4, 25, 23, 0, 0, 0, tz)
	end := time.Date(2026, 4, 26, 1, 0, 0, 0, tz)
	mem.AppendEvent(logtest.MakeEvent(log.KindCalEventSeen, "caldav", now,
		calPayload("ov", "Overnight", start, end)))

	got, err := TodaysCalendar{Cfg: newCfg(now, tz)}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Len(t, got, 1)
}

func TestTodaysCalendar_AttendeesAndLastModifiedRoundtrip(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 30, 9, 0, 0, 0, tz)

	mem := logtest.NewMemReader()
	start := time.Date(2026, 4, 30, 11, 0, 0, 0, tz)
	mod := time.Date(2026, 4, 30, 15, 45, 0, 0, time.UTC)
	mem.AppendEvent(logtest.MakeEvent(log.KindCalEventSeen, "caldav", now, map[string]any{
		"uid":   "att-1",
		"title": "Acuity — Series B review",
		"start": start, "end": start.Add(time.Hour),
		"attendees":     []string{"Saru Patel", "Lin Vega", "Park Choi"},
		"last_modified": mod,
	}))

	got, err := TodaysCalendar{Cfg: newCfg(now, tz)}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, []string{"Saru Patel", "Lin Vega", "Park Choi"}, got[0].Attendees,
		"attendees survive event-log round-trip")
	require.Equal(t, mod.UTC(), got[0].LastModified.UTC(),
		"LAST-MODIFIED survives round-trip")
}

// Older payloads from before V2.3.0 P3 lack the attendees / last_modified
// fields — they should decode to empty / zero, not error.
func TestTodaysCalendar_LegacyPayloadHasNoAttendees(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 30, 9, 0, 0, 0, tz)
	mem := logtest.NewMemReader()
	start := time.Date(2026, 4, 30, 11, 0, 0, 0, tz)
	mem.AppendEvent(logtest.MakeEvent(log.KindCalEventSeen, "caldav", now,
		calPayload("legacy", "Legacy event", start, start.Add(time.Hour))))
	got, err := TodaysCalendar{Cfg: newCfg(now, tz)}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Empty(t, got[0].Attendees)
	require.True(t, got[0].LastModified.IsZero())
}

func TestTodaysCalendar_DSTSpring(t *testing.T) {
	// Last Sunday of March 2026 in Europe/Athens — clocks jump 03:00 → 04:00.
	tz := mustTZ(t, "Europe/Athens")
	now := time.Date(2026, 3, 29, 9, 0, 0, 0, tz)

	mem := logtest.NewMemReader()
	start := time.Date(2026, 3, 29, 14, 0, 0, 0, tz)
	mem.AppendEvent(logtest.MakeEvent(log.KindCalEventSeen, "caldav", now,
		calPayload("dst", "DST day event", start, start.Add(time.Hour))))

	got, err := TodaysCalendar{Cfg: newCfg(now, tz)}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, 14, got[0].Start.In(tz).Hour())
}

// Spring-forward in America/Los_Angeles 2026-03-08: clocks jump 02:00 → 03:00,
// so the local wall-clock 02:30 doesn't exist. Go's time.Date normalizes the
// non-existent local time to the next valid instant; the event must still
// appear exactly once on its real day.
func TestTodaysCalendar_DSTSpringForward_NonexistentLocalTime(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 3, 8, 9, 0, 0, 0, tz)

	mem := logtest.NewMemReader()
	// Construct the start as 02:30 local on the spring-forward day.
	start := time.Date(2026, 3, 8, 2, 30, 0, 0, tz)
	mem.AppendEvent(logtest.MakeEvent(log.KindCalEventSeen, "caldav", now,
		calPayload("sf", "Pre-dawn event", start, start.Add(30*time.Minute))))

	got, err := TodaysCalendar{Cfg: newCfg(now, tz)}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Len(t, got, 1, "non-existent local time must yield exactly one event")
	require.True(t, got[0].Start.Day() == 8 && got[0].Start.Month() == 3,
		"event must remain on 2026-03-08, not bleed into the prior day")
}

// Fall-back in America/Los_Angeles 2026-11-01: 02:00 PDT → 01:00 PST, so
// 01:30 happens twice. The event must appear exactly once regardless of
// which 01:30 it is.
func TestTodaysCalendar_DSTFallBack_AmbiguousLocalTime(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, tz)

	mem := logtest.NewMemReader()
	start := time.Date(2026, 11, 1, 1, 30, 0, 0, tz)
	mem.AppendEvent(logtest.MakeEvent(log.KindCalEventSeen, "caldav", now,
		calPayload("fb", "Ambiguous-time event", start, start.Add(30*time.Minute))))

	got, err := TodaysCalendar{Cfg: newCfg(now, tz)}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Len(t, got, 1, "ambiguous local time must yield exactly one event")
	require.Equal(t, 1, got[0].Start.In(tz).Hour())
}

// On the fall-back day, an all-day event is 25 hours of wall-clock time
// (one extra repeated hour). Computing dayEnd as dayStart + 24h would miss
// the last hour. Verify the projection still includes it; whether dayEnd
// uses 24h or local-midnight-next-day is an implementation detail, but the
// event filter must keep the all-day event regardless.
func TestTodaysCalendar_DSTFallBack_AllDayEventStillIncluded(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, tz)

	mem := logtest.NewMemReader()
	dayStart := time.Date(2026, 11, 1, 0, 0, 0, 0, tz)
	// All-day spans local 00:00 → 00:00 next day; that's 25h on this day.
	allDayEnd := time.Date(2026, 11, 2, 0, 0, 0, 0, tz)
	mem.AppendEvent(logtest.MakeEvent(log.KindCalEventSeen, "caldav", now,
		calPayload("a", "All-day fall-back", dayStart, allDayEnd)))

	got, err := TodaysCalendar{Cfg: newCfg(now, tz)}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Len(t, got, 1, "all-day event must overlap today even on fall-back day")
}

// An event recorded with an explicit foreign TZID (e.g. London) while the
// user is in Los Angeles must still surface on the user's local calendar
// when the moment falls on the user's "today". Validates that overlap
// computation uses the user's TZ for boundaries, not the event's TZID.
func TestTodaysCalendar_ForeignTZIDEvent(t *testing.T) {
	userTZ := mustTZ(t, "America/Los_Angeles")
	londonTZ := mustTZ(t, "Europe/London")

	now := time.Date(2026, 4, 25, 9, 0, 0, 0, userTZ)
	// 22:00 London on 2026-04-25 == 14:00 LA on 2026-04-25 — overlaps user's today.
	londonStart := time.Date(2026, 4, 25, 22, 0, 0, 0, londonTZ)

	mem := logtest.NewMemReader()
	mem.AppendEvent(logtest.MakeEvent(log.KindCalEventSeen, "caldav", now,
		calPayload("foreign", "London call", londonStart, londonStart.Add(time.Hour))))

	got, err := TodaysCalendar{Cfg: newCfg(now, userTZ)}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Len(t, got, 1, "foreign-TZID event overlapping user's today must be included")
	// Start must be rendered in the user's TZ (14:00 LA), not in the event's TZID (22:00 London).
	require.Equal(t, 14, got[0].Start.In(userTZ).Hour())
}

// When Config.Clock is set, projections must use it instead of the legacy
// TZ/Now fields. Validates the migration path: a Fixed clock changes the
// "today" window even when the legacy TZ/Now fields are absent.
func TestTodaysCalendar_UsesClockField(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 9, 0, 0, 0, tz)

	mem := logtest.NewMemReader()
	start := time.Date(2026, 4, 25, 14, 0, 0, 0, tz)
	mem.AppendEvent(logtest.MakeEvent(log.KindCalEventSeen, "caldav", now,
		calPayload("c", "Today via Clock", start, start.Add(time.Hour))))

	cfg := Config{
		Clock:        clock.NewFixed(now, tz),
		LookbackDays: 14,
	}
	got, err := TodaysCalendar{Cfg: cfg}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "Today via Clock", got[0].Title)
}
