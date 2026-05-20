package schedule

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/config"
	"github.com/zenocy/zeno-v2/internal/eventbus"
	"github.com/zenocy/zeno-v2/internal/llm"
	zlog "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/sensor"
)

// captureWriter records every appended event in memory. Used to assert
// sync.completed emission without standing up SQLite.
type captureWriter struct {
	mu     sync.Mutex
	events []zlog.Event
}

func (c *captureWriter) Append(_ context.Context, kind, source string, _ any) (zlog.Event, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := zlog.Event{Kind: kind, Source: source, TS: time.Now().UTC()}
	c.events = append(c.events, e)
	return e, nil
}

func (c *captureWriter) byKind(kind string) []zlog.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []zlog.Event{}
	for _, e := range c.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

type counterSensor struct {
	name  string
	count atomic.Int32
	err   error
	delay time.Duration
}

func (c *counterSensor) Name() string { return c.name }
func (c *counterSensor) Sync(ctx context.Context) error {
	c.count.Add(1)
	if c.delay > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.delay):
		}
	}
	return c.err
}

func quietEntry() *logrus.Entry {
	l := logrus.New()
	l.Out = io.Discard
	return l.WithField("c", "sched-test")
}

func TestScheduler_Fires(t *testing.T) {
	c := &counterSensor{name: "x"}
	s, err := New(config.ScheduleConfig{SyncCron: "@every 1s"}, []sensor.Sensor{c}, nil, quietEntry())
	require.NoError(t, err)
	s.Start()
	defer s.Stop()

	time.Sleep(2200 * time.Millisecond)
	require.GreaterOrEqual(t, int(c.count.Load()), 2)
}

func TestScheduler_StartStop(t *testing.T) {
	c := &counterSensor{name: "x"}
	s, err := New(config.ScheduleConfig{SyncCron: "@every 1s"}, []sensor.Sensor{c}, nil, quietEntry())
	require.NoError(t, err)
	s.Start()
	stopped := s.Stop()
	select {
	case <-stopped.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not stop in time")
	}
}

func TestSyncAll_AllOK(t *testing.T) {
	a := &counterSensor{name: "a"}
	b := &counterSensor{name: "b"}
	s, err := New(config.ScheduleConfig{}, []sensor.Sensor{a, b}, nil, quietEntry())
	require.NoError(t, err)

	results := s.SyncAll(context.Background())
	require.Len(t, results, 2)
	for _, r := range results {
		require.True(t, r.OK)
		require.Empty(t, r.Err)
		require.Greater(t, r.Duration, time.Duration(0))
	}
	require.Equal(t, int32(1), a.count.Load())
	require.Equal(t, int32(1), b.count.Load())
}

func TestSyncAll_OnePassesOneFails(t *testing.T) {
	a := &counterSensor{name: "ok"}
	b := &counterSensor{name: "broken", err: errors.New("boom")}
	s, _ := New(config.ScheduleConfig{}, []sensor.Sensor{a, b}, nil, quietEntry())

	results := s.SyncAll(context.Background())
	byName := map[string]SyncResult{}
	for _, r := range results {
		byName[r.Name] = r
	}
	require.True(t, byName["ok"].OK)
	require.False(t, byName["broken"].OK)
	require.Contains(t, byName["broken"].Err, "boom")
}

func TestSyncAll_PerSensorTimeout(t *testing.T) {
	slow := &counterSensor{name: "slow", delay: 2 * time.Second}
	s, _ := New(config.ScheduleConfig{}, []sensor.Sensor{slow}, nil, quietEntry())
	s.WithPerSensorTimeout(200 * time.Millisecond)

	start := time.Now()
	results := s.SyncAll(context.Background())
	require.Less(t, time.Since(start), time.Second, "must not wait full delay")
	require.Len(t, results, 1)
	require.False(t, results[0].OK)
	require.Contains(t, results[0].Err, "deadline")
}

func TestSyncAll_RespectsCallerCtx(t *testing.T) {
	slow := &counterSensor{name: "slow", delay: 2 * time.Second}
	s, _ := New(config.ScheduleConfig{}, []sensor.Sensor{slow}, nil, quietEntry())

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	results := s.SyncAll(ctx)
	require.Len(t, results, 1)
	require.False(t, results[0].OK)
}

func TestScheduler_BadCronExpression(t *testing.T) {
	_, err := New(config.ScheduleConfig{SyncCron: "not-a-cron"}, nil, nil, quietEntry())
	require.Error(t, err)
}

func TestScheduler_MorningSynthIsNoOp(t *testing.T) {
	a := &counterSensor{name: "a"}
	buf := &safeBuffer{}
	logger := logrus.New()
	logger.Out = buf
	logger.Level = logrus.InfoLevel

	s, err := New(config.ScheduleConfig{
		MorningCron: "@every 1s",
	}, []sensor.Sensor{a}, nil, logger.WithField("c", "sched"))
	require.NoError(t, err)
	s.Start()
	defer s.Stop()

	time.Sleep(1300 * time.Millisecond)
	require.Equal(t, int32(0), a.count.Load(), "morning_synth must not invoke sensors")
	require.Contains(t, buf.String(), "morning_synth fired")
}

func TestSyncAll_ConcurrentInvocations(t *testing.T) {
	a := &counterSensor{name: "a", delay: 50 * time.Millisecond}
	s, _ := New(config.ScheduleConfig{}, []sensor.Sensor{a}, nil, quietEntry())

	done := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func() {
			s.SyncAll(context.Background())
			done <- struct{}{}
		}()
	}
	<-done
	<-done
	require.Equal(t, int32(2), a.count.Load(), "sensor invoked once per concurrent SyncAll")
}

func TestScheduler_SyncAll_SkipsOverlappingTicks(t *testing.T) {
	// robfig/cron's @every parser rounds to ~1s resolution, so the test cycle
	// uses 1s ticks with a sensor that takes 1.5s per Sync. Over ~3.3s of
	// wall time we expect ~3 ticks; the second tick fires while the first is
	// still in flight and must be skipped.
	slow := &counterSensor{name: "slow", delay: 1500 * time.Millisecond}
	buf := &safeBuffer{}
	logger := logrus.New()
	logger.Out = buf
	logger.Level = logrus.WarnLevel

	s, err := New(config.ScheduleConfig{SyncCron: "@every 1s"}, []sensor.Sensor{slow}, nil, logger.WithField("c", "sched-test"))
	require.NoError(t, err)
	s.Start()
	defer s.Stop()

	time.Sleep(3300 * time.Millisecond)

	got := slow.count.Load()
	require.GreaterOrEqual(t, int(got), 1, "at least one tick must have run")
	require.LessOrEqual(t, int(got), 3, "overlapping ticks must be skipped, got %d runs", got)
	require.Contains(t, buf.String(), "sync_all skipped",
		"a skipped tick must log a warning")
}

func TestScheduler_EmitsSyncCompletedAfterCronTick(t *testing.T) {
	a := &counterSensor{name: "a"}
	cap := &captureWriter{}
	s, err := New(config.ScheduleConfig{SyncCron: "@every 1s"}, []sensor.Sensor{a}, nil, quietEntry())
	require.NoError(t, err)
	s.WithEventLog(cap)
	s.Start()
	defer s.Stop()

	time.Sleep(2200 * time.Millisecond)

	got := cap.byKind(zlog.KindSyncCompleted)
	require.GreaterOrEqual(t, len(got), 1, "scheduled sync_all must emit sync.completed")
	for _, e := range got {
		require.Equal(t, "scheduler", e.Source)
	}
}

func TestScheduler_NoEventLog_NoSyncCompletedEmitted(t *testing.T) {
	// Without WithEventLog wired, the scheduler must run sensors but skip the
	// emit cleanly. (Tests / replay rely on this.)
	a := &counterSensor{name: "a"}
	s, err := New(config.ScheduleConfig{SyncCron: "@every 1s"}, []sensor.Sensor{a}, nil, quietEntry())
	require.NoError(t, err)
	s.Start()
	defer s.Stop()

	time.Sleep(1200 * time.Millisecond)
	require.GreaterOrEqual(t, int(a.count.Load()), 1, "sensor should still run")
}

// TestScheduler_RunRecognitionNow_SingleFlight pins V2.5.0 wiring: the
// scheduler enforces single-flight on recognition the same way it does
// for morning synth. A second concurrent call returns
// ErrRecognitionInFlight, and an unwired call returns ErrNoRecognitionFunc.
func TestScheduler_RunRecognitionNow_SingleFlight(t *testing.T) {
	s, err := New(config.ScheduleConfig{}, nil, nil, quietEntry())
	require.NoError(t, err)

	// No fn wired → ErrNoRecognitionFunc.
	require.ErrorIs(t, s.RunRecognitionNow(context.Background()), ErrNoRecognitionFunc)

	// Wire a slow recognition fn and verify single-flight rejection.
	gate := make(chan struct{})
	released := make(chan struct{})
	s.WithRecognition(func(ctx context.Context) error {
		<-gate
		<-released
		return nil
	})

	go func() {
		_ = s.RunRecognitionNow(context.Background())
	}()

	gate <- struct{}{} // release first call into its body
	// Give the first call time to set the flag.
	time.Sleep(50 * time.Millisecond)
	require.ErrorIs(t, s.RunRecognitionNow(context.Background()), ErrRecognitionInFlight)
	close(released)
}

// V2.5 post-launch: recognition no longer has its own cron. It chains off
// every morning_synth tick (the cron path here) and off RunMorningNow (the
// manual /api/synth/now path; covered separately below). Async — the
// scheduler kicks recognition in a goroutine after morning_synth completes
// so the morning budget isn't extended.
func TestScheduler_Recognition_FiresAfterMorningCron(t *testing.T) {
	var morningCalls, recogCalls atomic.Int32
	morningFn := func(ctx context.Context) error {
		morningCalls.Add(1)
		return nil
	}
	recogFn := func(ctx context.Context) error {
		recogCalls.Add(1)
		return nil
	}

	s, err := New(config.ScheduleConfig{MorningCron: "@every 1s"}, nil, morningFn, quietEntry())
	require.NoError(t, err)
	s.WithRecognition(recogFn)
	s.Start()
	defer s.Stop()

	require.Eventually(t, func() bool {
		return morningCalls.Load() >= 1 && recogCalls.Load() >= 1
	}, 3*time.Second, 50*time.Millisecond,
		"morning_synth must fire and recognition must chain off it; got morning=%d recog=%d",
		morningCalls.Load(), recogCalls.Load())
}

// TestScheduler_ServiceTier_PropagatesToCtx pins that WithServiceTier
// causes both the manual *Now triggers and the cron-fired tick handlers
// to stamp the configured tier onto the ctx they hand to user-supplied
// fns. The LLM client downstream reads ServiceTierFromContext, so this
// is the ground-truth integration point.
func TestScheduler_ServiceTier_PropagatesToCtx(t *testing.T) {
	var (
		morningProfile, recognitionProfile, injectProfile llm.CallProfile
		mu                                                sync.Mutex
	)
	capture := func(into *llm.CallProfile) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			mu.Lock()
			*into = llm.CallProfileFromContext(ctx)
			mu.Unlock()
			return nil
		}
	}

	s, err := New(config.ScheduleConfig{}, nil, capture(&morningProfile), quietEntry())
	require.NoError(t, err)
	s.WithRecognition(capture(&recognitionProfile)).
		WithInject(func(ctx context.Context, _ any) error {
			mu.Lock()
			injectProfile = llm.CallProfileFromContext(ctx)
			mu.Unlock()
			return nil
		})

	require.NoError(t, s.RunMorningNow(context.Background()))
	require.NoError(t, s.RunRecognitionNow(context.Background()))
	require.NoError(t, s.RunInjectNow(context.Background()))

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, llm.CallProfileBackground, morningProfile, "RunMorningNow must stamp the background profile")
	require.Equal(t, llm.CallProfileBackground, recognitionProfile, "RunRecognitionNow must stamp the background profile")
	require.Equal(t, llm.CallProfileBackground, injectProfile, "RunInjectNow must stamp the background profile")
}

// TestScheduler_ServiceTier_PropagatesOnCronTick pins that the
// cron-fired (not manual) entry points also stamp the configured tier.
// The cron path runs in its own goroutine with a fresh
// `context.Background()` — easy to regress by forgetting one of the
// tick handlers since they don't share a wrapper. Each handler is
// covered with its own assertion below.
func TestScheduler_ServiceTier_PropagatesOnCronTick(t *testing.T) {
	var (
		morningProfile, recognitionProfile llm.CallProfile
		mu                                 sync.Mutex
		morningSeen, recogSeen             = make(chan struct{}, 1), make(chan struct{}, 1)
	)
	morningFn := func(ctx context.Context) error {
		mu.Lock()
		morningProfile = llm.CallProfileFromContext(ctx)
		mu.Unlock()
		select {
		case morningSeen <- struct{}{}:
		default:
		}
		return nil
	}
	recogFn := func(ctx context.Context) error {
		mu.Lock()
		recognitionProfile = llm.CallProfileFromContext(ctx)
		mu.Unlock()
		select {
		case recogSeen <- struct{}{}:
		default:
		}
		return nil
	}

	// MorningCron @every 1s drives runMorningSynth, which chains
	// runRecognition off itself — so a single cron tick exercises both
	// non-manual tick handlers in one shot.
	s, err := New(config.ScheduleConfig{MorningCron: "@every 1s"}, nil, morningFn, quietEntry())
	require.NoError(t, err)
	s.WithRecognition(recogFn)
	s.Start()
	defer s.Stop()

	select {
	case <-morningSeen:
	case <-time.After(3 * time.Second):
		t.Fatal("cron-fired morning_synth did not fire within 3s")
	}
	select {
	case <-recogSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("cron-chained recognition did not fire within 2s")
	}

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, llm.CallProfileBackground, morningProfile,
		"runMorningSynth (cron path) must stamp the background profile")
	require.Equal(t, llm.CallProfileBackground, recognitionProfile,
		"runRecognition (chained off cron) must stamp the background profile")
}

// V2.5 post-launch: RunMorningNow (UI force-refresh) chains recognition
// the same way the cron does. Recognition is async — wait for it.
func TestScheduler_Recognition_FiresAfterRunMorningNow(t *testing.T) {
	var recogCalls atomic.Int32
	morningFn := func(ctx context.Context) error { return nil }
	recogFn := func(ctx context.Context) error {
		recogCalls.Add(1)
		return nil
	}

	s, err := New(config.ScheduleConfig{}, nil, morningFn, quietEntry())
	require.NoError(t, err)
	s.WithRecognition(recogFn)

	require.NoError(t, s.RunMorningNow(context.Background()))
	require.Eventually(t, func() bool { return recogCalls.Load() >= 1 },
		time.Second, 20*time.Millisecond,
		"recognition must fire asynchronously after RunMorningNow")
}

func TestScheduler_MorningSynth_SkipsOverlappingTicks(t *testing.T) {
	var entered atomic.Int32
	slow := func(ctx context.Context) error {
		entered.Add(1)
		select {
		case <-ctx.Done():
		case <-time.After(1500 * time.Millisecond):
		}
		return nil
	}

	buf := &safeBuffer{}
	logger := logrus.New()
	logger.Out = buf
	logger.Level = logrus.WarnLevel

	s, err := New(config.ScheduleConfig{MorningCron: "@every 1s"}, nil, slow, logger.WithField("c", "sched-test"))
	require.NoError(t, err)
	s.Start()
	defer s.Stop()

	time.Sleep(3300 * time.Millisecond)

	got := entered.Load()
	require.GreaterOrEqual(t, int(got), 1)
	require.LessOrEqual(t, int(got), 3, "overlapping morning_synth ticks must be skipped, got %d", got)
	require.Contains(t, buf.String(), "morning_synth skipped")
}

// V2.3.x: RefreshCron fires the same morningSynth fn at additional times
// of day so the briefing/state register tracks the user's daypart instead
// of remaining frozen on the 07:00 morning_calm computation.
func TestScheduler_RefreshCronFires(t *testing.T) {
	var ticks atomic.Int32
	fn := func(ctx context.Context) error {
		ticks.Add(1)
		return nil
	}

	// MorningCron empty so only RefreshCron is registered — proves the new
	// entry alone fires runMorningSynth.
	s, err := New(config.ScheduleConfig{RefreshCron: "@every 1s"}, nil, fn, quietEntry())
	require.NoError(t, err)
	s.Start()
	defer s.Stop()

	time.Sleep(2200 * time.Millisecond)
	require.GreaterOrEqual(t, int(ticks.Load()), 1, "refresh_cron must fire at least once in 2s")
}

// V2.3.0 P3: RunInjectNow mirrors RunMorningNow's single-flight pattern.
// ErrNoInjectFunc when no fn registered; ErrInjectInFlight on overlap.
func TestScheduler_RunInjectNow_Errors(t *testing.T) {
	s, err := New(config.ScheduleConfig{}, nil, nil, quietEntry())
	require.NoError(t, err)

	require.ErrorIs(t, s.RunInjectNow(context.Background()), ErrNoInjectFunc,
		"RunInjectNow with no fn registered must return ErrNoInjectFunc")

	// Register a slow fn and confirm overlap returns ErrInjectInFlight.
	started := make(chan struct{})
	release := make(chan struct{})
	s.WithInject(func(ctx context.Context, signal any) error {
		close(started)
		<-release
		return nil
	})

	go func() { _ = s.RunInjectNow(context.Background()) }()
	<-started // first call is in-flight

	require.ErrorIs(t, s.RunInjectNow(context.Background()), ErrInjectInFlight,
		"overlapping RunInjectNow must return ErrInjectInFlight")
	close(release)
}

// V2.4: SyncAll attaches an EventPublisher to each per-sensor sync
// context when WithBus is set, so sensors can call sensor.PublishObserved
// inside Sync.
func TestScheduler_SyncAll_AttachesPublisherToSensorContext(t *testing.T) {
	captured := &capturingSensor{}
	s, err := New(config.ScheduleConfig{}, []sensor.Sensor{captured}, nil, quietEntry())
	require.NoError(t, err)

	bus := eventbus.New(quietEntry())
	s.WithBus(bus)

	results := s.SyncAll(context.Background())
	require.Len(t, results, 1)
	require.True(t, results[0].OK)

	require.NotNil(t, captured.pub, "publisher must be present in sensor ctx when WithBus is set")
	require.Same(t, bus, captured.pub, "publisher attached to ctx is the bus passed to WithBus")
}

// V2.4: SyncAll is a no-op for publisher attachment when WithBus was not
// called — pins backward compat for tests/replay paths that don't wire a
// bus.
func TestScheduler_SyncAll_NilBus_PublisherAbsentFromCtx(t *testing.T) {
	captured := &capturingSensor{}
	s, err := New(config.ScheduleConfig{}, []sensor.Sensor{captured}, nil, quietEntry())
	require.NoError(t, err)

	results := s.SyncAll(context.Background())
	require.Len(t, results, 1)
	require.True(t, results[0].OK)

	require.Nil(t, captured.pub, "no WithBus → no publisher attached → ctx returns nil")
}

// capturingSensor extracts the publisher from its Sync ctx for V2.4
// SyncAll tests above.
type capturingSensor struct {
	pub sensor.EventPublisher
}

func (c *capturingSensor) Name() string { return "capturing" }
func (c *capturingSensor) Sync(ctx context.Context) error {
	c.pub = sensor.PublisherFromContext(ctx)
	return nil
}

// mustLoadTZ resolves a zone name and fails the test on error.
func mustLoadTZ(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	require.NoError(t, err)
	return loc
}

// nextEntryAt returns the firing time of the first cron entry, simulating
// the wall clock at `after`. The scheduler library's run loop calls
// Schedule.Next(time.Now().In(c.location)); SpecSchedule.Next inherits the
// input time's zone when its own Location is time.Local (the parser default),
// so this helper mirrors that behavior by converting `after` into the
// scheduler's configured location before asking for the next firing.
func nextEntryAt(t *testing.T, s *Scheduler, after time.Time) time.Time {
	t.Helper()
	s.entriesMu.Lock()
	defer s.entriesMu.Unlock()
	require.NotNil(t, s.cron, "cron must be built (call Start before this)")
	entries := s.cron.Entries()
	require.NotEmpty(t, entries, "no entries registered")
	return entries[0].Schedule.Next(after.In(s.location))
}

// WithLocation must propagate to cron entry scheduling so a "0 7 * * *"
// expression fires at 07:00 in the configured zone, not 07:00 UTC.
func TestScheduler_WithLocation_FiringTimeIsLocal(t *testing.T) {
	la := mustLoadTZ(t, "America/Los_Angeles")
	s, err := New(config.ScheduleConfig{MorningCron: "0 7 * * *"}, nil, nil, quietEntry())
	require.NoError(t, err)
	s.WithLocation(la)
	s.Start()
	defer s.Stop()

	// At 2026-04-25 06:30 PST the next 07:00 local must be 07:00 PST,
	// which is 14:00 UTC.
	probe := time.Date(2026, 4, 25, 6, 30, 0, 0, la)
	next := nextEntryAt(t, s, probe)

	require.Equal(t, 7, next.In(la).Hour(), "must fire at 07:00 in LA, not UTC")
	require.Equal(t, 14, next.UTC().Hour(), "07:00 PST == 14:00 UTC")
}

// Spring-forward (LA, 2026-03-08): a "0 2 * * *" cron in local time hits
// a non-existent wall clock — robfig/cron must skip past 02:00 PST
// without double-firing or panicking.
func TestScheduler_WithLocation_SpringForwardSkipsNonexistentLocal(t *testing.T) {
	la := mustLoadTZ(t, "America/Los_Angeles")
	s, err := New(config.ScheduleConfig{MorningCron: "0 2 * * *"}, nil, nil, quietEntry())
	require.NoError(t, err)
	s.WithLocation(la)
	s.Start()
	defer s.Stop()

	// 2026-03-07 23:00 PST — the next 02:00 local should land on 03/09 since
	// 03/08 02:00 doesn't exist (clock jumps to 03:00). robfig/cron's
	// behavior for non-existent-local schedules is to skip the day.
	probe := time.Date(2026, 3, 7, 23, 0, 0, 0, la)
	next := nextEntryAt(t, s, probe)
	require.True(t, next.Day() == 9 && next.Month() == 3,
		"next 02:00 local after spring-forward must land on 2026-03-09, got %s", next.In(la))
}

// Fall-back (LA, 2026-11-01): 02:00 → 01:00, so 01:30 happens twice.
// A "30 1 * * *" cron should fire exactly once on 11-01 — the cron lib
// uses the second occurrence (PST), not the first (PDT). We assert the
// next firing after midnight lands the same day rather than skipping.
func TestScheduler_WithLocation_FallBackFiresOnce(t *testing.T) {
	la := mustLoadTZ(t, "America/Los_Angeles")
	s, err := New(config.ScheduleConfig{MorningCron: "30 1 * * *"}, nil, nil, quietEntry())
	require.NoError(t, err)
	s.WithLocation(la)
	s.Start()
	defer s.Stop()

	// 2026-11-01 00:00 PDT — the next 01:30 must still land on 11-01.
	probe := time.Date(2026, 11, 1, 0, 0, 0, 0, la)
	next := nextEntryAt(t, s, probe)
	local := next.In(la)
	require.Equal(t, time.November, local.Month())
	require.Equal(t, 1, local.Day(), "fall-back day must still tick")
	require.Equal(t, 1, local.Hour())
	require.Equal(t, 30, local.Minute())
}

// Retarget swaps the active timezone of a running scheduler. The next
// firing must reflect the new zone immediately.
func TestScheduler_Retarget_ChangesFiringTimezone(t *testing.T) {
	la := mustLoadTZ(t, "America/Los_Angeles")
	athens := mustLoadTZ(t, "Europe/Athens")

	s, err := New(config.ScheduleConfig{MorningCron: "0 7 * * *"}, nil, nil, quietEntry())
	require.NoError(t, err)
	s.WithLocation(la)
	s.Start()
	defer s.Stop()

	probe := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	beforeNext := nextEntryAt(t, s, probe)
	require.Equal(t, 7, beforeNext.In(la).Hour())
	require.NotEqual(t, 7, beforeNext.In(athens).Hour(),
		"sanity: 07:00 LA is NOT 07:00 Athens")

	s.Retarget(athens)

	afterNext := nextEntryAt(t, s, probe)
	require.Equal(t, 7, afterNext.In(athens).Hour(), "after Retarget, 07:00 must mean Athens local")
	require.NotEqual(t, beforeNext.UTC(), afterNext.UTC(), "firing instant must shift")
	require.Equal(t, athens.String(), s.Location().String())
}

// Retarget is a no-op when the requested location matches the current one.
// We validate this by asserting the underlying cron pointer doesn't change.
func TestScheduler_Retarget_SameLocationIsNoop(t *testing.T) {
	la := mustLoadTZ(t, "America/Los_Angeles")
	s, err := New(config.ScheduleConfig{MorningCron: "0 7 * * *"}, nil, nil, quietEntry())
	require.NoError(t, err)
	s.WithLocation(la)
	s.Start()
	defer s.Stop()

	s.entriesMu.Lock()
	before := s.cron
	s.entriesMu.Unlock()

	s.Retarget(la)

	s.entriesMu.Lock()
	after := s.cron
	s.entriesMu.Unlock()

	require.Same(t, before, after, "same-location Retarget must not rebuild the cron")
}

// Retarget called before Start records the location without building cron.
// Subsequent Start uses the stored location.
func TestScheduler_Retarget_BeforeStart(t *testing.T) {
	athens := mustLoadTZ(t, "Europe/Athens")

	s, err := New(config.ScheduleConfig{MorningCron: "0 7 * * *"}, nil, nil, quietEntry())
	require.NoError(t, err)
	s.Retarget(athens)
	require.Equal(t, athens.String(), s.Location().String())

	s.Start()
	defer s.Stop()
	probe := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	next := nextEntryAt(t, s, probe)
	require.Equal(t, 7, next.In(athens).Hour())
}

// Retarget waits for any in-flight cron callback to finish before swapping
// the underlying cron.Cron — i.e. it does not race with a job currently
// executing on the cron's goroutine.
func TestScheduler_Retarget_WaitsForInFlightJob(t *testing.T) {
	la := mustLoadTZ(t, "America/Los_Angeles")
	athens := mustLoadTZ(t, "Europe/Athens")

	jobStarted := make(chan struct{})
	jobBlock := make(chan struct{})
	jobDone := make(chan struct{})

	morningSynth := func(_ context.Context) error {
		close(jobStarted)
		<-jobBlock
		close(jobDone)
		return nil
	}
	s, err := New(config.ScheduleConfig{MorningCron: "@every 100ms"}, nil, morningSynth, quietEntry())
	require.NoError(t, err)
	s.WithLocation(la)
	s.Start()
	defer s.Stop()

	select {
	case <-jobStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("morning_synth never fired")
	}

	retargetReturned := make(chan struct{})
	go func() {
		s.Retarget(athens)
		close(retargetReturned)
	}()

	// Retarget must NOT return while the job is blocked.
	select {
	case <-retargetReturned:
		t.Fatal("Retarget returned before in-flight job finished")
	case <-time.After(150 * time.Millisecond):
	}

	close(jobBlock)
	select {
	case <-retargetReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("Retarget did not finish after job released")
	}
	<-jobDone
	require.Equal(t, athens.String(), s.Location().String())
}

// Retarget on a never-started scheduler swaps location without panicking
// and without leaving cron in a partially-built state.
func TestScheduler_Retarget_BeforeStart_NoEntries(t *testing.T) {
	athens := mustLoadTZ(t, "Europe/Athens")
	s, err := New(config.ScheduleConfig{}, nil, nil, quietEntry())
	require.NoError(t, err)
	s.Retarget(athens)
	require.Equal(t, athens.String(), s.Location().String())
}
