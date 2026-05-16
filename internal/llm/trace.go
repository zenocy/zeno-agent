package llm

import (
	"context"
	"sync"
	"time"
)

// StepKind tags a TraceStep as either a tool call or a one-line model thought.
type StepKind string

const (
	KindTool    StepKind = "tool"
	KindThought StepKind = "thought"
)

// TraceStep is one observation from the tool loop. Tool steps carry an Op
// verb (READ/CHECK/...), a Target string, and an optional Note (error or
// hint). Thought steps carry T (the inner-monologue sentence). The wire
// field names match the frontend Trace.tsx renderer's expectations.
//
// V2.5.0 Phase 3 added Refs — observation IDs the tool touched while
// producing this step. Used by the eval harness to score retrieval
// quality without instrumenting every projection call. The field is
// `omitempty` so existing trace fixtures and the React renderer remain
// byte-equal when unset.
type TraceStep struct {
	Kind   StepKind `json:"kind"`
	Op     string   `json:"op,omitempty"`
	Target string   `json:"target,omitempty"`
	Note   string   `json:"note,omitempty"`
	T      string   `json:"t,omitempty"`
	Refs   []string `json:"refs,omitempty"`
	MsAt   int64    `json:"ms_elapsed"`
}

// Trace is the full record of one loop run. Stopped is set by the loop's
// terminal branch ("ok", "iteration_cap", "deadline", "duplicate",
// "repair_exhausted", "error").
type Trace struct {
	Steps   []TraceStep `json:"steps"`
	Stopped string      `json:"stopped"`
	TotalMs int64       `json:"total_ms"`
}

// Accumulator collects TraceSteps with relative timestamps. Construct via
// NewAccumulator at the start of a loop run; call Build at the end.
type Accumulator struct {
	steps []TraceStep
	t0    time.Time
}

// NewAccumulator stamps the start time and returns an empty accumulator.
func NewAccumulator() *Accumulator {
	return &Accumulator{t0: time.Now()}
}

// RecordTool appends a tool step. Op is the uppercase verb shown in the UI
// (e.g. READ); target identifies the resource (thread subject, event UID,
// weather window). Note carries an error message or short result hint.
//
// Returns the recorded step so callers can forward the same value to a
// live publisher (V2.4 LiveTraceFunc) — guaranteeing the durable trace
// and the live SSE stream carry byte-identical step bodies.
func (a *Accumulator) RecordTool(op, target, note string) TraceStep {
	step := TraceStep{
		Kind:   KindTool,
		Op:     op,
		Target: target,
		Note:   note,
		MsAt:   time.Since(a.t0).Milliseconds(),
	}
	a.steps = append(a.steps, step)
	return step
}

// RecordToolWithRefs is the V2.5.0 Phase 3 variant. Identical to
// RecordTool but also attaches a slice of observation IDs the tool
// touched. The eval harness folds these to score retrieval quality.
//
// Existing call sites (V2.4 read_thread / read_event / read_weather_window)
// stay on RecordTool so the trace JSON is byte-equal across V2.4
// fixtures. Phase 3's new tools (lookup_concern / read_concern_evidence)
// land on this sibling.
func (a *Accumulator) RecordToolWithRefs(op, target, note string, refs []string) TraceStep {
	step := TraceStep{
		Kind:   KindTool,
		Op:     op,
		Target: target,
		Note:   note,
		Refs:   refs,
		MsAt:   time.Since(a.t0).Milliseconds(),
	}
	a.steps = append(a.steps, step)
	return step
}

// RecordThought appends a one-sentence inner-monologue step. The text is
// written to the T field so it serializes as {"kind":"thought","t":"..."} —
// matching the Trace.tsx ThoughtStep renderer.
//
// Returns the recorded step (see RecordTool's contract).
func (a *Accumulator) RecordThought(text string) TraceStep {
	step := TraceStep{
		Kind: KindThought,
		T:    text,
		MsAt: time.Since(a.t0).Milliseconds(),
	}
	a.steps = append(a.steps, step)
	return step
}

// Steps returns a copy of the accumulated steps so callers can read without
// racing future appends.
func (a *Accumulator) Steps() []TraceStep {
	out := make([]TraceStep, len(a.steps))
	copy(out, a.steps)
	return out
}

// Build seals the accumulator into a Trace tagged with the given stop reason.
func (a *Accumulator) Build(stopped string) Trace {
	return Trace{
		Steps:   a.Steps(),
		Stopped: stopped,
		TotalMs: time.Since(a.t0).Milliseconds(),
	}
}

// V2.5.0 Phase 3 — context-bound refs collector.
//
// Tools that touch known observation IDs while producing their output
// (e.g. read_concern_evidence) call AppendRefsToContext to publish
// those IDs to the loop. After the tool returns, the loop pulls them
// via RefsFromContext and attaches them to the recorded TraceStep.
//
// Context-bound (rather than tool-struct-bound) avoids races when a
// tool is registered once and called concurrently across requests.
// Each Execute gets its own context with its own collector.

type refsCollectorKey struct{}

// refsCollector is the per-Execute container.
type refsCollector struct {
	mu   sync.Mutex
	refs []string
}

// WithRefsCollector returns a child context carrying a fresh refs
// collector. The loop calls this once per tool.Execute, then reads
// the populated refs after the call returns.
func WithRefsCollector(ctx context.Context) context.Context {
	return context.WithValue(ctx, refsCollectorKey{}, &refsCollector{})
}

// AppendRefsToContext appends observation IDs to the collector bound
// to ctx. No-op when the context has no collector (eval / replay
// paths, or non-Phase-3 tools).
func AppendRefsToContext(ctx context.Context, ids ...string) {
	c, _ := ctx.Value(refsCollectorKey{}).(*refsCollector)
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refs = append(c.refs, ids...)
}

// RefsFromContext returns a copy of the refs accumulated on ctx.
// Returns nil if no collector is attached.
func RefsFromContext(ctx context.Context) []string {
	c, _ := ctx.Value(refsCollectorKey{}).(*refsCollector)
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.refs) == 0 {
		return nil
	}
	out := make([]string, len(c.refs))
	copy(out, c.refs)
	return out
}
