package projection

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/zenocy/zeno-v2/internal/log"
)

// Weather projects the latest weather.snapshot event into the UI-shaped view
// the right-rail widget consumes. Returns nil when the snapshot is missing
// or stale beyond staleSnapshotAfter.
type Weather struct{ Cfg Config }

// Name returns the projection identifier.
func (p Weather) Name() string { return "weather" }

// rawWeatherFull mirrors the full payload shape written by the weather
// sensor — a superset of rawWeatherSnapshot (which only reads the fields
// RunWindow needs).
type rawWeatherFull struct {
	CapturedAt time.Time            `json:"captured_at"`
	Timezone   string               `json:"timezone"`
	Location   string               `json:"location"`
	Current    rawWeatherHourFull   `json:"current"`
	Hourly     []rawWeatherHourFull `json:"hourly"`
	Daily      []rawWeatherDayFull  `json:"daily"`
}

type rawWeatherHourFull struct {
	Time     time.Time `json:"time"`
	TempC    float64   `json:"temp_c"`
	Code     int       `json:"code"`
	WindKmh  float64   `json:"wind_kmh"`
	PrecipMM float64   `json:"precip_mm"`
	Label    string    `json:"label,omitempty"`
}

type rawWeatherDayFull struct {
	Date     time.Time `json:"date"`
	TempMaxC float64   `json:"temp_max_c"`
	TempMinC float64   `json:"temp_min_c"`
	Code     int       `json:"code"`
	PrecipMM float64   `json:"precip_mm"`
	Label    string    `json:"label,omitempty"`
}

// Compute reads the latest weather snapshot and returns the UI view.
func (p Weather) Compute(ctx context.Context, r log.Reader) (*WeatherView, error) {
	now := p.Cfg.now()
	tz := p.Cfg.tz()

	ev, err := r.Latest(ctx, log.KindWeatherSnapshot)
	if err != nil {
		return nil, err
	}
	if ev == nil {
		return nil, nil
	}
	if now.Sub(ev.TS) > staleSnapshotAfter {
		return nil, nil
	}

	var snap rawWeatherFull
	if err := json.Unmarshal(ev.Payload, &snap); err != nil {
		return nil, fmt.Errorf("decode weather snapshot: %w", err)
	}

	hourly := make([]WeatherHourPoint, 0, len(snap.Hourly))
	for _, h := range snap.Hourly {
		hourly = append(hourly, WeatherHourPoint{
			Time:  h.Time.In(tz),
			TempC: h.TempC,
		})
	}

	daily := projectDaily(snap.Daily, now.In(tz), tz)

	return &WeatherView{
		CapturedAt: snap.CapturedAt,
		Timezone:   snap.Timezone,
		Location:   snap.Location,
		Current: WeatherCurrent{
			Time:     snap.Current.Time.In(tz),
			TempC:    snap.Current.TempC,
			Label:    snap.Current.Label,
			WindKmh:  snap.Current.WindKmh,
			PrecipMM: snap.Current.PrecipMM,
		},
		Hourly:   hourly,
		NowIndex: nowIndex(hourly, now.In(tz)),
		Daily:    daily,
	}, nil
}

// projectDaily filters the daily forecast to the next 3 days *after* today
// (i.e. tomorrow, day-after, day-after-that). The "current" day is already
// reflected in the hourly graph, so showing it again would be redundant.
func projectDaily(raw []rawWeatherDayFull, now time.Time, tz *time.Location) []WeatherDayPoint {
	if len(raw) == 0 {
		return nil
	}
	today := now.In(tz)
	out := make([]WeatherDayPoint, 0, 3)
	for _, d := range raw {
		dd := d.Date.In(tz)
		if !sameOrAfter(dd, today) {
			continue
		}
		if sameDay(dd, today) {
			continue
		}
		out = append(out, WeatherDayPoint{
			Date:     dd,
			TempMaxC: d.TempMaxC,
			TempMinC: d.TempMinC,
			Label:    d.Label,
			Code:     d.Code,
		})
		if len(out) == 3 {
			break
		}
	}
	return out
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

func sameOrAfter(a, b time.Time) bool {
	ad := time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, a.Location())
	bd := time.Date(b.Year(), b.Month(), b.Day(), 0, 0, 0, 0, b.Location())
	return !ad.Before(bd)
}

// nowIndex returns the index of the hour matching `now` truncated to the
// hour. Falls back to 0 when no entry matches (e.g. snapshot built right at
// an hour boundary and clock skew put `now` slightly before the first hour).
func nowIndex(hourly []WeatherHourPoint, now time.Time) int {
	target := now.Truncate(time.Hour)
	for i, h := range hourly {
		if h.Time.Equal(target) {
			return i
		}
	}
	return 0
}
