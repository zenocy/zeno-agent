package injectsub

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/eventbus"
	"github.com/zenocy/zeno-v2/internal/schedule"
	"github.com/zenocy/zeno-v2/internal/store"
)

// stubRunner records every RunInjectNow invocation. Optional `err` and
// `block` channels let individual tests model single-flight (return
// ErrInjectInFlight) and slow synth (block until released).
type stubRunner struct {
	mu     sync.Mutex
	calls  atomic.Int32
	err    error
	block  chan struct{}
	onCall func()
}

func (s *stubRunner) RunInjectNow(ctx context.Context) error {
	s.calls.Add(1)
	s.mu.Lock()
	onCall := s.onCall
	block := s.block
	err := s.err
	s.mu.Unlock()
	if onCall != nil {
		onCall()
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (s *stubRunner) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func quietEntry() *logrus.Entry {
	l := logrus.New()
	l.Out = io.Discard
	l.Level = logrus.DebugLevel
	return l.WithField("c", "subscriber-test")
}

func quietBus() *eventbus.Bus {
	return eventbus.New(quietEntry())
}

// waitFor polls predicate every 5 ms until true or timeout elapses.
func waitFor(predicate func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return predicate()
}

// TestSubscriber_VIPEmail_FiresInject: a mail.received observation
// triggers exactly one RunInjectNow call.
func TestSubscriber_VIPEmail_FiresInject(t *testing.T) {
	bus := quietBus()
	runner := &stubRunner{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, Deps{Bus: bus, Runner: runner, Logger: quietEntry()})

	require.True(t, waitFor(func() bool { return bus.SubscriberCount() >= 1 }, time.Second))

	bus.Publish(eventbus.SensorEventObservedEvent{
		Kind_: "mail.received", EvidenceID: "INBOX:42:11",
		Payload: map[string]any{"from": "vip@example.test"},
	})

	require.True(t, waitFor(func() bool { return runner.calls.Load() == 1 }, time.Second),
		"observation must drive exactly one RunInjectNow call")
}

// TestSubscriber_CalendarMove_FiresInject: a cal.event_changed
// observation also triggers RunInjectNow (Kind_ filter passes).
func TestSubscriber_CalendarMove_FiresInject(t *testing.T) {
	bus := quietBus()
	runner := &stubRunner{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, Deps{Bus: bus, Runner: runner, Logger: quietEntry()})

	require.True(t, waitFor(func() bool { return bus.SubscriberCount() >= 1 }, time.Second))

	bus.Publish(eventbus.SensorEventObservedEvent{
		Kind_: "cal.event_changed", EvidenceID: "uid-board-call",
		Payload: map[string]any{"title": "Board call moved up"},
	})

	require.True(t, waitFor(func() bool { return runner.calls.Load() == 1 }, time.Second))
}

// TestSubscriber_NonObservationEvent_Ignored: card.appended and
// synth.started events arrive on the same bus but are filtered out.
// Only SensorEventObservedEvent reaches RunInjectNow.
func TestSubscriber_NonObservationEvent_Ignored(t *testing.T) {
	bus := quietBus()
	runner := &stubRunner{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, Deps{Bus: bus, Runner: runner, Logger: quietEntry()})

	require.True(t, waitFor(func() bool { return bus.SubscriberCount() >= 1 }, time.Second))

	bus.Publish(eventbus.CardAppendedEvent{Card: store.Card{ID: "c1"}})
	bus.Publish(eventbus.SynthStartedEvent{RunID: "r1", Stage: "morning"})
	bus.Publish(eventbus.SensorEventObservedEvent{Kind_: "mail.received", EvidenceID: "x"})
	bus.Publish(eventbus.SynthCompletedEvent{RunID: "r1", Stage: "morning"})

	require.True(t, waitFor(func() bool { return runner.calls.Load() == 1 }, time.Second),
		"only the SensorEventObservedEvent should drive a RunInjectNow call")

	time.Sleep(100 * time.Millisecond)
	require.Equal(t, int32(1), runner.calls.Load())
}

// TestSubscriber_SingleFlight_DropsConcurrentObservations: when
// RunInjectNow returns ErrInjectInFlight, the subscriber logs at debug
// and continues; it does not hang or retry.
func TestSubscriber_SingleFlight_DropsConcurrentObservations(t *testing.T) {
	bus := quietBus()
	runner := &stubRunner{err: schedule.ErrInjectInFlight}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, Deps{Bus: bus, Runner: runner, Logger: quietEntry()})

	require.True(t, waitFor(func() bool { return bus.SubscriberCount() >= 1 }, time.Second))

	for i := 0; i < 3; i++ {
		bus.Publish(eventbus.SensorEventObservedEvent{Kind_: "mail.received", EvidenceID: "id"})
	}

	require.True(t, waitFor(func() bool { return runner.calls.Load() == 3 }, time.Second),
		"each observation invokes RunInjectNow even when single-flight rejects them")
}

// TestSubscriber_ContinuesAfterRunInjectError: a synth error is logged
// but does not kill the subscriber. The next observation still fires.
func TestSubscriber_ContinuesAfterRunInjectError(t *testing.T) {
	bus := quietBus()
	runner := &stubRunner{err: errors.New("synth boom")}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, Deps{Bus: bus, Runner: runner, Logger: quietEntry()})

	require.True(t, waitFor(func() bool { return bus.SubscriberCount() >= 1 }, time.Second))

	bus.Publish(eventbus.SensorEventObservedEvent{Kind_: "mail.received", EvidenceID: "first"})
	require.True(t, waitFor(func() bool { return runner.calls.Load() == 1 }, time.Second))

	runner.setErr(nil)
	bus.Publish(eventbus.SensorEventObservedEvent{Kind_: "cal.event_changed", EvidenceID: "second"})
	require.True(t, waitFor(func() bool { return runner.calls.Load() == 2 }, time.Second),
		"subscriber must keep consuming after a non-single-flight error")
}

// TestSubscriber_GracefulDrain: cancelling the parent ctx returns the
// subscriber goroutine cleanly within a short window.
func TestSubscriber_GracefulDrain(t *testing.T) {
	bus := quietBus()
	runner := &stubRunner{}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, Deps{Bus: bus, Runner: runner, Logger: quietEntry()})
		close(done)
	}()

	require.True(t, waitFor(func() bool { return bus.SubscriberCount() >= 1 }, time.Second))

	cancel()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("subscriber did not drain within 500 ms after ctx cancel")
	}

	require.Equal(t, 0, bus.SubscriberCount(), "subscriber must Unsubscribe on exit")

	bus.Publish(eventbus.SensorEventObservedEvent{Kind_: "mail.received", EvidenceID: "post-cancel"})
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, int32(0), runner.calls.Load())
}
