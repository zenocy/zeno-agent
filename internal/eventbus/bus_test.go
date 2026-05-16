package eventbus

import (
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/llm"
	"github.com/zenocy/zeno-v2/internal/store"
)

func newTestBus() *Bus {
	l := logrus.NewEntry(logrus.New())
	l.Logger.SetOutput(&discardWriter{})
	return New(l)
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// receiveCard reads the next event from sub, asserts it's a CardAppendedEvent,
// and returns the underlying Card. Fails the test if the event has the wrong
// type or the channel times out.
func receiveCard(t *testing.T, sub chan Event, timeout time.Duration) store.Card {
	t.Helper()
	select {
	case ev := <-sub:
		ce, ok := ev.(CardAppendedEvent)
		require.Truef(t, ok, "expected CardAppendedEvent, got %T", ev)
		return ce.Card
	case <-time.After(timeout):
		t.Fatal("subscriber did not receive event")
		return store.Card{}
	}
}

func TestBus_PublishCardToSingleSubscriber(t *testing.T) {
	b := newTestBus()
	sub := b.Subscribe()
	defer b.Unsubscribe(sub)

	b.PublishCard(store.Card{ID: "c1", Title: "test"})

	got := receiveCard(t, sub, time.Second)
	require.Equal(t, "c1", got.ID)
}

func TestBus_PublishFansOutToAllSubscribers(t *testing.T) {
	b := newTestBus()
	a := b.Subscribe()
	c := b.Subscribe()
	defer b.Unsubscribe(a)
	defer b.Unsubscribe(c)

	require.Equal(t, 2, b.SubscriberCount())

	b.PublishCard(store.Card{ID: "fan"})

	for i, sub := range []chan Event{a, c} {
		got := receiveCard(t, sub, time.Second)
		require.Equalf(t, "fan", got.ID, "subscriber %d", i)
	}
}

// TestBus_PublishOtherEventTypes pins that the typed bus carries non-card
// events round-trip — V2.4's hot path uses TraceStepEvent / SynthDeltaEvent /
// SynthStartedEvent / SynthCompletedEvent / SensorEventObservedEvent alongside
// CardAppendedEvent on the same bus.
func TestBus_PublishOtherEventTypes(t *testing.T) {
	b := newTestBus()
	sub := b.Subscribe()
	defer b.Unsubscribe(sub)

	cases := []Event{
		SynthStartedEvent{RunID: "r1", Stage: "morning", Date: "2026-04-30"},
		TraceStepEvent{RunID: "r1", Stage: "cards", Step: llm.TraceStep{Kind: llm.KindThought, T: "looking at calendar"}},
		SynthDeltaEvent{RunID: "r1", Stage: "briefing", Delta: "Ten minutes "},
		SynthCompletedEvent{RunID: "r1", Stage: "morning", Stopped: "ok", TotalMs: 28412},
		SensorEventObservedEvent{Kind_: "mail.received", EvidenceID: "msg-42"},
		CardAppendedEvent{Card: store.Card{ID: "c-tail"}},
	}

	for _, want := range cases {
		b.Publish(want)
	}

	for i, want := range cases {
		select {
		case got := <-sub:
			require.Equalf(t, want.Kind(), got.Kind(), "case %d kind", i)
			require.Equalf(t, want, got, "case %d round-trip", i)
		case <-time.After(time.Second):
			t.Fatalf("case %d: subscriber did not receive event", i)
		}
	}
}

func TestBus_UnsubscribeRemovesAndCloses(t *testing.T) {
	b := newTestBus()
	sub := b.Subscribe()
	require.Equal(t, 1, b.SubscriberCount())

	b.Unsubscribe(sub)
	require.Equal(t, 0, b.SubscriberCount())

	// Channel is closed, so reading returns the zero value with ok=false.
	_, ok := <-sub
	require.False(t, ok, "channel must be closed after Unsubscribe")
}

func TestBus_UnsubscribeUnknownChannelIsNoOp(t *testing.T) {
	b := newTestBus()
	stranger := make(chan Event)
	b.Unsubscribe(stranger) // should not panic
}

// TestBus_PublishNilEventIsNoOp guards against a nil interface causing a
// nil dereference inside Publish's logger field call.
func TestBus_PublishNilEventIsNoOp(t *testing.T) {
	b := newTestBus()
	sub := b.Subscribe()
	defer b.Unsubscribe(sub)

	b.Publish(nil) // must not panic; must not deliver anything

	select {
	case ev := <-sub:
		t.Fatalf("expected no event, got %T", ev)
	case <-time.After(50 * time.Millisecond):
		// expected — nil is dropped silently
	}
}

// Slow-subscriber drop: when a subscriber's buffer is full, Publish must
// not block — it drops and logs. The durable card persistence means this
// is acceptable lossy behavior for the live stream.
func TestBus_PublishDoesNotBlockOnSlowSubscriber(t *testing.T) {
	b := newTestBus().WithBufferSize(1)
	sub := b.Subscribe()
	defer b.Unsubscribe(sub)

	// Fill the single buffer slot.
	b.PublishCard(store.Card{ID: "fills"})

	done := make(chan struct{})
	go func() {
		// This Publish would block forever if Publish were synchronous.
		b.PublishCard(store.Card{ID: "drops"})
		b.PublishCard(store.Card{ID: "also-drops"})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on full buffer; non-blocking semantics violated")
	}

	// The first card got through; subsequent ones were dropped.
	first := receiveCard(t, sub, time.Second)
	require.Equal(t, "fills", first.ID)
	select {
	case extra := <-sub:
		t.Fatalf("dropped card unexpectedly delivered: %v", extra)
	default:
	}
}

// Concurrent publish/subscribe is safe under the RWMutex.
func TestBus_ConcurrentPublishSubscribeIsRaceFree(t *testing.T) {
	b := newTestBus().WithBufferSize(64)

	subs := make([]chan Event, 4)
	for i := range subs {
		subs[i] = b.Subscribe()
	}
	defer func() {
		for _, s := range subs {
			b.Unsubscribe(s)
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			b.PublishCard(store.Card{ID: "p"})
		}(i)
	}
	wg.Wait()
	// No assertion on count — buffer size 64 across 50 publishes per sub
	// means none drop, but the test's value is the race detector + the
	// fact that Publish returned promptly across all goroutines.
}

// TestBus_UnsubscribeDuringPublish exercises the race of Unsubscribe firing
// concurrently with Publish — the publisher's loop reads through a snapshot
// of subs under RLock; Unsubscribe takes the write lock so the close happens
// only after the in-flight publish releases. The test fails (panic on send
// to closed channel, or data race) if that ordering ever inverts.
func TestBus_UnsubscribeDuringPublish(t *testing.T) {
	b := newTestBus().WithBufferSize(4)
	var wg sync.WaitGroup

	for i := 0; i < 16; i++ {
		sub := b.Subscribe()
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Drain a couple events before unsubscribing so Publish has work to do.
			for j := 0; j < 2; j++ {
				select {
				case <-sub:
				case <-time.After(50 * time.Millisecond):
				}
			}
			b.Unsubscribe(sub)
		}()
	}
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.PublishCard(store.Card{ID: "x"})
		}()
	}
	wg.Wait()
	// Survival is the assertion. -race must not flag and no panic on
	// "send on closed channel" should fire.
}

// TestBus_PublishWithNoSubscribersIsNoOp pins the empty-bus path so
// future refactors don't accidentally introduce a nil-deref.
func TestBus_PublishWithNoSubscribersIsNoOp(t *testing.T) {
	b := newTestBus()
	require.NotPanics(t, func() {
		b.PublishCard(store.Card{ID: "ghost"})
		b.Publish(SynthStartedEvent{RunID: "r", Stage: "cards"})
	})
	require.Equal(t, 0, b.SubscriberCount())
}

// TestBus_DropsAreLoggedNotBlocking pairs with the existing slow-subscriber
// test: the slow path also emits a WARN. We capture log output and verify
// the kind label lands so dashboards can alert on it.
func TestBus_DropsCarryEventKindInLog(t *testing.T) {
	logger := logrus.NewEntry(logrus.New())
	captured := &captureWriter{}
	logger.Logger.SetOutput(captured)
	b := New(logger).WithBufferSize(1)

	sub := b.Subscribe()
	defer b.Unsubscribe(sub)
	b.Publish(SynthStartedEvent{RunID: "r1", Stage: "cards"}) // fills buffer
	b.Publish(SynthStartedEvent{RunID: "r2", Stage: "cards"}) // dropped + logs
	require.Contains(t, captured.String(), "subscriber buffer full")
	// One of the recognized event Kind() values must show up — proves the
	// logger gets the kind label, not just the generic message.
	require.Contains(t, captured.String(), "synth.started")
}

type captureWriter struct {
	mu sync.Mutex
	b  []byte
}

func (c *captureWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.b = append(c.b, p...)
	return len(p), nil
}

func (c *captureWriter) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.b)
}
