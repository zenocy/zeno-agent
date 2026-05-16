package metrics

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// SnapshotKind is the event log Kind written by the emitter on each tick.
// Mirrored as log.KindStatsSnapshot to avoid an import cycle (the metrics
// package must not depend on internal/log).
const SnapshotKind = "stats.snapshot"

// AppendFunc mirrors log.Writer.Append's shape, supplied by the caller so
// this package stays free of an internal/log import. The return value is
// discarded; only the error matters.
type AppendFunc func(ctx context.Context, kind, source string, payload any) error

// EmitterConfig governs the periodic emitter goroutine.
type EmitterConfig struct {
	Interval time.Duration    // default 60s if zero
	Logger   *logrus.Entry    // optional
	Append   AppendFunc       // required; nil disables persistence
	Source   string           // event Source field; default "metrics"
	Hooks    []SnapshotHook   // optional pre-emit callbacks
	Now      func() time.Time // override clock for tests
}

// SnapshotHook runs before a snapshot is gathered so callers can refresh
// pull-style gauges (e.g. set the SSE subscriber count from the eventbus).
type SnapshotHook func(*Metrics)

// Emitter periodically gathers a Snapshot and appends it as an event.
type Emitter struct {
	m   *Metrics
	cfg EmitterConfig

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewEmitter constructs an Emitter against m using cfg. Call Start to begin
// the goroutine and Stop to shut it down.
func NewEmitter(m *Metrics, cfg EmitterConfig) *Emitter {
	if cfg.Interval <= 0 {
		cfg.Interval = 60 * time.Second
	}
	if cfg.Source == "" {
		cfg.Source = "metrics"
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Emitter{m: m, cfg: cfg}
}

// Start launches the emitter goroutine. ctx cancellation triggers a clean
// stop. Safe to call once per Emitter.
func (e *Emitter) Start(ctx context.Context) {
	if e.cfg.Append == nil {
		// Persistence disabled — nothing to do. Log once and return.
		if e.cfg.Logger != nil {
			e.cfg.Logger.Debug("metrics: emitter has no append fn, snapshots disabled")
		}
		return
	}
	cctx, cancel := context.WithCancel(ctx)
	e.cancel = cancel
	e.wg.Add(1)
	go e.run(cctx)
}

// Stop signals the goroutine to exit and waits for it.
func (e *Emitter) Stop() {
	if e.cancel != nil {
		e.cancel()
	}
	e.wg.Wait()
}

func (e *Emitter) run(ctx context.Context) {
	defer e.wg.Done()
	ticker := time.NewTicker(e.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.tick(ctx)
		}
	}
}

func (e *Emitter) tick(ctx context.Context) {
	for _, h := range e.cfg.Hooks {
		h(e.m)
	}
	snap := e.m.Snapshot()
	snap.TS = e.cfg.Now().UTC()
	payload, err := json.Marshal(snap)
	if err != nil {
		if e.cfg.Logger != nil {
			e.cfg.Logger.WithError(err).Warn("metrics: snapshot marshal failed")
		}
		return
	}
	if err := e.cfg.Append(ctx, SnapshotKind, e.cfg.Source, json.RawMessage(payload)); err != nil {
		if e.cfg.Logger != nil {
			e.cfg.Logger.WithError(err).Warn("metrics: snapshot append failed")
		}
	}
}

// EmitOnce runs one tick synchronously. Test helper.
func (e *Emitter) EmitOnce(ctx context.Context) { e.tick(ctx) }
