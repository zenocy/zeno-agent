package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"
	"github.com/sirupsen/logrus"
)

// Client is a minimal wrapper over the OpenAI-compatible chat completions API.
// Network-level retries are handled here via RetryPolicy; tool-loop and
// repair retries live a layer up in loop.go / repair.go. JSON-schema response
// format support is gated by jsonSchemaMode ("off"|"auto"|"on") so callers
// can probe at startup and fall back to json_object when unsupported.
//
// Reasoning-model support: LM Studio and similar servers split chain-of-
// thought from final answer across `content` and `reasoning_content` fields.
// The sashabaranov library reads only `content`, so for those endpoints we
// run our own HTTP path (chatCompletionDirect) that reads both, falls back
// to reasoning_content when content is empty, and strips <think>...</think>
// tags before returning.
type Client struct {
	api              *openai.Client
	httpClient       *http.Client
	baseURL          string
	apiKey           string
	model            string
	timeout          time.Duration
	retry            RetryPolicy
	jsonSchemaMode   string // "off" | "auto" | "on"; 0-value treated as "off"
	defaultMaxTokens int    // applied to every ChatCompletion when caller doesn't override via WithMaxTokens
	noThink          bool   // when true, callers may inspect via NoThink() and prepend "/no_think" on supported models

	// streamSchema gates whether ChatCompletionStream serves
	// json_schema-constrained requests. Default true (the V2.4 hot
	// path). Flip to false when the P0 streaming smoke test shows
	// the endpoint produces malformed mid-stream JSON under
	// constrained decoding — the streaming path then falls back to
	// chatCompletionDirect for those calls. Text-only payloads
	// (briefing, Ask body) still stream regardless of this flag.
	streamSchema bool

	// trafficLogger emits one DEBUG line per upstream HTTP call
	// (chat completions + Reachable probes) so an operator can
	// audit token usage and spot pathological 1-token loops in the
	// log. Nil disables logging — the path stays silent for tests
	// that don't wire it.
	trafficLogger *logrus.Entry
}

// ClientConfig holds the constructor parameters.
type ClientConfig struct {
	Endpoint       string        // e.g. http://host.docker.internal:11434/v1
	APIKey         string        // may be empty for local endpoints
	Model          string        // e.g. "gemma3:4b"
	Timeout        time.Duration // 0 → 120s (per-HTTP-call transport timeout)
	Retry          RetryPolicy   // 0-valued fields fall back to retry defaults
	JSONSchemaMode string        // "off" (default) | "auto" | "on"
	MaxTokens      int           // 0 → no default (callers must pass WithMaxTokens explicitly)
	NoThink        bool          // when true, signals callers (e.g. briefing) to prepend "/no_think" on supported models

	// StreamSchema gates whether streaming serves json_schema-
	// constrained requests. The constructor defaults this to true
	// (zero-value-friendly inversion: see NewClient). Flip to false
	// after the V2.4 P0 streaming smoke test if the endpoint
	// produces malformed mid-stream JSON. Field name uses the
	// affirmative form so config files read naturally
	// (`stream_schema: false`).
	StreamSchema *bool
}

// NewClient constructs a Client.
func NewClient(cfg ClientConfig) *Client {
	if cfg.Endpoint == "" {
		cfg.Endpoint = "http://localhost:11434/v1"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 120 * time.Second
	}
	mode := cfg.JSONSchemaMode
	if mode == "" {
		mode = "off"
	}

	// streamSchema defaults to true so the zero-valued ClientConfig
	// gives the V2.4 hot-path behavior. Callers explicitly opt out
	// by setting StreamSchema to a *bool pointing at false (e.g.
	// after the P0 smoke test confirms incompatibility).
	streamSchema := true
	if cfg.StreamSchema != nil {
		streamSchema = *cfg.StreamSchema
	}

	baseURL := strings.TrimRight(cfg.Endpoint, "/")
	httpClient := &http.Client{Timeout: cfg.Timeout}

	apiCfg := openai.DefaultConfig(cfg.APIKey)
	apiCfg.BaseURL = baseURL
	apiCfg.HTTPClient = httpClient

	return &Client{
		api:              openai.NewClientWithConfig(apiCfg),
		httpClient:       httpClient,
		baseURL:          baseURL,
		apiKey:           cfg.APIKey,
		model:            cfg.Model,
		timeout:          cfg.Timeout,
		retry:            cfg.Retry,
		jsonSchemaMode:   mode,
		defaultMaxTokens: cfg.MaxTokens,
		noThink:          cfg.NoThink,
		streamSchema:     streamSchema,
	}
}

// NoThink reports whether callers should prepend "/no_think" to system
// prompts on supported (Qwen3-family) models. Default false; flipped via
// llm.no_think config or ZENO_LLM_NO_THINK env.
func (c *Client) NoThink() bool {
	if c == nil {
		return false
	}
	return c.noThink
}

// SetRetryInstrumentation wires the per-attempt logger and the terminal
// observer for the retry loop. Both are optional. Safe to call once at boot
// before any concurrent ChatCompletion traffic.
func (c *Client) SetRetryInstrumentation(logger *logrus.Entry, observer RetryObserver) {
	if c == nil {
		return
	}
	c.retry.Logger = logger
	c.retry.Observer = observer
}

// SetTrafficLogger wires the per-call DEBUG logger that emits one line
// per upstream HTTP call. Safe to call once at boot before any
// concurrent traffic. Nil clears it.
func (c *Client) SetTrafficLogger(logger *logrus.Entry) {
	if c == nil {
		return
	}
	c.trafficLogger = logger
}

// trafficLog emits a single DEBUG line if a traffic logger is wired,
// otherwise it's a no-op. Centralized so chat / stream / probe paths
// stay terse and consistent.
func (c *Client) trafficLog(fields logrus.Fields, msg string) {
	if c == nil || c.trafficLogger == nil {
		return
	}
	c.trafficLogger.WithFields(fields).Debug(msg)
}

// JSONSchemaEnabled reports whether the client should send a json_schema
// response_format on structured-output calls. Returns true when mode is "on"
// or "auto" (the auto probe defaults to true until Phase 0 wires a real
// startup probe; flipping to false on probe failure is a one-line change).
func (c *Client) JSONSchemaEnabled() bool {
	if c == nil {
		return false
	}
	switch c.jsonSchemaMode {
	case "on", "auto":
		return true
	default:
		return false
	}
}

// Reachable confirms the endpoint is up by issuing a free
// `GET /v1/models` request. Replaces the prior 1-token chat-completion
// "ping" which silently burned tokens against paid endpoints
// (Vertex/Gemini, OpenRouter) every time the metrics publisher tick
// fired (every 10s while a UI is connected) and on every /api/health
// hit. /v1/models is part of the OpenAI-compatible surface every major
// provider exposes (Ollama, LM Studio, vLLM, Vertex compat, OpenRouter)
// and returns no token usage.
//
// Used by metrics_publisher.computeHealth and the /api/health handler.
func (c *Client) Reachable(ctx context.Context) error {
	if c == nil {
		return errors.New("nil llm client")
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/models", nil)
	if err != nil {
		c.trafficLog(logrus.Fields{
			"purpose": "reachable",
			"error":   err.Error(),
		}, "llm: reachable probe build_request failed")
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.trafficLog(logrus.Fields{
			"purpose":     "reachable",
			"duration_ms": time.Since(start).Milliseconds(),
			"error":       err.Error(),
		}, "llm: reachable probe transport error")
		return err
	}
	defer resp.Body.Close()
	// Drain a small prefix so the connection can be reused; we don't
	// need the model list itself.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	if resp.StatusCode >= 400 {
		c.trafficLog(logrus.Fields{
			"purpose":     "reachable",
			"status":      resp.StatusCode,
			"duration_ms": time.Since(start).Milliseconds(),
		}, "llm: reachable probe non-2xx")
		return fmt.Errorf("models endpoint returned %d", resp.StatusCode)
	}
	c.trafficLog(logrus.Fields{
		"purpose":     "reachable",
		"status":      resp.StatusCode,
		"duration_ms": time.Since(start).Milliseconds(),
	}, "llm: reachable probe ok")
	return nil
}

// Model returns the configured default model name.
func (c *Client) Model() string {
	if c == nil {
		return ""
	}
	return c.model
}

// ChatOption tunes a single ChatCompletion call. Apply via WithTemperature,
// WithMaxTokens, etc.
type ChatOption func(*chatOpts)

type chatOpts struct {
	temperature    float32
	hasTemp        bool
	maxTokens      int
	jsonMode       bool
	jsonSchemaName string
	jsonSchemaRaw  []byte
}

// WithTemperature sets the sampling temperature for this call.
func WithTemperature(t float32) ChatOption {
	return func(o *chatOpts) { o.temperature = t; o.hasTemp = true }
}

// WithMaxTokens caps generated tokens (0 = upstream default).
func WithMaxTokens(n int) ChatOption {
	return func(o *chatOpts) { o.maxTokens = n }
}

// WithJSONMode requests `response_format: {"type":"json_object"}`. Only set
// for the briefing call where the model must return one JSON object.
func WithJSONMode() ChatOption {
	return func(o *chatOpts) { o.jsonMode = true }
}

// WithJSONSchema requests `response_format: {"type":"json_schema", ...}` with
// the given schema. Takes precedence over WithJSONMode when both are set.
// Schema must be a JSON-Schema object (the same shape GenerateSchema emits);
// the marshalled bytes are cached on the option so each call is cheap.
//
// Most local-model endpoints (llama.cpp grammars, vLLM guided JSON, Ollama
// with format=json + schema, OpenRouter on supporting models) honor this and
// constrain decoding to schema-valid output. If the endpoint rejects the
// field, the call surfaces a 400; the caller's repair loop will not help —
// flip llm.json_schema_mode back to "off" and rely on validation + repair.
func WithJSONSchema(name string, schema map[string]any) ChatOption {
	return func(o *chatOpts) {
		raw, err := json.Marshal(schema)
		if err != nil {
			return
		}
		o.jsonSchemaName = name
		o.jsonSchemaRaw = raw
	}
}

// shouldStream decides whether ChatCompletion routes a single call to
// the streaming path. The trigger: a non-nil StreamContent OR
// StreamThinking callback in ctx (a UI consumer is waiting for live
// deltas). When the request would carry a json_schema response_format
// AND the configured streamSchema is false, the decision flips back to
// non-streaming — schema-constrained streaming may produce malformed
// mid-stream JSON on some endpoints (the V2.4 P0 smoke test
// characterizes this; the flag is the runtime escape hatch).
func (c *Client) shouldStream(ctx context.Context, o *chatOpts) bool {
	if StreamContentFromContext(ctx) == nil && StreamThinkingFromContext(ctx) == nil {
		return false
	}
	if !c.streamSchema && o.jsonSchemaName != "" {
		return false
	}
	return true
}

// rawSchemaMarshaler adapts a pre-marshalled JSON-Schema byte slice to the
// json.Marshaler interface that openai.ChatCompletionResponseFormatJSONSchema
// expects.
type rawSchemaMarshaler struct{ raw []byte }

func (r rawSchemaMarshaler) MarshalJSON() ([]byte, error) { return r.raw, nil }

// ChatCompletion runs one chat completion. The decision to stream is
// transparent to callers: if ctx carries a StreamContent or
// StreamThinking callback (V2.4 live-trace surface), the call routes
// to ChatCompletionStream so the UI sees deltas as they arrive. With
// no streaming callbacks attached, it stays on the non-streaming
// chatCompletionDirect path that V2.3 has run in production —
// avoiding a stream-protocol regression on every read of cards/
// briefings/Ask done by replay, eval, or tests.
//
// Tool-call argument strings are JSON-decoded into ToolCall.Arguments
// regardless of which path served the call; per-call parse failures
// are reported via ChatResult.ToolArgsErrors so the caller's repair
// flow can drive a bounded round-trip.
func (c *Client) ChatCompletion(
	ctx context.Context,
	messages []Message,
	tools []ToolDefinition,
	opts ...ChatOption,
) (ChatResult, error) {
	if c == nil {
		return ChatResult{}, errors.New("nil llm client")
	}
	o := chatOpts{}
	for _, opt := range opts {
		opt(&o)
	}

	// V2.4 router: stream when a UI consumer is listening AND the
	// schema-streaming gate allows it for this particular call.
	// Without callbacks the request stays on the non-streaming path —
	// no behavior change for V2.3 callers (eval, replay, tests).
	if c.shouldStream(ctx, &o) {
		return c.ChatCompletionStream(ctx, messages, tools, opts...)
	}

	req := openai.ChatCompletionRequest{
		Model:    c.model,
		Messages: convertMessagesOut(messages),
	}
	if len(tools) > 0 {
		req.Tools = convertToolsOut(tools)
	}
	if o.hasTemp {
		req.Temperature = o.temperature
	}
	if o.maxTokens > 0 {
		req.MaxTokens = o.maxTokens
	} else if c.defaultMaxTokens > 0 {
		req.MaxTokens = c.defaultMaxTokens
	}
	switch {
	case o.jsonSchemaName != "" && len(o.jsonSchemaRaw) > 0:
		req.ResponseFormat = &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatTypeJSONSchema,
			JSONSchema: &openai.ChatCompletionResponseFormatJSONSchema{
				Name:   o.jsonSchemaName,
				Schema: rawSchemaMarshaler{raw: o.jsonSchemaRaw},
				Strict: true,
			},
		}
	case o.jsonMode:
		req.ResponseFormat = &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatTypeJSONObject,
		}
	}

	start := time.Now()
	var thinking string
	resp, err := retryChat(ctx, c.retry, func(ctx context.Context) (openai.ChatCompletionResponse, error) {
		r, t, e := c.chatCompletionDirect(ctx, req)
		// Last successful attempt wins. Captures the thinking from the
		// response we're actually returning; earlier failed attempts'
		// thinking is irrelevant.
		if e == nil {
			thinking = t
		}
		return r, e
	})
	if err != nil {
		c.trafficLog(logrus.Fields{
			"purpose":     "chat",
			"path":        "direct",
			"model":       c.model,
			"messages":    len(messages),
			"tools":       len(tools),
			"max_tokens":  req.MaxTokens,
			"json_mode":   o.jsonMode,
			"json_schema": o.jsonSchemaName,
			"duration_ms": time.Since(start).Milliseconds(),
			"error":       err.Error(),
		}, "llm: chat completion failed")
		return ChatResult{}, err
	}
	finish := ""
	if len(resp.Choices) > 0 {
		finish = string(resp.Choices[0].FinishReason)
	}
	c.trafficLog(logrus.Fields{
		"purpose":           "chat",
		"path":              "direct",
		"model":             c.model,
		"messages":          len(messages),
		"tools":             len(tools),
		"max_tokens":        req.MaxTokens,
		"json_mode":         o.jsonMode,
		"json_schema":       o.jsonSchemaName,
		"prompt_tokens":     resp.Usage.PromptTokens,
		"completion_tokens": resp.Usage.CompletionTokens,
		"finish_reason":     finish,
		"duration_ms":       time.Since(start).Milliseconds(),
	}, "llm: chat completion ok")

	out := ChatResult{TotalDuration: time.Since(start)}
	out.PromptTokens = resp.Usage.PromptTokens
	out.CompletionTokens = resp.Usage.CompletionTokens
	out.ThinkingContent = thinking

	if len(resp.Choices) == 0 {
		out.Empty = true
		return out, nil
	}
	choice := resp.Choices[0]
	out.FinishReason = string(choice.FinishReason)
	out.Content = choice.Message.Content

	for _, tc := range choice.Message.ToolCalls {
		args := map[string]any{}
		if strings.TrimSpace(tc.Function.Arguments) != "" {
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				out.ToolArgsErrors = append(out.ToolArgsErrors, ToolArgsParseError{
					ToolCallID:  tc.ID,
					Name:        tc.Function.Name,
					RawJSON:     tc.Function.Arguments,
					ParseErrMsg: err.Error(),
				})
				// Still include the tool call so the loop can correlate the
				// repair attempt by ID.
				out.ToolCalls = append(out.ToolCalls, ToolCall{
					ID:        tc.ID,
					Name:      tc.Function.Name,
					Arguments: nil,
				})
				continue
			}
		}
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: args,
		})
	}

	if out.Content == "" && len(out.ToolCalls) == 0 && len(out.ToolArgsErrors) == 0 {
		out.Empty = true
	}
	return out, nil
}

// convertMessagesOut maps internal Message → openai.ChatCompletionMessage.
// ToolCalls on assistant messages are serialized as JSON-encoded arguments
// strings to match the OpenAI wire format.
func convertMessagesOut(in []Message) []openai.ChatCompletionMessage {
	out := make([]openai.ChatCompletionMessage, 0, len(in))
	for _, m := range in {
		om := openai.ChatCompletionMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
		}
		if len(m.ToolCalls) > 0 {
			om.ToolCalls = make([]openai.ToolCall, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				args, _ := json.Marshal(tc.Arguments)
				om.ToolCalls = append(om.ToolCalls, openai.ToolCall{
					ID:   tc.ID,
					Type: openai.ToolTypeFunction,
					Function: openai.FunctionCall{
						Name:      tc.Name,
						Arguments: string(args),
					},
				})
			}
		}
		out = append(out, om)
	}
	return out
}

// convertToolsOut maps internal ToolDefinition → openai.Tool with the
// function.parameters built as a JSON-Schema object from the param specs.
func convertToolsOut(defs []ToolDefinition) []openai.Tool {
	out := make([]openai.Tool, 0, len(defs))
	for _, d := range defs {
		out = append(out, openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        d.Name,
				Description: d.Description,
				Parameters:  paramsToSchema(d.Parameters),
			},
		})
	}
	return out
}

// paramsToSchema converts a flat list of ToolParamSpec into the JSON-Schema
// object the OpenAI tool API expects (an `object` with `properties` and
// `required`).
func paramsToSchema(params []ToolParamSpec) map[string]any {
	props := map[string]any{}
	required := []string{}
	for _, p := range params {
		entry := map[string]any{}
		if p.Type != "" {
			entry["type"] = p.Type
		}
		if p.Description != "" {
			entry["description"] = p.Description
		}
		if len(p.Enum) > 0 {
			vals := make([]any, 0, len(p.Enum))
			for _, v := range p.Enum {
				vals = append(vals, v)
			}
			entry["enum"] = vals
		}
		if p.Items != nil {
			entry["items"] = map[string]any{"type": p.Items.Type}
		}
		props[p.Name] = entry
		if p.Required {
			required = append(required, p.Name)
		}
	}
	out := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}
