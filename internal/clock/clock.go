// Package clock is the canonical source of truth for "what time is it now"
// and "what timezone is the user in" for date-bound code paths.
//
// Two implementations:
//
//   - Real, backed by a settings.Service-shaped TZ getter. Now() returns the
//     wall clock in the user's current location; Location() returns it.
//     Both reads always observe the live settings snapshot — when the user
//     changes their TZ via PUT /api/settings, every subsequent call reflects
//     the new zone without a restart.
//
//   - Fixed, a thread-safe test fake. Holds a pinned time and location,
//     exposes Advance(d) and SetLocation(loc) for test setup.
//
// Use Clock for any logic whose result depends on a date, day boundary,
// daypart, or wall-clock hour (e.g. "today's calendar", cron-driven
// briefing daypart, day-relative filtering). Do NOT replace time.Now()
// in code that measures durations — latency timers, retry backoffs, and
// metrics elapsed-time samples should keep using time.Now() directly.
package clock

import (
	"sync"
	"time"
)

// Clock is the minimal interface every date-bound caller depends on.
type Clock interface {
	// Now returns the current wall-clock time, normalized to Location().
	// Callers that only need an instant (and don't care about zone) can
	// equivalently use t := c.Now(); the embedded Location is informational.
	Now() time.Time
	// Location returns the user's current timezone. Always non-nil; falls
	// back to time.UTC when the underlying source is unset.
	Location() *time.Location
}

// LocationSource is the contract Real depends on. settings.Service satisfies
// it via its TZ() method. Defining a small interface (rather than importing
// settings) keeps the clock package free of the import cycle that would
// arise from settings → clock → settings.
type LocationSource interface {
	TZ() *time.Location
}

// Real is the production Clock. Now() and Location() always re-read from
// the underlying source so that live TZ changes propagate instantly.
type Real struct {
	src LocationSource
}

// NewReal builds a Real backed by a LocationSource (typically a *settings.Service).
// Passing nil yields a Clock that always returns time.UTC.
func NewReal(src LocationSource) *Real {
	return &Real{src: src}
}

// Now returns the current wall-clock time in the user's current location.
func (r *Real) Now() time.Time {
	return time.Now().In(r.Location())
}

// Location returns the user's current timezone, falling back to UTC.
func (r *Real) Location() *time.Location {
	if r.src == nil {
		return time.UTC
	}
	if loc := r.src.TZ(); loc != nil {
		return loc
	}
	return time.UTC
}

// LiveTZ wraps a LocationSource (typically *settings.Service) with a
// caller-supplied wall-clock function so tests can pin "now" while still
// observing live TZ edits. Used by the system harness to verify the
// PUT /api/settings → projection re-target loop without depending on the
// host's actual time.Now.
//
// Construct via NewLiveTZ. Now() returns now().In(src.TZ()). Location()
// returns src.TZ(). Both fall back to time.UTC when their source is nil.
type LiveTZ struct {
	src LocationSource
	now func() time.Time
}

// NewLiveTZ constructs a LiveTZ. nowFn nil falls back to time.Now.
func NewLiveTZ(src LocationSource, nowFn func() time.Time) *LiveTZ {
	if nowFn == nil {
		nowFn = time.Now
	}
	return &LiveTZ{src: src, now: nowFn}
}

// Now returns now() converted to the live source location.
func (l *LiveTZ) Now() time.Time { return l.now().In(l.Location()) }

// Location returns the live source location, falling back to UTC.
func (l *LiveTZ) Location() *time.Location {
	if l.src == nil {
		return time.UTC
	}
	if loc := l.src.TZ(); loc != nil {
		return loc
	}
	return time.UTC
}

// Fixed is a deterministic Clock for tests. Safe for concurrent use.
type Fixed struct {
	mu  sync.RWMutex
	t   time.Time
	loc *time.Location
}

// NewFixed returns a Fixed Clock pinned at t in loc. If loc is nil it falls
// back to time.UTC. The returned t from Now() is normalized to loc so callers
// don't need to .In() it themselves.
func NewFixed(t time.Time, loc *time.Location) *Fixed {
	if loc == nil {
		loc = time.UTC
	}
	return &Fixed{t: t.In(loc), loc: loc}
}

// Now returns the pinned time in the current location.
func (f *Fixed) Now() time.Time {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.t
}

// Location returns the pinned location.
func (f *Fixed) Location() *time.Location {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.loc
}

// Advance moves the pinned time forward by d. Negative durations are allowed.
func (f *Fixed) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

// Set overwrites the pinned time. The new time is normalized to the current
// location so Now() always returns a time in loc.
func (f *Fixed) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = t.In(f.loc)
}

// SetLocation swaps the pinned location and re-normalizes the pinned time
// to the new zone (the underlying instant is preserved). Passing nil falls
// back to UTC.
func (f *Fixed) SetLocation(loc *time.Location) {
	if loc == nil {
		loc = time.UTC
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loc = loc
	f.t = f.t.In(loc)
}
