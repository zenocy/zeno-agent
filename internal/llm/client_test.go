package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNewClient_AppliesDefaults(t *testing.T) {
	c := NewClient(ClientConfig{Model: "gpt-test"})
	require.Equal(t, "gpt-test", c.Model())
	require.Equal(t, "http://localhost:11434/v1", c.baseURL,
		"empty endpoint must default to localhost:11434/v1")
	require.Equal(t, 120*time.Second, c.timeout)
	require.Equal(t, "off", c.jsonSchemaMode, "empty mode normalizes to off")
	require.False(t, c.JSONSchemaEnabled())
	require.False(t, c.NoThink())
	require.True(t, c.streamSchema, "default zero-value StreamSchema must enable schema streaming")
}

func TestNewClient_TrimsTrailingSlash(t *testing.T) {
	c := NewClient(ClientConfig{Endpoint: "http://x.test/v1/"})
	require.Equal(t, "http://x.test/v1", c.baseURL)
}

func TestClient_JSONSchemaEnabled(t *testing.T) {
	cases := []struct {
		mode string
		want bool
	}{
		{"on", true},
		{"auto", true},
		{"off", false},
		{"", false},
		{"garbage", false},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			c := NewClient(ClientConfig{JSONSchemaMode: tc.mode})
			require.Equal(t, tc.want, c.JSONSchemaEnabled())
		})
	}
}

func TestClient_StreamSchemaExplicitOff(t *testing.T) {
	off := false
	c := NewClient(ClientConfig{StreamSchema: &off})
	require.False(t, c.streamSchema, "explicit StreamSchema=&false must disable schema streaming")
}

func TestClient_NoThinkPropagates(t *testing.T) {
	c := NewClient(ClientConfig{NoThink: true})
	require.True(t, c.NoThink())
}

func TestClient_NilReceiverIsSafe(t *testing.T) {
	var c *Client
	require.False(t, c.NoThink(), "nil receiver must not panic")
	require.False(t, c.JSONSchemaEnabled())
	require.Equal(t, "", c.Model())
	require.Error(t, c.Reachable(context.Background()))
}

// shouldStream routes to the streaming path only when (a) the ctx carries a
// stream callback AND (b) the call's schema-streaming gate allows it.
func TestShouldStream_NoCallbacks(t *testing.T) {
	c := NewClient(ClientConfig{})
	require.False(t, c.shouldStream(context.Background(), &chatOpts{}))
}

func TestShouldStream_CallbacksPresent(t *testing.T) {
	c := NewClient(ClientConfig{})
	ctx := ContextWithStreamContent(context.Background(), StreamContentFunc(func(string) {}))
	require.True(t, c.shouldStream(ctx, &chatOpts{}))
}

// streamSchema=false + a json_schema request must drop back to non-streaming
// even when the ctx is wired for streaming. This is the V2.4 P0 escape hatch.
func TestShouldStream_SchemaGatedOff(t *testing.T) {
	off := false
	c := NewClient(ClientConfig{StreamSchema: &off})
	ctx := ContextWithStreamContent(context.Background(), StreamContentFunc(func(string) {}))
	require.False(t, c.shouldStream(ctx, &chatOpts{jsonSchemaName: "card"}),
		"schema-gated streaming off must keep schema calls on the non-streaming path")
	require.True(t, c.shouldStream(ctx, &chatOpts{}),
		"text-only calls still stream when callbacks are wired")
}

// paramsToSchema produces an OpenAI-shaped JSON schema. Required-vs-optional
// must round-trip; enum and array items must surface; an empty params list
// must still emit a valid object schema with no required key.
func TestParamsToSchema_RequiredAndOptional(t *testing.T) {
	out := paramsToSchema([]ToolParamSpec{
		{Name: "id", Type: "string", Required: true, Description: "the id"},
		{Name: "tags", Type: "array", Required: false, Items: &ToolParamItems{Type: "string"}},
		{Name: "kind", Type: "string", Enum: []string{"a", "b"}, Required: true},
	})
	require.Equal(t, "object", out["type"])

	props, ok := out["properties"].(map[string]any)
	require.True(t, ok, "properties must be a map")
	require.Contains(t, props, "id")
	require.Contains(t, props, "tags")
	require.Contains(t, props, "kind")

	required, ok := out["required"].([]string)
	require.True(t, ok)
	require.ElementsMatch(t, []string{"id", "kind"}, required)

	tags := props["tags"].(map[string]any)
	items, ok := tags["items"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "string", items["type"])

	kind := props["kind"].(map[string]any)
	enum, ok := kind["enum"].([]any)
	require.True(t, ok)
	require.Len(t, enum, 2)
}

func TestParamsToSchema_NoRequiredOmitsKey(t *testing.T) {
	out := paramsToSchema(nil)
	require.Equal(t, "object", out["type"])
	require.NotContains(t, out, "required",
		"empty param list must omit `required` (OpenAI rejects empty arrays)")
}

func TestConvertMessagesOut_PassesToolCallID(t *testing.T) {
	in := []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "tc-1", Name: "search", Arguments: map[string]any{"q": "go"}}}},
		{Role: "tool", Content: "result", ToolCallID: "tc-1"},
	}
	out := convertMessagesOut(in)
	require.Len(t, out, 3)
	require.Equal(t, "user", out[0].Role)
	require.Len(t, out[1].ToolCalls, 1)
	require.Equal(t, "tc-1", out[1].ToolCalls[0].ID)
	require.Equal(t, "search", out[1].ToolCalls[0].Function.Name)
	// Arguments are JSON-marshalled to the wire.
	var args map[string]any
	require.NoError(t, json.Unmarshal([]byte(out[1].ToolCalls[0].Function.Arguments), &args))
	require.Equal(t, "go", args["q"])
	require.Equal(t, "tc-1", out[2].ToolCallID)
}

// Reachable hits a real HTTP endpoint via the GET /models probe. The
// chat-completion ping it replaced silently burned 1 token per call on
// paid endpoints; /models is free and universally supported on
// OpenAI-compatible servers. The handler asserts the path so a future
// refactor to chat-completions on this hot path is caught here.
func TestReachable_OKAgainstStubServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method,
			"reachable must not POST — only GET /models is free of token charges")
		require.Equal(t, "/models", r.URL.Path)
		_, _ = io.WriteString(w, `{"object":"list","data":[]}`)
	}))
	defer srv.Close()

	c := NewClient(ClientConfig{Endpoint: srv.URL, Model: "stub"})
	require.NoError(t, c.Reachable(context.Background()))
}

func TestReachable_ServerErrorSurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error": "kaboom"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(ClientConfig{Endpoint: srv.URL, Model: "stub", Retry: RetryPolicy{MaxAttempts: 1}})
	require.Error(t, c.Reachable(context.Background()))
}
