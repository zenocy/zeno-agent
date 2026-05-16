// Package logtest provides an in-memory log.Reader for unit tests so
// projections and other consumers can be exercised without touching SQLite.
//
// MemReader is intentionally simple: events are stored verbatim in a slice and
// queries scan it. Behaviour mirrors the GORM-backed store: results are
// returned oldest-first by TS regardless of insertion order.
package logtest

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"

	"github.com/zenocy/zeno-v2/internal/log"
)

// MemReader is a slice-backed log.Store. Despite the name (kept for
// readability of tests), it implements both Reader and Writer so a single
// instance can stand in wherever a Store is expected.
type MemReader struct {
	mu     sync.RWMutex
	events []log.Event
	now    func() time.Time
}

var (
	_ log.Reader = (*MemReader)(nil)
	_ log.Writer = (*MemReader)(nil)
	_ log.Store  = (*MemReader)(nil)
)

// NewMemReader builds a reader from a slice of events. The reader takes
// ownership of the slice (callers should not mutate it after).
func NewMemReader(events ...log.Event) *MemReader {
	return &MemReader{events: events, now: time.Now}
}

// WithNow overrides the clock used by Append's TS field.
func (r *MemReader) WithNow(now func() time.Time) *MemReader {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.now = now
	return r
}

// MakeEvent is a convenience for tests that don't care about IDs. Payload is
// JSON-marshaled; pass nil for an empty payload.
func MakeEvent(kind, source string, ts time.Time, payload any) log.Event {
	var js datatypes.JSON
	if payload == nil {
		js = datatypes.JSON([]byte("null"))
	} else {
		b, err := json.Marshal(payload)
		if err != nil {
			panic("logtest.MakeEvent: marshal payload: " + err.Error())
		}
		js = datatypes.JSON(b)
	}
	return log.Event{
		ID:      "", // ID isn't load-bearing for projection tests; leave blank
		TS:      ts.UTC(),
		Kind:    kind,
		Source:  source,
		Payload: js,
	}
}

// Append implements log.Writer. The payload is JSON-encoded and a random ID
// is generated, mirroring the gormStore behaviour.
func (r *MemReader) Append(_ context.Context, kind, source string, payload any) (log.Event, error) {
	var js datatypes.JSON
	if payload == nil {
		js = datatypes.JSON([]byte("null"))
	} else {
		b, err := json.Marshal(payload)
		if err != nil {
			return log.Event{}, err
		}
		js = datatypes.JSON(b)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e := log.Event{
		ID:      uuid.New().String(),
		TS:      r.now().UTC(),
		Kind:    kind,
		Source:  source,
		Payload: js,
	}
	r.events = append(r.events, e)
	return e, nil
}

// AppendEvent lets tests inject pre-built events directly. Useful for
// arranging history with a specific TS.
func (r *MemReader) AppendEvent(e log.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

// Events returns a snapshot of all events for assertions.
func (r *MemReader) Events() []log.Event {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]log.Event, len(r.events))
	copy(out, r.events)
	return out
}

// Since returns events at or after t, oldest first.
func (r *MemReader) Since(_ context.Context, t time.Time) ([]log.Event, error) {
	tu := t.UTC()
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]log.Event, 0, len(r.events))
	for _, e := range r.events {
		if !e.TS.Before(tu) {
			out = append(out, e)
		}
	}
	sortByTS(out)
	return out, nil
}

// ByKind returns events whose kind matches one of the given kinds, oldest
// first. Pass no kinds to get everything.
func (r *MemReader) ByKind(_ context.Context, kinds ...string) ([]log.Event, error) {
	var match func(string) bool
	if len(kinds) == 0 {
		match = func(string) bool { return true }
	} else {
		set := make(map[string]struct{}, len(kinds))
		for _, k := range kinds {
			set[k] = struct{}{}
		}
		match = func(k string) bool { _, ok := set[k]; return ok }
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]log.Event, 0, len(r.events))
	for _, e := range r.events {
		if match(e.Kind) {
			out = append(out, e)
		}
	}
	sortByTS(out)
	return out, nil
}

// Latest returns the most recent event of the given kind, or nil if none.
func (r *MemReader) Latest(_ context.Context, kind string) (*log.Event, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var latest *log.Event
	for i := range r.events {
		e := r.events[i]
		if e.Kind != kind {
			continue
		}
		if latest == nil || e.TS.After(latest.TS) {
			cp := e
			latest = &cp
		}
	}
	return latest, nil
}

func sortByTS(events []log.Event) {
	sort.SliceStable(events, func(i, j int) bool {
		return events[i].TS.Before(events[j].TS)
	})
}
