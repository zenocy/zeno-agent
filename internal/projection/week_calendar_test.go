package projection

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/log/logtest"
)

func TestWeekCalendar_HappyPath(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 8, 0, 0, 0, tz) // Saturday
	cfg := newCfg(now, tz)

	mem := logtest.NewMemReader()
	// Today + tomorrow are excluded; events on day+2 .. day+8 are included.
	mem.AppendEvent(logtest.MakeEvent(log.KindCalEventSeen, "caldav", now,
		calPayload("td", "Today", time.Date(2026, 4, 25, 10, 0, 0, 0, tz), time.Date(2026, 4, 25, 11, 0, 0, 0, tz))))
	mem.AppendEvent(logtest.MakeEvent(log.KindCalEventSeen, "caldav", now,
		calPayload("tm", "Tomorrow", time.Date(2026, 4, 26, 10, 0, 0, 0, tz), time.Date(2026, 4, 26, 11, 0, 0, 0, tz))))

	// In-window: today+2 (Apr 27) through today+8 (May 3). today+9 (May 4)
	// is the half-open upper bound and excluded.
	for i, day := range []int{27, 28, 30, 2} {
		month := time.April
		d := day
		if d == 2 {
			month = time.May
		}
		s := time.Date(2026, month, d, 10, 0, 0, 0, tz)
		mem.AppendEvent(logtest.MakeEvent(log.KindCalEventSeen, "caldav",
			now.Add(time.Duration(i)*time.Minute),
			calPayload(fmtUID("w", i), fmtUID("Week", i), s, s.Add(30*time.Minute))))
	}

	// Outside the window: 10 days out
	mem.AppendEvent(logtest.MakeEvent(log.KindCalEventSeen, "caldav", now,
		calPayload("far", "Far future", time.Date(2026, 5, 5, 10, 0, 0, 0, tz), time.Date(2026, 5, 5, 11, 0, 0, 0, tz))))

	got, err := WeekCalendar{Cfg: cfg}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Len(t, got, 4, "only events in [today+2d, today+9d) appear")
	for i := 1; i < len(got); i++ {
		require.True(t, got[i].Start.After(got[i-1].Start), "sorted by start ascending")
	}
}

func TestWeekCalendar_ExcludesTodayAndTomorrow(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 8, 0, 0, 0, tz)

	mem := logtest.NewMemReader()
	mem.AppendEvent(logtest.MakeEvent(log.KindCalEventSeen, "caldav", now,
		calPayload("td", "Today", time.Date(2026, 4, 25, 10, 0, 0, 0, tz), time.Date(2026, 4, 25, 11, 0, 0, 0, tz))))
	mem.AppendEvent(logtest.MakeEvent(log.KindCalEventSeen, "caldav", now,
		calPayload("tm", "Tomorrow", time.Date(2026, 4, 26, 10, 0, 0, 0, tz), time.Date(2026, 4, 26, 11, 0, 0, 0, tz))))

	got, err := WeekCalendar{Cfg: newCfg(now, tz)}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Empty(t, got, "today and tomorrow should not appear in the week horizon")
}

func TestWeekCalendar_EmptyLog(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	mem := logtest.NewMemReader()
	got, err := WeekCalendar{Cfg: newCfg(time.Now().In(tz), tz)}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got)
}
