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
type serviceTierKey struct{}
type callProfileKey struct{}

// CallProfile labels a chat completion call as either "background"
// (cron-fired or manually-triggered work the operator isn't actively
// watching) or "interactive" (chat, Ask, draft, voice). Each provider
// maps the profile to its own latency/cost knob:
//
//   - OpenAI/OpenRouter: service_tier (background→flex, interactive→priority)
//   - Gemini: thinkingConfig.thinkingLevel (background→high, interactive→low)
//
// The mapping table is per-provider config; the profile itself is
// provider-agnostic so the HTTP middleware and scheduler can stamp it
// without knowing which backend is wired up.
type CallProfile string

// Recognized CallProfile values. "" is treated as unset (no profile-
// driven mapping; the provider falls back to its default behavior).
const (
	CallProfileBackground  CallProfile = "background"
	CallProfileInteractive CallProfile = "interactive"
)

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

// ContextWithServiceTier attaches an OpenRouter service tier to ctx.
// Allowed values: "default", "flex", "priority" (provider-dependent;
// see https://openrouter.ai/docs/guides/features/service-tiers).
// Empty tier returns ctx unchanged so callers can pass a config value
// verbatim without guarding for the "operator hasn't opted in" case.
func ContextWithServiceTier(ctx context.Context, tier string) context.Context {
	if tier == "" {
		return ctx
	}
	return context.WithValue(ctx, serviceTierKey{}, tier)
}

// ServiceTierFromContext returns the service tier carried by ctx, or "" if unset.
func ServiceTierFromContext(ctx context.Context) string {
	t, _ := ctx.Value(serviceTierKey{}).(string)
	return t
}

// ContextWithCallProfile attaches a CallProfile to ctx. Empty profile
// returns ctx unchanged so callers can pass a config value verbatim
// without guarding for the unset case.
func ContextWithCallProfile(ctx context.Context, p CallProfile) context.Context {
	if p == "" {
		return ctx
	}
	return context.WithValue(ctx, callProfileKey{}, p)
}

// CallProfileFromContext returns the CallProfile carried by ctx, or ""
// if unset.
func CallProfileFromContext(ctx context.Context) CallProfile {
	p, _ := ctx.Value(callProfileKey{}).(CallProfile)
	return p
}
