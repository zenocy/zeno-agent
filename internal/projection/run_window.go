package projection

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/zenocy/zeno-v2/internal/log"
)

// staleSnapshotAfter is how old a weather snapshot can be before the run
// window projection treats the data as missing. Keeps the projection from
// answering with stale forecast on a long-broken sensor.
const staleSnapshotAfter = 6 * time.Hour

// RunWindow finds the first contiguous outdoor-friendly window today: clear
// weather, light wind, no precipitation, between EarliestHour and LatestHour
// in the configured TZ, not overlapping a calendar event.
type RunWindow struct {
	Cfg Config
}

// Name returns the projection identifier.
func (p RunWindow) Name() string { return "run_window" }

// rawWeatherSnapshot mirrors the payload shape written by the weather sensor.
type rawWeatherSnapshot struct {
	CapturedAt time.Time      `json:"captured_at"`
	Timezone   string         `json:"timezone"`
	Hourly     []rawHourPoint `json:"hourly"`
}

type rawHourPoint struct {
	Time     time.Time `json:"time"`
	Code     int       `json:"code"`
	WindKmh  float64   `json:"wind_kmh"`
	PrecipMM float64   `json:"precip_mm"`
}

// Compute returns the first qualifying window, or nil.
func (p RunWindow) Compute(ctx context.Context, r log.Reader) (*Window, error) {
	now := p.Cfg.now()
	tz := p.Cfg.tz()

	weatherEv, err := r.Latest(ctx, log.KindWeatherSnapshot)
	if err != nil {
		return nil, err
	}
	if weatherEv == nil {
		return nil, nil
	}
	if now.Sub(weatherEv.TS) > staleSnapshotAfter {
		return nil, nil
	}
	var snap rawWeatherSnapshot
	if err := json.Unmarshal(weatherEv.Payload, &snap); err != nil {
		return nil, fmt.Errorf("decode weather snapshot: %w", err)
	}

	cal, err := TodaysCalendar{Cfg: p.Cfg}.Compute(ctx, r)
	if err != nil {
		return nil, err
	}

	earliest := p.Cfg.RunWindowEarliestHour
	latest := p.Cfg.RunWindowLatestHour
	if earliest <= 0 {
		earliest = 6
	}
	if latest <= 0 {
		latest = 20
	}
	minMinutes := p.Cfg.RunWindowMinMinutes
	if minMinutes <= 0 {
		minMinutes = 45
	}
	maxWind := p.Cfg.RunWindowMaxWindKmh
	if maxWind <= 0 {
		maxWind = 25
	}

	dayStart := time.Date(now.Year(), now.Month(), now.Day(), earliest, 0, 0, 0, tz)
	dayEnd := time.Date(now.Year(), now.Month(), now.Day(), latest, 0, 0, 0, tz)
	if !dayEnd.After(dayStart) {
		return nil, nil
	}

	gaps := buildGaps(dayStart, dayEnd, cal, tz)
	for _, g := range gaps {
		if w, ok := scanForGoodSpan(g.start, g.end, snap.Hourly, tz, time.Duration(minMinutes)*time.Minute, maxWind); ok {
			return w, nil
		}
	}
	return nil, nil
}

type interval struct{ start, end time.Time }

func buildGaps(dayStart, dayEnd time.Time, cal []CalendarEvent, tz *time.Location) []interval {
	busy := make([]interval, 0, len(cal))
	for _, e := range cal {
		s := e.Start.In(tz)
		en := e.End.In(tz)
		if !en.After(dayStart) || !s.Before(dayEnd) {
			continue
		}
		if s.Before(dayStart) {
			s = dayStart
		}
		if en.After(dayEnd) {
			en = dayEnd
		}
		busy = append(busy, interval{start: s, end: en})
	}
	sort.SliceStable(busy, func(i, j int) bool { return busy[i].start.Before(busy[j].start) })

	gaps := make([]interval, 0, len(busy)+1)
	cursor := dayStart
	for _, b := range busy {
		if b.start.After(cursor) {
			gaps = append(gaps, interval{start: cursor, end: b.start})
		}
		if b.end.After(cursor) {
			cursor = b.end
		}
	}
	if dayEnd.After(cursor) {
		gaps = append(gaps, interval{start: cursor, end: dayEnd})
	}
	return gaps
}

// scanForGoodSpan walks the hourly slice within [start, end] and returns the
// first contiguous span ≥ minDuration where every hourly entry meets the
// thresholds. The span is clamped to the gap.
func scanForGoodSpan(start, end time.Time, hourly []rawHourPoint, tz *time.Location, minDuration time.Duration, maxWind float64) (*Window, bool) {
	if !end.After(start) {
		return nil, false
	}
	type hour struct {
		t    time.Time
		good bool
		code int
	}
	hours := make([]hour, 0, len(hourly))
	for _, h := range hourly {
		ht := h.Time.In(tz)
		if ht.Before(start.Add(-time.Hour)) || ht.After(end) {
			continue
		}
		good := isClearCode(h.Code) && h.WindKmh <= maxWind && h.PrecipMM == 0
		hours = append(hours, hour{t: ht, good: good, code: h.Code})
	}
	if len(hours) == 0 {
		return nil, false
	}
	sort.SliceStable(hours, func(i, j int) bool { return hours[i].t.Before(hours[j].t) })

	// A "span" is a consecutive run of good hours. Clamp to the gap and
	// require >= minDuration. Each hourly entry covers from its time to the
	// next entry's time (or +1h for the last).
	for i := 0; i < len(hours); i++ {
		if !hours[i].good {
			continue
		}
		spanStart := hours[i].t
		if spanStart.Before(start) {
			spanStart = start
		}
		j := i
		var dominantCode int = hours[i].code
		for j+1 < len(hours) && hours[j+1].good {
			j++
			dominantCode = hours[j].code
		}
		// End of span is the start of hour after the last good one, or +1h.
		var spanEnd time.Time
		if j+1 < len(hours) {
			spanEnd = hours[j+1].t
		} else {
			spanEnd = hours[j].t.Add(time.Hour)
		}
		if spanEnd.After(end) {
			spanEnd = end
		}
		if spanEnd.Sub(spanStart) >= minDuration {
			return &Window{
				Start:     spanStart,
				End:       spanEnd,
				Condition: weatherLabel(dominantCode),
			}, true
		}
		i = j // skip past this run
	}
	return nil, false
}

func isClearCode(code int) bool {
	return code == 0 || code == 1 || code == 2
}

// weatherLabel returns a short condition string for the run window. We keep
// this minimal — the briefing voice will rephrase.
func weatherLabel(code int) string {
	switch code {
	case 0:
		return "clear"
	case 1:
		return "mainly clear"
	case 2:
		return "partly cloudy"
	default:
		return "open"
	}
}
