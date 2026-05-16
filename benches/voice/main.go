// Bench: voice quality (gate 1).
//
// Generates a morning briefing against a fixture projection set, using one
// or more LLM endpoints. The output is appended to benches/REPORT.md as a
// side-by-side block for human reading. The judgement is human, not
// automated — see benches/voice/RUBRIC.md.
//
// Usage:
//
//	go run ./benches/voice \
//	    -endpoint=http://localhost:11434/v1 \
//	    -model=gemma3:4b \
//	    -label="gemma3:4b @ ollama"
//
// Run repeatedly with different -endpoint/-model/-label combinations to
// build up a side-by-side comparison.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

const briefingPromptTemplate = `You are Zeno, an ambient cognitive surface for a single user. Read the voice rules and the projections below; produce ONLY the morning briefing prose.

%s

---

The day's projections (JSON):

%s

---

Output strictly the briefing in this format:

Eyebrow: <one short lowercase phrase, e.g. "this morning · 4 things worth knowing">
Title: <one or two short sentences, with one word wrapped in *asterisks* for italics>
Summary: <one literary paragraph, 2–4 sentences>
Tension: <integer 0–100>

No preamble. No closing. No bullet points. No emoji.`

func main() {
	var (
		endpoint    = flag.String("endpoint", "http://localhost:11434/v1", "OpenAI-compatible endpoint")
		model       = flag.String("model", "gemma3:4b", "model name")
		apiKey      = flag.String("api-key", "", "API key (optional)")
		label       = flag.String("label", "", "label for this run (defaults to model)")
		report      = flag.String("report", "benches/REPORT.md", "path to append results to")
		fixturePath = flag.String("fixture", "benches/fixtures/morning_projections.json", "projections fixture")
		voicePath   = flag.String("voice", "prompts/_voice.md", "voice rules markdown")
		timeout     = flag.Duration("timeout", 360*time.Second, "request timeout")
	)
	flag.Parse()
	if *label == "" {
		*label = *model
	}

	fixture, err := os.ReadFile(*fixturePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[voice] read fixture: %v\n", err)
		os.Exit(1)
	}
	voice, err := os.ReadFile(*voicePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[voice] read voice rules: %v\n", err)
		os.Exit(1)
	}

	cfg := openai.DefaultConfig(*apiKey)
	cfg.BaseURL = strings.TrimRight(*endpoint, "/")
	client := openai.NewClientWithConfig(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	prompt := fmt.Sprintf(briefingPromptTemplate, string(voice), string(fixture))
	start := time.Now()
	resp, err := client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: *model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleUser, Content: prompt},
		},
		Temperature: 0.7,
	})
	dur := time.Since(start)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[voice] %s: %v\n", *label, err)
		os.Exit(1)
	}
	out := resp.Choices[0].Message.Content

	block := fmt.Sprintf("\n## voice · %s · %s\n\n_duration: %s, prompt tokens: %d, completion tokens: %d_\n\n```\n%s\n```\n",
		*label, time.Now().Format(time.RFC3339), dur.Round(time.Millisecond),
		resp.Usage.PromptTokens, resp.Usage.CompletionTokens, strings.TrimSpace(out))

	if err := appendFile(*report, block); err != nil {
		fmt.Fprintf(os.Stderr, "[voice] write report: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("[voice] %s ok (%s) → %s\n", *label, dur.Round(time.Millisecond), filepath.Clean(*report))
}

func appendFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(content)
	return err
}
