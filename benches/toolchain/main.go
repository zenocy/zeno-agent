// Bench: tool-call reliability (gate 2).
//
// Runs a 5-step tool chain (read inbox → filter → summarize → draft → return)
// against the configured LLM endpoint, repeated N times. Each step's tool
// call is dispatched against a stub that returns a deterministic fixture
// answer; the bench measures only the model's ability to chain tool calls
// reliably.
//
// Usage:
//
//	go run ./benches/toolchain -endpoint=http://localhost:11434/v1 -model=gemma3:4b -runs=20
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

type toolStub struct {
	name        string
	description string
	parameters  map[string]any
	answer      string
}

var chain = []toolStub{
	{
		name:        "read_inbox",
		description: "Return the latest 20 emails as a JSON list.",
		parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
			"required":   []string{},
		},
		answer: `[{"from":"Saru","subject":"re: redline","preview":"Two questions remain"},{"from":"Sam","subject":"Lia's recital","preview":"can you be there?"},{"from":"Ashby","subject":"Owen panel","preview":"hold expires"}]`,
	},
	{
		name:        "filter_priority",
		description: "Filter the email list to those that need a response today.",
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"emails": map[string]any{"type": "string", "description": "JSON list of emails"},
			},
			"required": []string{"emails"},
		},
		answer: `[{"from":"Saru","subject":"re: redline","preview":"Two questions remain"}]`,
	},
	{
		name:        "summarize_thread",
		description: "Summarize a single email thread.",
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"thread": map[string]any{"type": "string", "description": "thread JSON"},
			},
			"required": []string{"thread"},
		},
		answer: "Saru wants answers on option-pool sizing and the 1x non-participating preferred. He references your Friday redline. Tone is positive.",
	},
	{
		name:        "draft_reply",
		description: "Draft a short reply to a summarized thread.",
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"summary": map[string]any{"type": "string"},
			},
			"required": []string{"summary"},
		},
		answer: "Saru — option pool sits post-money, and we have flex on the 1x. Let's walk it at 11. — M",
	},
	{
		name:        "return_card",
		description: "Return the final response card. After this, you are done.",
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"draft": map[string]any{"type": "string"},
			},
			"required": []string{"draft"},
		},
		answer: "ok",
	},
}

func main() {
	var (
		endpoint = flag.String("endpoint", "http://localhost:11434/v1", "OpenAI-compatible endpoint")
		model    = flag.String("model", "gemma3:4b", "model name")
		apiKey   = flag.String("api-key", "", "API key (optional)")
		label    = flag.String("label", "", "label (defaults to model)")
		runs     = flag.Int("runs", 20, "number of runs")
		report   = flag.String("report", "benches/REPORT.md", "path to append summary to")
		timeout  = flag.Duration("timeout", 60*time.Second, "per-run timeout")
	)
	flag.Parse()
	if *label == "" {
		*label = *model
	}

	cfg := openai.DefaultConfig(*apiKey)
	cfg.BaseURL = strings.TrimRight(*endpoint, "/")
	client := openai.NewClientWithConfig(cfg)

	tools := buildTools()
	system := "You are an agent. Use the tools in order: read_inbox, filter_priority, summarize_thread, draft_reply, return_card. Call exactly one tool per turn. After return_card, stop."

	successes := 0
	var totalDur time.Duration
	for i := 1; i <= *runs; i++ {
		start := time.Now()
		ok, err := runOne(context.Background(), client, *model, *timeout, tools, system)
		totalDur += time.Since(start)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[toolchain %d/%d] error: %v\n", i, *runs, err)
			continue
		}
		if ok {
			successes++
		}
		fmt.Printf("[toolchain %d/%d] %s\n", i, *runs, statusOf(ok, err))
	}

	rate := float64(successes) / float64(*runs) * 100
	avg := totalDur / time.Duration(*runs)
	block := fmt.Sprintf("\n## toolchain · %s · %s\n\n_runs=%d, success=%d (%.1f%%), avg=%s_\n",
		*label, time.Now().Format(time.RFC3339), *runs, successes, rate, avg.Round(time.Millisecond))
	if err := appendFile(*report, block); err != nil {
		fmt.Fprintf(os.Stderr, "[toolchain] write report: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("[toolchain] %s: %d/%d (%.1f%%)\n", *label, successes, *runs, rate)
	if rate < 80 {
		os.Exit(2)
	}
}

func buildTools() []openai.Tool {
	out := make([]openai.Tool, 0, len(chain))
	for _, t := range chain {
		out = append(out, openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        t.name,
				Description: t.description,
				Parameters:  t.parameters,
			},
		})
	}
	return out
}

// runOne returns (success, err). Success means the model called every step in
// chain order and finished by calling return_card.
func runOne(ctx context.Context, client *openai.Client, model string, timeout time.Duration, tools []openai.Tool, system string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	expected := 0
	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: system},
		{Role: openai.ChatMessageRoleUser, Content: "Run the chain and return the response card."},
	}

	for iter := 0; iter < 8; iter++ {
		resp, err := client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
			Model:    model,
			Messages: messages,
			Tools:    tools,
		})
		if err != nil {
			return false, err
		}
		choice := resp.Choices[0]
		messages = append(messages, choice.Message)

		if len(choice.Message.ToolCalls) == 0 {
			return false, nil
		}
		// Use the first tool call only (some models emit several).
		tc := choice.Message.ToolCalls[0]
		if tc.Function.Name != chain[expected].name {
			return false, nil
		}
		// Acknowledge with the canned answer.
		messages = append(messages, openai.ChatCompletionMessage{
			Role:       openai.ChatMessageRoleTool,
			ToolCallID: tc.ID,
			Content:    chain[expected].answer,
		})
		if chain[expected].name == "return_card" {
			return true, nil
		}
		expected++
		if expected >= len(chain) {
			return true, nil
		}
	}
	return false, nil
}

func statusOf(ok bool, err error) string {
	switch {
	case err != nil:
		return "error"
	case ok:
		return "ok"
	default:
		return "fail"
	}
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
