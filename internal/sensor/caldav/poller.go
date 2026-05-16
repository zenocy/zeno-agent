package caldav

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/config"
	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/sensor"
)

const (
	defaultLookaheadDays = 7
	defaultLookbackDays  = 30
)

// LocationSource is the contract Sensor uses to look up the user's current
// timezone on every Sync. *settings.Service satisfies it via TZ(); tests
// pass a stub or wrap a *time.Location.
type LocationSource interface {
	TZ() *time.Location
}

type fixedLocation struct{ loc *time.Location }

func (f fixedLocation) TZ() *time.Location { return f.loc }

// FixedLocation wraps a *time.Location as a LocationSource. Useful for tests
// and call sites that don't yet thread through the live settings service.
func FixedLocation(loc *time.Location) LocationSource {
	if loc == nil {
		loc = time.UTC
	}
	return fixedLocation{loc: loc}
}

// Sensor implements sensor.Sensor for a CalDAV calendar.
//
// The TZ used to resolve floating iCalendar times is read live on every
// Sync via the LocationSource so a TZ change via PUT /api/settings takes
// effect on the next poll without restarting the daemon. Events with an
// explicit TZID continue to use that TZID per RFC 5545 — the LocationSource
// is only the fallback for floating events.
type Sensor struct {
	cfg       config.CalDAVConfig
	provider  Provider
	reader    log.Reader
	writer    log.Writer
	loc       LocationSource
	log       *logrus.Entry
	now       func() time.Time
	lookahead time.Duration
	lookback  time.Duration
}

// New constructs a CalDAV Sensor with the given Provider. loc is consulted
// once per Sync; pass settings.Service or wrap a *time.Location with
// FixedLocation. nil falls back to a UTC FixedLocation.
func New(cfg config.CalDAVConfig, loc LocationSource, p Provider, r log.Reader, w log.Writer, l *logrus.Entry) *Sensor {
	if loc == nil {
		loc = FixedLocation(time.UTC)
	}
	return &Sensor{
		cfg:       cfg,
		provider:  p,
		reader:    r,
		writer:    w,
		loc:       loc,
		log:       l,
		now:       time.Now,
		lookahead: defaultLookaheadDays * 24 * time.Hour,
		lookback:  defaultLookbackDays * 24 * time.Hour,
	}
}

// tz reads the current floating-time fallback location, defaulting to UTC.
func (s *Sensor) tz() *time.Location {
	if s.loc == nil {
		return time.UTC
	}
	if l := s.loc.TZ(); l != nil {
		return l
	}
	return time.UTC
}

// WithNow overrides the clock (used by tests).
func (s *Sensor) WithNow(now func() time.Time) *Sensor { s.now = now; return s }

// Name returns the sensor identifier.
func (s *Sensor) Name() string { return "caldav" }

// listSnapshotPayload is the JSON shape of a cal.list_snapshot event:
// the full set of UIDs returned by the provider for the current poll
// window. Latest-per-poll is authoritative; projections use it to filter
// the cal.event_seen / cal.event_changed fold down to events still
// present on the server.
type listSnapshotPayload struct {
	UIDs []string  `json:"uids"`
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// eventPayload is the JSON shape of cal.event_seen / cal.event_changed events.
//
// Attendees and LastModified (V2.3.0 P3) feed the inject detector's
// calendar-move path via the projection layer.
type eventPayload struct {
	UID          string    `json:"uid"`
	Title        string    `json:"title,omitempty"`
	Location     string    `json:"location,omitempty"`
	Tag          string    `json:"tag,omitempty"`
	Start        time.Time `json:"start"`
	End          time.Time `json:"end"`
	ETag         string    `json:"etag,omitempty"`
	Attendees    []string  `json:"attendees,omitempty"`
	LastModified time.Time `json:"last_modified,omitzero"`
}

// Sync lists upcoming events and emits cal.event_seen for newly observed UIDs
// and cal.event_changed when fields change vs the last observation.
func (s *Sensor) Sync(ctx context.Context) error {
	now := s.now()
	from := now.Add(-1 * time.Hour) // small buffer for events that just started
	to := now.Add(s.lookahead)

	raws, err := s.provider.ListEvents(ctx, from, to)
	if err != nil {
		if s.log != nil {
			s.log.WithError(err).WithField("calendar_url", s.cfg.URL).
				Warn("caldav: list events failed")
		}
		return fmt.Errorf("list events: %w", err)
	}

	// Build a map of the last-known payload per UID from history.
	lastByUID, err := s.lastSeen(ctx, now.Add(-s.lookback))
	if err != nil {
		return fmt.Errorf("read history: %w", err)
	}

	seen, changed, skipped := 0, 0, 0
	uids := make([]string, 0, len(raws))
	for _, r := range raws {
		ev, perr := ParseVEVENT(r.ICS, s.tz())
		if perr != nil {
			skipped++
			if s.log != nil {
				s.log.WithError(perr).WithField("uid", r.UID).Warn("skip unparseable VEVENT")
			}
			continue
		}
		uids = append(uids, ev.UID)
		payload := eventPayload{
			UID:          ev.UID,
			Title:        ev.Title,
			Location:     ev.Location,
			Tag:          ev.Tag,
			Start:        ev.Start,
			End:          ev.End,
			ETag:         r.ETag,
			Attendees:    ev.Attendees,
			LastModified: ev.LastModified,
		}
		prev, prevSeen := lastByUID[ev.UID]
		switch {
		case !prevSeen:
			if _, err := s.writer.Append(ctx, log.KindCalEventSeen, s.Name(), payload); err != nil {
				return fmt.Errorf("append cal.event_seen: %w", err)
			}
			seen++
			// V2.4 reactive trigger: publish strictly AFTER successful append.
			sensor.PublishObserved(ctx, "cal.event_seen", payload.UID, payload.asMap())
		case fieldsDiffer(prev, payload):
			if _, err := s.writer.Append(ctx, log.KindCalEventChanged, s.Name(), payload); err != nil {
				return fmt.Errorf("append cal.event_changed: %w", err)
			}
			changed++
			sensor.PublishObserved(ctx, "cal.event_changed", payload.UID, payload.asMap())
		}
	}

	// Snapshot the parsed UID set so projections can drop events that
	// vanished externally. Best-effort: a write failure is logged but
	// not fatal — the sync as a whole already succeeded.
	sort.Strings(uids)
	snapEv, err := s.writer.Append(ctx, log.KindCalListSnapshot, s.Name(), listSnapshotPayload{
		UIDs: uids, From: from, To: to,
	})
	if err != nil {
		if s.log != nil {
			s.log.WithError(err).Warn("caldav: snapshot append failed")
		}
	} else {
		// Wakes the projection publisher so today's calendar list (the
		// projection used by the UI rail) is recomputed and broadcast
		// over SSE without waiting for the UI's poll.
		sensor.PublishObserved(ctx, log.KindCalListSnapshot, snapEv.ID, nil)
	}

	if s.log != nil {
		s.log.WithFields(logrus.Fields{
			"events_listed": len(raws),
			"seen":          seen,
			"changed":       changed,
			"skipped":       skipped,
		}).Info("caldav: sync complete")
	}
	return nil
}

// asMap renders the payload as a map for the V2.4 reactive-trigger
// PublishObserved call. The map shape is informational (the inject
// subscriber re-folds projections rather than reading payloads), but the
// fields below are useful for debugging the event stream.
func (p eventPayload) asMap() map[string]any {
	return map[string]any{
		"uid":           p.UID,
		"title":         p.Title,
		"location":      p.Location,
		"tag":           p.Tag,
		"start":         p.Start,
		"end":           p.End,
		"last_modified": p.LastModified,
		"attendees":     p.Attendees,
	}
}

// lastSeen folds the recent cal.* history into the latest payload per UID.
func (s *Sensor) lastSeen(ctx context.Context, since time.Time) (map[string]eventPayload, error) {
	events, err := s.reader.ByKind(ctx, log.KindCalEventSeen, log.KindCalEventChanged)
	if err != nil {
		return nil, err
	}
	// Filter to events newer than `since`, then sort ascending by TS so the
	// latest write wins when we accumulate by UID.
	filtered := make([]log.Event, 0, len(events))
	for _, e := range events {
		if !e.TS.Before(since) {
			filtered = append(filtered, e)
		}
	}
	sort.SliceStable(filtered, func(i, j int) bool { return filtered[i].TS.Before(filtered[j].TS) })

	out := make(map[string]eventPayload, len(filtered))
	for _, e := range filtered {
		var p eventPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		out[p.UID] = p
	}
	return out, nil
}

// fieldsDiffer returns true when the user-visible fields of a calendar event
// have changed. ETag is intentionally not part of the comparison (some servers
// flip etags without underlying field changes — avoid noise).
//
// LAST-MODIFIED IS compared: a server-side bump there is the canonical signal
// that the event changed, and the inject detector's calendar-move path keys
// off it, so we want a cal.event_changed event to land whenever LAST-MODIFIED
// advances. Attendee list churn is also user-visible.
func fieldsDiffer(a, b eventPayload) bool {
	if a.Title != b.Title || a.Location != b.Location || a.Tag != b.Tag {
		return true
	}
	if !a.Start.Equal(b.Start) || !a.End.Equal(b.End) {
		return true
	}
	if !a.LastModified.Equal(b.LastModified) {
		return true
	}
	if !attendeesEqual(a.Attendees, b.Attendees) {
		return true
	}
	return false
}

func attendeesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
