// Package llm holds the data types shared across the LLM client, the tool
// loop, and the synthesizer. Ported from zeno-common/llm.
package llm

import "time"

// Message is one entry in a chat conversation.
type Message struct {
	Role            string     `json:"role"`
	Content         string     `json:"content"`
	ToolCallID      string     `json:"tool_call_id,omitempty"`
	ToolCalls       []ToolCall `json:"tool_calls,omitempty"`
	CacheBreakpoint bool       `json:"cache_breakpoint,omitempty"`

	// NoMerge is honored by message normalization to keep this message from
	// merging with an adjacent same-role one. Used to keep a STATIC cache-prefix
	// separated from DYNAMIC content (memories, skills) so dynamic edits don't
	// bust the static cache. Not serialized.
	NoMerge bool `json:"-"`
}

// ToolDefinition declares a callable tool.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  []ToolParamSpec `json:"parameters"`
}

// ToolParamSpec is one parameter of a tool.
type ToolParamSpec struct {
	Name        string          `json:"name"`
	Type        string          `json:"type"`
	Description string          `json:"description"`
	Required    bool            `json:"required"`
	Enum        []string        `json:"enum,omitempty"`
	Items       *ToolParamItems `json:"items,omitempty"`
}

// ToolParamItems describes the element schema for an array parameter.
type ToolParamItems struct {
	Type string `json:"type"`
}

// ToolCall is one tool invocation produced by the model.
type ToolCall struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`

	// ProviderState is opaque bytes the originating provider asks us
	// to echo back verbatim when this tool call is re-sent in the next
	// turn's message history. Today: Gemini's thought_signature, which
	// the model emits alongside each FunctionCall when thinking is on
	// and which Gemini's API rejects (HTTP 400 INVALID_ARGUMENT) if
	// stripped on echo-back. OpenAI-compatible providers don't have an
	// equivalent and leave this field empty / ignore it on input.
	// Not serialized — it's binary and an implementation detail of the
	// model's internal reasoning chain, not part of the persisted trace.
	ProviderState []byte `json:"-"`
}

// Citation is one source reference attached to a ChatResult. Populated
// by providers that surface grounding metadata (e.g. Gemini with
// Google Search grounding). StartIndex/EndIndex are byte offsets into
// Content marking the supported span; zero values mean the citation
// applies to the whole response.
type Citation struct {
	Title      string
	URI        string
	StartIndex int
	EndIndex   int
}

// ChatResult is the outcome of a chat completion.
type ChatResult struct {
	Content           string
	ThinkingContent   string
	ToolCalls         []ToolCall
	PromptTokens      int
	CachedTokens      int
	CompletionTokens  int
	TotalDuration     time.Duration
	FirstByteDuration time.Duration
	RawRequestJSON    string
	RawResponseJSON   string

	// Degraded means the resilient layer fell back to a static response. The
	// caller must skip the tool loop and surface Content verbatim.
	Degraded bool

	// Partial means streaming ended with content already flushed but without a
	// clean finish_reason. Content is valid but incomplete; do not retry.
	Partial bool

	// Empty means the upstream returned a syntactically valid response with no
	// choices, content, or tool calls. Used to justify one extra retry.
	Empty bool

	// FinishReason is the raw finish_reason returned by the upstream, if any.
	// Notable provider-specific values: Gemini surfaces "SAFETY" or
	// "PROHIBITED_CONTENT" when the model refused — these are not Empty
	// and not retryable; callers must inspect and degrade gracefully.
	FinishReason string

	// ToolArgsErrors collects per-tool-call JSON parse failures. When non-empty
	// the tool loop must engage the repair flow before dispatching tool calls.
	ToolArgsErrors []ToolArgsParseError

	// Citations carries provider-supplied grounding metadata (currently
	// only Gemini's Google Search grounding). Empty on providers that
	// don't expose grounding. The loop merges these into the trace via
	// recordCitations after a natural-exit response.
	Citations []Citation
}
