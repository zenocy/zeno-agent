package projection

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/zenocy/zeno-v2/internal/log"
)

// TodaysCalendar is the projection that returns today's events ordered by
// start, in the configured local timezone.
type TodaysCalendar struct {
	Cfg Config
}

// Name returns the projection identifier.
func (p TodaysCalendar) Name() string { return "todays_calendar" }

// rawCalEvent matches the payload shape written by the CalDAV sensor.
//
// Attendees and LastModified (V2.3.0 P3) flow through to CalendarEvent and
// power the inject detector's calendar-move path. Older payloads that
// pre-date these fields decode with empty / zero values and behave as
// "no attendees, never modified" — the detector treats both as inert.
type rawCalEvent struct {
	UID          string    `json:"uid"`
	Title        string    `json:"title"`
	Location     string    `json:"location"`
	Tag          string    `json:"tag"`
	Start        time.Time `json:"start"`
	End          time.Time `json:"end"`
	Attendees    []string  `json:"attendees,omitempty"`
	LastModified time.Time `json:"last_modified,omitzero"`
}

// Compute returns events that overlap today's local-time window [00:00, 24:00).
// When the same UID appears in cal.event_seen and a later cal.event_changed,
// the latest payload wins.
//
// The latest cal.list_snapshot filters the fold down to UIDs the server
// is still listing — so events the user deleted in their calendar UI
// drop out within one poll cycle. If no snapshot exists yet (first
// deploy), the projection includes every UID it has seen so the briefing
// isn't blanked.
func (p TodaysCalendar) Compute(ctx context.Context, r log.Reader) ([]CalendarEvent, error) {
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
		// ByKind returns oldest-first, so a later iteration overwrites.
		latest[raw.UID] = raw
	}

	if hasSnapshot {
		for uid := range latest {
			if _, ok := presentUIDs[uid]; !ok {
				delete(latest, uid)
			}
		}
	}

	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, tz)
	dayEnd := dayStart.Add(24 * time.Hour)

	out := make([]CalendarEvent, 0, len(latest))
	for _, raw := range latest {
		startLocal := raw.Start.In(tz)
		endLocal := raw.End.In(tz)
		// Keep the event if any portion overlaps today.
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

// rawCalListSnapshot mirrors the listSnapshotPayload written by the
// CalDAV poller. Kept private to the projection package so the
// projection layer doesn't import the sensor.
type rawCalListSnapshot struct {
	UIDs []string `json:"uids"`
}

// loadCalListSnapshot returns (hasSnapshot, present-UID-set). hasSnapshot
// is false when no cal.list_snapshot has ever been written, which lets
// the projection default to include-all on first deploy. Otherwise the
// latest snapshot (by event TS) is authoritative — NOT subject to the
// lookback window, since it represents current server truth.
func loadCalListSnapshot(ctx context.Context, r log.Reader) (bool, map[string]struct{}, error) {
	ev, err := r.Latest(ctx, log.KindCalListSnapshot)
	if err != nil {
		return false, nil, err
	}
	if ev == nil {
		return false, nil, nil
	}
	var snap rawCalListSnapshot
	if err := json.Unmarshal(ev.Payload, &snap); err != nil {
		return false, nil, nil
	}
	set := make(map[string]struct{}, len(snap.UIDs))
	for _, u := range snap.UIDs {
		set[u] = struct{}{}
	}
	return true, set, nil
}
