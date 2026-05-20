package llm

import (
	"context"

	"github.com/sirupsen/logrus"
)

// Provider is the abstraction every LLM backend implements. The OpenAI-
// compatible client (*Client) and the native Gemini client both satisfy
// this surface; callers in synth, action, eval, and the HTTP layer hold
// Providers (not concrete types) so a new backend can be added without
// touching the call sites.
//
// The narrower chatCompleter seam in loop.go intentionally stays
// chat-only — RunLoop has no need for the wider surface.
type Provider interface {
	// ChatCompletion runs one chat completion. Streaming is selected
	// transparently via ctx-borne callbacks (see ContextWithStreamContent).
	ChatCompletion(ctx context.Context, messages []Message, tools []ToolDefinition, opts ...ChatOption) (ChatResult, error)

	// Reachable confirms the backend is up via a free/cheap probe. Used
	// by /api/health and the metrics publisher tick.
	Reachable(ctx context.Context) error

	// Model returns the configured default model name (for logging /
	// metrics labels).
	Model() string

	// JSONSchemaEnabled reports whether structured-output requests
	// should attach a response_format schema. Callers gate their
	// WithJSONSchema(...) options on this flag.
	JSONSchemaEnabled() bool

	// NoThink reports whether callers should suppress chain-of-thought
	// (e.g. by prepending "/no_think" on Qwen3-family models).
	NoThink() bool

	// NativeSearchEnabled reports whether the provider has its own
	// in-model web search grounding capability that's currently
	// enabled (Gemini's google_search tool). When true, synth surfaces
	// SHOULD NOT register the third-party search_web tool — instead
	// they attach WithGoogleSearch() to their ChatOptions and let the
	// provider ground responses natively, returning citations via
	// ChatResult.Citations. read_url and other URL-fetching tools
	// remain useful and should still be registered. Providers that
	// don't expose native grounding always return false.
	NativeSearchEnabled() bool

	// SetRetryInstrumentation wires the per-attempt logger and terminal
	// observer for the retry loop. Both are optional. Safe to call once
	// at boot before any concurrent traffic.
	SetRetryInstrumentation(logger *logrus.Entry, observer RetryObserver)

	// SetTrafficLogger wires a per-call DEBUG logger that emits one
	// line per upstream HTTP call. Nil clears it. Safe to call once at
	// boot.
	SetTrafficLogger(logger *logrus.Entry)
}

// Compile-time guarantee that the OpenAI-compatible *Client satisfies
// Provider. New backends should add a similar assertion next to their
// constructor.
var _ Provider = (*Client)(nil)
