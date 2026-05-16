package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// recordingServer is a minimal OpenAI-compatible test server that
// records each incoming request's `stream` flag so the router tests
// can prove which code path served the call. It responds with a
// fixed non-streaming completion when stream=false, and a fixed
// streaming sequence when stream=true.
type recordingServer struct {
	streamFlags []bool // one entry per request, in order received
	mu          chan struct{}
	srv         *httptest.Server
}

func newRecordingServer(t *testing.T) *recordingServer {
	t.Helper()
	rs := &recordingServer{mu: make(chan struct{}, 1)}
	rs.srv = httptest.NewServer(http.HandlerFunc(rs.handle))
	t.Cleanup(rs.srv.Close)
	return rs
}

func (rs *recordingServer) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	defer r.Body.Close()
	var probe struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &probe)
	rs.mu <- struct{}{}
	rs.streamFlags = append(rs.streamFlags, probe.Stream)
	<-rs.mu

	if !probe.Stream {
		// Non-streaming: respond with a complete chat-completion JSON.
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintln(w, `{
            "id": "chatcmpl-stub",
            "object": "chat.completion",
            "created": 0,
            "model": "test",
            "choices": [{
                "index": 0,
                "message": {"role": "assistant", "content": "non-streaming reply"},
                "finish_reason": "stop"
            }],
            "usage": {"prompt_tokens": 10, "completion_tokens": 4, "total_tokens": 14}
        }`)
		return
	}

	// Streaming: emit a tiny SSE conversation matching the OpenAI
	// spec.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	chunks := []string{
		`{"choices":[{"delta":{"content":"streaming "}}]}`,
		`{"choices":[{"delta":{"content":"reply"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`,
	}
	for _, c := range chunks {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", c)
		if flusher != nil {
			flusher.Flush()
		}
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func (rs *recordingServer) flagsCopy() []bool {
	rs.mu <- struct{}{}
	out := append([]bool(nil), rs.streamFlags...)
	<-rs.mu
	return out
}

// TestChatCompletion_NoCallbacksUsesNonStreaming pins the V2.3-compat
// path: a client with no streaming callbacks attached to ctx makes a
// non-streaming request, regardless of streamSchema.
func TestChatCompletion_NoCallbacksUsesNonStreaming(t *testing.T) {
	rs := newRecordingServer(t)
	c := NewClient(ClientConfig{Endpoint: rs.srv.URL, Model: "test"})

	res, err := c.ChatCompletion(context.Background(),
		[]Message{{Role: "user", Content: "hi"}}, nil)
	require.NoError(t, err)
	require.Equal(t, "non-streaming reply", res.Content)

	flags := rs.flagsCopy()
	require.Len(t, flags, 1)
	require.False(t, flags[0], "no-callback request must NOT set stream=true")
}

// TestChatCompletion_WithContentCallbackStreams pins that attaching a
// StreamContentFunc to ctx routes to the streaming path. The callback
// must fire for the deltas as they arrive AND the final ChatResult
// must aggregate them into a coherent Content string.
func TestChatCompletion_WithContentCallbackStreams(t *testing.T) {
	rs := newRecordingServer(t)
	c := NewClient(ClientConfig{Endpoint: rs.srv.URL, Model: "test"})

	var got []string
	cb := StreamContentFunc(func(d string) { got = append(got, d) })
	ctx := ContextWithStreamContent(context.Background(), cb)

	res, err := c.ChatCompletion(ctx,
		[]Message{{Role: "user", Content: "hi"}}, nil)
	require.NoError(t, err)
	require.Equal(t, "streaming reply", res.Content)
	require.Equal(t, []string{"streaming ", "reply"}, got)
	require.Equal(t, "stop", res.FinishReason)
	require.Equal(t, 12, res.PromptTokens)
	require.Equal(t, 3, res.CompletionTokens)

	flags := rs.flagsCopy()
	require.Len(t, flags, 1)
	require.True(t, flags[0], "callback-attached request must set stream=true")
}

// TestChatCompletion_WithThinkingCallbackStreams pins the same routing
// trigger for StreamThinkingFunc. Either callback alone is enough to
// route — both being absent is the only "no stream" case.
func TestChatCompletion_WithThinkingCallbackStreams(t *testing.T) {
	rs := newRecordingServer(t)
	c := NewClient(ClientConfig{Endpoint: rs.srv.URL, Model: "test"})

	thinkingCalls := atomic.Int32{}
	cb := StreamThinkingFunc(func(string) { thinkingCalls.Add(1) })
	ctx := ContextWithStreamThinking(context.Background(), cb)

	_, err := c.ChatCompletion(ctx,
		[]Message{{Role: "user", Content: "hi"}}, nil)
	require.NoError(t, err)

	flags := rs.flagsCopy()
	require.Len(t, flags, 1)
	require.True(t, flags[0], "thinking-callback request must set stream=true")
}

// TestChatCompletion_StreamSchemaFalseFallsBackForJSONSchema pins the
// schema-streaming gate: when streamSchema is false AND the request
// carries a json_schema, the router falls back to non-streaming.
// Text-only streaming is still routed through the streaming path —
// the gate only blocks schema-constrained calls.
func TestChatCompletion_StreamSchemaFalseFallsBackForJSONSchema(t *testing.T) {
	rs := newRecordingServer(t)
	streamSchema := false
	c := NewClient(ClientConfig{
		Endpoint:     rs.srv.URL,
		Model:        "test",
		StreamSchema: &streamSchema,
	})

	cb := StreamContentFunc(func(string) {})
	ctx := ContextWithStreamContent(context.Background(), cb)

	// First call: with json_schema → must fall back to non-streaming.
	_, err := c.ChatCompletion(ctx,
		[]Message{{Role: "user", Content: "hi"}}, nil,
		WithJSONSchema("Card", map[string]any{"type": "object"}),
	)
	require.NoError(t, err)

	// Second call: text-only → still streams (gate only affects schema).
	_, err = c.ChatCompletion(ctx,
		[]Message{{Role: "user", Content: "hi"}}, nil)
	require.NoError(t, err)

	flags := rs.flagsCopy()
	require.Len(t, flags, 2)
	require.False(t, flags[0], "json_schema request with streamSchema=false must NOT stream")
	require.True(t, flags[1], "text-only request with callback must still stream")
}

// TestChatCompletion_StreamSchemaTrueStreamsJSONSchema pins the
// default behavior: a client that hasn't explicitly opted out of
// schema streaming routes json_schema requests through the streaming
// path when callbacks are attached. This is the V2.4 hot path until
// the P0 smoke test proves it unsafe on Qwen3 + LM Studio.
func TestChatCompletion_StreamSchemaTrueStreamsJSONSchema(t *testing.T) {
	rs := newRecordingServer(t)
	c := NewClient(ClientConfig{Endpoint: rs.srv.URL, Model: "test"})
	// No StreamSchema in cfg → defaults to true.

	cb := StreamContentFunc(func(string) {})
	ctx := ContextWithStreamContent(context.Background(), cb)

	_, err := c.ChatCompletion(ctx,
		[]Message{{Role: "user", Content: "hi"}}, nil,
		WithJSONSchema("Card", map[string]any{"type": "object"}),
	)
	require.NoError(t, err)

	flags := rs.flagsCopy()
	require.Len(t, flags, 1)
	require.True(t, flags[0], "json_schema request with streamSchema=true must stream")
}

// TestChatCompletion_NonStreamingPreservesV23ResponseShape pins that
// the non-streaming path's response parsing still works after the V2.4
// router refactor — a sanity check that we didn't accidentally break
// the existing chatCompletionDirect-served path.
func TestChatCompletion_NonStreamingPreservesV23ResponseShape(t *testing.T) {
	rs := newRecordingServer(t)
	c := NewClient(ClientConfig{Endpoint: rs.srv.URL, Model: "test"})

	res, err := c.ChatCompletion(context.Background(),
		[]Message{{Role: "user", Content: "hi"}}, nil)
	require.NoError(t, err)
	require.Equal(t, "non-streaming reply", res.Content)
	require.Equal(t, "stop", res.FinishReason)
	require.Equal(t, 10, res.PromptTokens)
	require.Equal(t, 4, res.CompletionTokens)
	require.False(t, res.Empty)
	require.Empty(t, res.ToolCalls)
}
