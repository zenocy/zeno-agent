package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"
	"github.com/sirupsen/logrus"
)

// ChatCompletionStream runs one streaming chat completion against the
// configured endpoint and returns the same ChatResult shape that
// ChatCompletion returns from the non-streaming path. The aggregation
// guarantee: with no streaming callbacks attached to ctx, the result is
// byte-equal (modulo TotalDuration) to what the non-streaming path
// returns for the same request — callers can swap paths without
// behavior changes.
//
// V2.4 wires this method behind ChatCompletion's router: when ctx
// carries a StreamContentFunc / StreamThinkingFunc, ChatCompletion
// delegates here so the UI sees deltas as they arrive. Otherwise the
// non-streaming path stays in use (reasoning_content + <think>
// extraction lives there and is well-tested at runtime against LM
// Studio).
//
// Tool-call accumulation: OpenAI's stream protocol delivers tool call
// arguments byte-by-byte in deltas keyed by tool_call.index. We track
// partial state per index, then JSON-decode at end-of-stream. Per-call
// parse failures still surface via ChatResult.ToolArgsErrors so the
// loop's repair flow works identically.
//
// Schema-streaming gate: when the configured streamSchema is false AND
// the request carries a json_schema response_format, the call falls
// back to chatCompletionDirect. The fallback decision is informed by
// the V2.4 P0 streaming smoke test — if Qwen3 + LM Studio + json_schema
// streaming proves unstable, flip llm.stream_schema=false and only
// text-only payloads flow through this method.
func (c *Client) ChatCompletionStream(
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

	// Schema-streaming gate. Routing back to non-streaming is the safe
	// fallback when the endpoint produces malformed mid-stream JSON
	// under constrained decoding.
	if !c.streamSchema && o.jsonSchemaName != "" {
		return c.ChatCompletion(ctx, messages, tools, opts...)
	}

	req := openai.ChatCompletionRequest{
		Model:    c.model,
		Messages: convertMessagesOut(messages),
		Stream:   true,
		// IncludeUsage causes the upstream to send a final-only chunk
		// with non-nil Usage. Without this flag prompt/completion
		// counts come back as zeros across the entire stream.
		StreamOptions: &openai.StreamOptions{IncludeUsage: true},
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
	tier := c.resolveServiceTier(ctx, o.serviceTier)
	if tier != "" {
		req.ServiceTier = openai.ServiceTier(tier)
	}

	contentCb := StreamContentFromContext(ctx)
	thinkingCb := StreamThinkingFromContext(ctx)

	start := time.Now()
	stream, err := c.api.CreateChatCompletionStream(ctx, req)
	if err != nil {
		c.trafficLog(logrus.Fields{
			"purpose":      "chat",
			"path":         "stream",
			"model":        c.model,
			"messages":     len(messages),
			"tools":        len(tools),
			"max_tokens":   req.MaxTokens,
			"json_mode":    o.jsonMode,
			"json_schema":  o.jsonSchemaName,
			"service_tier": tier,
			"duration_ms":  time.Since(start).Milliseconds(),
			"error":        err.Error(),
		}, "llm: stream open failed")
		return ChatResult{TotalDuration: time.Since(start)}, err
	}
	defer stream.Close()

	res, aggErr := aggregateStream(stream, start, contentCb, thinkingCb)
	fields := logrus.Fields{
		"purpose":           "chat",
		"path":              "stream",
		"model":             c.model,
		"messages":          len(messages),
		"tools":             len(tools),
		"max_tokens":        req.MaxTokens,
		"json_mode":         o.jsonMode,
		"json_schema":       o.jsonSchemaName,
		"service_tier":      tier,
		"prompt_tokens":     res.PromptTokens,
		"cached_tokens":     res.CachedTokens,
		"completion_tokens": res.CompletionTokens,
		"finish_reason":     res.FinishReason,
		"duration_ms":       res.TotalDuration.Milliseconds(),
		"first_byte_ms":     res.FirstByteDuration.Milliseconds(),
	}
	if aggErr != nil {
		fields["error"] = aggErr.Error()
		c.trafficLog(fields, "llm: chat completion stream failed")
	} else {
		c.trafficLog(fields, "llm: chat completion stream ok")
	}
	return res, aggErr
}

// partialToolCall accumulates one tool call across stream chunks. The
// SDK's protocol delivers fragments keyed by tool_call.index: the first
// fragment usually carries the id + name + the start of arguments; the
// rest carry argument-string deltas. We rebuild the full tool call on
// stream close.
type partialToolCall struct {
	id     string
	name   string
	args   strings.Builder
	closed bool // true once we observe finish_reason for this choice
}

// aggregateStream consumes the stream until [DONE] or an error and
// returns the assembled ChatResult. Extracted so unit tests can fake
// the stream without going through the OpenAI HTTP path.
//
// Streaming protocol notes:
//   - Each chunk contains zero or more choices' deltas.
//   - The first chunk for choice 0 typically carries Role:"assistant"
//     and an empty Content; subsequent chunks carry Content fragments.
//   - reasoning_content fragments arrive on the same delta object,
//     under their own field. We forward them to thinkingCb and
//     accumulate into a separate buffer.
//   - Tool calls arrive with index keys (the SDK preserves the
//     server's per-call index). Arguments are concatenated as raw
//     JSON-string fragments.
//   - The final chunk (when StreamOptions.IncludeUsage=true) has a
//     non-nil Usage and an empty Choices. Capture it as the source
//     of truth for token counts.
func aggregateStream(
	stream chatCompletionStreamRecv,
	start time.Time,
	contentCb StreamContentFunc,
	thinkingCb StreamThinkingFunc,
) (ChatResult, error) {
	var (
		body, thinking    strings.Builder
		toolCallsByIndex  = map[int]*partialToolCall{}
		finishReason      string
		promptTokens      int
		cachedTokens      int
		completionTokens  int
		firstByteCaptured bool
		firstByteAt       time.Duration
	)

	for {
		chunk, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			return ChatResult{
				TotalDuration:     time.Since(start),
				FirstByteDuration: firstByteAt,
				Content:           body.String(),
				ThinkingContent:   thinking.String(),
			}, recvErr
		}
		if !firstByteCaptured {
			firstByteAt = time.Since(start)
			firstByteCaptured = true
		}
		if chunk.Usage != nil {
			promptTokens = chunk.Usage.PromptTokens
			completionTokens = chunk.Usage.CompletionTokens
			if chunk.Usage.PromptTokensDetails != nil {
				cachedTokens = chunk.Usage.PromptTokensDetails.CachedTokens
			}
		}

		for _, choice := range chunk.Choices {
			delta := choice.Delta

			if delta.Content != "" {
				body.WriteString(delta.Content)
				if contentCb != nil {
					contentCb(delta.Content)
				}
			}
			if delta.ReasoningContent != "" {
				thinking.WriteString(delta.ReasoningContent)
				if thinkingCb != nil {
					thinkingCb(delta.ReasoningContent)
				}
			}

			for i, tc := range delta.ToolCalls {
				// The SDK exposes Index as *int (pointer) so unset is
				// distinguishable from index=0. When unset, fall back
				// to the position within the delta's tool_calls array.
				idx := i
				if tc.Index != nil {
					idx = *tc.Index
				}
				ptc, ok := toolCallsByIndex[idx]
				if !ok {
					ptc = &partialToolCall{}
					toolCallsByIndex[idx] = ptc
				}
				if tc.ID != "" {
					ptc.id = tc.ID
				}
				if tc.Function.Name != "" {
					ptc.name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					ptc.args.WriteString(tc.Function.Arguments)
				}
			}

			if choice.FinishReason != "" {
				finishReason = string(choice.FinishReason)
				for _, ptc := range toolCallsByIndex {
					ptc.closed = true
				}
			}
		}
	}

	out := ChatResult{
		TotalDuration:     time.Since(start),
		FirstByteDuration: firstByteAt,
		FinishReason:      finishReason,
		PromptTokens:      promptTokens,
		CachedTokens:      cachedTokens,
		CompletionTokens:  completionTokens,
	}

	// Apply the same think-tag handling the non-streaming path does:
	// inline <think>...</think> blocks in body get stripped from
	// Content and folded into ThinkingContent. This keeps the
	// streaming and non-streaming aggregations interchangeable for
	// the caller.
	rawBody := body.String()
	if t := extractThinkBlocks(rawBody); t != "" {
		if thinking.Len() > 0 {
			thinking.WriteString("\n")
		}
		thinking.WriteString(t)
	}
	cleanBody := stripThinkTags(rawBody)

	out.Content = cleanBody
	out.ThinkingContent = strings.TrimSpace(thinking.String())

	// Reasoning-only fallback: when content stripped to empty but
	// thinking has text, the model put the answer in
	// reasoning_content. Adopt that as the visible content (mirrors
	// pickContentField behavior in the non-streaming path).
	if cleanBody == "" && out.ThinkingContent != "" {
		out.Content = out.ThinkingContent
	}

	// Assemble tool calls in deterministic index order (the map's
	// iteration is random; tests need a stable order).
	if len(toolCallsByIndex) > 0 {
		indices := make([]int, 0, len(toolCallsByIndex))
		for k := range toolCallsByIndex {
			indices = append(indices, k)
		}
		sort.Ints(indices)
		for _, idx := range indices {
			ptc := toolCallsByIndex[idx]
			args := map[string]any{}
			if strings.TrimSpace(ptc.args.String()) != "" {
				if err := json.Unmarshal([]byte(ptc.args.String()), &args); err != nil {
					out.ToolArgsErrors = append(out.ToolArgsErrors, ToolArgsParseError{
						ToolCallID:  ptc.id,
						Name:        ptc.name,
						RawJSON:     ptc.args.String(),
						ParseErrMsg: err.Error(),
					})
					out.ToolCalls = append(out.ToolCalls, ToolCall{
						ID:        ptc.id,
						Name:      ptc.name,
						Arguments: nil,
					})
					continue
				}
			}
			out.ToolCalls = append(out.ToolCalls, ToolCall{
				ID:        ptc.id,
				Name:      ptc.name,
				Arguments: args,
			})
		}
	}

	if out.Content == "" && len(out.ToolCalls) == 0 && len(out.ToolArgsErrors) == 0 {
		out.Empty = true
	}
	return out, nil
}

// chatCompletionStreamRecv is the narrow seam aggregateStream uses to
// receive chunks. *openai.ChatCompletionStream satisfies it; tests pass
// a fake that scripts the chunk sequence without an HTTP roundtrip.
type chatCompletionStreamRecv interface {
	Recv() (openai.ChatCompletionStreamResponse, error)
}
