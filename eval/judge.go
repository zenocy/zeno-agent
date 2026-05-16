package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zenocy/zeno-v2/internal/llm"
)

// JudgeClient is the narrow surface eval needs from llm.Client. Defined as
// an interface so tests can stub it without booting a real endpoint.
type JudgeClient interface {
	ChatCompletion(
		ctx context.Context,
		messages []llm.Message,
		tools []llm.ToolDefinition,
		opts ...llm.ChatOption,
	) (llm.ChatResult, error)
}

// JudgeRequest is the input to the generic LLM-judge wrapper. Dimension
// is the rubric name that lands in Score.Dimension (and in the report).
// System and User are the two prompt halves; Schema constrains the
// response to a small JSON object the wrapper can decode.
type JudgeRequest struct {
	Dimension string
	System    string
	User      string
}

// JudgeResponse is the schema-constrained shape the model returns.
// Value is bounded 0..3 to match the rest of the Scoreboard. Notes
// carries one-line reasoning so the report can render an explanation.
type JudgeResponse struct {
	Value int    `json:"value"`
	Notes string `json:"notes"`
}

// judgeResponseSchema is the JSON-Schema object enforced via
// WithJSONSchema. Strict; integer 0..3; short notes.
var judgeResponseSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"value": map[string]any{
			"type":    "integer",
			"minimum": 0,
			"maximum": 3,
		},
		"notes": map[string]any{
			"type":      "string",
			"maxLength": 280,
		},
	},
	"required":             []string{"value", "notes"},
	"additionalProperties": false,
}

// LLMJudge runs one rubric pass against the configured LLM client. If the
// client is nil (offline test runs, CI without endpoint, replay) the
// sentinel Score{Value: -1} is returned so report tooling renders "—"
// rather than faking pass/fail. Any client error is surfaced verbatim
// alongside the sentinel — callers decide whether the judge is required
// for their gate.
func LLMJudge(ctx context.Context, client JudgeClient, req JudgeRequest) (Score, error) {
	if client == nil {
		return Score{Dimension: req.Dimension, Value: -1}, nil
	}
	if strings.TrimSpace(req.System) == "" || strings.TrimSpace(req.User) == "" {
		return Score{Dimension: req.Dimension, Value: -1}, errors.New("judge: empty system or user prompt")
	}

	res, err := client.ChatCompletion(ctx,
		[]llm.Message{
			{Role: "system", Content: req.System},
			{Role: "user", Content: req.User},
		},
		nil,
		llm.WithTemperature(0.0),
		llm.WithJSONSchema(req.Dimension, judgeResponseSchema),
	)
	if err != nil {
		return Score{Dimension: req.Dimension, Value: -1}, err
	}
	if strings.TrimSpace(res.Content) == "" {
		return Score{Dimension: req.Dimension, Value: -1}, errors.New("judge: empty response")
	}

	var parsed JudgeResponse
	if err := json.Unmarshal([]byte(res.Content), &parsed); err != nil {
		return Score{Dimension: req.Dimension, Value: -1}, fmt.Errorf("judge: parse response: %w", err)
	}
	if parsed.Value < 0 || parsed.Value > 3 {
		return Score{Dimension: req.Dimension, Value: -1}, fmt.Errorf("judge: out-of-range value %d", parsed.Value)
	}
	notes := strings.TrimSpace(parsed.Notes)
	out := Score{Dimension: req.Dimension, Value: parsed.Value}
	if notes != "" {
		out.Hits = []string{notes}
	}
	return out, nil
}
