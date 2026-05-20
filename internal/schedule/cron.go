// Package schedule wires a robfig/cron scheduler around the sensor list.
// Two jobs: sync_all (cfg.SyncCron) fans out to every Sensor concurrently,
// morning_synth (cfg.MorningCron) is a no-op stub for Phase 1.
//
// Sensors must be safe to call concurrently — both the cron tick and a
// manual /api/sync/now request can fire SyncAll at the same time.
package schedule

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/config"
	"github.com/zenocy/zeno-v2/internal/eventbus"
	"github.com/zenocy/zeno-v2/internal/llm"
	zlog "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/sensor"
)

// ErrMorningInFlight is returned by RunMorningNow when a prior morning synth
// (cron-driven or manual) is still running. Callers should surface this as a
// 409 Conflict to the user rather than retrying.
var ErrMorningInFlight = errors.New("morning synth already in flight")

// ErrNoMorningSynth is returned by RunMorningNow when the scheduler was
// constructed without a morningSynth function (tests, replay).
var ErrNoMorningSynth = errors.New("no morning synth function configured")

// ErrInjectInFlight is returned by RunInjectNow when a prior inject pass
// (cron-driven or manual) is still running.
var ErrInjectInFlight = errors.New("inject pipeline already in flight")

// ErrNoInjectFunc is returned by RunInjectNow when the scheduler was
// constructed without an inject function (tests, replay).
var ErrNoInjectFunc = errors.New("no inject function configured")

// ErrRecognitionInFlight is returned by RunRecognitionNow when a prior
// recognition pass (cron-driven or manual) is still running.
var ErrRecognitionInFlight = errors.New("recognition pass already in flight")

// ErrNoRecognitionFunc is returned by RunRecognitionNow when the scheduler
// was constructed without a RecognitionFunc.
var ErrNoRecognitionFunc = errors.New("no recognition function configured")

const defaultPerSensorTimeout = 60 * time.Second

// SyncResult captures the outcome of one sensor's Sync call.
type SyncResult struct {
	Name     string        `json:"name"`
	OK       bool          `json:"ok"`
	Err      string        `json:"error,omitempty"`
	Duration time.Duration `json:"duration"`
}

// MorningSynthFunc is invoked on every morning_synth cron tick. Returning an
// error is logged but does not stop the scheduler. Pass nil to keep the cron
// firing for tests but skip the actual synth work.
type MorningSynthFunc func(context.Context) error

// RecognitionFunc is invoked on every recognition cron tick. Returning an
// error is logged but does not stop the scheduler. Pass nil to keep the
// cron firing for tests but skip the actual recognition work.
type RecognitionFunc func(context.Context) error

// InjectFunc is invoked from the manual /api/synth/now?kind=inject debug
// path and from the V2.4 reactive subscriber on each sensor observation.
//
// `signal` is nil on reactive ticks — the orchestrator builds detector
// deps, runs sensor.Detect, and (on a returned signal) calls
// synth.SynthesizeInject.
//
// `signal` is non-nil on manual debug ticks — the orchestrator skips
// Detect entirely and uses the supplied signal directly. The scheduler
// keeps the signal opaque (`any`) so the schedule package doesn't import
// synth; the orchestrator type-asserts on its way through. Returning an
// error is logged but does not stop the scheduler.
type InjectFunc func(ctx context.Context, signal any) error

// entrySpec captures one cron entry as data so the scheduler can rebuild
// its underlying cron.Cron when the user's TZ changes (Retarget). The
// validated spec is also re-validated by cron.AddFunc on each rebuild;
// since New ran the parser already, AddFunc will not error.
type entrySpec struct {
	job  string // "sync_all" | "morning_synth" | "refresh"
	spec string
	fn   func()
}

// Scheduler owns the cron loop and the sensor list.
type Scheduler struct {
	parser           cron.ScheduleParser
	sensors          []sensor.Sensor
	cfg              config.ScheduleConfig
	perSensorTimeout time.Duration
	morningBudget    time.Duration // total budget for one morning_synth tick; 0 → 90s
	injectBudget     time.Duration // total budget for one reactive inject; 0 → 60s
	log              *logrus.Entry
	morningSynth     MorningSynthFunc
	injectFn         InjectFunc    // V2.3.0 P3 inject orchestrator; manual /api/synth/now path still uses it.
	eventLog         zlog.Writer   // optional; nil → no sync.completed events emitted
	bus              *eventbus.Bus // V2.4: when non-nil, SyncAll attaches a publisher to per-sensor ctx so sensors can call sensor.PublishObserved.

	// entriesMu guards location, entries, cron, started — i.e. everything
	// touched by Start/Stop/Retarget. The cron-fired callbacks (runSyncAll,
	// runMorningSynth, runRecognition) do NOT acquire this mutex; they
	// coordinate via the per-job atomic.Bool guards below, so Retarget can
	// hold the mutex while waiting for in-flight jobs to drain.
	entriesMu sync.Mutex
	entries   []entrySpec
	location  *time.Location
	cron      *cron.Cron // built lazily by Start / rebuilt by Retarget
	started   bool

	// Single-flight guards: if a cron tick fires while the prior run is still
	// in flight, the new tick is skipped. Manual /api/sync/now still goes
	// through SyncAll directly and is not blocked by these flags. The
	// injectRunning flag is shared with the V2.4 reactive subscriber so
	// observation-driven and manual injects interlock.
	syncRunning        atomic.Bool
	morningRunning     atomic.Bool
	injectRunning      atomic.Bool
	recognitionRunning atomic.Bool

	// onCron, when non-nil, is invoked once per cron firing with the job
	// name ("sync_all"|"morning_synth"|"recognition") and outcome
	// ("ok"|"error"|"skipped"). Wired by main.go to metrics.ObserveCron.
	onCron func(job, outcome string)
	// onSensor mirrors metrics.ObserveSensor; called once per sensor sync.
	onSensor func(sensor, outcome string, dur time.Duration, records map[string]int)

	// V2.5.0: daily concern recognition.
	recognitionFn     RecognitionFunc
	recognitionBudget time.Duration // 0 → 60s

	// serviceTier is attached to every background-fired ctx (cron tick
	// or manual *Now trigger) via llm.ContextWithServiceTier so the
	// LLM client can route those calls to a non-default OpenRouter
	// service tier (typically "flex" for cost savings on work the
	// operator isn't actively watching). Empty string = no tier
	// override; the LLM call omits the field. Wired via WithServiceTier.
	serviceTier string
}

// New constructs a Scheduler. Returns an error if any cron expression
// fails to parse. morningSynth may be nil — in that case the morning_synth
// cron still fires but only logs.
//
// New does NOT yet build the underlying cron.Cron; it validates and persists
// each entry's spec so Start (or Retarget) can build the cron with the
// current timezone. The default location is time.UTC; call WithLocation
// before Start, or call Retarget at any time, to change it.
func New(cfg config.ScheduleConfig, sensors []sensor.Sensor, morningSynth MorningSynthFunc, l *logrus.Entry) (*Scheduler, error) {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	s := &Scheduler{
		parser:           parser,
		sensors:          sensors,
		cfg:              cfg,
		perSensorTimeout: defaultPerSensorTimeout,
		log:              l,
		morningSynth:     morningSynth,
		location:         time.UTC,
	}
	add := func(job, spec string, fn func()) error {
		if spec == "" {
			return nil
		}
		if _, err := parser.Parse(spec); err != nil {
			return fmt.Errorf("parse %s_cron %q: %w", job, spec, err)
		}
		s.entries = append(s.entries, entrySpec{job: job, spec: spec, fn: fn})
		return nil
	}
	if err := add("sync", cfg.SyncCron, s.runSyncAll); err != nil {
		return nil, err
	}
	if err := add("morning", cfg.MorningCron, s.runMorningSynth); err != nil {
		return nil, err
	}
	// RefreshCron fires the same synth pass at mid-day / end-of-day so the
	// briefing+state register tracks the user's daypart. Shares the
	// morningRunning single-flight guard with MorningCron — if a 12:00 tick
	// arrives while the 07:00 run is somehow still in flight, it's skipped.
	if err := add("refresh", cfg.RefreshCron, s.runMorningSynth); err != nil {
		return nil, err
	}
	// Recognition has no dedicated cron — it chains off every morning_synth
	// tick (cron + RunMorningNow) so concerns track the same daypart as the
	// briefing they may feed.
	return s, nil
}

// WithLocation sets the timezone the underlying cron.Cron will use to
// interpret schedules. Default is time.UTC. Safe to call before Start;
// callers that want to change the zone after Start must use Retarget.
// Passing nil falls back to UTC.
func (s *Scheduler) WithLocation(loc *time.Location) *Scheduler {
	if loc == nil {
		loc = time.UTC
	}
	s.entriesMu.Lock()
	defer s.entriesMu.Unlock()
	s.location = loc
	return s
}

// Location returns the cron's current timezone (the one used to interpret
// the registered schedules). Always non-nil.
func (s *Scheduler) Location() *time.Location {
	s.entriesMu.Lock()
	defer s.entriesMu.Unlock()
	return s.location
}

// Retarget swaps the timezone of the running scheduler. Safe to call at
// any time, including before Start (in which case it just records the new
// location). When the scheduler is running, Retarget:
//
//  1. Stops the existing cron.Cron and waits for any in-flight jobs to
//     finish (those jobs use atomic single-flight guards independent of
//     the entriesMu mutex, so this can't deadlock).
//  2. Builds a new cron.Cron with the new location and re-registers every
//     entry validated at New time.
//  3. Starts the new cron so future ticks fire at the new local times.
//
// If the requested location matches the current one (string match), this
// is a no-op. Passing nil falls back to UTC.
func (s *Scheduler) Retarget(loc *time.Location) {
	if loc == nil {
		loc = time.UTC
	}
	s.entriesMu.Lock()
	defer s.entriesMu.Unlock()
	if s.location != nil && s.location.String() == loc.String() {
		return
	}
	wasStarted := s.started
	if s.cron != nil && s.started {
		stopCtx := s.cron.Stop()
		s.started = false
		// In-flight cron callbacks (runSyncAll/runMorningSynth/...) coordinate
		// via per-job atomic.Bool guards and never acquire entriesMu, so
		// blocking on Done while holding the lock cannot deadlock with them.
		<-stopCtx.Done()
	}
	if s.log != nil {
		s.log.WithFields(logrus.Fields{"from": s.location.String(), "to": loc.String()}).
			Info("scheduler retargeting timezone")
	}
	s.location = loc
	s.cron = s.buildCronLocked()
	if wasStarted {
		s.cron.Start()
		s.started = true
	}
}

// buildCronLocked constructs a fresh cron.Cron with the current location
// and registers every entry. Caller must hold entriesMu.
func (s *Scheduler) buildCronLocked() *cron.Cron {
	c := cron.New(
		cron.WithParser(s.parser),
		cron.WithLogger(cron.DiscardLogger),
		cron.WithLocation(s.location),
	)
	for _, e := range s.entries {
		// Spec was validated in New; AddFunc will not return an error here.
		_, _ = c.AddFunc(e.spec, e.fn)
	}
	return c
}

// WithPerSensorTimeout overrides the per-sensor timeout.
func (s *Scheduler) WithPerSensorTimeout(d time.Duration) *Scheduler {
	s.perSensorTimeout = d
	return s
}

// WithEventLog wires an event-log writer so the scheduler can emit
// `sync.completed` events after each scheduled sync wave. Used by the
// /api/health handler to surface `last_sync_at`.
func (s *Scheduler) WithEventLog(w zlog.Writer) *Scheduler {
	s.eventLog = w
	return s
}

// WithMorningBudget sets the wall-clock ceiling on one morning_synth tick.
// The Runner subdivides this between cards and briefing internally; the
// scheduler treats it as the hard SIGTERM-or-cancel threshold.
func (s *Scheduler) WithMorningBudget(d time.Duration) *Scheduler {
	s.morningBudget = d
	return s
}

// WithInject attaches the inject orchestrator used by the manual
// /api/synth/now?kind=inject path (RunInjectNow / RunInjectNowWithSignal)
// and by the reactive subscriber, which calls RunInjectNow under the
// shared injectRunning single-flight guard — see cmd/zeno/inject_subscriber.go.
func (s *Scheduler) WithInject(fn InjectFunc) *Scheduler {
	s.injectFn = fn
	return s
}

// WithBus wires the V2.4 typed eventbus into the scheduler. When set,
// SyncAll attaches an EventPublisher to each per-sensor sync context so
// sensors can call sensor.PublishObserved after a successful log append.
// Nil bus is a no-op (the helper already short-circuits when the publisher
// is absent from ctx).
func (s *Scheduler) WithBus(bus *eventbus.Bus) *Scheduler {
	s.bus = bus
	return s
}

// WithInjectBudget sets the wall-clock ceiling on one reactive inject pass.
// 0 falls back to 60s. Inject is single-card and should normally land
// well under that.
func (s *Scheduler) WithInjectBudget(d time.Duration) *Scheduler {
	s.injectBudget = d
	return s
}

// Start begins the cron loop; non-blocking. Idempotent — calling Start on
// an already-running scheduler is a no-op. Builds the underlying cron.Cron
// the first time it is called using the configured WithLocation (default UTC).
func (s *Scheduler) Start() {
	s.entriesMu.Lock()
	defer s.entriesMu.Unlock()
	if s.started {
		return
	}
	if s.cron == nil {
		s.cron = s.buildCronLocked()
	}
	s.cron.Start()
	s.started = true
}

// Stop halts the cron loop; returns a context that closes once in-flight
// jobs finish. Idempotent: a second call returns an already-closed context.
func (s *Scheduler) Stop() context.Context {
	s.entriesMu.Lock()
	defer s.entriesMu.Unlock()
	if !s.started || s.cron == nil {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}
	ctx := s.cron.Stop()
	s.started = false
	return ctx
}

// SyncAll runs every sensor concurrently with per-sensor timeouts. One
// sensor's failure never aborts another.
//
// V2.4: when WithBus has attached a non-nil bus, each per-sensor context
// carries an EventPublisher (via sensor.ContextWithPublisher) so sensors
// can call sensor.PublishObserved after every successful log append. The
// inject subscriber consumes those observations on the same bus.
func (s *Scheduler) SyncAll(ctx context.Context) []SyncResult {
	results := make([]SyncResult, len(s.sensors))
	var wg sync.WaitGroup
	for i, snr := range s.sensors {
		wg.Add(1)
		go func(idx int, sn sensor.Sensor) {
			defer wg.Done()
			start := time.Now()
			subCtx, cancel := context.WithTimeout(ctx, s.perSensorTimeout)
			defer cancel()
			if s.bus != nil {
				subCtx = sensor.ContextWithPublisher(subCtx, s.bus)
			}
			err := sn.Sync(subCtx)
			dur := time.Since(start)
			results[idx] = SyncResult{
				Name:     sn.Name(),
				OK:       err == nil,
				Duration: dur,
			}
			if err != nil {
				results[idx].Err = err.Error()
				if s.log != nil {
					s.log.WithError(err).WithField("sensor", sn.Name()).Warn("sensor sync failed")
				}
			}
			if s.onSensor != nil {
				outcome := "ok"
				if err != nil {
					outcome = "error"
				}
				s.onSensor(sn.Name(), outcome, dur, nil)
			}
		}(i, snr)
	}
	wg.Wait()
	return results
}

func (s *Scheduler) runSyncAll() {
	if !s.syncRunning.CompareAndSwap(false, true) {
		if s.log != nil {
			s.log.Warn("sync_all skipped: prior run still in flight")
		}
		if s.onCron != nil {
			s.onCron("sync_all", "skipped")
		}
		return
	}
	defer s.syncRunning.Store(false)

	if s.log != nil {
		s.log.Debug("sync_all firing")
	}
	results := s.SyncAll(context.Background())

	// Emit sync.completed so /api/health can surface last_sync_at. Skipped
	// when no event log is wired (tests and replay) and when zero sensors
	// ran (an empty wave shouldn't move the "last sync" needle).
	if s.eventLog != nil && len(results) > 0 {
		ok, fail := 0, 0
		for _, r := range results {
			if r.OK {
				ok++
			} else {
				fail++
			}
		}
		if _, err := s.eventLog.Append(context.Background(), zlog.KindSyncCompleted, "scheduler", map[string]any{
			"count":      len(results),
			"ok_count":   ok,
			"fail_count": fail,
		}); err != nil && s.log != nil {
			s.log.WithError(err).Warn("append sync.completed failed")
		}
	}

	if s.onCron != nil {
		outcome := "ok"
		for _, r := range results {
			if !r.OK {
				outcome = "error"
				break
			}
		}
		s.onCron("sync_all", outcome)
	}
}

// RunMorningNow runs the morning synth on demand using the same single-flight
// guard as the cron path. Returns ErrMorningInFlight if a prior run is still
// active and ErrNoMorningSynth if the scheduler has no synth function wired.
// The caller's ctx applies; the cron's morningBudget does NOT apply here
// (callers are expected to set their own deadline).
func (s *Scheduler) RunMorningNow(ctx context.Context) error {
	if s.morningSynth == nil {
		return ErrNoMorningSynth
	}
	if !s.morningRunning.CompareAndSwap(false, true) {
		return ErrMorningInFlight
	}
	defer s.morningRunning.Store(false)
	ctx = llm.ContextWithServiceTier(ctx, s.serviceTier)
	err := s.morningSynth(ctx)
	// V2.5 post-launch: chain recognition off the manual trigger so a UI
	// force-refresh re-runs concern recognition with fresh observations.
	// Async — recognition uses its own ctx (independent of the API caller's
	// ctx, which is cancelled when the response is sent) and its own
	// recognitionBudget. Synth's err is returned to the caller unchanged;
	// recognition runs regardless of synth outcome since it reads the
	// observation log directly.
	if s.recognitionFn != nil {
		go s.runRecognition()
	}
	return err
}

// RunInjectNow runs the V2.3.0 P3 inject pipeline on demand under the
// same single-flight guard as the inject cron path. The injectFn is
// invoked with signal=nil so the orchestrator runs sensor.Detect itself.
//
// Returns ErrInjectInFlight on concurrent call, ErrNoInjectFunc if no
// fn was registered via WithInject.
func (s *Scheduler) RunInjectNow(ctx context.Context) error {
	return s.runInjectWithSignal(ctx, nil)
}

// RunInjectNowWithSignal is the manual debug variant: it bypasses
// sensor.Detect by supplying a pre-built signal (typed by the caller as
// *synth.InjectSignal — the schedule package keeps it `any` to avoid
// the import cycle). The orchestrator type-asserts and invokes
// synth.SynthesizeInject directly.
//
// Returns the same error set as RunInjectNow.
func (s *Scheduler) RunInjectNowWithSignal(ctx context.Context, signal any) error {
	return s.runInjectWithSignal(ctx, signal)
}

func (s *Scheduler) runInjectWithSignal(ctx context.Context, signal any) error {
	if s.injectFn == nil {
		return ErrNoInjectFunc
	}
	if !s.injectRunning.CompareAndSwap(false, true) {
		return ErrInjectInFlight
	}
	defer s.injectRunning.Store(false)
	ctx = llm.ContextWithServiceTier(ctx, s.serviceTier)
	return s.injectFn(ctx, signal)
}

func (s *Scheduler) runMorningSynth() {
	if !s.morningRunning.CompareAndSwap(false, true) {
		if s.log != nil {
			s.log.Warn("morning_synth skipped: prior run still in flight")
		}
		if s.onCron != nil {
			s.onCron("morning_synth", "skipped")
		}
		return
	}
	defer s.morningRunning.Store(false)

	if s.log != nil {
		s.log.Info("morning_synth fired")
	}
	if s.morningSynth == nil {
		return
	}
	budget := s.morningBudget
	if budget <= 0 {
		budget = 90 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	ctx = llm.ContextWithServiceTier(ctx, s.serviceTier)
	err := s.morningSynth(ctx)
	if err != nil && s.log != nil {
		s.log.WithError(err).Error("morning_synth failed")
	}
	if s.onCron != nil {
		outcome := "ok"
		if err != nil {
			outcome = "error"
		}
		s.onCron("morning_synth", outcome)
	}
	// V2.5 post-launch: chain recognition after every morning_synth tick.
	// Async so the cron's morningBudget isn't extended; recognition has
	// its own recognitionBudget + recognitionRunning single-flight.
	if s.recognitionFn != nil {
		go s.runRecognition()
	}
}

// InjectBudget returns the configured wall-clock ceiling for one reactive
// inject pass, or 60 s if WithInjectBudget was never called. The V2.4
// inject subscriber reads this so daemon-level tunables flow through.
func (s *Scheduler) InjectBudget() time.Duration {
	if s.injectBudget <= 0 {
		return 60 * time.Second
	}
	return s.injectBudget
}

// WithCronObserver wires a callback invoked once per cron firing. job ∈
// {"sync_all","morning_synth","recognition"}, outcome ∈ {"ok","error","skipped"}.
func (s *Scheduler) WithCronObserver(fn func(job, outcome string)) *Scheduler {
	s.onCron = fn
	return s
}

// WithSensorObserver wires a callback invoked once per sensor sync.
func (s *Scheduler) WithSensorObserver(fn func(sensor, outcome string, dur time.Duration, records map[string]int)) *Scheduler {
	s.onSensor = fn
	return s
}

// WithRecognition wires the concern recognition pass. Recognition chains
// off every morning_synth tick (cron path + the /api/synth/now manual
// trigger) so concerns track the same daypart as the briefing they may
// feed.
//
// Pass nil to clear; recognition becomes a no-op until rewired. Single-flight
// is enforced via recognitionRunning, so a chain-off-morning trigger
// arriving while a prior recognition is still running is skipped rather
// than queued.
func (s *Scheduler) WithRecognition(fn RecognitionFunc) *Scheduler {
	s.recognitionFn = fn
	return s
}

// WithServiceTier sets the OpenRouter service tier the scheduler will
// attach to every background ctx it creates (morning synth, recognition,
// inject, plus the manual *Now variants). Empty string is a no-op so
// callers can pass a config value verbatim. Allowed values are validated
// upstream in config.validateServiceTier; passing an unknown string here
// silently goes through and OpenRouter will return a 400 at request time.
func (s *Scheduler) WithServiceTier(tier string) *Scheduler {
	s.serviceTier = tier
	return s
}

// WithRecognitionBudget sets the wall-clock ceiling for one recognition
// tick. 0 falls back to 60s — recognition is one LLM call plus
// bookkeeping; 60s is generous for a 35B local model on a normal-length
// observation log.
func (s *Scheduler) WithRecognitionBudget(d time.Duration) *Scheduler {
	s.recognitionBudget = d
	return s
}

// RunRecognitionNow runs the recognition pass on demand under the same
// single-flight guard as the cron path. The caller's ctx applies; the
// recognitionBudget does NOT (callers should set their own deadline).
//
// Returns ErrRecognitionInFlight on concurrent call, ErrNoRecognitionFunc
// when the scheduler has no fn wired.
func (s *Scheduler) RunRecognitionNow(ctx context.Context) error {
	if s.recognitionFn == nil {
		return ErrNoRecognitionFunc
	}
	if !s.recognitionRunning.CompareAndSwap(false, true) {
		return ErrRecognitionInFlight
	}
	defer s.recognitionRunning.Store(false)
	ctx = llm.ContextWithServiceTier(ctx, s.serviceTier)
	return s.recognitionFn(ctx)
}

func (s *Scheduler) runRecognition() {
	if !s.recognitionRunning.CompareAndSwap(false, true) {
		if s.log != nil {
			s.log.Warn("recognition skipped: prior run still in flight")
		}
		if s.onCron != nil {
			s.onCron("recognition", "skipped")
		}
		return
	}
	defer s.recognitionRunning.Store(false)

	if s.log != nil {
		s.log.Info("recognition fired")
	}
	if s.recognitionFn == nil {
		return
	}
	budget := s.recognitionBudget
	if budget <= 0 {
		budget = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	ctx = llm.ContextWithServiceTier(ctx, s.serviceTier)
	err := s.recognitionFn(ctx)
	if err != nil && s.log != nil {
		s.log.WithError(err).Error("recognition failed")
	}
	if s.onCron != nil {
		outcome := "ok"
		if err != nil {
			outcome = "error"
		}
		s.onCron("recognition", outcome)
	}
}
