package llm

import (
	"context"
	"time"
)

// StreamContentFunc is called with each content token delta during streaming.
type StreamContentFunc func(delta string)

// StreamThinkingFunc is called with each thinking token delta during streaming.
type StreamThinkingFunc func(delta string)

// StreamProgressFunc is called during the LLM tool loop to report progress.
type StreamProgressFunc func(event StreamEvent)

// StreamEvent describes a progress event during LLM processing.
type StreamEvent struct {
	Type     string // "tool_call", "tool_result", "iteration"
	ToolName string
	ToolArgs map[string]any
	Error    bool
}

type streamContentKey struct{}
type streamThinkingKey struct{}
type streamProgressKey struct{}
type perCallDeadlineKey struct{}

// ContextWithStreamContent returns a new context carrying a content streaming callback.
func ContextWithStreamContent(ctx context.Context, fn StreamContentFunc) context.Context {
	return context.WithValue(ctx, streamContentKey{}, fn)
}

// StreamContentFromContext extracts the content streaming callback, or returns nil.
func StreamContentFromContext(ctx context.Context) StreamContentFunc {
	fn, _ := ctx.Value(streamContentKey{}).(StreamContentFunc)
	return fn
}

// ContextWithStreamThinking returns a new context carrying a thinking streaming callback.
func ContextWithStreamThinking(ctx context.Context, fn StreamThinkingFunc) context.Context {
	return context.WithValue(ctx, streamThinkingKey{}, fn)
}

// StreamThinkingFromContext extracts the thinking streaming callback, or returns nil.
func StreamThinkingFromContext(ctx context.Context) StreamThinkingFunc {
	fn, _ := ctx.Value(streamThinkingKey{}).(StreamThinkingFunc)
	return fn
}

// ContextWithStreamProgress returns a new context carrying a progress callback.
func ContextWithStreamProgress(ctx context.Context, fn StreamProgressFunc) context.Context {
	return context.WithValue(ctx, streamProgressKey{}, fn)
}

// StreamProgressFromContext extracts the progress callback, or returns nil.
func StreamProgressFromContext(ctx context.Context) StreamProgressFunc {
	fn, _ := ctx.Value(streamProgressKey{}).(StreamProgressFunc)
	return fn
}

// ContextWithPerCallDeadline attaches a per-call deadline to the context. The
// resilient provider wraps each underlying HTTP call with WithTimeout(d) so a
// hung provider call cannot consume the entire turn deadline. d <= 0 disables.
func ContextWithPerCallDeadline(ctx context.Context, d time.Duration) context.Context {
	if d <= 0 {
		return ctx
	}
	return context.WithValue(ctx, perCallDeadlineKey{}, d)
}

// PerCallDeadlineFromContext returns the per-call deadline, or 0 if unset.
func PerCallDeadlineFromContext(ctx context.Context) time.Duration {
	d, _ := ctx.Value(perCallDeadlineKey{}).(time.Duration)
	return d
}
