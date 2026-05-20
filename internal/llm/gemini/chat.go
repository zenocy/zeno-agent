package gemini

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"google.golang.org/genai"

	"github.com/zenocy/zeno-v2/internal/llm"
)

// ChatCompletion is the Gemini implementation of llm.Provider's chat
// surface. Routing matches the OpenAI client: when ctx carries a
// StreamContent or StreamThinking callback, the call goes to the
// streaming path; otherwise it stays on the non-streaming path that
// eval / replay / tests exercise.
func (c *Client) ChatCompletion(
	ctx context.Context,
	messages []llm.Message,
	tools []llm.ToolDefinition,
	opts ...llm.ChatOption,
) (llm.ChatResult, error) {
	if c == nil {
		return llm.ChatResult{}, errors.New("nil gemini client")
	}
	params := llm.ApplyChatOptions(opts)

	system, contents := convertMessages(messages)

	cfg := &genai.GenerateContentConfig{
		SystemInstruction: system,
	}
	if params.HasTemperature {
		t := params.Temperature
		cfg.Temperature = &t
	}
	maxTokens := params.MaxTokens
	if maxTokens == 0 {
		maxTokens = c.cfg.MaxTokens
	}
	if maxTokens > 0 {
		cfg.MaxOutputTokens = int32(maxTokens)
	}

	useGoogleSearch := params.GoogleSearch && c.cfg.EnableGoogleSearch
	if len(tools) > 0 || useGoogleSearch {
		cfg.Tools = convertTools(tools, useGoogleSearch)
	}
	// Gemini rejects mixing the built-in google_search tool with
	// function declarations unless the request explicitly opts in via
	// tool_config.include_server_side_tool_invocations=true. The flag
	// also makes the response include the server-side tool calls in
	// the Content stream, which is exactly what we need so the trace
	// surface can see when grounding fired.
	if useGoogleSearch && len(tools) > 0 {
		yes := true
		cfg.ToolConfig = &genai.ToolConfig{IncludeServerSideToolInvocations: &yes}
	}

	if params.JSONSchemaName != "" && len(params.JSONSchemaRaw) > 0 {
		// Re-decode the cached schema bytes into a map[string]any so we
		// can normalize through schemaToGemini. The OpenAI path treats
		// JSONSchemaRaw as opaque bytes; for Gemini we need the typed
		// form.
		var rawMap map[string]any
		if err := decodeJSONBytes(params.JSONSchemaRaw, &rawMap); err == nil {
			schema, err := schemaToGemini(rawMap)
			if err != nil {
				return llm.ChatResult{}, fmt.Errorf("gemini: schema %q: %w", params.JSONSchemaName, err)
			}
			cfg.ResponseMIMEType = "application/json"
			cfg.ResponseSchema = schema
		}
	} else if params.JSONMode {
		cfg.ResponseMIMEType = "application/json"
	}

	// Thinking config: include thoughts + thinking level (Gemini 3+).
	level := c.resolveThinkingLevel(ctx, params.ThinkingLevel)
	includeThoughts := c.resolveIncludeThoughts(params.IncludeThoughts)
	if level != "" || includeThoughts {
		tc := &genai.ThinkingConfig{IncludeThoughts: includeThoughts}
		if level != "" {
			mapped, ok := mapThinkingLevel(level)
			if !ok {
				return llm.ChatResult{}, fmt.Errorf("gemini: invalid thinking_level %q (allowed: minimal, low, medium, high)", level)
			}
			tc.ThinkingLevel = mapped
		}
		cfg.ThinkingConfig = tc
	}

	// Service tier: per-call WithServiceTier wins; otherwise the
	// CallProfile + client config mapping (background/interactive)
	// drives the value.
	if tier := c.resolveServiceTier(ctx, params.ServiceTier); tier != "" {
		mapped, ok := mapServiceTier(tier)
		if !ok {
			return llm.ChatResult{}, fmt.Errorf("gemini: invalid service_tier %q (allowed: flex, standard, priority)", tier)
		}
		cfg.ServiceTier = mapped
	}

	if c.shouldStream(ctx) {
		return c.chatCompletionStream(ctx, contents, cfg)
	}
	return c.chatCompletionDirect(ctx, contents, cfg)
}

func (c *Client) chatCompletionDirect(
	ctx context.Context,
	contents []*genai.Content,
	cfg *genai.GenerateContentConfig,
) (llm.ChatResult, error) {
	start := time.Now()
	resp, err := retryCall(ctx, c.retry, func(ctx context.Context) (*genai.GenerateContentResponse, error) {
		return c.api.Models.GenerateContent(ctx, c.model, contents, cfg)
	})
	dur := time.Since(start)
	if err != nil {
		c.trafficLog(logrus.Fields{
			"purpose":     "chat",
			"path":        "direct",
			"model":       c.model,
			"duration_ms": dur.Milliseconds(),
			"error":       err.Error(),
		}, "gemini: chat completion failed")
		return llm.ChatResult{}, err
	}
	out := convertResult(resp)
	out.TotalDuration = dur

	c.trafficLog(logrus.Fields{
		"purpose":           "chat",
		"path":              "direct",
		"model":             c.model,
		"prompt_tokens":     out.PromptTokens,
		"cached_tokens":     out.CachedTokens,
		"completion_tokens": out.CompletionTokens,
		"finish_reason":     out.FinishReason,
		"tool_calls":        len(out.ToolCalls),
		"citations":         len(out.Citations),
		"duration_ms":       dur.Milliseconds(),
	}, "gemini: chat completion ok")
	return out, nil
}

// shouldStream reports whether a streaming UI callback is attached to
// the context — the trigger for routing through the streaming path.
func (c *Client) shouldStream(ctx context.Context) bool {
	return llm.StreamContentFromContext(ctx) != nil || llm.StreamThinkingFromContext(ctx) != nil
}

// mapThinkingLevel maps the lowercase config string to the genai
// ThinkingLevel enum. Returns ok=false for an unrecognized value so
// callers can fail loudly rather than silently dropping the knob.
func mapThinkingLevel(level string) (genai.ThinkingLevel, bool) {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "minimal":
		return genai.ThinkingLevelMinimal, true
	case "low":
		return genai.ThinkingLevelLow, true
	case "medium":
		return genai.ThinkingLevelMedium, true
	case "high":
		return genai.ThinkingLevelHigh, true
	}
	return "", false
}

// mapServiceTier maps the lowercase config string to the genai
// ServiceTier enum. Empty returns false → the caller omits the
// service_tier field on the request (Gemini defaults to standard).
func mapServiceTier(tier string) (genai.ServiceTier, bool) {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "flex":
		return genai.ServiceTierFlex, true
	case "standard":
		return genai.ServiceTierStandard, true
	case "priority":
		return genai.ServiceTierPriority, true
	}
	return "", false
}
