// streamcheck is the V2.4 P0 smoke-test driver. It hits a configured
// OpenAI-compatible endpoint (typically LM Studio + Qwen3) with three
// chat-completion requests:
//
//  1. non-stream + json_schema (Card schema)   — V2.3 baseline
//  2. stream    + no schema (text completion)  — text streaming sanity
//  3. stream    + json_schema (Card schema)    — V2.4 risk: does
//     schema-constrained
//     streaming produce
//     valid mid-stream JSON?
//
// For each it records TTFB, TTFT (first non-empty delta for streaming
// runs; n/a for the non-streaming run), total wall time, and prompt
// + completion token counts. Output is one Markdown report on stdout —
// pipe into doc/v2.4/streaming_smoke_test.md.
//
// The driver is intentionally self-contained: it does not import zeno's
// internal/llm so it can serve as a reproduction case if zeno's loop
// behavior diverges from raw SDK behavior.
//
// Usage:
//
//	BASE_URL=http://192.168.88.45:1234/v1 \
//	MODEL=qwen3.6-35b-a3b \
//	go run ./cmd/internal/streamcheck > doc/v2.4/streaming_smoke_test.md
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

// cardSchema mirrors the Card schema the V2.3+ cards loop uses. We
// only need a representative shape, not byte-equality with the real
// internal/synth/schema.go output — the question is whether streaming
// + json_schema works at all on this endpoint.
var cardSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"title": map[string]any{"type": "string", "minLength": 1, "maxLength": 80},
		"sub":   map[string]any{"type": "string", "maxLength": 200},
		"meta":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 4},
	},
	"required":             []string{"title", "sub"},
	"additionalProperties": false,
}

// rawSchema adapts the schema map to openai.ChatCompletionResponseFormatJSONSchema's
// json.Marshaler-shaped Schema field.
type rawSchema map[string]any

func (s rawSchema) MarshalJSON() ([]byte, error) { return json.Marshal(map[string]any(s)) }

// systemPrompt is short on purpose — the smoke test characterizes
// streaming behavior, not voice quality.
const systemPrompt = `You are a calm, literary morning briefing assistant. Reply with exactly one card describing tomorrow's most important meeting.`

const userPrompt = `Tomorrow at 09:30 is a board pre-read with three external attendees including a partner-track investor. Produce one card.`

type result struct {
	name             string
	stream           bool
	schema           bool
	ttfbMs           int64
	ttftMs           int64 // 0 when not streaming
	totalMs          int64
	promptTokens     int
	completionTokens int
	body             string
	finalParsesJSON  bool
	midStreamJSON    string // "n/a" | "yes" | "no" | "mixed"
	err              string
}

func main() {
	baseURL := envOrDefault("BASE_URL", "http://192.168.88.45:1234/v1")
	model := envOrDefault("MODEL", "qwen3.6-35b-a3b")

	cfg := openai.DefaultConfig("")
	cfg.BaseURL = baseURL
	client := openai.NewClientWithConfig(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	a := runConfigA(ctx, client, model)
	b := runConfigB(ctx, client, model)
	c := runConfigC(ctx, client, model)

	report(os.Stdout, baseURL, model, []result{a, b, c})
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Config A: non-stream + json_schema (V2.3 baseline). Should always work.
func runConfigA(ctx context.Context, client *openai.Client, model string) result {
	r := result{name: "A: non-stream + json_schema", stream: false, schema: true, midStreamJSON: "n/a"}
	start := time.Now()
	resp, err := client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: systemPrompt},
			{Role: openai.ChatMessageRoleUser, Content: userPrompt},
		},
		ResponseFormat: &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatTypeJSONSchema,
			JSONSchema: &openai.ChatCompletionResponseFormatJSONSchema{
				Name:   "Card",
				Schema: rawSchema(cardSchema),
				Strict: false,
			},
		},
	})
	r.totalMs = time.Since(start).Milliseconds()
	r.ttfbMs = r.totalMs // for non-streaming, TTFB == total
	if err != nil {
		r.err = err.Error()
		return r
	}
	if len(resp.Choices) > 0 {
		r.body = resp.Choices[0].Message.Content
	}
	r.promptTokens = resp.Usage.PromptTokens
	r.completionTokens = resp.Usage.CompletionTokens
	r.finalParsesJSON = parsesJSON(r.body)
	return r
}

// Config B: stream + no schema. Sanity check that streaming text deltas
// arrive at all.
func runConfigB(ctx context.Context, client *openai.Client, model string) result {
	r := result{name: "B: stream + no schema", stream: true, schema: false, midStreamJSON: "n/a"}
	start := time.Now()
	stream, err := client.CreateChatCompletionStream(ctx, openai.ChatCompletionRequest{
		Model: model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: "You are a calm morning briefing assistant."},
			{Role: openai.ChatMessageRoleUser, Content: "Briefly summarize a 09:30 board pre-read for tomorrow."},
		},
		Stream:        true,
		StreamOptions: &openai.StreamOptions{IncludeUsage: true},
	})
	r.ttfbMs = time.Since(start).Milliseconds()
	if err != nil {
		r.totalMs = r.ttfbMs
		r.err = err.Error()
		return r
	}
	defer stream.Close()
	r.body, r.ttftMs, r.promptTokens, r.completionTokens, err = drainStream(stream, start)
	r.totalMs = time.Since(start).Milliseconds()
	if err != nil {
		r.err = err.Error()
	}
	return r
}

// Config C: stream + json_schema. The risky one. V2.4 needs to know
// whether each delta carries valid-as-of-then JSON, whether the final
// document parses, and whether truncation or schema violations occur.
func runConfigC(ctx context.Context, client *openai.Client, model string) result {
	r := result{name: "C: stream + json_schema", stream: true, schema: true}
	start := time.Now()
	stream, err := client.CreateChatCompletionStream(ctx, openai.ChatCompletionRequest{
		Model: model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: systemPrompt},
			{Role: openai.ChatMessageRoleUser, Content: userPrompt},
		},
		Stream:        true,
		StreamOptions: &openai.StreamOptions{IncludeUsage: true},
		ResponseFormat: &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatTypeJSONSchema,
			JSONSchema: &openai.ChatCompletionResponseFormatJSONSchema{
				Name:   "Card",
				Schema: rawSchema(cardSchema),
				Strict: false,
			},
		},
	})
	r.ttfbMs = time.Since(start).Milliseconds()
	if err != nil {
		r.totalMs = r.ttfbMs
		r.err = err.Error()
		return r
	}
	defer stream.Close()

	// For schema streaming we sample mid-stream parseability: every time
	// a new chunk arrives, try parsing the accumulated body. The fraction
	// of samples that parse cleanly tells us whether the model emits
	// incrementally-valid JSON or whether only the end-of-stream parses.
	var (
		body                   strings.Builder
		ttft                   int64
		samples, parsedSamples int
		promptTok, complTok    int
	)
	for {
		chunk, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			r.err = recvErr.Error()
			break
		}
		if u := chunk.Usage; u != nil {
			promptTok = u.PromptTokens
			complTok = u.CompletionTokens
		}
		for _, choice := range chunk.Choices {
			delta := choice.Delta.Content
			if delta == "" {
				continue
			}
			if ttft == 0 {
				ttft = time.Since(start).Milliseconds()
			}
			body.WriteString(delta)
			samples++
			if parsesJSON(body.String()) {
				parsedSamples++
			}
		}
	}
	r.body = body.String()
	r.ttftMs = ttft
	r.promptTokens = promptTok
	r.completionTokens = complTok
	r.totalMs = time.Since(start).Milliseconds()
	r.finalParsesJSON = parsesJSON(r.body)

	switch {
	case samples == 0:
		r.midStreamJSON = "no-deltas"
	case parsedSamples == samples:
		r.midStreamJSON = "yes (every sample parsed)"
	case parsedSamples == 0:
		r.midStreamJSON = "no (only end-of-stream parses)"
	case parsedSamples == 1 && r.finalParsesJSON:
		r.midStreamJSON = "no (only end-of-stream parses)"
	default:
		r.midStreamJSON = fmt.Sprintf("mixed (%d/%d samples parsed)", parsedSamples, samples)
	}
	return r
}

// drainStream reads a streaming response into a single body string and
// records TTFT (first non-empty delta) + final usage. Used by config B.
func drainStream(stream *openai.ChatCompletionStream, start time.Time) (body string, ttftMs int64, promptTok, complTok int, err error) {
	var b strings.Builder
	for {
		chunk, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			return b.String(), ttftMs, promptTok, complTok, recvErr
		}
		if u := chunk.Usage; u != nil {
			promptTok = u.PromptTokens
			complTok = u.CompletionTokens
		}
		for _, choice := range chunk.Choices {
			delta := choice.Delta.Content
			if delta == "" {
				continue
			}
			if ttftMs == 0 {
				ttftMs = time.Since(start).Milliseconds()
			}
			b.WriteString(delta)
		}
	}
	return b.String(), ttftMs, promptTok, complTok, nil
}

func parsesJSON(s string) bool {
	if strings.TrimSpace(s) == "" {
		return false
	}
	var into any
	return json.Unmarshal([]byte(s), &into) == nil
}

// report writes the Markdown report. Mirrors the structure of
// doc/v2.4/streaming_smoke_test.md so the output drops in cleanly.
func report(w io.Writer, baseURL, model string, results []result) {
	fmt.Fprintln(w, "# V2.4.0 — Streaming Smoke Test (driver output)")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "- **Endpoint:** %s\n", baseURL)
	fmt.Fprintf(w, "- **Model:** %s\n", model)
	fmt.Fprintf(w, "- **Date:** %s\n", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Fprintln(w)

	for _, r := range results {
		fmt.Fprintf(w, "## %s\n\n", r.name)
		if r.err != "" {
			fmt.Fprintf(w, "**ERROR:** %s\n\n", r.err)
		}
		fmt.Fprintf(w, "- TTFB: %d ms\n", r.ttfbMs)
		if r.stream {
			fmt.Fprintf(w, "- TTFT (first non-empty delta): %d ms\n", r.ttftMs)
		}
		fmt.Fprintf(w, "- Total: %d ms\n", r.totalMs)
		fmt.Fprintf(w, "- Prompt tokens: %d\n", r.promptTokens)
		fmt.Fprintf(w, "- Completion tokens: %d\n", r.completionTokens)
		if r.schema {
			fmt.Fprintf(w, "- Final body parses against schema: %v\n", r.finalParsesJSON)
		}
		if r.stream && r.schema {
			fmt.Fprintf(w, "- Mid-stream JSON validity: %s\n", r.midStreamJSON)
		}
		fmt.Fprintf(w, "\n```\n%s\n```\n\n", truncate(r.body, 800))
	}

	fmt.Fprintln(w, "## Decision input")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "If Config C produced valid final JSON AND mid-stream JSON validity is `yes`,")
	fmt.Fprintln(w, "V2.4 streams everything (`llm.stream_schema = true`).")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "If Config C errored, produced invalid final JSON, or mid-stream JSON")
	fmt.Fprintln(w, "validity is `no` / `mixed`, V2.4 streams text-only payloads")
	fmt.Fprintln(w, "(briefing, Ask body) and keeps schema-constrained calls non-streaming")
	fmt.Fprintln(w, "(`llm.stream_schema = false`).")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…[truncated]"
}
