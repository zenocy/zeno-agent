package projection

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/zenocy/zeno-v2/internal/log"
)

// TomorrowsCalendar returns events whose local-time interval overlaps the
// day after today, in the configured local timezone. Same fold + snapshot
// semantics as TodaysCalendar; only the day window differs.
type TomorrowsCalendar struct {
	Cfg Config
}

func (p TomorrowsCalendar) Name() string { return "tomorrows_calendar" }

func (p TomorrowsCalendar) Compute(ctx context.Context, r log.Reader) ([]CalendarEvent, error) {
	now := p.Cfg.now()
	tz := p.Cfg.tz()

	events, err := r.ByKind(ctx, log.KindCalEventSeen, log.KindCalEventChanged)
	if err != nil {
		return nil, err
	}

	hasSnapshot, presentUIDs, err := loadCalListSnapshot(ctx, r)
	if err != nil {
		return nil, err
	}

	since := now.Add(-p.Cfg.lookback())
	latest := make(map[string]rawCalEvent, len(events))
	for _, e := range events {
		if e.TS.Before(since) {
			continue
		}
		var raw rawCalEvent
		if err := json.Unmarshal(e.Payload, &raw); err != nil {
			continue
		}
		latest[raw.UID] = raw
	}

	if hasSnapshot {
		for uid := range latest {
			if _, ok := presentUIDs[uid]; !ok {
				delete(latest, uid)
			}
		}
	}

	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, tz).Add(24 * time.Hour)
	dayEnd := dayStart.Add(24 * time.Hour)

	out := make([]CalendarEvent, 0, len(latest))
	for _, raw := range latest {
		startLocal := raw.Start.In(tz)
		endLocal := raw.End.In(tz)
		if !endLocal.After(dayStart) || !startLocal.Before(dayEnd) {
			continue
		}
		out = append(out, CalendarEvent{
			UID:          raw.UID,
			Title:        raw.Title,
			Location:     raw.Location,
			Tag:          raw.Tag,
			Start:        startLocal,
			End:          endLocal,
			Attendees:    raw.Attendees,
			LastModified: raw.LastModified,
		})
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}
