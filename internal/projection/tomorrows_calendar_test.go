package projection

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/log/logtest"
)

func TestTomorrowsCalendar_HappyPath(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 8, 0, 0, 0, tz)
	cfg := newCfg(now, tz)

	mem := logtest.NewMemReader()
	// Three events tomorrow, one today, one day-after-tomorrow.
	for i, h := range []int{14, 9, 11} {
		s := time.Date(2026, 4, 26, h, 0, 0, 0, tz)
		mem.AppendEvent(logtest.MakeEvent(log.KindCalEventSeen, "caldav",
			now.Add(time.Duration(i)*time.Minute),
			calPayload(fmtUID("tm", i), fmtUID("Tomorrow", i), s, s.Add(30*time.Minute))))
	}
	mem.AppendEvent(logtest.MakeEvent(log.KindCalEventSeen, "caldav", now,
		calPayload("td", "Today", time.Date(2026, 4, 25, 10, 0, 0, 0, tz), time.Date(2026, 4, 25, 11, 0, 0, 0, tz))))
	mem.AppendEvent(logtest.MakeEvent(log.KindCalEventSeen, "caldav", now,
		calPayload("d2", "Day after tomorrow", time.Date(2026, 4, 27, 10, 0, 0, 0, tz), time.Date(2026, 4, 27, 11, 0, 0, 0, tz))))

	got, err := TomorrowsCalendar{Cfg: cfg}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Len(t, got, 3, "only the three tomorrow events should appear")
	for i := 1; i < len(got); i++ {
		require.True(t, got[i].Start.After(got[i-1].Start), "sorted by start ascending")
	}
}

func TestTomorrowsCalendar_EmptyLog(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	mem := logtest.NewMemReader()
	got, err := TomorrowsCalendar{Cfg: newCfg(time.Now().In(tz), tz)}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got)
}

func TestTomorrowsCalendar_OverlapsMidnight(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 8, 0, 0, 0, tz)

	mem := logtest.NewMemReader()
	// Event starts late tomorrow, ends day after — should appear in tomorrow.
	start := time.Date(2026, 4, 26, 23, 0, 0, 0, tz)
	end := time.Date(2026, 4, 27, 1, 0, 0, 0, tz)
	mem.AppendEvent(logtest.MakeEvent(log.KindCalEventSeen, "caldav", now,
		calPayload("ov", "Late tomorrow", start, end)))

	got, err := TomorrowsCalendar{Cfg: newCfg(now, tz)}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Len(t, got, 1)
}

func TestTomorrowsCalendar_SnapshotFiltersDeleted(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 8, 0, 0, 0, tz)

	mem := logtest.NewMemReader()
	for i, h := range []int{9, 14, 18} {
		s := time.Date(2026, 4, 26, h, 0, 0, 0, tz)
		mem.AppendEvent(logtest.MakeEvent(log.KindCalEventSeen, "caldav",
			now.Add(time.Duration(i)*time.Minute),
			calPayload(fmtUID("e", i), fmtUID("Event", i), s, s.Add(30*time.Minute))))
	}
	mem.AppendEvent(logtest.MakeEvent(log.KindCalListSnapshot, "caldav",
		now.Add(5*time.Minute), calSnapshotPayload([]string{"e-0", "e-2"})))

	got, err := TomorrowsCalendar{Cfg: newCfg(now, tz)}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Len(t, got, 2)
}
