package eval

import (
	"context"
	"fmt"
)

// LLM-judge with-vs-empty memory-grounding diff.
//
// The judge runs the same fixture twice (once with seeded memory, once with
// an empty memory state) and asks a frontier model: "Did the with-memory
// briefing add substance, or just redecorate?" Substance = different
// priorities, different framing, different cards. Redecorate = same content,
// different word choices.
//
// V2.5 wires this through the generic LLMJudge wrapper in eval/judge.go.
// Pass a nil client (offline / CI without endpoint) and the sentinel
// Score{Value:-1} flows through unchanged — the V2.2 stub semantics still
// hold for anyone running without a configured judge.

// MemoryJudgeResult is the structured output the judge returns. Kept for
// historical reasons; the generic JudgeResponse covers the same ground.
type MemoryJudgeResult struct {
	AddedSubstance bool   `json:"added_substance"`
	Reasoning      string `json:"reasoning"`
}

const memoryJudgeSystem = `You are a strict reviewer evaluating whether a "with-memory" briefing adds real substance over an "empty-memory" briefing. Substance means different priorities, different framing, or different cards — not just different word choices. Decorative differences (synonyms, sentence reorderings, variant openers) score 0–1. Real differences (a card that exists only in one version, a tension shift, a different opener subject because of memory) score 2–3. Be terse. Respond with JSON: {"value": 0..3, "notes": "<≤200 chars>"}.`

// LLMJudgeMemoryGrounding asks the judge whether the with-memory briefing
// adds substance over the empty-memory baseline. Both inputs are full
// briefing prose. Pass a nil client to keep the offline sentinel behavior.
func LLMJudgeMemoryGrounding(ctx context.Context, client JudgeClient, withMemory, withoutMemory string) (Score, error) {
	user := fmt.Sprintf("EMPTY-MEMORY BRIEFING:\n%s\n\nWITH-MEMORY BRIEFING:\n%s\n\nDoes the with-memory version add substance, or only redecorate?", withoutMemory, withMemory)
	return LLMJudge(ctx, client, JudgeRequest{
		Dimension: "memory_with_vs_empty",
		System:    memoryJudgeSystem,
		User:      user,
	})
}

// stubLLMJudgeMemoryGrounding is retained for any V2.2-era call site that
// still expects the offline sentinel directly. New code should call
// LLMJudgeMemoryGrounding(ctx, nil, ...) — that path returns the same
// sentinel and is the documented offline-friendly entry point.
func stubLLMJudgeMemoryGrounding() Score {
	return Score{Dimension: "memory_with_vs_empty", Value: -1}
}
