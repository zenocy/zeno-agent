// Package injectsub hosts the V2.4 reactive inject subscriber. The
// subscriber is a process-lifetime goroutine that consumes
// SensorEventObservedEvent payloads from the typed eventbus and fires a
// reactive inject pass through Runner — typically *schedule.Scheduler,
// whose RunInjectNow method shares the injectRunning single-flight gate
// with the manual /api/synth/now?kind=inject path.
//
// The package lives outside cmd/zeno so system tests can drive it
// end-to-end (Go forbids importing `package main`).
package injectsub

import (
	"context"
	"errors"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/eventbus"
	"github.com/zenocy/zeno-v2/internal/schedule"
)

// Runner is the seam the subscriber consults to fire a reactive inject
// pass. *schedule.Scheduler.RunInjectNow satisfies it; the indirection
// lets unit tests stub a fast / errorful / single-flight-tripped runner
// without standing up a full scheduler.
type Runner interface {
	RunInjectNow(ctx context.Context) error
}

// Deps bundles everything the subscriber goroutine needs. Built once at
// boot in cmd/zeno/main.go::runServe.
type Deps struct {
	Bus    *eventbus.Bus
	Runner Runner
	Budget time.Duration // per-observation wall-clock ceiling; <=0 → 60 s
	Logger *logrus.Entry
}

// Run consumes SensorEventObservedEvent payloads from the typed bus and
// fires a reactive inject pass through deps.Runner.
//
// Single-flight semantics: deps.Runner (Scheduler.RunInjectNow) shares
// the same injectRunning atomic that the manual /api/synth/now?kind=inject
// path uses, so reactive and manual triggers interlock.
// schedule.ErrInjectInFlight is treated as a debug-log "drop" — the
// subsequent observation in the burst is the next chance to synth.
//
// Detector behavior (deny-by-default + 30-min debounce + projection-fold
// on every Compute) is unchanged from V2.3; the subscriber only changes
// the trigger.
//
// Run blocks until ctx is cancelled or the subscribe channel closes; it
// is intended to be invoked from a long-lived goroutine.
func Run(ctx context.Context, deps Deps) {
	if deps.Bus == nil || deps.Runner == nil {
		if deps.Logger != nil {
			deps.Logger.Warn("inject subscriber: nil bus or runner; not started")
		}
		return
	}
	budget := deps.Budget
	if budget <= 0 {
		budget = 60 * time.Second
	}

	sub := deps.Bus.Subscribe()
	defer deps.Bus.Unsubscribe(sub)

	if deps.Logger != nil {
		deps.Logger.Info("inject subscriber: started")
	}

	for {
		select {
		case <-ctx.Done():
			if deps.Logger != nil {
				deps.Logger.Info("inject subscriber: context cancelled, draining")
			}
			return
		case ev, ok := <-sub:
			if !ok {
				return
			}
			obs, isObs := ev.(eventbus.SensorEventObservedEvent)
			if !isObs {
				continue
			}
			handleObservation(ctx, deps, budget, obs)
		}
	}
}

// handleObservation fires a single reactive inject pass under a per-call
// budget. Errors and single-flight drops are logged but never bubble out
// — the subscriber goroutine must stay alive for the daemon's lifetime.
func handleObservation(parent context.Context, deps Deps, budget time.Duration, obs eventbus.SensorEventObservedEvent) {
	runCtx, cancel := context.WithTimeout(parent, budget)
	defer cancel()

	err := deps.Runner.RunInjectNow(runCtx)
	switch {
	case err == nil:
		// Either Detect returned nil (calm signal, debounced) or a synth
		// fired and published a card. Either way, continue.
	case errors.Is(err, schedule.ErrInjectInFlight):
		if deps.Logger != nil {
			deps.Logger.WithField("kind", obs.Kind_).
				WithField("evidence_id", obs.EvidenceID).
				Debug("inject subscriber: synth in flight, dropping observation")
		}
	default:
		if deps.Logger != nil {
			deps.Logger.WithError(err).
				WithField("kind", obs.Kind_).
				WithField("evidence_id", obs.EvidenceID).
				Warn("inject subscriber: reactive synth failed")
		}
	}
}
