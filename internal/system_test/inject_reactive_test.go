package system_test

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/eventbus"
	"github.com/zenocy/zeno-v2/internal/injectsub"
	"github.com/zenocy/zeno-v2/internal/store"
)

// startSubscriber spins up the injectsub.Run goroutine against the
// harness's scheduler+bus. Returns a teardown that cancels and waits for
// drain — every test must defer it before asserting bus.SubscriberCount().
func startSubscriber(t *testing.T, h *Harness) func() {
	t.Helper()
	require.NotNil(t, h.Bus, "harness must have a bus wired before starting subscriber")

	h.Scheduler.WithBus(h.Bus)

	subCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		injectsub.Run(subCtx, injectsub.Deps{
			Bus:    h.Bus,
			Runner: h.Scheduler,
			Budget: 5 * time.Second,
			Logger: logrus.NewEntry(logrus.New()),
		})
		close(done)
	}()
	require.Eventually(t, func() bool { return h.Bus.SubscriberCount() >= 1 }, time.Second, 10*time.Millisecond,
		"subscriber must subscribe to bus")

	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("subscriber did not drain after cancel")
		}
	}
}

// TestSpine_ReactiveInject_VIPEmailTriggersCardViaSSE is the load-bearing
// system test for Phase 4. It exercises the full path:
//
//	bus.Publish(SensorEventObservedEvent)
//	  → runInjectSubscriber consumes
//	  → Scheduler.RunInjectNow
//	  → stub injectFn publishes card
//	  → SSE client receives `card.appended`
//
// No real LLM or detector — the stub injectFn unconditionally publishes,
// matching the existing V2.3 system-test pattern. The detector's
// deny-by-default contract is covered by the unit-level soak test
// (internal/sensor/inject_detector_soak_test.go) which is much faster
// to drive across many fixtures.
func TestSpine_ReactiveInject_VIPEmailTriggersCardViaSSE(t *testing.T) {
	tz, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 30, 10, 0, 0, 0, tz)

	bus := eventbus.New(logrus.NewEntry(logrus.New()))
	card := store.Card{
		ID:     "vip-saru-1",
		Date:   "2026-04-30",
		Title:  "Saru Patel — series B redline",
		Sub:    "Reply needed by 11:00.",
		Origin: "inject",
	}
	var injectCalls atomic.Int32
	stubInject := func(ctx context.Context, signal any) error {
		injectCalls.Add(1)
		bus.PublishCard(card)
		return nil
	}

	h := NewHarness(t, HarnessConfig{
		TZ:         tz,
		Now:        func() time.Time { return now },
		Bus:        bus,
		WithInject: stubInject,
	})
	defer h.Close()

	stop := startSubscriber(t, h)
	defer stop()

	r, closeSSE := openSSE(t, h)
	defer closeSSE()
	require.Eventually(t, func() bool { return bus.SubscriberCount() >= 2 }, time.Second, 10*time.Millisecond,
		"reactive subscriber + SSE client both subscribed")

	// Sensor publishes an observation directly onto the bus (simulating
	// a successful IMAP poll). The subscriber consumes and runs the
	// stub injectFn, which publishes the card.
	bus.Publish(eventbus.SensorEventObservedEvent{
		Kind_:      "mail.received",
		EvidenceID: "INBOX:1024:11",
		Payload:    map[string]any{"from": "saru@example.test", "subject": "redline"},
	})

	event, data, err := readSSEEvent(t, r, time.Now().Add(2*time.Second))
	require.NoError(t, err, "SSE client must receive card.appended within 2s of observation")
	require.Equal(t, "card.appended", event)
	require.Contains(t, data, `"id":"vip-saru-1"`)
	require.Contains(t, data, `"origin":"inject"`)
	require.Equal(t, int32(1), injectCalls.Load(), "exactly one inject pass")
}

// TestSpine_ReactiveInject_NonObservationEvents_DontTrigger pins the
// subscriber's filter at the system level: events that aren't
// SensorEventObservedEvent (card.appended, synth.started, etc.) flow on
// the bus without invoking the inject orchestrator. This is the wire-
// shape guarantee for the bus's typed-event union.
func TestSpine_ReactiveInject_NonObservationEvents_DontTrigger(t *testing.T) {
	tz, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 30, 10, 0, 0, 0, tz)

	bus := eventbus.New(logrus.NewEntry(logrus.New()))
	var injectCalls atomic.Int32
	stub := func(ctx context.Context, signal any) error {
		injectCalls.Add(1)
		return nil
	}

	h := NewHarness(t, HarnessConfig{
		TZ: tz, Now: func() time.Time { return now },
		Bus: bus, WithInject: stub,
	})
	defer h.Close()
	stop := startSubscriber(t, h)
	defer stop()

	// Publish a parade of non-observation events.
	bus.Publish(eventbus.CardAppendedEvent{Card: store.Card{ID: "noise-1"}})
	bus.Publish(eventbus.SynthStartedEvent{RunID: "r1", Stage: "morning"})
	bus.Publish(eventbus.TraceStepEvent{RunID: "r1", Stage: "morning"})
	bus.Publish(eventbus.SynthDeltaEvent{RunID: "r1", Stage: "morning", Delta: "..."})
	bus.Publish(eventbus.SynthCompletedEvent{RunID: "r1", Stage: "morning"})

	// Drain time — the subscriber's loop should have processed all of
	// these as no-ops.
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, int32(0), injectCalls.Load(),
		"non-observation events must NEVER reach the inject orchestrator")

	// Sanity: a real observation does still trigger a call.
	bus.Publish(eventbus.SensorEventObservedEvent{Kind_: "cal.event_changed", EvidenceID: "real"})
	require.Eventually(t, func() bool { return injectCalls.Load() == 1 }, time.Second, 10*time.Millisecond,
		"observation event still triggers the orchestrator")
}

// TestSpine_ReactiveInject_ManualAndReactive_SingleFlightInterlock pins
// the design call from the plan: subscriber routes through
// Scheduler.RunInjectNow which shares the injectRunning atomic with the
// manual /api/synth/now path, so concurrent reactive + manual triggers
// interlock — exactly one synth pass at a time.
//
// Without this interlock (subscriber owning its own atomic), the manual
// trigger would double-fire when overlapping with a reactive run, and
// the 30-min debounce would be the only safety net.
func TestSpine_ReactiveInject_ManualAndReactive_SingleFlightInterlock(t *testing.T) {
	tz, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 30, 10, 0, 0, 0, tz)

	bus := eventbus.New(logrus.NewEntry(logrus.New()))

	// `release` controls when the slow stub returns. The first invocation
	// blocks; later invocations (if the single-flight gate were broken)
	// would also block, but they should never run.
	release := make(chan struct{})
	var injectCalls atomic.Int32
	slowInject := func(ctx context.Context, signal any) error {
		n := injectCalls.Add(1)
		if n == 1 {
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}

	h := NewHarness(t, HarnessConfig{
		TZ: tz, Now: func() time.Time { return now },
		Bus: bus, WithInject: slowInject,
	})
	defer h.Close()
	stop := startSubscriber(t, h)
	defer stop()

	// 1) Reactive trigger: publish an observation, the subscriber takes
	//    the injectRunning gate (slowInject blocks).
	bus.Publish(eventbus.SensorEventObservedEvent{Kind_: "mail.received", EvidenceID: "first"})
	require.Eventually(t, func() bool { return injectCalls.Load() == 1 }, time.Second, 10*time.Millisecond,
		"reactive trigger must enter the orchestrator")

	// 2) Manual trigger while reactive is in flight. The handler should
	//    return 409 (RunInjectNowWithSignal sees ErrInjectInFlight via the
	//    shared atomic).
	status, body := h.Post("/api/synth/now?kind=inject&inject_kind=email&inject_subject=manual%20signal")
	require.Equalf(t, http.StatusConflict, status,
		"manual trigger during reactive flight must return 409; body=%s", body)
	require.Equal(t, int32(1), injectCalls.Load(),
		"manual trigger must NOT bypass the single-flight gate; only the reactive call ran")

	// 3) Release the reactive call.
	close(release)
	// The manual call returned without entering the orchestrator (gate
	// rejected it), so injectCalls stays at 1. Confirm steady state.
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, int32(1), injectCalls.Load())

	// 4) After the reactive run completes, a fresh manual trigger does
	//    succeed — the gate frees as expected.
	status2, body2 := h.Post("/api/synth/now?kind=inject&inject_kind=email&inject_subject=manual%20signal&force=1")
	require.Equalf(t, http.StatusOK, status2, "manual after reactive completion succeeds; body=%s", body2)
	require.Equal(t, int32(2), injectCalls.Load(), "second manual trigger fires after gate release")
}

