package eval

import (
	"strings"
)

// V2.5.0 Phase 2 — concern recognition + retrospective tagging rubrics.
//
// Three deterministic scores land here, all 0..3 banded to match the rest
// of the Scoreboard:
//
//   - ScoreConcernRecognitionExact — proposal name overlap with fixture's
//     ground truth. Loose: a 5-out-of-7 match scores 3, 3-out-of-7 scores 2,
//     1-out-of-7 scores 1, 0 → 0.
//   - ScoreRetrospectiveTagPrecisionRecall — set math against the fixture's
//     expected_concern_tags map. Returns two Scores; the report carries both.
//   - ScoreRecognitionDailyCap — pass=3 / fail=0 boolean rubric on the cap
//     contract.
//
// The LLM judge for voice quality lives in eval/judge_concerns.go; voice
// is too subjective to score deterministically.

// ScoreConcernRecognitionExact compares the actual proposal names against
// the fixture-declared expected_concerns. A proposal "matches" an
// expected concern when its NormalizeName overlaps any of the
// `name_contains` substrings (case-insensitive). When the fixture
// declares zero expected concerns (a negative fixture), the rubric
// inverts: zero proposals = 3, any proposal = 0 (the model should not
// surface noise).
func ScoreConcernRecognitionExact(actual []string, expected []FixtureExpectedConcern) Score {
	dim := "concern_recognition_exact"
	if len(expected) == 0 {
		// Negative fixture: zero proposals = perfect.
		if len(actual) == 0 {
			return Score{Dimension: dim, Value: 3}
		}
		return Score{Dimension: dim, Value: 0, Hits: actual}
	}
	matched := 0
	for _, e := range expected {
		if anyProposalMatches(actual, e) {
			matched++
		}
	}
	ratio := float64(matched) / float64(len(expected))
	switch {
	case ratio >= 0.99:
		return Score{Dimension: dim, Value: 3}
	case ratio >= 0.66:
		return Score{Dimension: dim, Value: 2}
	case ratio > 0:
		return Score{Dimension: dim, Value: 1}
	default:
		return Score{Dimension: dim, Value: 0}
	}
}

func anyProposalMatches(actual []string, exp FixtureExpectedConcern) bool {
	for _, a := range actual {
		al := strings.ToLower(a)
		// Exact normalized name first.
		if strings.Contains(al, strings.ToLower(exp.Name)) {
			return true
		}
		// Then any of the substring hints.
		for _, hint := range exp.NameContains {
			if strings.Contains(al, strings.ToLower(hint)) {
				return true
			}
		}
	}
	return false
}

// ScoreRetrospectiveTagPrecisionRecall is the precision/recall pair on
// the actual-vs-expected tag set. Banding mirrors the brief's exit
// criteria (precision ≥0.85 → 3, ≥0.7 → 2, ≥0.5 → 1, else 0; recall
// uses the same bands but at 0.7/0.5/0.3).
func ScoreRetrospectiveTagPrecisionRecall(actual, expected []string) (Score, Score) {
	expSet := toSet(expected)
	actSet := toSet(actual)

	tp := 0
	for id := range actSet {
		if _, ok := expSet[id]; ok {
			tp++
		}
	}
	fp := len(actSet) - tp
	fn := len(expSet) - tp

	var precision, recall float64
	if tp+fp > 0 {
		precision = float64(tp) / float64(tp+fp)
	}
	if tp+fn > 0 {
		recall = float64(tp) / float64(tp+fn)
	}

	pScore := bandScore("retrospective_precision", precision, 0.85, 0.7, 0.5)
	rScore := bandScore("retrospective_recall", recall, 0.7, 0.5, 0.3)
	return pScore, rScore
}

// ScoreRecognitionDailyCap is a hard pass/fail: the recognition pass
// must never propose more than `cap` concerns. The rubric scores 3 on
// pass and 0 on violation; it's the regression gate that protects the
// user from being buried in proposals on day 1.
func ScoreRecognitionDailyCap(proposalCount, dailyCap int) Score {
	if dailyCap <= 0 {
		dailyCap = 2
	}
	if proposalCount <= dailyCap {
		return Score{Dimension: "recognition_daily_cap", Value: 3}
	}
	return Score{
		Dimension: "recognition_daily_cap",
		Value:     0,
		Hits:      []string{"proposals exceed daily cap"},
	}
}

func toSet(xs []string) map[string]struct{} {
	out := make(map[string]struct{}, len(xs))
	for _, x := range xs {
		out[x] = struct{}{}
	}
	return out
}

func bandScore(dim string, v, hi, mid, lo float64) Score {
	switch {
	case v >= hi:
		return Score{Dimension: dim, Value: 3}
	case v >= mid:
		return Score{Dimension: dim, Value: 2}
	case v >= lo:
		return Score{Dimension: dim, Value: 1}
	default:
		return Score{Dimension: dim, Value: 0}
	}
}

// V2.5.0 Phase 3 — concern-scoped query rubrics.
//
// These three score the surfacing pass end-to-end against fixture
// declarations. They are deterministic (no LLM judge); the concern
// voice judgment lives in eval/judge_concerns.go.

// ScoreConcernQueryMatch is pass/fail on the lookup_concern outcome.
// A match scores 3 only when the actual concern_id matches the
// fixture's expected_concern_id. The negative case
// (must_not_match_concern: true) scores 3 when actual is empty.
// Anything else scores 0.
func ScoreConcernQueryMatch(actualConcernID string, test FixtureConcernQueryTest) Score {
	dim := "concern_query_match"
	actual := strings.TrimSpace(actualConcernID)
	if test.MustNotMatchConcern {
		if actual == "" {
			return Score{Dimension: dim, Value: 3}
		}
		return Score{Dimension: dim, Value: 0, Hits: []string{"unexpected concern matched: " + actual}}
	}
	if actual == test.ExpectedConcernID {
		return Score{Dimension: dim, Value: 3}
	}
	return Score{Dimension: dim, Value: 0, Hits: []string{"got " + actual + ", want " + test.ExpectedConcernID}}
}

// ScoreConcernRetrievalQuality is precision-banded on the intersection
// of the trace's accessed observation IDs and the fixture's
// expected_evidence_ids. Precision = |actual ∩ expected| / |actual|;
// 0.7 → 3, 0.5 → 2, 0.3 → 1, else 0.
//
// We use precision (not recall) because the model may pull MORE
// evidence than strictly needed — that's fine. What matters is that
// the evidence it pulled was actually relevant. A retrieval that
// pulls only irrelevant observations gets 0 even with full recall.
func ScoreConcernRetrievalQuality(accessed, expected []string) Score {
	dim := "concern_retrieval_quality"
	if len(accessed) == 0 {
		// No retrieval at all. If the fixture expected some, score 0;
		// if the fixture also expected none, score 3 (correct refusal).
		if len(expected) == 0 {
			return Score{Dimension: dim, Value: 3}
		}
		return Score{Dimension: dim, Value: 0}
	}
	expSet := toSet(expected)
	tp := 0
	for _, id := range accessed {
		if _, ok := expSet[id]; ok {
			tp++
		}
	}
	precision := float64(tp) / float64(len(accessed))
	return bandScore(dim, precision, 0.7, 0.5, 0.3)
}

// ScoreConcernsBlockBytePresence is the voice-canon regression gate.
// It validates the concerns block's presence/absence in the briefing
// prose against the fixture's seed_concerns expectation:
//
//   - With concerns seeded: the briefing must mention at least one of
//     the seeded names (otherwise the surfacing path didn't fire).
//   - Without concerns: the briefing must NOT mention any of the
//     seeded concern names (since none should be in scope).
//
// Returns 3 on pass, 0 on fail with the offending term in Hits. A
// borderline case where the seeded concern shares a token with an
// unrelated proper noun is acceptable noise; tighten via fixture
// authoring rather than parser heuristics.
func ScoreConcernsBlockBytePresence(briefing string, seededNames []string, hasConcerns bool) Score {
	dim := "concerns_block_byte_presence"
	lowered := strings.ToLower(briefing)
	mentioned := []string{}
	for _, n := range seededNames {
		if strings.Contains(lowered, strings.ToLower(strings.TrimSpace(n))) {
			mentioned = append(mentioned, n)
		}
	}
	if hasConcerns {
		if len(mentioned) == 0 {
			return Score{Dimension: dim, Value: 0, Hits: []string{"briefing did not reference any seeded concern"}}
		}
		return Score{Dimension: dim, Value: 3}
	}
	// No concerns: any mention is leakage.
	if len(mentioned) > 0 {
		return Score{Dimension: dim, Value: 0, Hits: mentioned}
	}
	return Score{Dimension: dim, Value: 3}
}
