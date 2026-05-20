package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"google.golang.org/genai"

	"github.com/zenocy/zeno-v2/internal/llm"
)

// chatCompletionStream consumes the SSE stream from
// GenerateContentStream and aggregates it into a single ChatResult.
// Each chunk's parts are routed:
//
//   - Part.Thought == true → StreamThinkingFunc + ThinkingContent
//   - Part.Text (no Thought) → StreamContentFunc + Content
//   - Part.FunctionCall → ToolCalls (whole calls, not deltas — Gemini
//     emits each FunctionCall as a single part on the wire, unlike
//     OpenAI which streams the args string)
//
// Grounding metadata and finish reason are read from the LAST chunk
// that carries them (Gemini sends them on the final candidate).
func (c *Client) chatCompletionStream(
	ctx context.Context,
	contents []*genai.Content,
	cfg *genai.GenerateContentConfig,
) (llm.ChatResult, error) {
	start := time.Now()
	contentCb := llm.StreamContentFromContext(ctx)
	thinkingCb := llm.StreamThinkingFromContext(ctx)

	out := llm.ChatResult{}
	var (
		contentBuilder strings.Builder
		thinkBuilder   strings.Builder
		ttfb           time.Duration
		seenAny        bool
		toolCalls      []llm.ToolCall
		toolErrors     []llm.ToolArgsParseError
		finalCandidate *genai.Candidate
	)

	for resp, err := range c.api.Models.GenerateContentStream(ctx, c.model, contents, cfg) {
		if err != nil {
			c.trafficLog(logrus.Fields{
				"purpose":     "chat",
				"path":        "stream",
				"model":       c.model,
				"duration_ms": time.Since(start).Milliseconds(),
				"error":       err.Error(),
			}, "gemini: stream failed")
			return llm.ChatResult{}, err
		}
		if resp == nil {
			continue
		}
		if !seenAny {
			ttfb = time.Since(start)
			seenAny = true
		}
		if resp.UsageMetadata != nil {
			// Gemini sends incremental + final counts; the last chunk
			// is authoritative.
			out.PromptTokens = int(resp.UsageMetadata.PromptTokenCount)
			out.CachedTokens = int(resp.UsageMetadata.CachedContentTokenCount)
			out.CompletionTokens = int(resp.UsageMetadata.CandidatesTokenCount)
		}
		if len(resp.Candidates) == 0 {
			continue
		}
		cand := resp.Candidates[0]
		finalCandidate = cand
		if cand.Content == nil {
			continue
		}
		for _, p := range cand.Content.Parts {
			if p == nil {
				continue
			}
			if p.FunctionCall != nil {
				args := p.FunctionCall.Args
				if args == nil {
					args = map[string]any{}
				}
				id := p.FunctionCall.ID
				if id == "" {
					id = fmt.Sprintf("call_%s_%d", p.FunctionCall.Name, len(toolCalls))
				}
				toolCalls = append(toolCalls, llm.ToolCall{
					ID:        id,
					Name:      p.FunctionCall.Name,
					Arguments: args,
					// Preserve the thought signature for echo-back on
					// the next turn — Gemini rejects FunctionCall parts
					// missing it when thinking is on.
					ProviderState: p.ThoughtSignature,
				})
				continue
			}
			if p.Thought {
				if p.Text != "" {
					if thinkingCb != nil {
						thinkingCb(p.Text)
					}
					if thinkBuilder.Len() > 0 {
						thinkBuilder.WriteByte('\n')
					}
					thinkBuilder.WriteString(p.Text)
				}
				continue
			}
			if p.Text != "" {
				if contentCb != nil {
					contentCb(p.Text)
				}
				contentBuilder.WriteString(p.Text)
			}
		}
	}

	out.Content = contentBuilder.String()
	out.ThinkingContent = thinkBuilder.String()
	out.ToolCalls = toolCalls
	out.ToolArgsErrors = toolErrors
	out.TotalDuration = time.Since(start)
	out.FirstByteDuration = ttfb

	if finalCandidate != nil {
		out.FinishReason = string(finalCandidate.FinishReason)
		if finalCandidate.GroundingMetadata != nil {
			out.Citations = extractCitations(finalCandidate.GroundingMetadata)
		}
	}
	if out.Content == "" && len(out.ToolCalls) == 0 && len(out.ToolArgsErrors) == 0 && !isSafetyFinishStr(out.FinishReason) {
		out.Empty = true
	}

	c.trafficLog(logrus.Fields{
		"purpose":           "chat",
		"path":              "stream",
		"model":             c.model,
		"prompt_tokens":     out.PromptTokens,
		"cached_tokens":     out.CachedTokens,
		"completion_tokens": out.CompletionTokens,
		"finish_reason":     out.FinishReason,
		"tool_calls":        len(out.ToolCalls),
		"citations":         len(out.Citations),
		"ttfb_ms":           out.FirstByteDuration.Milliseconds(),
		"duration_ms":       out.TotalDuration.Milliseconds(),
	}, "gemini: stream ok")
	return out, nil
}

// isSafetyFinishStr is the string-keyed counterpart to isSafetyFinish
// used after we've already converted the finish reason to its raw
// string form (stream path).
func isSafetyFinishStr(fr string) bool {
	return isSafetyFinish(genai.FinishReason(fr))
}

// decodeJSONBytes is a tiny helper around json.Unmarshal kept here so
// chat.go doesn't pull encoding/json directly. Centralizing it makes
// it easy to swap for a faster decoder later.
func decodeJSONBytes(b []byte, out any) error {
	return json.Unmarshal(b, out)
}
