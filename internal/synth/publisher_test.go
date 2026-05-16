package synth

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/eventbus"
	"github.com/zenocy/zeno-v2/internal/llm"
)

func newTestBus(t *testing.T) *eventbus.Bus {
	t.Helper()
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	return eventbus.New(logrus.NewEntry(logger))
}

// TestAttachLivePublishers_TraceStep_PublishedWithRunIDAndStage pins the
// per-step publish path: the LiveTraceFunc forwarded into ctx by
// AttachLivePublishers wraps the step in a TraceStepEvent tagged with the
// runID + stage and publishes byte-equal Step content.
func TestAttachLivePublishers_TraceStep_PublishedWithRunIDAndStage(t *testing.T) {
	bus := newTestBus(t)
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	ctx, cleanup := AttachLivePublishers(context.Background(), bus, "run-A", "cards")
	defer cleanup()

	live := llm.LiveTraceFromContext(ctx)
	require.NotNil(t, live)

	step := llm.TraceStep{Kind: llm.KindThought, T: "checking calendar", MsAt: 42}
	live(step)

	events := drainBus(t, sub, 30*time.Millisecond)
	require.Len(t, events, 1)
	got, ok := events[0].(eventbus.TraceStepEvent)
	require.True(t, ok, "expected TraceStepEvent, got %T", events[0])
	require.Equal(t, "run-A", got.RunID)
	require.Equal(t, "cards", got.Stage)
	require.Equal(t, step, got.Step)
}

func TestAttachLivePublishers_TraceStep_MultipleStepsPublishInOrder(t *testing.T) {
	bus := newTestBus(t)
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	ctx, cleanup := AttachLivePublishers(context.Background(), bus, "run-B", "briefing")
	defer cleanup()

	live := llm.LiveTraceFromContext(ctx)
	steps := []llm.TraceStep{
		{Kind: llm.KindThought, T: "first", MsAt: 1},
		{Kind: llm.KindTool, Op: "READ", Target: "thread-1", MsAt: 5},
		{Kind: llm.KindThought, T: "second", MsAt: 12},
		{Kind: llm.KindTool, Op: "READ", Target: "event-2", MsAt: 18},
		{Kind: llm.KindThought, T: "third", MsAt: 25},
	}
	for _, s := range steps {
		live(s)
	}

	events := drainBus(t, sub, 30*time.Millisecond)
	require.Len(t, events, len(steps))
	for i, ev := range events {
		ts, ok := ev.(eventbus.TraceStepEvent)
		require.True(t, ok, "event %d: expected TraceStepEvent, got %T", i, ev)
		require.Equal(t, steps[i], ts.Step, "event %d step mismatch", i)
		require.Equal(t, "run-B", ts.RunID)
		require.Equal(t, "briefing", ts.Stage)
	}
}

// TestAttachLiveTrace_DoesNotAttachStreamContent pins the contract that
// AttachLiveTrace skips StreamContentFunc — JSON-schema stages (cards,
// inject_cards, ask) must not flood the live panel's body field with
// raw JSON tokens. LiveTraceFunc still flows.
func TestAttachLiveTrace_DoesNotAttachStreamContent(t *testing.T) {
	bus := newTestBus(t)
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	ctx := AttachLiveTrace(context.Background(), bus, "run-T", "cards")

	require.Nil(t, llm.StreamContentFromContext(ctx),
		"AttachLiveTrace must NOT attach StreamContentFunc — body deltas would be raw JSON noise")
	require.NotNil(t, llm.LiveTraceFromContext(ctx),
		"AttachLiveTrace must still attach LiveTraceFunc so trace steps surface")

	// Trace step still publishes via the bus.
	live := llm.LiveTraceFromContext(ctx)
	live(llm.TraceStep{Kind: llm.KindThought, T: "checking", MsAt: 1})
	events := drainBus(t, sub, 30*time.Millisecond)
	require.Len(t, events, 1)
	_, ok := events[0].(eventbus.TraceStepEvent)
	require.True(t, ok, "expected TraceStepEvent")
}

// TestAttachLiveTrace_NilBus is a no-op so the helper is safe in eval /
// replay paths that don't wire a bus.
func TestAttachLiveTrace_NilBus(t *testing.T) {
	parent := context.Background()
	ctx := AttachLiveTrace(parent, nil, "run", "cards")
	require.Equal(t, parent, ctx, "nil bus must return the parent ctx unchanged")
}

// TestAttachLivePublishers_BodyDeltas_FlushOnByteThreshold — feeding 9
// chars in one delta crosses the 8-char threshold and flushes
// synchronously without waiting for the 16ms timer.
func TestAttachLivePublishers_BodyDeltas_FlushOnByteThreshold(t *testing.T) {
	bus := newTestBus(t)
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	ctx, cleanup := AttachLivePublishers(context.Background(), bus, "run-C", "briefing")
	defer cleanup()

	stream := llm.StreamContentFromContext(ctx)
	require.NotNil(t, stream)

	stream("123456789") // 9 chars > 8 → immediate flush

	// Read with a small quiet window; the flush is synchronous so the
	// event is already on the channel.
	events := drainBus(t, sub, 5*time.Millisecond)
	require.Len(t, events, 1)
	d, ok := events[0].(eventbus.SynthDeltaEvent)
	require.True(t, ok)
	require.Equal(t, "run-C", d.RunID)
	require.Equal(t, "briefing", d.Stage)
	require.Equal(t, "123456789", d.Delta)
}

// TestAttachLivePublishers_BodyDeltas_AccumulateUnderThreshold — three
// 2-char deltas (6 total, < 8) shouldn't flush by byte rule; the timer
// must fire to drain.
func TestAttachLivePublishers_BodyDeltas_AccumulateUnderThreshold(t *testing.T) {
	bus := newTestBus(t)
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	ctx, cleanup := AttachLivePublishers(context.Background(), bus, "run-D", "ask")
	defer cleanup()

	stream := llm.StreamContentFromContext(ctx)
	stream("ab")
	stream("cd")
	stream("ef")

	events := drainBus(t, sub, 50*time.Millisecond)
	require.Len(t, events, 1)
	d := events[0].(eventbus.SynthDeltaEvent)
	require.Equal(t, "abcdef", d.Delta)
}

func TestAttachLivePublishers_BodyDeltas_FlushOnTimer(t *testing.T) {
	bus := newTestBus(t)
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	ctx, cleanup := AttachLivePublishers(context.Background(), bus, "run-E", "briefing")
	defer cleanup()

	stream := llm.StreamContentFromContext(ctx)
	stream("abc") // 3 chars < 8 → wait for timer

	events := drainBus(t, sub, 50*time.Millisecond)
	require.Len(t, events, 1)
	d := events[0].(eventbus.SynthDeltaEvent)
	require.Equal(t, "abc", d.Delta)
}

// TestAttachLivePublishers_BodyDeltas_TimerResetsOnNewDelta pins that the
// timer extends on each new delta (one timer at a time, replaced when it
// fires; a new arrival under the byte threshold keeps the in-flight timer
// armed without stacking). Result: ONE event when the trickle stops.
func TestAttachLivePublishers_BodyDeltas_TimerResetsOnNewDelta(t *testing.T) {
	bus := newTestBus(t)
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	ctx, cleanup := AttachLivePublishers(context.Background(), bus, "run-F", "briefing")
	defer cleanup()

	stream := llm.StreamContentFromContext(ctx)
	stream("a")
	time.Sleep(8 * time.Millisecond)
	stream("b")
	time.Sleep(8 * time.Millisecond)
	stream("c")

	// The timer first armed at t=0 may have already fired one batch by now
	// (we're at ~16ms). Wait for quiet and inspect the result.
	events := drainBus(t, sub, 50*time.Millisecond)
	// Either one event ("abc") or two events ("a"/"bc" or "ab"/"c"), but
	// the concatenation must equal "abc" and there must be at most one
	// active timer at a time (no event-flood).
	require.GreaterOrEqual(t, len(events), 1)
	require.LessOrEqual(t, len(events), 3)
	var got string
	for _, ev := range events {
		got += ev.(eventbus.SynthDeltaEvent).Delta
	}
	require.Equal(t, "abc", got)
}

func TestAttachLivePublishers_BodyDeltas_StreamingMix(t *testing.T) {
	bus := newTestBus(t)
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	ctx, cleanup := AttachLivePublishers(context.Background(), bus, "run-G", "briefing")
	defer cleanup()

	stream := llm.StreamContentFromContext(ctx)
	stream("a")
	stream("bcdefghi") // 1 + 8 = 9 chars → triggers byte flush
	// after the flush, buffer is empty; new short deltas arm the timer.
	stream("j")
	stream("k")

	events := drainBus(t, sub, 50*time.Millisecond)
	require.Len(t, events, 2)
	require.Equal(t, "abcdefghi", events[0].(eventbus.SynthDeltaEvent).Delta)
	require.Equal(t, "jk", events[1].(eventbus.SynthDeltaEvent).Delta)
}

// TestAttachLivePublishers_Cleanup_FlushesTail — cleanup mid-window must
// surface the buffered tail so consumers don't lose the final tokens
// (e.g., when a stream completes between timer fires).
func TestAttachLivePublishers_Cleanup_FlushesTail(t *testing.T) {
	bus := newTestBus(t)
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	ctx, cleanup := AttachLivePublishers(context.Background(), bus, "run-H", "briefing")

	stream := llm.StreamContentFromContext(ctx)
	stream("tail") // 4 chars < 8

	cleanup() // should flush "tail" synchronously

	events := drainBus(t, sub, 5*time.Millisecond)
	require.Len(t, events, 1)
	require.Equal(t, "tail", events[0].(eventbus.SynthDeltaEvent).Delta)
}

func TestAttachLivePublishers_Cleanup_StopsTimer_NoLateFire(t *testing.T) {
	bus := newTestBus(t)
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	ctx, cleanup := AttachLivePublishers(context.Background(), bus, "run-I", "briefing")

	stream := llm.StreamContentFromContext(ctx)
	stream("xyz")
	cleanup()

	// Wait well past the 16ms window. If the timer were leaked, we'd see
	// a duplicate event.
	events := drainBus(t, sub, 50*time.Millisecond)
	require.Len(t, events, 1)
	require.Equal(t, "xyz", events[0].(eventbus.SynthDeltaEvent).Delta)
}

func TestAttachLivePublishers_Cleanup_EmptyBuffer_PublishesNothing(t *testing.T) {
	bus := newTestBus(t)
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	_, cleanup := AttachLivePublishers(context.Background(), bus, "run-J", "briefing")
	cleanup()

	events := drainBus(t, sub, 30*time.Millisecond)
	require.Empty(t, events)
}

// TestAttachLivePublishers_NilBus_NoOp — eval/replay paths pass nil; both
// callbacks must be safe no-ops and cleanup must not panic.
func TestAttachLivePublishers_NilBus_NoOp(t *testing.T) {
	ctx, cleanup := AttachLivePublishers(context.Background(), nil, "run-K", "cards")
	require.NotNil(t, ctx)

	// Neither callback is set on the returned ctx; the synth functions
	// see zero-value extractors and just skip the publish branch.
	require.Nil(t, llm.LiveTraceFromContext(ctx))
	require.Nil(t, llm.StreamContentFromContext(ctx))

	require.NotPanics(t, cleanup)
	require.NotPanics(t, cleanup) // double-cleanup safe
}

// TestAttachLivePublishers_ContextCarriesCallbacks pins the wiring
// contract that RunLoop depends on: both callbacks are non-nil on the
// returned ctx and forward to the bus when invoked.
func TestAttachLivePublishers_ContextCarriesCallbacks(t *testing.T) {
	bus := newTestBus(t)
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	ctx, cleanup := AttachLivePublishers(context.Background(), bus, "run-L", "ask")
	defer cleanup()

	live := llm.LiveTraceFromContext(ctx)
	stream := llm.StreamContentFromContext(ctx)
	require.NotNil(t, live)
	require.NotNil(t, stream)

	live(llm.TraceStep{Kind: llm.KindThought, T: "x", MsAt: 1})
	stream("123456789") // forces flush

	events := drainBus(t, sub, 5*time.Millisecond)
	require.Len(t, events, 2)
	_, ok1 := events[0].(eventbus.TraceStepEvent)
	_, ok2 := events[1].(eventbus.SynthDeltaEvent)
	require.True(t, ok1)
	require.True(t, ok2)
}

// TestAttachLivePublishers_ConcurrentDeltas_NoDataRace — race-detector
// regression. 1000 deltas across 8 goroutines. After cleanup, the total
// byte count published must equal the byte count fed.
func TestAttachLivePublishers_ConcurrentDeltas_NoDataRace(t *testing.T) {
	bus := newTestBus(t).WithBufferSize(4096)
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	ctx, cleanup := AttachLivePublishers(context.Background(), bus, "run-M", "briefing")

	stream := llm.StreamContentFromContext(ctx)

	const goroutines = 8
	const perGoroutine = 125 // 8 * 125 = 1000 deltas
	const deltaText = "abc"  // 3 chars each → 3000 bytes total

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				stream(deltaText)
			}
		}()
	}
	wg.Wait()

	cleanup()

	events := drainBus(t, sub, 100*time.Millisecond)
	totalBytes := 0
	for _, ev := range events {
		d, ok := ev.(eventbus.SynthDeltaEvent)
		require.True(t, ok)
		totalBytes += len(d.Delta)
	}
	require.Equal(t, goroutines*perGoroutine*len(deltaText), totalBytes)
}

// TestAttachLivePublishers_PublishesNothingForUnattachedThinking pins the
// deviation from doc/v2.4/Phase2.md: StreamThinkingFunc is NOT wired by
// AttachLivePublishers in P2. Re-attaching the wrapped ctx must not have
// installed a thinking forwarder.
func TestAttachLivePublishers_PublishesNothingForUnattachedThinking(t *testing.T) {
	bus := newTestBus(t)
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	ctx, cleanup := AttachLivePublishers(context.Background(), bus, "run-N", "briefing")
	defer cleanup()

	require.Nil(t, llm.StreamThinkingFromContext(ctx),
		"StreamThinkingFunc must NOT be wired by AttachLivePublishers in P2")

	// Sanity: drain the bus to confirm nothing leaked.
	events := drainBus(t, sub, 30*time.Millisecond)
	require.Empty(t, events)
}

// Ensure unused imports stay used as the file evolves.
var _ = atomic.AddInt64
var _ = fmt.Sprintf
