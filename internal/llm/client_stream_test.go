package llm

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

// scriptedStream is a fake chatCompletionStreamRecv that walks a
// pre-built slice of chunks. Once exhausted, returns io.EOF — matching
// the SDK's signal for "stream closed cleanly".
type scriptedStream struct {
	chunks []openai.ChatCompletionStreamResponse
	err    error // when non-nil, returned after all chunks drain
	errAt  int   // index where err fires (-1 → after all chunks)
	idx    int
	closed bool
}

func (s *scriptedStream) Recv() (openai.ChatCompletionStreamResponse, error) {
	if s.err != nil && (s.errAt < 0 || s.idx >= s.errAt) && s.idx >= len(s.chunks) {
		return openai.ChatCompletionStreamResponse{}, s.err
	}
	if s.err != nil && s.errAt >= 0 && s.idx >= s.errAt {
		return openai.ChatCompletionStreamResponse{}, s.err
	}
	if s.idx >= len(s.chunks) {
		return openai.ChatCompletionStreamResponse{}, io.EOF
	}
	out := s.chunks[s.idx]
	s.idx++
	return out, nil
}

// chunkContent builds a content-only delta chunk.
func chunkContent(s string) openai.ChatCompletionStreamResponse {
	return openai.ChatCompletionStreamResponse{
		Choices: []openai.ChatCompletionStreamChoice{{
			Delta: openai.ChatCompletionStreamChoiceDelta{Content: s},
		}},
	}
}

// chunkThinking builds a reasoning_content-only delta chunk.
func chunkThinking(s string) openai.ChatCompletionStreamResponse {
	return openai.ChatCompletionStreamResponse{
		Choices: []openai.ChatCompletionStreamChoice{{
			Delta: openai.ChatCompletionStreamChoiceDelta{ReasoningContent: s},
		}},
	}
}

// chunkUsage builds the final usage-only chunk that LM Studio sends
// when StreamOptions.IncludeUsage is set. Choices is empty.
func chunkUsage(prompt, completion int) openai.ChatCompletionStreamResponse {
	return openai.ChatCompletionStreamResponse{
		Usage: &openai.Usage{
			PromptTokens:     prompt,
			CompletionTokens: completion,
			TotalTokens:      prompt + completion,
		},
	}
}

// chunkToolCallStart builds the typical first delta for a tool call:
// id + name + initial argument fragment.
func chunkToolCallStart(idx int, id, name, args string) openai.ChatCompletionStreamResponse {
	i := idx
	return openai.ChatCompletionStreamResponse{
		Choices: []openai.ChatCompletionStreamChoice{{
			Delta: openai.ChatCompletionStreamChoiceDelta{
				ToolCalls: []openai.ToolCall{{
					Index: &i,
					ID:    id,
					Type:  openai.ToolTypeFunction,
					Function: openai.FunctionCall{
						Name:      name,
						Arguments: args,
					},
				}},
			},
		}},
	}
}

// chunkToolCallArgsDelta builds a follow-up tool-call delta carrying
// only an arguments-string fragment.
func chunkToolCallArgsDelta(idx int, args string) openai.ChatCompletionStreamResponse {
	i := idx
	return openai.ChatCompletionStreamResponse{
		Choices: []openai.ChatCompletionStreamChoice{{
			Delta: openai.ChatCompletionStreamChoiceDelta{
				ToolCalls: []openai.ToolCall{{
					Index: &i,
					Function: openai.FunctionCall{
						Arguments: args,
					},
				}},
			},
		}},
	}
}

// chunkFinish closes a choice with a finish reason and no other delta
// content. Mirrors the upstream's "tool_calls" or "stop" terminator.
func chunkFinish(reason string) openai.ChatCompletionStreamResponse {
	return openai.ChatCompletionStreamResponse{
		Choices: []openai.ChatCompletionStreamChoice{{
			FinishReason: openai.FinishReason(reason),
		}},
	}
}

// TestAggregateStream_ContentDeltasFireCallbackInOrder pins the
// happy-path content stream: every non-empty delta fires the
// StreamContentFunc once, in the order received, and the final
// ChatResult's Content is the concatenation.
func TestAggregateStream_ContentDeltasFireCallbackInOrder(t *testing.T) {
	stream := &scriptedStream{chunks: []openai.ChatCompletionStreamResponse{
		chunkContent("Ten "),
		chunkContent("minutes "),
		chunkContent("between "),
		chunkContent("meetings."),
		chunkFinish("stop"),
		chunkUsage(100, 8),
	}}

	var got []string
	cb := StreamContentFunc(func(d string) { got = append(got, d) })

	res, err := aggregateStream(stream, time.Now(), cb, nil)
	require.NoError(t, err)
	require.Equal(t, []string{"Ten ", "minutes ", "between ", "meetings."}, got)
	require.Equal(t, "Ten minutes between meetings.", res.Content)
	require.Equal(t, 100, res.PromptTokens)
	require.Equal(t, 8, res.CompletionTokens)
	require.Equal(t, "stop", res.FinishReason)
	require.False(t, res.Empty)
}

// TestAggregateStream_ThinkingDeltasFireCallback pins the
// reasoning_content path: thinking deltas fire StreamThinkingFunc and
// land in ChatResult.ThinkingContent. When body is empty but thinking
// has text, the model produced a reasoning-only response and
// aggregateStream adopts it as Content (mirrors pickContentField).
func TestAggregateStream_ThinkingDeltasFireCallback(t *testing.T) {
	stream := &scriptedStream{chunks: []openai.ChatCompletionStreamResponse{
		chunkThinking("Considering "),
		chunkThinking("the calendar..."),
		chunkContent("Here are the cards."),
		chunkFinish("stop"),
		chunkUsage(50, 5),
	}}

	var thoughts []string
	thinkingCb := StreamThinkingFunc(func(d string) { thoughts = append(thoughts, d) })

	res, err := aggregateStream(stream, time.Now(), nil, thinkingCb)
	require.NoError(t, err)
	require.Equal(t, []string{"Considering ", "the calendar..."}, thoughts)
	require.Equal(t, "Considering the calendar...", res.ThinkingContent)
	require.Equal(t, "Here are the cards.", res.Content)
}

// TestAggregateStream_ReasoningOnlyAdoptedAsContent pins the fallback
// when content stays empty but thinking has text — the streaming path
// must produce a non-empty Content matching the non-streaming path's
// pickContentField precedence (content → reasoning_content → reasoning).
func TestAggregateStream_ReasoningOnlyAdoptedAsContent(t *testing.T) {
	stream := &scriptedStream{chunks: []openai.ChatCompletionStreamResponse{
		chunkThinking("The answer is 42."),
		chunkFinish("stop"),
	}}
	res, err := aggregateStream(stream, time.Now(), nil, nil)
	require.NoError(t, err)
	require.Equal(t, "The answer is 42.", res.Content,
		"reasoning_content alone must be adopted as Content for back-compat with the non-streaming path")
}

// TestAggregateStream_ToolCallDeltasAccumulateByIndex pins the
// tool-call accumulation contract: arguments arrive across multiple
// chunks, indexed by tool_call.index, and the final ChatResult's
// ToolCalls slice is in deterministic index order.
func TestAggregateStream_ToolCallDeltasAccumulateByIndex(t *testing.T) {
	stream := &scriptedStream{chunks: []openai.ChatCompletionStreamResponse{
		chunkToolCallStart(0, "call_a", "read_thread", `{"subject":"red`),
		chunkToolCallStart(1, "call_b", "check_weather", `{"window`),
		chunkToolCallArgsDelta(0, `line"}`),
		chunkToolCallArgsDelta(1, `":"morning"}`),
		chunkFinish("tool_calls"),
		chunkUsage(200, 12),
	}}
	res, err := aggregateStream(stream, time.Now(), nil, nil)
	require.NoError(t, err)
	require.Len(t, res.ToolCalls, 2)
	require.Equal(t, "call_a", res.ToolCalls[0].ID)
	require.Equal(t, "read_thread", res.ToolCalls[0].Name)
	require.Equal(t, "redline", res.ToolCalls[0].Arguments["subject"])
	require.Equal(t, "call_b", res.ToolCalls[1].ID)
	require.Equal(t, "check_weather", res.ToolCalls[1].Name)
	require.Equal(t, "morning", res.ToolCalls[1].Arguments["window"])
	require.Equal(t, "tool_calls", res.FinishReason)
	require.Equal(t, 200, res.PromptTokens)
	require.Equal(t, 12, res.CompletionTokens)
}

// TestAggregateStream_ToolCallParseFailureSurfacesViaErrors pins that
// malformed JSON in tool args still surfaces via ChatResult.ToolArgsErrors
// — the loop's repair flow expects this, regardless of which path
// served the call.
func TestAggregateStream_ToolCallParseFailureSurfacesViaErrors(t *testing.T) {
	stream := &scriptedStream{chunks: []openai.ChatCompletionStreamResponse{
		chunkToolCallStart(0, "call_x", "read_thread", `{"subject":}`), // invalid JSON
		chunkFinish("tool_calls"),
	}}
	res, err := aggregateStream(stream, time.Now(), nil, nil)
	require.NoError(t, err)
	require.Len(t, res.ToolCalls, 1)
	require.Nil(t, res.ToolCalls[0].Arguments, "args slot must be nil when JSON parse fails")
	require.Len(t, res.ToolArgsErrors, 1)
	require.Equal(t, "call_x", res.ToolArgsErrors[0].ToolCallID)
	require.Contains(t, res.ToolArgsErrors[0].RawJSON, `"subject":}`)
}

// TestAggregateStream_ThinkBlocksStrippedFromContent pins the
// behavior parity with chatCompletionDirect: <think>...</think>
// blocks inline in body get stripped from Content and folded into
// ThinkingContent. The streaming path must produce the same final
// shape so callers can swap paths transparently.
func TestAggregateStream_ThinkBlocksStrippedFromContent(t *testing.T) {
	stream := &scriptedStream{chunks: []openai.ChatCompletionStreamResponse{
		chunkContent("<think>"),
		chunkContent("considering options..."),
		chunkContent("</think>"),
		chunkContent("Final answer."),
		chunkFinish("stop"),
	}}
	res, err := aggregateStream(stream, time.Now(), nil, nil)
	require.NoError(t, err)
	require.Equal(t, "Final answer.", res.Content)
	require.Contains(t, res.ThinkingContent, "considering options...")
}

// TestAggregateStream_ErrorMidStreamReturnsPartialResult pins the
// error-handling contract: when Recv fails mid-stream, the caller
// gets the error back along with whatever content has streamed so
// far (in case the caller wants to log it for debugging).
func TestAggregateStream_ErrorMidStreamReturnsPartialResult(t *testing.T) {
	stream := &scriptedStream{
		chunks: []openai.ChatCompletionStreamResponse{
			chunkContent("Half-"),
			chunkContent("written "),
		},
		err:   errors.New("upstream connection reset"),
		errAt: 2,
	}

	var got []string
	cb := StreamContentFunc(func(d string) { got = append(got, d) })

	res, err := aggregateStream(stream, time.Now(), cb, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "upstream connection reset")
	require.Equal(t, []string{"Half-", "written "}, got, "callbacks fire for deltas seen before the error")
	require.Equal(t, "Half-written ", res.Content)
}

// TestAggregateStream_NoCallbacksAggregatesIdentically pins the
// "single hot path" contract from the V2.4 plan: a streaming run with
// no UI consumer attached still produces a coherent ChatResult — the
// streaming path's behavior must be invariant under StreamContentFunc /
// StreamThinkingFunc nullity.
func TestAggregateStream_NoCallbacksAggregatesIdentically(t *testing.T) {
	stream := &scriptedStream{chunks: []openai.ChatCompletionStreamResponse{
		chunkContent("First sentence. "),
		chunkContent("Second sentence."),
		chunkFinish("stop"),
		chunkUsage(40, 7),
	}}
	res, err := aggregateStream(stream, time.Now(), nil, nil)
	require.NoError(t, err)
	require.Equal(t, "First sentence. Second sentence.", res.Content)
	require.Equal(t, 40, res.PromptTokens)
	require.Equal(t, 7, res.CompletionTokens)
}

// TestAggregateStream_EmptyStreamProducesEmpty pins that an SSE stream
// closing immediately produces a result tagged Empty=true rather than
// an obscure error — matches the non-streaming path's behavior on
// upstreams that return a 200 with no choices.
func TestAggregateStream_EmptyStreamProducesEmpty(t *testing.T) {
	stream := &scriptedStream{chunks: []openai.ChatCompletionStreamResponse{}}
	res, err := aggregateStream(stream, time.Now(), nil, nil)
	require.NoError(t, err)
	require.True(t, res.Empty)
	require.Empty(t, res.Content)
	require.Empty(t, res.ToolCalls)
}

// TestAggregateStream_FirstByteDurationCaptured pins that the
// FirstByteDuration field is populated when the first chunk lands —
// V2.4 latency rubric depends on this for TTFB measurement.
func TestAggregateStream_FirstByteDurationCaptured(t *testing.T) {
	stream := &scriptedStream{chunks: []openai.ChatCompletionStreamResponse{
		chunkContent("hi"),
		chunkFinish("stop"),
	}}
	start := time.Now()
	res, err := aggregateStream(stream, start, nil, nil)
	require.NoError(t, err)
	require.True(t, res.FirstByteDuration > 0,
		"FirstByteDuration must be non-zero once first chunk arrives")
	require.True(t, res.TotalDuration >= res.FirstByteDuration)
}

// TestAggregateStream_DeadlineMidStreamReturnsContextErr pins the
// cancellation behavior: when ctx is cancelled mid-stream (in
// production: caller's deadline expires), Recv surfaces the ctx error
// and aggregateStream propagates it. Callbacks already fired for
// pre-cancel deltas remain fired (no rollback).
func TestAggregateStream_DeadlineMidStreamReturnsContextErr(t *testing.T) {
	stream := &scriptedStream{
		chunks: []openai.ChatCompletionStreamResponse{
			chunkContent("partial"),
		},
		err:   context.DeadlineExceeded,
		errAt: 1,
	}
	var got int32
	cb := StreamContentFunc(func(d string) { atomic.AddInt32(&got, 1) })

	res, err := aggregateStream(stream, time.Now(), cb, nil)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, int32(1), atomic.LoadInt32(&got), "the pre-cancel delta still fires its callback")
	require.Equal(t, "partial", res.Content)
}

// TestAggregateStream_FinishReasonCapturedFromAnyChoice pins that the
// finish reason is captured even when it arrives on a chunk that
// doesn't carry content/tool deltas (the typical OpenAI shape:
// finish_reason on the last choice's delta-empty chunk).
func TestAggregateStream_FinishReasonCapturedFromAnyChoice(t *testing.T) {
	stream := &scriptedStream{chunks: []openai.ChatCompletionStreamResponse{
		chunkContent("body"),
		{
			// finish_reason on a chunk with no delta content
			Choices: []openai.ChatCompletionStreamChoice{{
				Delta:        openai.ChatCompletionStreamChoiceDelta{},
				FinishReason: openai.FinishReason("length"),
			}},
		},
	}}
	res, err := aggregateStream(stream, time.Now(), nil, nil)
	require.NoError(t, err)
	require.Equal(t, "length", res.FinishReason)
	require.Equal(t, "body", res.Content)
}

// TestShouldStream pins the routing decision in ChatCompletion. With no
// callbacks attached the decision is "stay non-streaming" regardless of
// the schema flag; with at least one callback attached the schema flag
// is the gate. Pure unit test against shouldStream.
func TestShouldStream(t *testing.T) {
	contentCb := StreamContentFunc(func(string) {})
	thinkingCb := StreamThinkingFunc(func(string) {})

	cases := []struct {
		name         string
		ctx          context.Context
		streamSchema bool
		hasSchema    bool
		want         bool
	}{
		{"no_callbacks_no_stream", context.Background(), true, false, false},
		{"no_callbacks_with_schema_no_stream", context.Background(), true, true, false},
		{"content_cb_no_schema_streams", ContextWithStreamContent(context.Background(), contentCb), true, false, true},
		{"thinking_cb_no_schema_streams", ContextWithStreamThinking(context.Background(), thinkingCb), true, false, true},
		{"content_cb_schema_streams_when_allowed", ContextWithStreamContent(context.Background(), contentCb), true, true, true},
		{"content_cb_schema_falls_back_when_disabled", ContextWithStreamContent(context.Background(), contentCb), false, true, false},
		{"thinking_cb_schema_falls_back_when_disabled", ContextWithStreamThinking(context.Background(), thinkingCb), false, true, false},
		{"thinking_cb_no_schema_streams_even_when_schema_disabled", ContextWithStreamThinking(context.Background(), thinkingCb), false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{streamSchema: tc.streamSchema}
			o := &chatOpts{}
			if tc.hasSchema {
				o.jsonSchemaName = "TestSchema"
				o.jsonSchemaRaw = []byte(`{"type":"object"}`)
			}
			require.Equal(t, tc.want, c.shouldStream(tc.ctx, o))
		})
	}
}
