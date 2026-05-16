package eval

import (
	"context"
	"fmt"
	"strings"
)

// V2.5 concern-recognition + concern-voice judges. Both ride the generic
// LLMJudge wrapper so they inherit the offline sentinel and the strict
// JSON-schema response shape.

const concernRecognitionJudgeSystem = `You are a strict reviewer scoring a daily concern-recognition pass. A "concern" is a long-running situation across emails or events — construction at the house, an upcoming trip, a hiring search. It is NOT a project. Penalize project-management language: "in progress", "blocked", "on hold", "follow-up", "action item", "owner", "deadline", "deliverable", "milestone", status badges, completion percentages. Score 0–3:
0 — proposes noise, or uses PM language
1 — proposes plausibly but with PM tone or weak grouping
2 — proposes real concerns with mostly-good voice
3 — proposes real concerns in calm, descriptive Zeno voice
Respond with JSON: {"value": 0..3, "notes": "<≤200 chars>"}.`

const concernVoiceJudgeSystem = `You are a strict reviewer of one concern's name + description for voice. Zeno's voice is calm, declarative, literary — never project-management language. Penalize: "in progress", "blocked", "on hold", "follow-up", "action item", "owner", "deadline", "deliverable", "milestone", "kanban", "track", "status", percentages, exclamation marks, emoji. The description should read as a thread of attention, not a task. Score 0–3:
0 — PM language or shouting
1 — neutral but generic
2 — quiet and descriptive
3 — distinctly Zeno: short, literary, weighted
Respond with JSON: {"value": 0..3, "notes": "<≤200 chars>"}.`

// LLMJudgeConcernRecognition scores a recognition pass output (a list of
// proposals as `name — description` lines) against the fixture's expected
// concern names. Used to grade the daily pass's calmness and clustering
// judgment in one number.
func LLMJudgeConcernRecognition(ctx context.Context, client JudgeClient, proposals, expectedNames []string) (Score, error) {
	var b strings.Builder
	b.WriteString("EXPECTED CONCERNS (ground truth):\n")
	if len(expectedNames) == 0 {
		b.WriteString("(none — this fixture is negative; the pass should propose 0–1 weak items at most)\n")
	} else {
		for _, n := range expectedNames {
			fmt.Fprintf(&b, "- %s\n", n)
		}
	}
	b.WriteString("\nPROPOSED:\n")
	if len(proposals) == 0 {
		b.WriteString("(none)\n")
	} else {
		for _, p := range proposals {
			fmt.Fprintf(&b, "- %s\n", p)
		}
	}
	b.WriteString("\nDoes this proposal set match the expected concerns, in calm Zeno voice, without PM language?")
	return LLMJudge(ctx, client, JudgeRequest{
		Dimension: "concern_recognition",
		System:    concernRecognitionJudgeSystem,
		User:      b.String(),
	})
}

// LLMJudgeConcernVoice scores one description for voice. Used in the
// recognition rubric and (post-Phase 2) in the briefing-prose check
// when the briefing references a concern.
func LLMJudgeConcernVoice(ctx context.Context, client JudgeClient, name, description string) (Score, error) {
	user := fmt.Sprintf("CONCERN NAME: %s\nDESCRIPTION: %s\n\nIs this in Zeno's voice — calm, declarative, no PM language?", name, description)
	return LLMJudge(ctx, client, JudgeRequest{
		Dimension: "concern_voice",
		System:    concernVoiceJudgeSystem,
		User:      user,
	})
}
