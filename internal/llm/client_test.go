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

// captureServiceTier stands up a stub OpenAI-compatible /chat/completions
// endpoint, captures the JSON body the client sends, and returns the
// captured service_tier (or "" if absent). Used by the next four tests to
// verify the request-build path picks the right tier without coupling to
// the upstream OpenRouter API.
func captureServiceTier(t *testing.T, ctx context.Context, opts ...ChatOption) (string, bool) {
	t.Helper()
	var (
		gotTier    string
		tierField  bool
		gotRequest = make(chan struct{}, 1)
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		if v, ok := payload["service_tier"]; ok {
			tierField = true
			gotTier, _ = v.(string)
		}
		select {
		case gotRequest <- struct{}{}:
		default:
		}
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`)
	}))
	defer srv.Close()

	c := NewClient(ClientConfig{Endpoint: srv.URL, Model: "stub", Retry: RetryPolicy{MaxAttempts: 1}})
	_, err := c.ChatCompletion(ctx, []Message{{Role: "user", Content: "hi"}}, nil, opts...)
	require.NoError(t, err)
	<-gotRequest
	return gotTier, tierField
}

func TestServiceTier_OptionWins(t *testing.T) {
	ctx := ContextWithServiceTier(context.Background(), "flex")
	got, present := captureServiceTier(t, ctx, WithServiceTier("priority"))
	require.True(t, present, "option set: field must be present")
	require.Equal(t, "priority", got, "WithServiceTier must override ctx-borne tier")
}

func TestServiceTier_CtxFallback(t *testing.T) {
	ctx := ContextWithServiceTier(context.Background(), "flex")
	got, present := captureServiceTier(t, ctx)
	require.True(t, present)
	require.Equal(t, "flex", got, "ctx tier is used when no per-call option")
}

func TestServiceTier_OmittedWhenUnset(t *testing.T) {
	_, present := captureServiceTier(t, context.Background())
	require.False(t, present,
		"with no option and no ctx tier, request must NOT include service_tier")
}

func TestServiceTier_EmptyOptionIsNoop(t *testing.T) {
	// WithServiceTier("") must not stomp the ctx fallback — empty string
	// should behave like "operator didn't set anything for this call".
	ctx := ContextWithServiceTier(context.Background(), "flex")
	got, present := captureServiceTier(t, ctx, WithServiceTier(""))
	require.True(t, present)
	require.Equal(t, "flex", got)
}

func TestContextWithServiceTier_EmptyReturnsSameCtx(t *testing.T) {
	parent := context.Background()
	child := ContextWithServiceTier(parent, "")
	require.Equal(t, "", ServiceTierFromContext(child),
		"empty tier must not stash a value")
}

// captureStreamServiceTier stands up a stub OpenAI-compatible
// /chat/completions endpoint that speaks Server-Sent Events, captures
// the JSON body the client sends, and returns the service_tier value.
// Used to prove the streaming-path request build sets ServiceTier on
// the wire — the direct-path tests can't cover that since the stream
// path goes through the openai SDK's marshaller, not chatCompletionDirect.
func captureStreamServiceTier(t *testing.T, ctx context.Context, opts ...ChatOption) (string, bool) {
	t.Helper()
	var (
		gotTier   string
		tierField bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		if v, ok := payload["service_tier"]; ok {
			tierField = true
			gotTier, _ = v.(string)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, ok := w.(http.Flusher)
		require.True(t, ok, "test server must support flushing for SSE")
		// One content chunk + a usage chunk + [DONE]. Enough to exit
		// aggregateStream cleanly; we don't care about the payload.
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	c := NewClient(ClientConfig{Endpoint: srv.URL, Model: "stub", Retry: RetryPolicy{MaxAttempts: 1}})
	// Attach a stream callback so ChatCompletion routes to the
	// streaming path via shouldStream — the same trigger production uses.
	ctx = ContextWithStreamContent(ctx, StreamContentFunc(func(string) {}))
	_, err := c.ChatCompletion(ctx, []Message{{Role: "user", Content: "hi"}}, nil, opts...)
	require.NoError(t, err)
	return gotTier, tierField
}

func TestServiceTier_Stream_OptionOnTheWire(t *testing.T) {
	got, present := captureStreamServiceTier(t, context.Background(), WithServiceTier("flex"))
	require.True(t, present, "stream path must place service_tier on the wire when set via option")
	require.Equal(t, "flex", got)
}

func TestServiceTier_Stream_CtxOnTheWire(t *testing.T) {
	ctx := ContextWithServiceTier(context.Background(), "priority")
	got, present := captureStreamServiceTier(t, ctx)
	require.True(t, present, "stream path must place ctx-borne service_tier on the wire")
	require.Equal(t, "priority", got)
}

func TestServiceTier_Stream_OmittedWhenUnset(t *testing.T) {
	_, present := captureStreamServiceTier(t, context.Background())
	require.False(t, present,
		"stream path with no option and no ctx tier must NOT include service_tier")
}

// TestServiceTier_AllValuesRoundTrip pins that every value
// config.validateServiceTier accepts ("default", "flex", "priority")
// reaches the wire byte-for-byte. Catches a regression where someone
// adds a normalization step (e.g. lowercasing or aliasing "standard"
// → "default") inside the LLM client and silently breaks the contract
// with operators who configured a specific value.
func TestServiceTier_AllValuesRoundTrip(t *testing.T) {
	for _, tier := range []string{"default", "flex", "priority"} {
		t.Run(tier, func(t *testing.T) {
			got, present := captureServiceTier(t, context.Background(), WithServiceTier(tier))
			require.True(t, present)
			require.Equal(t, tier, got, "value must round-trip unchanged")
		})
	}
}

// resolveServiceTier is shared by both ChatCompletion (direct) and
// ChatCompletionStream, so testing it here proves both paths pick the
// right tier without needing two near-identical httptest harnesses.
// Pinned precedence: per-call option > ctx ContextWithServiceTier >
// ctx CallProfile + client config > "".
func TestResolveServiceTier(t *testing.T) {
	cases := []struct {
		name       string
		ctxTier    string
		profile    CallProfile
		bgTier     string
		intTier    string
		optTier    string
		want       string
	}{
		{name: "both empty", want: ""},
		{name: "ctx only", ctxTier: "flex", want: "flex"},
		{name: "opt only", optTier: "priority", want: "priority"},
		{name: "opt wins over ctx", ctxTier: "flex", optTier: "priority", want: "priority"},
		{name: "empty opt falls through", ctxTier: "flex", want: "flex"},
		{name: "background profile maps to config", profile: CallProfileBackground, bgTier: "flex", want: "flex"},
		{name: "interactive profile maps to config", profile: CallProfileInteractive, intTier: "priority", want: "priority"},
		{name: "ctx tier beats profile", ctxTier: "default", profile: CallProfileBackground, bgTier: "flex", want: "default"},
		{name: "opt tier beats profile", optTier: "priority", profile: CallProfileBackground, bgTier: "flex", want: "priority"},
		{name: "profile with empty config falls through", profile: CallProfileBackground, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.ctxTier != "" {
				ctx = ContextWithServiceTier(ctx, tc.ctxTier)
			}
			if tc.profile != "" {
				ctx = ContextWithCallProfile(ctx, tc.profile)
			}
			c := &Client{serviceTierBackground: tc.bgTier, serviceTierInteractive: tc.intTier}
			require.Equal(t, tc.want, c.resolveServiceTier(ctx, tc.optTier))
		})
	}
}
