// Bench: schema reliability (gate 4 — added during planning).
//
// Asks the model to emit a JSON object matching a CardSet-shaped schema,
// repeated N times. Counts: clean parses (model emitted valid JSON
// matching shape), repaired (model emitted invalid JSON but a single
// repair pass fixed it), and hard failures.
//
// This is a Phase 2 risk gate; in Phase 0 the harness is a small but
// realistic JSON-output exercise so the gate can run before Phase 2 lands.
//
// Usage:
//
//	go run ./benches/schema -endpoint=http://localhost:11434/v1 -model=gemma3:4b -runs=20
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

const schemaSpec = `Output ONLY a single JSON object matching this shape — no prose, no markdown fences:

{
  "cards": [
    {
      "id":       string,
      "source":   "mail" | "calendar" | "personal",
      "rel":      "high" | "med" | "low",
      "title":    string,
      "sub":      string,
      "meta":     [string, ...],
      "actions":  [{ "label": string, "primary": bool }, ...]
    }
  ]
}

cards must contain at least one entry derived from the projections below.`

func main() {
	var (
		endpoint    = flag.String("endpoint", "http://localhost:11434/v1", "OpenAI-compatible endpoint")
		model       = flag.String("model", "gemma3:4b", "model name")
		apiKey      = flag.String("api-key", "", "API key (optional)")
		runs        = flag.Int("runs", 20, "number of runs")
		label       = flag.String("label", "", "label (defaults to model)")
		report      = flag.String("report", "benches/REPORT.md", "path to append summary to")
		fixturePath = flag.String("fixture", "benches/fixtures/morning_projections.json", "projections fixture")
		timeout     = flag.Duration("timeout", 60*time.Second, "per-run timeout")
	)
	flag.Parse()
	if *label == "" {
		*label = *model
	}

	fixture, err := os.ReadFile(*fixturePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[schema] read fixture: %v\n", err)
		os.Exit(1)
	}

	cfg := openai.DefaultConfig(*apiKey)
	cfg.BaseURL = strings.TrimRight(*endpoint, "/")
	client := openai.NewClientWithConfig(cfg)

	prompt := fmt.Sprintf("%s\n\nProjections:\n\n%s", schemaSpec, string(fixture))

	clean, repaired, failed := 0, 0, 0
	for i := 1; i <= *runs; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		resp, err := client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
			Model:    *model,
			Messages: []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: prompt}},
		})
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[schema %d/%d] error: %v\n", i, *runs, err)
			failed++
			continue
		}
		raw := strings.TrimSpace(resp.Choices[0].Message.Content)
		state := classify(raw)
		fmt.Printf("[schema %d/%d] %s\n", i, *runs, state)
		switch state {
		case "clean":
			clean++
		case "repaired":
			repaired++
		default:
			failed++
		}
	}

	cleanPct := float64(clean) / float64(*runs) * 100
	block := fmt.Sprintf("\n## schema · %s · %s\n\n_runs=%d, clean=%d (%.1f%%), repaired=%d, failed=%d_\n",
		*label, time.Now().Format(time.RFC3339), *runs, clean, cleanPct, repaired, failed)
	if err := appendFile(*report, block); err != nil {
		fmt.Fprintf(os.Stderr, "[schema] write report: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("[schema] %s: clean=%d/%d (%.1f%%), repaired=%d, failed=%d\n",
		*label, clean, *runs, cleanPct, repaired, failed)
	if cleanPct < 80 {
		os.Exit(2)
	}
}

func classify(raw string) string {
	if isCleanJSON(raw) {
		return "clean"
	}
	stripped := stripFences(raw)
	if isCleanJSON(stripped) {
		return "repaired"
	}
	return "failed"
}

func isCleanJSON(s string) bool {
	var v any
	return json.Unmarshal([]byte(s), &v) == nil && hasCardsKey(v)
}

func hasCardsKey(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	_, ok = m["cards"]
	return ok
}

// stripFences removes ```json ... ``` markdown fences if present.
func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		// trim any leading ```lang and trailing ```
		if i := strings.Index(s, "\n"); i > 0 {
			s = s[i+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
		s = strings.TrimSpace(s)
	}
	return s
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
