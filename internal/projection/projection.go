// Package projection folds the observation log into typed read-side views:
// today's calendar, open email threads, and a run window. Projections are
// computed on demand against a log.Reader; there is no caching for MVP.
package projection

import (
	"context"
	"time"

	"github.com/zenocy/zeno-v2/internal/clock"
	"github.com/zenocy/zeno-v2/internal/log"
)

// Projection turns a stream of log events into a typed result. T is intended
// to match the projection's domain shape (e.g. []CalendarEvent).
type Projection[T any] interface {
	Name() string
	Compute(ctx context.Context, r log.Reader) (T, error)
}

// Config holds the tunables for all three projections in this package.
//
// Clock is the canonical source for "now" and "user's timezone"; production
// passes a *clock.Real that re-reads settings.Service on every call so live
// TZ edits propagate without reconstruction. Tests typically pass a
// *clock.Fixed.
//
// TZ and Now are legacy fields retained for test ergonomics: dozens of
// tests construct Config{TZ: tz, Now: func() time.Time { return now }}
// directly. When Clock is nil, tz()/now() fall back to those legacy
// fields. Production code paths must set Clock so live TZ reload works.
type Config struct {
	Clock                 clock.Clock
	TZ                    *time.Location
	LookbackDays          int
	RunWindowMinMinutes   int
	RunWindowMaxWindKmh   float64
	RunWindowEarliestHour int
	RunWindowLatestHour   int
	OpenThreadsMax        int
	Now                   func() time.Time
}

func (c Config) now() time.Time {
	if c.Clock != nil {
		return c.Clock.Now()
	}
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c Config) tz() *time.Location {
	if c.Clock != nil {
		return c.Clock.Location()
	}
	if c.TZ != nil {
		return c.TZ
	}
	return time.UTC
}

func (c Config) lookback() time.Duration {
	d := c.LookbackDays
	if d <= 0 {
		d = 14
	}
	return time.Duration(d) * 24 * time.Hour
}
