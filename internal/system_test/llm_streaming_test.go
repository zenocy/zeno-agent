package system_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/llm"
)

// TestSpine_LLMStreaming_LiveTraceAndContentReachConsumers is the
// V2.4 P1 system test for the streaming LLM client + RunLoop seam.
// It wires a fake OpenAI-compatible streaming endpoint and a real
// llm.Client + RunLoop against it. The assertions prove that:
//
//  1. Content deltas reach the StreamContentFunc as the model emits
//     them (the user-perceived "typing in" UX).
//  2. Trace steps reach the LiveTraceFunc in the same order they
//     land in the sealed Trace (the live-trace UI mirror).
//  3. The final LoopResult has the same Content and step sequence
//     a non-streaming run would have produced.
//
// The fake server emulates LM Studio's behavior: streaming SSE
// chunks for both tool-call iterations and the final content
// iteration. Tool args arrive split across multiple deltas, exactly
// as Qwen3 + LM Studio do in production.
func TestSpine_LLMStreaming_LiveTraceAndContentReachConsumers(t *testing.T) {
	srv := newFakeStreamingLLM(t,
		// Iteration 1: tool call (read_thread).
		streamScript{
			toolCalls: []scriptedToolCall{
				{
					id:   "call_1",
					name: "read_thread",
					// Args split across two deltas — pin V2.4's
					// per-index accumulation.
					argFragments: []string{`{"x":"red`, `line"}`},
				},
			},
			finishReason: "tool_calls",
		},
		// Iteration 2: natural exit with content + an inline thought.
		streamScript{
			contentChunks: []string{
				"thought: synthesizing\n",
				"Final answer.",
			},
			finishReason: "stop",
		},
	)

	c := llm.NewClient(llm.ClientConfig{Endpoint: srv.URL, Model: "test-model"})

	tool := newSpineStubTool("read_thread", "thread-body")
	reg := llm.NewRegistry(tool)

	var (
		liveSteps     []llm.TraceStep
		contentDeltas []string
	)
	ctx := llm.ContextWithLiveTrace(context.Background(),
		func(s llm.TraceStep) { liveSteps = append(liveSteps, s) })
	ctx = llm.ContextWithStreamContent(ctx,
		func(d string) { contentDeltas = append(contentDeltas, d) })

	res, err := llm.RunLoop(ctx, c, "sys", "user", reg, llm.LoopConfig{MaxIterations: 4})
	require.NoError(t, err)
	require.Equal(t, llm.StopOK, res.Stopped)
	require.Equal(t, "Final answer.", res.Content)

	// Live trace mirrors the sealed trace step-for-step.
	require.Equal(t, len(res.Trace.Steps), len(liveSteps),
		"live publish count must match sealed Trace.Steps count")
	for i, want := range res.Trace.Steps {
		require.Equalf(t, want, liveSteps[i],
			"step %d: live publish must be byte-equal to sealed step", i)
	}

	// At minimum we expect: one tool step (the read_thread call) +
	// one thought step (the inline "thought: synthesizing").
	var (
		toolSteps    int
		thoughtSteps int
	)
	for _, s := range liveSteps {
		switch s.Kind {
		case llm.KindTool:
			toolSteps++
		case llm.KindThought:
			thoughtSteps++
		}
	}
	require.Equal(t, 1, toolSteps, "expected one tool step for the streamed tool call")
	require.GreaterOrEqual(t, thoughtSteps, 1, "expected the inline-thought step from iteration 2")

	// Content deltas: the second iteration sent two chunks. Both must
	// have reached the StreamContentFunc in order.
	require.Equal(t, []string{"thought: synthesizing\n", "Final answer."}, contentDeltas)

	// The fake tool was invoked once with the reassembled arguments.
	require.Equal(t, 1, tool.invocations())
	require.Equal(t, "redline", tool.lastArgs()["x"])
}

// TestSpine_LLMStreaming_NoCallbacksMirrorsV23 pins the contract that
// when no streaming callbacks are attached to ctx, the LLM client
// stays on the V2.3 non-streaming path. Behavior must be byte-equal
// to V2.3 — same Content, same step sequence, same token counts.
//
// This is the regression alarm: anything that accidentally routes
// callback-less calls through the streaming path would break replay,
// eval, and every test that doesn't explicitly opt in to V2.4's UI
// surface.
func TestSpine_LLMStreaming_NoCallbacksMirrorsV23(t *testing.T) {
	srv := newFakeStreamingLLM(t,
		streamScript{
			contentChunks: []string{"baseline answer."},
			finishReason:  "stop",
		},
	)

	c := llm.NewClient(llm.ClientConfig{Endpoint: srv.URL, Model: "test-model"})
	reg := llm.NewRegistry()

	res, err := llm.RunLoop(context.Background(), c, "sys", "user", reg,
		llm.LoopConfig{MaxIterations: 2})
	require.NoError(t, err)
	require.Equal(t, llm.StopOK, res.Stopped)
	require.Equal(t, "baseline answer.", res.Content)

	// The fake server records each request's `stream` flag. With no
	// callbacks attached to ctx, every request must NOT set stream=true.
	flags := srv.streamFlags()
	require.NotEmpty(t, flags)
	for i, f := range flags {
		require.Falsef(t, f, "request %d streamed unexpectedly", i)
	}
}

// TestSpine_LLMStreaming_ToolCallArgFragmentsReassemble pins the
// V2.4 tool-call accumulation contract end-to-end: when the upstream
// streams arguments as multiple JSON fragments (Qwen3's typical
// output shape), the reassembled arguments map matches what a
// non-streaming response would have produced.
func TestSpine_LLMStreaming_ToolCallArgFragmentsReassemble(t *testing.T) {
	srv := newFakeStreamingLLM(t,
		// Tool call with arguments split across FOUR deltas — really
		// tests the reassembly seam.
		streamScript{
			toolCalls: []scriptedToolCall{{
				id:   "call_x",
				name: "check_calendar",
				argFragments: []string{
					`{"window":`, `"morning"`, `,"date":`, `"2026-04-30"}`,
				},
			}},
			finishReason: "tool_calls",
		},
		streamScript{
			contentChunks: []string{"all done"},
			finishReason:  "stop",
		},
	)

	c := llm.NewClient(llm.ClientConfig{Endpoint: srv.URL, Model: "test-model"})
	tool := newSpineStubTool("check_calendar", "ok")
	reg := llm.NewRegistry(tool)

	cb := llm.StreamContentFunc(func(string) {})
	ctx := llm.ContextWithStreamContent(context.Background(), cb)

	res, err := llm.RunLoop(ctx, c, "sys", "user", reg, llm.LoopConfig{MaxIterations: 4})
	require.NoError(t, err)
	require.Equal(t, llm.StopOK, res.Stopped)
	require.Equal(t, 1, tool.invocations())

	args := tool.lastArgs()
	require.Equal(t, "morning", args["window"], "fragment reassembly must preserve nested fields")
	require.Equal(t, "2026-04-30", args["date"])

	// Ensure the request used streaming (proving this exercised the
	// V2.4 path, not a V2.3 fallback).
	flags := srv.streamFlags()
	for i, f := range flags {
		require.Truef(t, f, "request %d should have streamed (streaming callbacks attached)", i)
	}
	_ = res
}

// ---- fake server + helpers ----

// streamScript is one chat-completion's worth of streaming output.
// The fake server iterates one script per request in order received;
// when scripts run out, it 500s (test should not over-call).
type streamScript struct {
	contentChunks []string
	toolCalls     []scriptedToolCall
	finishReason  string
}

type scriptedToolCall struct {
	id           string
	name         string
	argFragments []string
}

type fakeStreamingLLM struct {
	*httptest.Server
	scripts []streamScript

	mu          chan struct{}
	idx         int
	streamFlag  []bool
	pingResp    string
	requestsOut atomic.Int32
}

func newFakeStreamingLLM(t *testing.T, scripts ...streamScript) *fakeStreamingLLM {
	t.Helper()
	f := &fakeStreamingLLM{
		scripts: scripts,
		mu:      make(chan struct{}, 1),
		pingResp: `{
            "id":"chatcmpl-ping","object":"chat.completion","created":0,
            "model":"test","choices":[{"index":0,
            "message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
            "usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
        }`,
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.Server.Close)
	return f
}

func (f *fakeStreamingLLM) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	defer func() { _ = r.Body.Close() }()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var probe struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &probe)
	f.requestsOut.Add(1)

	f.mu <- struct{}{}
	f.streamFlag = append(f.streamFlag, probe.Stream)
	idx := f.idx
	f.idx++
	<-f.mu

	if idx >= len(f.scripts) {
		http.Error(w, fmt.Sprintf("test: more requests (%d) than scripts (%d)", idx+1, len(f.scripts)), http.StatusInternalServerError)
		return
	}
	script := f.scripts[idx]

	if !probe.Stream {
		f.serveNonStreaming(w, script)
		return
	}
	f.serveStreaming(w, script)
}

func (f *fakeStreamingLLM) serveNonStreaming(w http.ResponseWriter, s streamScript) {
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{
		"id":      "chatcmpl-stub",
		"object":  "chat.completion",
		"created": 0,
		"model":   "test",
		"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 4, "total_tokens": 14},
	}
	choice := map[string]any{
		"index":         0,
		"finish_reason": s.finishReason,
	}
	if len(s.toolCalls) > 0 {
		var tc []map[string]any
		for _, c := range s.toolCalls {
			tc = append(tc, map[string]any{
				"id":   c.id,
				"type": "function",
				"function": map[string]any{
					"name":      c.name,
					"arguments": strings.Join(c.argFragments, ""),
				},
			})
		}
		choice["message"] = map[string]any{
			"role":       "assistant",
			"tool_calls": tc,
		}
	} else {
		choice["message"] = map[string]any{
			"role":    "assistant",
			"content": strings.Join(s.contentChunks, ""),
		}
	}
	resp["choices"] = []any{choice}
	_ = json.NewEncoder(w).Encode(resp)
}

func (f *fakeStreamingLLM) serveStreaming(w http.ResponseWriter, s streamScript) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)

	emit := func(payload string) {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
		if flusher != nil {
			flusher.Flush()
		}
	}

	for _, chunk := range s.contentChunks {
		emit(fmt.Sprintf(`{"choices":[{"delta":{"content":%s}}]}`, jsonString(chunk)))
	}
	for i, tc := range s.toolCalls {
		// First chunk: id + name + first arg fragment.
		if len(tc.argFragments) > 0 {
			emit(fmt.Sprintf(
				`{"choices":[{"delta":{"tool_calls":[{"index":%d,"id":%s,"type":"function","function":{"name":%s,"arguments":%s}}]}}]}`,
				i, jsonString(tc.id), jsonString(tc.name), jsonString(tc.argFragments[0]),
			))
			// Remaining args: pure argument-string fragments.
			for _, frag := range tc.argFragments[1:] {
				emit(fmt.Sprintf(
					`{"choices":[{"delta":{"tool_calls":[{"index":%d,"function":{"arguments":%s}}]}}]}`,
					i, jsonString(frag),
				))
			}
		}
	}
	if s.finishReason != "" {
		emit(fmt.Sprintf(`{"choices":[{"delta":{},"finish_reason":%s}]}`, jsonString(s.finishReason)))
	}
	emit(`{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`)
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func (f *fakeStreamingLLM) streamFlags() []bool {
	f.mu <- struct{}{}
	out := append([]bool(nil), f.streamFlag...)
	<-f.mu
	return out
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// spineStubTool is the system_test-package equivalent of the
// loop_test.go stub. We can't import that one (different package).
type spineStubTool struct {
	name      string
	out       string
	mu        chan struct{}
	calls     []map[string]any
	failOnArg map[string]bool
}

func newSpineStubTool(name, out string) *spineStubTool {
	return &spineStubTool{name: name, out: out, mu: make(chan struct{}, 1)}
}

func (t *spineStubTool) Name() string        { return t.name }
func (t *spineStubTool) Description() string { return "stub for spine streaming test" }
func (t *spineStubTool) Parameters() []llm.ToolParamSpec {
	return []llm.ToolParamSpec{
		{Name: "x", Type: "string", Required: false},
		{Name: "window", Type: "string", Required: false},
		{Name: "date", Type: "string", Required: false},
	}
}

func (t *spineStubTool) Execute(_ context.Context, args map[string]any) (string, error) {
	t.mu <- struct{}{}
	t.calls = append(t.calls, args)
	<-t.mu
	if t.failOnArg != nil {
		if v, ok := args["x"].(string); ok && t.failOnArg[v] {
			return "", errors.New("intentional failure")
		}
	}
	return t.out, nil
}

func (t *spineStubTool) invocations() int {
	t.mu <- struct{}{}
	n := len(t.calls)
	<-t.mu
	return n
}

func (t *spineStubTool) lastArgs() map[string]any {
	t.mu <- struct{}{}
	defer func() { <-t.mu }()
	if len(t.calls) == 0 {
		return nil
	}
	return t.calls[len(t.calls)-1]
}
