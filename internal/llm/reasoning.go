package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	openai "github.com/sashabaranov/go-openai"
)

// rawChatResponse is a permissive view of an OpenAI-compatible chat
// completion response that captures both the standard `content` field and
// the reasoning-model variants (`reasoning_content`, `reasoning`) some
// servers (LM Studio with Qwen3, DeepSeek-r1, OpenRouter) put the model's
// final answer into when chain-of-thought is enabled.
type rawChatResponse struct {
	Choices []rawChoice `json:"choices"`
	Usage   rawUsage    `json:"usage"`
}

type rawChoice struct {
	Index        int        `json:"index"`
	Message      rawMessage `json:"message"`
	FinishReason string     `json:"finish_reason"`
}

type rawMessage struct {
	Role             string        `json:"role"`
	Content          string        `json:"content"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	Reasoning        string        `json:"reasoning,omitempty"`
	ToolCalls        []rawToolCall `json:"tool_calls,omitempty"`
}

type rawToolCall struct {
	ID       string      `json:"id"`
	Type     string      `json:"type"`
	Function rawFunction `json:"function"`
}

type rawFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type rawUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// chatCompletionDirect is a stdlib-HTTP replacement for the openai library's
// CreateChatCompletion that ALSO captures `reasoning_content` (and
// `reasoning`) from the response. When `content` is empty but a reasoning
// field is populated, we adopt the reasoning text as content after stripping
// any `<think>...</think>` blocks. Returns (response, thinkingText, error)
// where thinkingText is the concatenation of all `<think>...</think>` block
// contents — surfaced upstream as ChatResult.ThinkingContent so the tool
// loop can record it as a Trace thought step.
//
// Why: LM Studio (and other reasoning-aware OpenAI-compat servers) split
// chain-of-thought from the final answer across two fields. The sashabaranov
// library types only the standard fields and silently drops the reasoning
// ones, so a Qwen3-style model in thinking mode hands us 0-byte content
// even when the actual answer is sitting in the response body.
func (c *Client) chatCompletionDirect(ctx context.Context, req openai.ChatCompletionRequest) (openai.ChatCompletionResponse, string, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return openai.ChatCompletionResponse{}, "", fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return openai.ChatCompletionResponse{}, "", fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return openai.ChatCompletionResponse{}, "", err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return openai.ChatCompletionResponse{}, "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		// Return an *openai.APIError so retryChat's classifier picks up
		// retryable HTTP statuses (429/5xx) without code changes there.
		return openai.ChatCompletionResponse{}, "", &openai.APIError{
			HTTPStatusCode: resp.StatusCode,
			Message:        string(respBody),
		}
	}

	var raw rawChatResponse
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return openai.ChatCompletionResponse{}, "", fmt.Errorf("parse response: %w (body: %s)", err, truncate(string(respBody), 300))
	}

	out := openai.ChatCompletionResponse{
		Usage: openai.Usage{
			PromptTokens:     raw.Usage.PromptTokens,
			CompletionTokens: raw.Usage.CompletionTokens,
			TotalTokens:      raw.Usage.TotalTokens,
		},
	}
	var thinking string
	for _, ch := range raw.Choices {
		// Capture <think> blocks and any dedicated reasoning fields BEFORE
		// stripping them off the visible content. First non-empty wins so
		// the Trace step doesn't double up.
		if thinking == "" {
			thinking = collectThinking(ch.Message)
		}
		msg := openai.ChatCompletionMessage{
			Role:    ch.Message.Role,
			Content: pickContentField(ch.Message),
		}
		// Tool calls pass through unchanged. The repair-loop / dispatch
		// downstream depends on tool args being a JSON string per the
		// OpenAI wire format; we preserve that.
		for _, tc := range ch.Message.ToolCalls {
			msg.ToolCalls = append(msg.ToolCalls, openai.ToolCall{
				ID:   tc.ID,
				Type: openai.ToolType(tc.Type),
				Function: openai.FunctionCall{
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				},
			})
		}
		out.Choices = append(out.Choices, openai.ChatCompletionChoice{
			Index:        ch.Index,
			Message:      msg,
			FinishReason: openai.FinishReason(ch.FinishReason),
		})
	}
	return out, thinking, nil
}

// collectThinking returns the model's chain-of-thought text from a message:
// the union of all <think>...</think> block contents found in any of the
// content/reasoning_content/reasoning fields, plus any standalone reasoning
// field text that isn't bracketed. Used to populate ChatResult.ThinkingContent
// so the Trace can show a thought step even when the model puts everything
// in tags or reasoning_content.
func collectThinking(m rawMessage) string {
	// First field with <think> blocks wins; we only need one thought step
	// per call, not a doubled-up concatenation across fields.
	for _, src := range []string{m.Content, m.ReasoningContent, m.Reasoning} {
		if t := extractThinkBlocks(src); t != "" {
			return t
		}
	}
	// No tagged blocks. reasoning_content / reasoning fields without tags
	// are themselves the model's chain-of-thought. Capture the first
	// non-empty one.
	for _, src := range []string{m.ReasoningContent, m.Reasoning} {
		if s := strings.TrimSpace(src); s != "" {
			return s
		}
	}
	return ""
}

// extractThinkBlocks returns the concatenated text inside all
// <think>...</think> blocks of s, joined by newline. Returns "" when no
// blocks present.
func extractThinkBlocks(s string) string {
	if s == "" {
		return ""
	}
	const open, close = "<think>", "</think>"
	out := []string{}
	for {
		startIdx := strings.Index(s, open)
		if startIdx < 0 {
			break
		}
		endIdx := strings.Index(s, close)
		if endIdx < 0 || endIdx < startIdx {
			out = append(out, strings.TrimSpace(s[startIdx+len(open):]))
			break
		}
		out = append(out, strings.TrimSpace(s[startIdx+len(open):endIdx]))
		s = s[endIdx+len(close):]
	}
	return strings.Join(out, "\n")
}

// pickContentField returns the message's effective text content, accounting
// for reasoning-model field splits. Precedence:
//
//  1. content (after <think>...</think> stripping) if non-empty
//  2. reasoning_content (after <think>...</think> stripping) if non-empty
//  3. reasoning (after <think>...</think> stripping) if non-empty
//  4. ""
//
// The strip step covers the case where the model emits inline think blocks
// in its content field (some servers don't split; they inline).
func pickContentField(m rawMessage) string {
	if v := stripThinkTags(m.Content); v != "" {
		return v
	}
	if v := stripThinkTags(m.ReasoningContent); v != "" {
		return v
	}
	if v := stripThinkTags(m.Reasoning); v != "" {
		return v
	}
	return ""
}

// stripThinkTags removes any `<think>...</think>` blocks from s and returns
// the surrounding text trimmed of whitespace. Handles unmatched opening
// `<think>` by taking only what's after it (the model emitted the answer
// after thinking but didn't close the tag — keep the answer, drop the
// thinking).
func stripThinkTags(s string) string {
	if s == "" {
		return ""
	}
	const open, close = "<think>", "</think>"
	for {
		startIdx := strings.Index(s, open)
		if startIdx < 0 {
			break
		}
		endIdx := strings.Index(s, close)
		if endIdx < 0 || endIdx < startIdx {
			// Unclosed <think>: drop everything up to and including the open tag.
			s = strings.TrimSpace(s[startIdx+len(open):])
			break
		}
		s = s[:startIdx] + s[endIdx+len(close):]
	}
	return strings.TrimSpace(s)
}

// truncate returns up to maxLen characters of s with an ellipsis when cut.
// Used for error-message body previews.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "…"
}
