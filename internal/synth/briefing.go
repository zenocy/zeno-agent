package synth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/llm"
	"github.com/zenocy/zeno-v2/internal/projection"
)

// isQwen3 reports whether the model name belongs to the Qwen3 family. Used
// to gate /no_think and other Qwen3-isms; safe substring match because the
// model name format is "qwen3.6-35b-a3b" or "qwen/qwen3.6-27b" etc.
func isQwen3(model string) bool {
	return strings.Contains(strings.ToLower(model), "qwen3")
}

// BriefingDeps bundles everything the briefing call needs.
type BriefingDeps struct {
	LLM     *llm.Client
	Prompts *PromptSet
	Date    string
	State   State // V2.3.0: adaptive register the briefing is synthesized under (template uses in P2)
	Logger  *logrus.Entry

	// V2.5.0 Phase 3: optional active-concerns context. Top-N by recency
	// from projection.ActiveConcerns. The runner computes once and shares
	// with cards; both surfaces see the same set so a concern referenced
	// in a card and the briefing prose stays coherent. Nil/empty → the
	// briefing prompt's concerns block renders nothing (template-guarded).
	Concerns []projection.Concern

	// LoopObserver receives per-LLM-call events for the briefing stage.
	// Briefing is a single call (plus optional repair) — no tool dispatch,
	// no loop iterations — so only OnLLMCall and OnSchemaRepair fire.
	LoopObserver llm.LoopObserver
}

// SynthesizeBriefing renders the briefing template seeded with cards, calls
// the LLM once (no tools, low temperature), and validates the output.
//
// Qwen3 family detection: when llm.no_think is enabled AND the configured
// model name contains "qwen3", prepend `/no_think` to the system prompt to
// suppress the chain-of-thought emission. The briefing call doesn't benefit
// from scratch reasoning (one shot, no tools), and Qwen3's CoT is the
// dominant token cost on local hardware (7700 completion tokens → ~500-1000
// with /no_think). Voice may regress; measure with `make eval` before
// flipping the knob on. Default off. Other surfaces (cards, reactive) keep
// CoT enabled regardless.
func SynthesizeBriefing(ctx context.Context, d BriefingDeps, cards CardSet) (Briefing, error) {
	systemBuf := &bytes.Buffer{}
	if err := d.Prompts.BriefingSystem.Execute(systemBuf, map[string]any{
		"Voice":      d.Prompts.Voice,
		"StateVoice": d.Prompts.StateVoice[d.State],
		"State":      string(d.State),
		"Date":       d.Date,
		"Cards":      cards.Cards,
		"Concerns":   d.Concerns,
	}); err != nil {
		return Briefing{}, fmt.Errorf("render briefing prompt: %w", err)
	}

	user := fmt.Sprintf("Render the briefing for %s.", d.Date)

	systemContent := systemBuf.String()
	if d.LLM.NoThink() && isQwen3(d.LLM.Model()) {
		systemContent = "/no_think\n\n" + systemContent
	}

	msgs := []llm.Message{
		{Role: "system", Content: systemContent},
		{Role: "user", Content: user},
	}
	callStart := time.Now()
	cr, err := d.LLM.ChatCompletion(ctx, msgs, nil, briefingChatOptions(d.LLM, 0.5)...)
	if d.LoopObserver.OnLLMCall != nil {
		d.LoopObserver.OnLLMCall("briefing", briefingOutcome(err), time.Since(callStart), cr.PromptTokens, cr.CompletionTokens)
	}
	if err != nil {
		return Briefing{}, fmt.Errorf("briefing call: %w", err)
	}

	br, validateErr := parseAndValidateBriefing(cr.Content)
	if validateErr == nil {
		clampTensionToBand(d, &br)
		br.Title = canonicalizeMarkdown(br.Title)
		br.Summary = canonicalizeMarkdown(br.Summary)
		br.Date = d.Date
		return br, nil
	}

	if d.Logger != nil {
		d.Logger.WithError(validateErr).
			WithField("raw_preview", briefingPreview(cr.Content, 800)).
			WithField("comp_tok", cr.CompletionTokens).
			Warn("briefing: validation failed — attempting repair")
	}
	if d.LoopObserver.OnSchemaRepair != nil {
		d.LoopObserver.OnSchemaRepair("briefing", "attempted")
	}

	// One repair attempt. The most common failure on local 7B/8B models is
	// missing or out-of-range tension; we call that out specifically.
	msgs = append(msgs,
		llm.Message{Role: "assistant", Content: cr.Content},
		llm.Message{Role: "system", Content: fmt.Sprintf(
			"Your previous JSON failed validation: %s. Re-emit ONLY a valid JSON Briefing object. "+
				"Required fields: date, eyebrow, title, summary, tension (integer 0-100). No prose, no code fences.",
			validateErr.Error(),
		)},
	)
	repairStart := time.Now()
	cr2, err := d.LLM.ChatCompletion(ctx, msgs, nil, briefingChatOptions(d.LLM, 0.3)...)
	if d.LoopObserver.OnLLMCall != nil {
		d.LoopObserver.OnLLMCall("briefing", briefingOutcome(err), time.Since(repairStart), cr2.PromptTokens, cr2.CompletionTokens)
	}
	if err != nil {
		return Briefing{}, fmt.Errorf("briefing repair call: %w", err)
	}
	br, err = parseAndValidateBriefing(cr2.Content)
	if err != nil {
		if d.Logger != nil {
			d.Logger.WithError(err).
				WithField("raw_preview", briefingPreview(cr2.Content, 800)).
				WithField("comp_tok", cr2.CompletionTokens).
				Warn("briefing: repair failed — degrading")
		}
		if d.LoopObserver.OnSchemaRepair != nil {
			d.LoopObserver.OnSchemaRepair("briefing", "exhausted")
		}
		return Briefing{}, fmt.Errorf("validate briefing: %w", err)
	}
	if d.LoopObserver.OnSchemaRepair != nil {
		d.LoopObserver.OnSchemaRepair("briefing", "succeeded")
	}
	clampTensionToBand(d, &br)
	br.Title = canonicalizeMarkdown(br.Title)
	br.Summary = canonicalizeMarkdown(br.Summary)
	br.Date = d.Date
	return br, nil
}

// briefingOutcome maps an LLM error to the same enum as the loop's
// outcomeFor helper. Kept here (not re-exported from llm) to avoid pulling
// internal/llm error helpers across packages.
func briefingOutcome(err error) string {
	if err == nil {
		return "ok"
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "timeout"
	}
	return "error"
}

// clampTensionToBand snaps an out-of-band tension to the band midpoint for
// d.State. The model on local 7B/8B endpoints anchors on tension=38 (the
// calm-morning canonical) regardless of register; an LLM repair round-trip
// returns the same value. Clamp deterministically and log so the trace
// records what happened. Empty/unknown state leaves the value untouched —
// the band is undefined.
func clampTensionToBand(d BriefingDeps, br *Briefing) {
	if !d.State.IsValid() {
		return
	}
	ok, band := TensionInBand(d.State, br.Tension)
	if ok {
		return
	}
	mid := (band[0] + band[1]) / 2
	if d.Logger != nil {
		d.Logger.WithField("tension", br.Tension).
			WithField("band", band).
			WithField("state", string(d.State)).
			WithField("clamped_to", mid).
			Info("briefing: tension out of band — clamping to band midpoint")
	}
	br.Tension = mid
}

// briefingPreview returns up to maxLen characters of s with an "(empty)"
// sentinel when s is empty. Used to surface what the model actually emitted
// on validation failure so we can tell apart truncation, schema-shape drift,
// and length-floor failures without re-running.
func briefingPreview(s string, maxLen int) string {
	if s == "" {
		return "(empty)"
	}
	if len(s) > maxLen {
		return s[:maxLen] + "…"
	}
	return s
}

// briefingChatOptions assembles the response_format options for the
// briefing call. When the client has json_schema mode enabled, send the
// compiled Briefing schema for by-construction-valid JSON; otherwise fall
// back to json_object mode (post-hoc validation + repair).
//
// MaxTokens is no longer set per-call here; the client carries a default
// (configurable via llm.max_tokens) so every structured-output call gets
// the budget without each call site having to remember.
func briefingChatOptions(client *llm.Client, temperature float32) []llm.ChatOption {
	opts := []llm.ChatOption{llm.WithTemperature(temperature)}
	if client.JSONSchemaEnabled() {
		opts = append(opts, llm.WithJSONSchema("briefing", BriefingSchemaMap()))
	} else {
		opts = append(opts, llm.WithJSONMode())
	}
	return opts
}

func parseAndValidateBriefing(raw string) (Briefing, error) {
	cleaned := stripCodeFences(raw)
	if cleaned == "" {
		return Briefing{}, fmt.Errorf("empty content")
	}
	if err := ValidateJSON(BriefingSchema(), []byte(cleaned)); err != nil {
		return Briefing{}, err
	}
	var out Briefing
	if err := json.Unmarshal([]byte(cleaned), &out); err != nil {
		return Briefing{}, err
	}
	return out, nil
}
