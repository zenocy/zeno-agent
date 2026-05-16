package eval

import (
	"regexp"
	"strings"
	"unicode"
)

// MemoryGrounding bundles the V2.2.0 memory_grounding rubric dimension. The
// three deterministic checks are filled in by ScoreMemoryGrounding; the
// LLM-judge slot (WithVsEmpty) is reserved for V2.2.x once the LLM-judge
// infrastructure is wired (see ScoreMemoryWithVsEmpty stub).
type MemoryGrounding struct {
	OpenerTells   Score    `json:"opener_tells"`  // 0/3 — surveillance phrasings
	FactDensity   Score    `json:"fact_density"`  // 0..3 — distinct subjects referenced
	VerbatimLeak  Score    `json:"verbatim_leak"` // 0/3 — multi-word fact text appears verbatim
	WithVsEmpty   Score    `json:"with_vs_empty"` // LLM-judge stub; Value=-1 when not run
	FactsInjected int      `json:"facts_injected"`
	Subjects      []string `json:"subjects_referenced,omitempty"`
	VerbatimHits  []string `json:"verbatim_hits,omitempty"`
}

// Total returns the sum of the three deterministic memory-grounding scores
// (max 9). The LLM-judge slot is excluded — it lives on a different scale
// and only runs when the judge infrastructure is up.
func (m MemoryGrounding) Total() int {
	return m.OpenerTells.Value + m.FactDensity.Value + m.VerbatimLeak.Value
}

// memoryOpenerTellsRE matches surveillance phrasings the briefing must never
// use when memory is present. Trips immediately on any single match.
var memoryOpenerTellsRE = regexp.MustCompile(`(?i)\b(I remember|as you mentioned|based on what I know|you (told|mentioned|said) me)\b`)

// ScoreMemoryOpenerTells returns Score 3 if the prose contains no opener-tell
// phrasings, 0 if any. Catches the obvious "I remember…" / "as you
// mentioned…" failure modes that make memory feel surveilled.
func ScoreMemoryOpenerTells(text string) Score {
	matches := memoryOpenerTellsRE.FindAllString(text, -1)
	if len(matches) == 0 {
		return Score{Dimension: "memory_opener_tells", Value: 3}
	}
	hits := make([]string, 0, len(matches))
	for _, m := range matches {
		hits = append(hits, "opener:"+m)
	}
	return Score{Dimension: "memory_opener_tells", Value: 0, Hits: hits}
}

// ScoreMemoryFactDensity counts the number of distinct memory-fact subjects
// referenced (case-insensitive substring match) in the briefing prose. The
// memory rule caps usage at three; this scorer maps:
//
//	≤3 → 3, 4 → 2, 5 → 1, ≥6 → 0.
//
// Returns the matched subjects in Hits for the per-fixture diff so a
// regression in this dimension is debuggable in one read.
func ScoreMemoryFactDensity(text string, facts []FixtureMemoryFact) Score {
	if len(facts) == 0 {
		return Score{Dimension: "memory_fact_density", Value: 3}
	}
	low := strings.ToLower(text)
	seen := make(map[string]struct{}, len(facts))
	for _, f := range facts {
		subj := strings.ToLower(strings.TrimSpace(f.Subject))
		if subj == "" {
			continue
		}
		if _, dup := seen[subj]; dup {
			continue
		}
		if containsWord(low, subj) {
			seen[subj] = struct{}{}
		}
	}
	hits := make([]string, 0, len(seen))
	for s := range seen {
		hits = append(hits, "subject:"+s)
	}
	val := 3
	switch {
	case len(seen) >= 6:
		val = 0
	case len(seen) == 5:
		val = 1
	case len(seen) == 4:
		val = 2
	}
	return Score{Dimension: "memory_fact_density", Value: val, Hits: hits}
}

// ScoreMemoryVerbatimLeak fails (Value=0) on any fact whose text has ≥3
// words and appears verbatim in the prose. Single-token facts (e.g. "Sam"
// alone) are excluded — they're too false-positive prone, since "Sam" can
// legitimately appear in unrelated prose. Returns the leaking fact texts in
// Hits so the per-fixture diff names which fact leaked.
func ScoreMemoryVerbatimLeak(text string, facts []FixtureMemoryFact) Score {
	if len(facts) == 0 {
		return Score{Dimension: "memory_verbatim_leak", Value: 3}
	}
	low := strings.ToLower(text)
	hits := []string{}
	seen := map[string]struct{}{}
	for _, f := range facts {
		factText := strings.TrimSpace(f.Fact)
		if wordCount(factText) < 3 {
			continue
		}
		needle := strings.ToLower(factText)
		if _, dup := seen[needle]; dup {
			continue
		}
		if strings.Contains(low, needle) {
			seen[needle] = struct{}{}
			hits = append(hits, "leak:"+factText)
		}
	}
	if len(hits) == 0 {
		return Score{Dimension: "memory_verbatim_leak", Value: 3}
	}
	return Score{Dimension: "memory_verbatim_leak", Value: 0, Hits: hits}
}

// ScoreMemoryGrounding runs all three deterministic memory checks against the
// combined briefing+cards prose given the fixture's seeded memory state.
// Pass an empty facts slice when memory wasn't seeded — the scorers degrade
// gracefully (full marks because there's nothing to violate).
func ScoreMemoryGrounding(text string, facts []FixtureMemoryFact) MemoryGrounding {
	density := ScoreMemoryFactDensity(text, facts)
	leak := ScoreMemoryVerbatimLeak(text, facts)
	subjects := make([]string, 0, len(density.Hits))
	for _, h := range density.Hits {
		subjects = append(subjects, strings.TrimPrefix(h, "subject:"))
	}
	verbatim := make([]string, 0, len(leak.Hits))
	for _, h := range leak.Hits {
		verbatim = append(verbatim, strings.TrimPrefix(h, "leak:"))
	}
	return MemoryGrounding{
		OpenerTells:   ScoreMemoryOpenerTells(text),
		FactDensity:   density,
		VerbatimLeak:  leak,
		WithVsEmpty:   stubLLMJudgeMemoryGrounding(),
		FactsInjected: len(facts),
		Subjects:      subjects,
		VerbatimHits:  verbatim,
	}
}

// containsWord reports whether needle appears as a whole word in haystack.
// Both arguments must already be lowercased. "sam" matches "with sam" and
// "sam." but not "samurai".
func containsWord(haystack, needle string) bool {
	if needle == "" {
		return false
	}
	idx := 0
	for {
		i := strings.Index(haystack[idx:], needle)
		if i < 0 {
			return false
		}
		start := idx + i
		end := start + len(needle)
		left := start == 0 || !isWordChar(rune(haystack[start-1]))
		right := end == len(haystack) || !isWordChar(rune(haystack[end]))
		if left && right {
			return true
		}
		idx = start + 1
	}
}

func isWordChar(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

func wordCount(s string) int {
	return len(strings.Fields(s))
}
