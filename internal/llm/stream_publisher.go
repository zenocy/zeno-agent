package llm

import "context"

// LiveTraceFunc is invoked by RunLoop each time it records a trace step
// (tool call or thought). The function receives the SAME TraceStep value
// that lands in the sealed Trace at end-of-run, so consumers can publish
// each step incrementally to a UI subscriber without coordinating with
// the durable trace persistence path.
//
// Implementations MUST NOT block. The runner forwards each call onto a
// non-blocking event-bus publish; a blocking LiveTraceFunc would back-
// pressure the loop's hot path. Best practice: fan out into a buffered
// channel with drop-on-full semantics, then return.
type LiveTraceFunc func(step TraceStep)

type liveTraceKey struct{}

// ContextWithLiveTrace returns a new context carrying the given live-trace
// callback. Pass nil to clear (rare). Mirrors the pattern used by
// ContextWithStreamContent / ContextWithStreamThinking in stream.go.
func ContextWithLiveTrace(ctx context.Context, fn LiveTraceFunc) context.Context {
	return context.WithValue(ctx, liveTraceKey{}, fn)
}

// LiveTraceFromContext extracts the live-trace callback. Returns nil
// when no callback is attached, in which case the runner skips
// publishing (the durable trace still records every step).
func LiveTraceFromContext(ctx context.Context) LiveTraceFunc {
	fn, _ := ctx.Value(liveTraceKey{}).(LiveTraceFunc)
	return fn
}
