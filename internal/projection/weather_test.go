package projection

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/log/logtest"
)

type fullHour struct {
	Hour     int
	TempC    float64
	Code     int
	WindKmh  float64
	PrecipMM float64
	Label    string
}

func makeFullSnapshot(day time.Time, tz *time.Location, location string, hours []fullHour, currentIdx int) any {
	type hp struct {
		Time     time.Time `json:"time"`
		TempC    float64   `json:"temp_c"`
		Code     int       `json:"code"`
		WindKmh  float64   `json:"wind_kmh"`
		PrecipMM float64   `json:"precip_mm"`
		Label    string    `json:"label,omitempty"`
	}
	hourly := make([]hp, 0, len(hours))
	for _, h := range hours {
		hourly = append(hourly, hp{
			Time:  time.Date(day.Year(), day.Month(), day.Day(), h.Hour, 0, 0, 0, tz),
			TempC: h.TempC, Code: h.Code, WindKmh: h.WindKmh, PrecipMM: h.PrecipMM, Label: h.Label,
		})
	}
	cur := hourly[currentIdx]
	return map[string]any{
		"captured_at": day,
		"timezone":    tz.String(),
		"location":    location,
		"current":     cur,
		"hourly":      hourly,
	}
}

func TestWeather_HappyPath(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 16, 0, 0, 0, tz)
	cfg := newCfg(now, tz)

	hours := []fullHour{
		{13, 13, 1, 5, 0, "Mainly clear"},
		{14, 14, 1, 6, 0, "Mainly clear"},
		{15, 15, 1, 7, 0, "Mainly clear"},
		{16, 17, 0, 8, 0, "Clear sky"},
		{17, 19, 0, 8, 0, "Clear sky"},
		{18, 19, 0, 8, 0, "Clear sky"},
	}

	mem := logtest.NewMemReader()
	mem.AppendEvent(logtest.MakeEvent(log.KindWeatherSnapshot, "weather", now,
		makeFullSnapshot(now, tz, "San Francisco", hours, 3)))

	got, err := Weather{Cfg: cfg}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "San Francisco", got.Location)
	require.Equal(t, 17.0, got.Current.TempC)
	require.Equal(t, "Clear sky", got.Current.Label)
	require.Len(t, got.Hourly, 6)
	require.Equal(t, 3, got.NowIndex, "16:00 lives at index 3")
}

func TestWeather_NoSnapshot(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 16, 0, 0, 0, tz)
	cfg := newCfg(now, tz)

	mem := logtest.NewMemReader()
	got, err := Weather{Cfg: cfg}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestWeather_StaleSnapshot(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 16, 0, 0, 0, tz)
	cfg := newCfg(now, tz)

	stale := now.Add(-3 * 24 * time.Hour)
	hours := []fullHour{{13, 13, 1, 5, 0, "Mainly clear"}}
	mem := logtest.NewMemReader()
	mem.AppendEvent(logtest.MakeEvent(log.KindWeatherSnapshot, "weather", stale,
		makeFullSnapshot(stale, tz, "Stale City", hours, 0)))

	got, err := Weather{Cfg: cfg}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Nil(t, got, "stale snapshot ignored")
}

func TestWeather_NowIndexFallbackZero(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 23, 0, 0, 0, tz)
	cfg := newCfg(now, tz)

	hours := []fullHour{{6, 13, 1, 5, 0, "Mainly clear"}}
	mem := logtest.NewMemReader()
	mem.AppendEvent(logtest.MakeEvent(log.KindWeatherSnapshot, "weather", now,
		makeFullSnapshot(now, tz, "X", hours, 0)))

	got, err := Weather{Cfg: cfg}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, 0, got.NowIndex, "no exact-hour match → fall back to 0")
}
