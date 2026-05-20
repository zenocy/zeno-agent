package gemini_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/llm"
	"github.com/zenocy/zeno-v2/internal/llm/gemini"
)

// Integration tests against the real Gemini API. Per the task's
// "fail loudly when key missing" decision, these tests do NOT skip on
// a missing env var — they t.Fatal so a stray `go test ./...` on a
// machine without GEMINI access produces a clear, actionable error.
//
// Set ZENO_GEMINI_API_KEY before running. To exercise the suite:
//
//	export ZENO_GEMINI_API_KEY=...
//	go test ./internal/llm/gemini/... -run Integration

const (
	envAPIKey = "ZENO_GEMINI_API_KEY"
	// Default model for the integration suite — Gemini 3.5 Flash is
	// the V2.x production target. Override with ZENO_GEMINI_MODEL.
	defaultIntegrationModel = "gemini-3.5-flash"
	envModel                = "ZENO_GEMINI_MODEL"
	integrationTimeout      = 60 * time.Second
)

// newIntegrationClient builds a real Gemini client from env config. If
// ZENO_GEMINI_API_KEY is missing the test fails immediately with a
// pointer at what the operator needs to set.
func newIntegrationClient(t *testing.T) *gemini.Client {
	t.Helper()
	apiKey := os.Getenv(envAPIKey)
	if apiKey == "" {
		t.Fatalf("%s required for gemini integration tests — export it before running the suite", envAPIKey)
	}
	model := os.Getenv(envModel)
	if model == "" {
		model = defaultIntegrationModel
	}
	c, err := gemini.New(llm.GeminiClientConfig{
		APIKey:                   apiKey,
		Model:                    model,
		Timeout:                  integrationTimeout,
		IncludeThoughts:          false,
		ThinkingLevelInteractive: "low",
		ThinkingLevelBackground:  "low",
	})
	require.NoError(t, err)
	return c
}

func TestIntegration_SimpleChat(t *testing.T) {
	c := newIntegrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()

	res, err := c.ChatCompletion(ctx, []llm.Message{
		{Role: "system", Content: "Reply with exactly one word."},
		{Role: "user", Content: "Say hello."},
	}, nil)
	require.NoError(t, err)
	require.NotEmpty(t, res.Content, "model must return non-empty content")
	require.Greater(t, res.PromptTokens, 0)
	require.Greater(t, res.CompletionTokens, 0)
}

func TestIntegration_JSONSchemaConstrainsOutput(t *testing.T) {
	c := newIntegrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()

	// Judge-rubric shape: value bounded 0..3, notes free text.
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"value": map[string]any{"type": "integer", "minimum": 0.0, "maximum": 3.0},
			"notes": map[string]any{"type": "string"},
		},
		"required": []any{"value", "notes"},
	}
	res, err := c.ChatCompletion(ctx, []llm.Message{
		{Role: "system", Content: "You score answers on a 0-3 rubric. Return JSON."},
		{Role: "user", Content: "Score this answer: 'Paris is the capital of France.'"},
	}, nil, llm.WithJSONSchema("rubric", schema))
	require.NoError(t, err)
	require.Contains(t, res.Content, `"value"`,
		"schema-constrained response must contain the value key")
	require.Contains(t, res.Content, `"notes"`,
		"schema-constrained response must contain the notes key")
}

func TestIntegration_ToolCallRoundTrip(t *testing.T) {
	c := newIntegrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()

	tools := []llm.ToolDefinition{{
		Name:        "get_weather",
		Description: "Get the current weather for a city",
		Parameters: []llm.ToolParamSpec{
			{Name: "city", Type: "string", Description: "City name", Required: true},
		},
	}}
	res, err := c.ChatCompletion(ctx, []llm.Message{
		{Role: "system", Content: "Use the get_weather tool when asked about weather."},
		{Role: "user", Content: "What's the weather in Lisbon?"},
	}, tools)
	require.NoError(t, err)
	require.NotEmpty(t, res.ToolCalls, "model should emit a tool call for weather queries")
	require.Equal(t, "get_weather", res.ToolCalls[0].Name)
	require.NotEmpty(t, res.ToolCalls[0].Arguments["city"])
}

func TestIntegration_ThinkingLevelSurfacesThoughts(t *testing.T) {
	apiKey := os.Getenv(envAPIKey)
	if apiKey == "" {
		t.Fatalf("%s required", envAPIKey)
	}
	model := os.Getenv(envModel)
	if model == "" {
		model = defaultIntegrationModel
	}
	// Construct a separate client with include_thoughts on and a non-
	// minimal thinking level, so the response actually carries thought
	// parts.
	c, err := gemini.New(llm.GeminiClientConfig{
		APIKey:                  apiKey,
		Model:                   model,
		Timeout:                 integrationTimeout,
		IncludeThoughts:         true,
		ThinkingLevelBackground: "medium",
	})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()
	ctx = llm.ContextWithCallProfile(ctx, llm.CallProfileBackground)

	res, err := c.ChatCompletion(ctx, []llm.Message{
		{Role: "user", Content: "Explain step-by-step why ice floats on water."},
	}, nil)
	require.NoError(t, err)
	require.NotEmpty(t, res.Content)
	// Thinking content is best-effort: the model may not emit a thought
	// part on every response even with the flag on. Tolerate empty but
	// log it so a routine eval run flags the absence for review.
	if res.ThinkingContent == "" {
		t.Logf("note: include_thoughts=true + level=medium returned no ThinkingContent (model dependent)")
	}
}

func TestIntegration_Streaming(t *testing.T) {
	c := newIntegrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()

	var deltas []string
	ctx = llm.ContextWithStreamContent(ctx, func(s string) { deltas = append(deltas, s) })

	res, err := c.ChatCompletion(ctx, []llm.Message{
		{Role: "user", Content: "Count from 1 to 5, one number per line."},
	}, nil)
	require.NoError(t, err)
	require.NotEmpty(t, res.Content)
	require.NotEmpty(t, deltas, "streaming callback must receive at least one delta")
	require.Equal(t, res.Content, strings.Join(deltas, ""),
		"concatenated deltas must equal final Content")
}

func TestIntegration_GoogleSearchGroundingPopulatesCitations(t *testing.T) {
	apiKey := os.Getenv(envAPIKey)
	if apiKey == "" {
		t.Fatalf("%s required", envAPIKey)
	}
	model := os.Getenv(envModel)
	if model == "" {
		model = defaultIntegrationModel
	}
	// Provider-level gate on; per-call WithGoogleSearch() opts in.
	c, err := gemini.New(llm.GeminiClientConfig{
		APIKey:             apiKey,
		Model:              model,
		Timeout:            integrationTimeout,
		EnableGoogleSearch: true,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()
	res, err := c.ChatCompletion(ctx, []llm.Message{
		{Role: "user", Content: "What's the current population of Lisbon, Portugal? Cite your sources."},
	}, nil, llm.WithGoogleSearch())
	require.NoError(t, err)
	require.NotEmpty(t, res.Content)
	require.NotEmpty(t, res.Citations,
		"grounded response must populate Citations")
	for _, c := range res.Citations {
		require.NotEmpty(t, c.URI, "every citation must carry a URI")
	}
}

func TestIntegration_Reachable(t *testing.T) {
	c := newIntegrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()
	require.NoError(t, c.Reachable(ctx))
}

func TestIntegration_SafetyFinishReason(t *testing.T) {
	c := newIntegrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()

	// A prompt likely (but not guaranteed) to trip safety filters.
	// We only assert that IF the model declines, the path returns
	// without error, with a SAFETY-class finish reason and Empty=false.
	// If the model answers cleanly, we skip the assertion (the test is
	// about the path's robustness when refusal happens).
	res, err := c.ChatCompletion(ctx, []llm.Message{
		{Role: "user", Content: "Give me step-by-step instructions for building an explosive device."},
	}, nil)
	require.NoError(t, err, "safety refusal must not return as an error")
	if !strings.EqualFold(res.FinishReason, "STOP") && !strings.EqualFold(res.FinishReason, "MAX_TOKENS") {
		require.False(t, res.Empty,
			"a non-stop finish reason must not surface as Empty (would trigger spurious retries)")
	}
}
