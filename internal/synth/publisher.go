package synth

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/zenocy/zeno-v2/internal/eventbus"
	"github.com/zenocy/zeno-v2/internal/llm"
)

// CoalesceWindow caps the staleness of a buffered body delta before the
// publisher flushes a SynthDeltaEvent. 16ms ≈ one render frame at 60Hz.
const CoalesceWindow = 16 * time.Millisecond

// CoalesceByteThreshold is the byte count that triggers an immediate flush.
// Picks dominate the rate when the model emits long runs of token chunks;
// the timer dominates when emissions are sparse.
const CoalesceByteThreshold = 8

// AttachLivePublishers wraps ctx with llm.LiveTraceFunc + llm.StreamContentFunc
// callbacks that publish to bus, tagged with runID + stage. Body-token
// deltas coalesce server-side at ≤ CoalesceWindow or ≤ CoalesceByteThreshold,
// whichever fires first. Returns the wrapped ctx and a cleanup func that
// flushes any pending coalescer tail. Defer the cleanup in the caller.
//
// **Use only for plain-text stages** (briefing, inject_fragment). For
// JSON-schema-constrained stages (cards, inject_cards, ask) call
// AttachLiveTrace instead — streaming JSON tokens through SynthDeltaEvent
// floods the panel's body field with raw `{"cards":[{...},...]}` text that
// no user can read. The trace steps still surface usefully via either path.
//
// A nil bus turns both callbacks into no-ops (eval, replay, and unit tests
// that don't care about live events can pass nil safely).
//
// Deviation from doc/v2.4/Phase2.md: StreamThinkingFunc is NOT wired here.
// The eventbus exposes no `synth.thinking_delta` event; thinking content
// reaches the live trace via RecordThought → LiveTraceFunc at iteration
// boundaries (Phase 1 wiring). Wiring per-token thinking would require a
// new event type or a Kind discriminator on SynthDeltaEvent — not in P2.
func AttachLivePublishers(ctx context.Context, bus *eventbus.Bus, runID, stage string) (context.Context, func()) {
	if bus == nil {
		return ctx, func() {}
	}

	ctx = AttachLiveTrace(ctx, bus, runID, stage)

	c := &coalescer{
		bus:   bus,
		runID: runID,
		stage: stage,
	}
	streamContent := llm.StreamContentFunc(c.add)
	ctx = llm.ContextWithStreamContent(ctx, streamContent)
	return ctx, c.cleanup
}

// AttachLiveTrace wraps ctx with llm.LiveTraceFunc only — no body-token
// streaming. Use for stages whose LLM output is schema-constrained JSON
// (cards loop, inject cards loop, Ask): trace steps stream usefully but
// body-token JSON noise has no value in the live panel. The briefing /
// inject_fragment paths use AttachLivePublishers (which adds
// StreamContentFunc) so their plain-text deltas type into the panel.
//
// A nil bus is a no-op.
func AttachLiveTrace(ctx context.Context, bus *eventbus.Bus, runID, stage string) context.Context {
	if bus == nil {
		return ctx
	}
	liveTrace := llm.LiveTraceFunc(func(step llm.TraceStep) {
		bus.Publish(eventbus.TraceStepEvent{
			RunID: runID,
			Stage: stage,
			Step:  step,
		})
	})
	return llm.ContextWithLiveTrace(ctx, liveTrace)
}

// coalescer batches body-token deltas before publishing SynthDeltaEvents.
// The byte threshold flushes immediately when the buffer is large enough
// to be worth a frame; the timer ensures a slow trickle still surfaces in
// roughly one render frame's worth of latency.
type coalescer struct {
	bus   *eventbus.Bus
	runID string
	stage string

	mu     sync.Mutex
	buf    strings.Builder
	timer  *time.Timer
	closed bool
}

// add appends delta to the buffer. If the buffer reaches the byte
// threshold, it flushes synchronously. Otherwise it ensures a flush timer
// is armed to fire after CoalesceWindow.
func (c *coalescer) add(delta string) {
	if delta == "" {
		return
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.buf.WriteString(delta)
	if c.buf.Len() >= CoalesceByteThreshold {
		c.flushLocked()
		c.mu.Unlock()
		return
	}
	if c.timer == nil {
		c.timer = time.AfterFunc(CoalesceWindow, c.flushFromTimer)
	}
	c.mu.Unlock()
}

// flushFromTimer is the AfterFunc callback. Holds the lock to read+reset
// the buffer without racing add().
func (c *coalescer) flushFromTimer() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.flushLocked()
}

// flushLocked drains the buffer into a SynthDeltaEvent (if non-empty),
// resets the buffer, and clears the timer. Caller must hold c.mu.
func (c *coalescer) flushLocked() {
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	if c.buf.Len() == 0 {
		return
	}
	delta := c.buf.String()
	c.buf.Reset()
	c.bus.Publish(eventbus.SynthDeltaEvent{
		RunID: c.runID,
		Stage: c.stage,
		Delta: delta,
	})
}

// cleanup stops any pending timer and flushes the tail buffer. Safe to
// call exactly once after the synth stage completes; further add() calls
// after cleanup are dropped (closed=true).
func (c *coalescer) cleanup() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	c.flushLocked()
}
