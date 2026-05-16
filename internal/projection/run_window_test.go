package projection

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/log/logtest"
)

type hourSpec struct {
	Hour     int
	Code     int
	WindKmh  float64
	PrecipMM float64
}

func makeWeatherPayload(t *testing.T, day time.Time, tz *time.Location, hours []hourSpec) any {
	t.Helper()
	type hp struct {
		Time     time.Time `json:"time"`
		Code     int       `json:"code"`
		WindKmh  float64   `json:"wind_kmh"`
		PrecipMM float64   `json:"precip_mm"`
	}
	hourly := make([]hp, 0, len(hours))
	for _, h := range hours {
		hourly = append(hourly, hp{
			Time:     time.Date(day.Year(), day.Month(), day.Day(), h.Hour, 0, 0, 0, tz),
			Code:     h.Code,
			WindKmh:  h.WindKmh,
			PrecipMM: h.PrecipMM,
		})
	}
	return map[string]any{
		"captured_at": day,
		"timezone":    tz.String(),
		"hourly":      hourly,
	}
}

func writeWeather(t *testing.T, mem *logtest.MemReader, ts time.Time, payload any) {
	t.Helper()
	mem.AppendEvent(logtest.MakeEvent(log.KindWeatherSnapshot, "weather", ts, payload))
}

func TestRunWindow_ClearMorning(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 6, 0, 0, 0, tz)
	cfg := newCfg(now, tz)

	mem := logtest.NewMemReader()
	hours := []hourSpec{
		{6, 1, 8, 0}, {7, 1, 9, 0}, {8, 2, 10, 0}, {9, 1, 11, 0},
	}
	writeWeather(t, mem, now, makeWeatherPayload(t, now, tz, hours))

	mtgStart := time.Date(2026, 4, 25, 9, 0, 0, 0, tz)
	mem.AppendEvent(logtest.MakeEvent(log.KindCalEventSeen, "caldav", now,
		calPayload("m1", "Standup", mtgStart, mtgStart.Add(time.Hour))))

	got, err := RunWindow{Cfg: cfg}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, 6, got.Start.In(tz).Hour())
	require.Equal(t, 9, got.End.In(tz).Hour())
}

func TestRunWindow_AllBusy(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 6, 0, 0, 0, tz)
	cfg := newCfg(now, tz)

	mem := logtest.NewMemReader()
	writeWeather(t, mem, now, makeWeatherPayload(t, now, tz, []hourSpec{{6, 1, 8, 0}, {12, 1, 8, 0}, {19, 1, 8, 0}}))
	dayStart := time.Date(2026, 4, 25, 6, 0, 0, 0, tz)
	dayEnd := time.Date(2026, 4, 25, 20, 0, 0, 0, tz)
	mem.AppendEvent(logtest.MakeEvent(log.KindCalEventSeen, "caldav", now,
		calPayload("busy", "Marathon meeting", dayStart, dayEnd)))

	got, err := RunWindow{Cfg: cfg}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestRunWindow_AllRainy(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 6, 0, 0, 0, tz)
	cfg := newCfg(now, tz)

	mem := logtest.NewMemReader()
	hours := []hourSpec{}
	for h := 6; h < 21; h++ {
		hours = append(hours, hourSpec{Hour: h, Code: 61, WindKmh: 8, PrecipMM: 1.5})
	}
	writeWeather(t, mem, now, makeWeatherPayload(t, now, tz, hours))

	got, err := RunWindow{Cfg: cfg}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestRunWindow_WindAboveThreshold(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 6, 0, 0, 0, tz)
	cfg := newCfg(now, tz)
	cfg.RunWindowMaxWindKmh = 25

	mem := logtest.NewMemReader()
	hours := []hourSpec{}
	for h := 6; h < 21; h++ {
		hours = append(hours, hourSpec{Hour: h, Code: 1, WindKmh: 30})
	}
	writeWeather(t, mem, now, makeWeatherPayload(t, now, tz, hours))

	got, err := RunWindow{Cfg: cfg}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestRunWindow_GapShorterThanMin(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 6, 0, 0, 0, tz)
	cfg := newCfg(now, tz)
	cfg.RunWindowMinMinutes = 60

	mem := logtest.NewMemReader()
	hours := []hourSpec{}
	for h := 6; h < 21; h++ {
		hours = append(hours, hourSpec{Hour: h, Code: 1, WindKmh: 8})
	}
	writeWeather(t, mem, now, makeWeatherPayload(t, now, tz, hours))

	// Stack meetings to leave only a 30-min gap from 06:30→07:00.
	mtg1Start := time.Date(2026, 4, 25, 6, 0, 0, 0, tz)
	mtg1End := time.Date(2026, 4, 25, 6, 30, 0, 0, tz)
	mtg2Start := time.Date(2026, 4, 25, 7, 0, 0, 0, tz)
	mtg2End := time.Date(2026, 4, 25, 20, 0, 0, 0, tz)
	mem.AppendEvent(logtest.MakeEvent(log.KindCalEventSeen, "caldav", now,
		calPayload("m1", "Early", mtg1Start, mtg1End)))
	mem.AppendEvent(logtest.MakeEvent(log.KindCalEventSeen, "caldav", now,
		calPayload("m2", "All day rest", mtg2Start, mtg2End)))

	got, err := RunWindow{Cfg: cfg}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Nil(t, got, "30 min gap < 60 min threshold")
}

func TestRunWindow_NoWeatherSnapshot(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 6, 0, 0, 0, tz)
	cfg := newCfg(now, tz)

	mem := logtest.NewMemReader()
	got, err := RunWindow{Cfg: cfg}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestRunWindow_StaleSnapshot(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 6, 0, 0, 0, tz)
	cfg := newCfg(now, tz)

	mem := logtest.NewMemReader()
	stale := now.Add(-3 * 24 * time.Hour)
	hours := []hourSpec{}
	for h := 6; h < 21; h++ {
		hours = append(hours, hourSpec{Hour: h, Code: 1, WindKmh: 8})
	}
	writeWeather(t, mem, stale, makeWeatherPayload(t, stale, tz, hours))

	got, err := RunWindow{Cfg: cfg}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Nil(t, got, "snapshot older than staleSnapshotAfter is ignored")
}

func TestRunWindow_DSTSpring(t *testing.T) {
	// 2026-03-29 in Athens — clocks jump 03:00 → 04:00.
	tz := mustTZ(t, "Europe/Athens")
	now := time.Date(2026, 3, 29, 6, 0, 0, 0, tz)
	cfg := newCfg(now, tz)

	mem := logtest.NewMemReader()
	hours := []hourSpec{}
	for h := 6; h < 21; h++ {
		hours = append(hours, hourSpec{Hour: h, Code: 1, WindKmh: 8})
	}
	writeWeather(t, mem, now, makeWeatherPayload(t, now, tz, hours))

	got, err := RunWindow{Cfg: cfg}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "Europe/Athens", got.Start.Location().String())
	require.True(t, got.End.After(got.Start))
}
