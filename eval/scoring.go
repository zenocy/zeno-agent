package eval

import (
	"regexp"
	"strings"

	"github.com/zenocy/zeno-v2/internal/synth"
)

// Score is one rubric dimension's result, 0–3.
type Score struct {
	Dimension string   `json:"dimension"`
	Value     int      `json:"value"`          // 0..3
	Hits      []string `json:"hits,omitempty"` // matched offending tokens (for explainability)
}

// Scoreboard collects all scores for one run.
type Scoreboard struct {
	Calmness        Score           `json:"calmness"`
	Decisiveness    Score           `json:"decisiveness"`
	Concreteness    Score           `json:"concreteness"`
	MemoryGrounding MemoryGrounding `json:"memory_grounding"`
	StateMatch      StateMatch      `json:"state_match"`     // V2.3.0: rubric stub at P0; real comparison at P1
	TensionInBand   TensionInBand   `json:"tension_in_band"` // V2.3.0 P2: tension within the actual state's band
	BriefingSchema  bool            `json:"briefing_schema_ok"`
	CardsSchema     bool            `json:"cards_schema_ok"`
	BriefingSchema_ string          `json:"briefing_schema_err,omitempty"`
	CardsSchema_    string          `json:"cards_schema_err,omitempty"`
	MustCards       []bool          `json:"must_cards,omitempty"` // parallel to MustCardLabels (incl. per-state suffixes)
	MustCardLabels  []string        `json:"must_card_labels,omitempty"`
	ItalicBalanced  bool            `json:"italic_balanced"` // every * in title/summary is paired
}

// Total returns the rubric subtotal across the three deterministic
// dimensions (max 9). LLM-judge voice_match (Phase 1.5) adds another 3.
func (s Scoreboard) Total() int {
	return s.Calmness.Value + s.Decisiveness.Value + s.Concreteness.Value
}

// AllMustCardsPresent reports whether every must-card matched.
func (s Scoreboard) AllMustCardsPresent() bool {
	for _, v := range s.MustCards {
		if !v {
			return false
		}
	}
	return true
}

// ----------------------------------------------------------------------
// Calmness — penalize urgency, exclamation, all-caps shouting, emoji.
// ----------------------------------------------------------------------

var calmnessPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"exclamation", regexp.MustCompile(`!`)},
	{"important", regexp.MustCompile(`(?i)\bimportant:`)},
	{"note", regexp.MustCompile(`(?i)\bnote:`)},
	{"tldr", regexp.MustCompile(`(?i)\btl;?dr\b`)},
	{"urgent", regexp.MustCompile(`(?i)\burgent\b`)},
	// Emoji range — covers emoticons, symbols, pictographs.
	{"emoji", regexp.MustCompile(`[\x{1F300}-\x{1FAFF}\x{2600}-\x{27BF}]`)},
}

// ScoreCalmness checks all combined briefing + card prose for calmness
// violations. Score: 3 = zero hits, 2 = 1 hit, 1 = 2-3 hits, 0 = 4+.
func ScoreCalmness(text string) Score {
	hits := []string{}
	for _, p := range calmnessPatterns {
		if matches := p.re.FindAllString(text, -1); len(matches) > 0 {
			for _, m := range matches {
				hits = append(hits, p.name+":"+m)
			}
		}
	}
	return Score{Dimension: "calmness", Value: bandFromHitCount(len(hits)), Hits: hits}
}

// ----------------------------------------------------------------------
// Decisiveness — penalize hedging and asking the reader to decide.
// ----------------------------------------------------------------------

var decisivenessPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"maybe", regexp.MustCompile(`(?i)\bmaybe\b`)},
	{"perhaps", regexp.MustCompile(`(?i)\bperhaps\b`)},
	{"might_want", regexp.MustCompile(`(?i)\byou might want to\b`)},
	{"feel_free", regexp.MustCompile(`(?i)\bfeel free to\b`)},
	{"let_me_know", regexp.MustCompile(`(?i)\blet me know\b`)},
	{"would_you_like", regexp.MustCompile(`(?i)\bwould you like\b`)},
	{"hope_helps", regexp.MustCompile(`(?i)\bhope this helps\b`)},
	{"happy_to_adjust", regexp.MustCompile(`(?i)\bhappy to (adjust|help)\b`)},
	{"if_youd_like", regexp.MustCompile(`(?i)\bif you('|’)?d like\b`)},
	{"as_an_ai", regexp.MustCompile(`(?i)\bas an ai\b`)},
}

// ScoreDecisiveness checks for hedge tokens and reader-deferral phrases.
func ScoreDecisiveness(text string) Score {
	hits := []string{}
	for _, p := range decisivenessPatterns {
		if matches := p.re.FindAllString(text, -1); len(matches) > 0 {
			for _, m := range matches {
				hits = append(hits, p.name+":"+m)
			}
		}
	}
	return Score{Dimension: "decisiveness", Value: bandFromHitCount(len(hits)), Hits: hits}
}

// ----------------------------------------------------------------------
// Concreteness — reward times/dates/numbers/named entities; penalize
// vague tokens. Score is ratio-based.
// ----------------------------------------------------------------------

var (
	concreteTimeRE  = regexp.MustCompile(`\b\d{1,2}:\d{2}\b`)
	concreteCountRE = regexp.MustCompile(`\b\d+(?:\.\d+)?(?:m|h|d|°|mm|km|kmh)?\b`)
	properNounRE    = regexp.MustCompile(`\b[A-Z][a-z]+(?:[ \-][A-Z][a-z]+)?\b`)
	vagueTokenRE    = regexp.MustCompile(`(?i)\b(today|soon|recently|recent|later|approximately|around|some|a few)\b`)
)

// ScoreConcreteness counts concrete vs vague tokens. Ratio ≥ 3 → 3, ≥ 2 →
// 2, ≥ 1 → 1, else 0. Empty text → 0.
func ScoreConcreteness(text string) Score {
	if strings.TrimSpace(text) == "" {
		return Score{Dimension: "concreteness", Value: 0}
	}
	concrete := len(concreteTimeRE.FindAllString(text, -1)) +
		len(concreteCountRE.FindAllString(text, -1)) +
		len(properNounRE.FindAllString(text, -1))
	vague := len(vagueTokenRE.FindAllString(text, -1))

	hits := []string{}
	for _, v := range vagueTokenRE.FindAllString(text, -1) {
		hits = append(hits, "vague:"+v)
	}

	denom := vague + 1 // +1 so zero-vague doesn't divide by zero
	ratio := float64(concrete) / float64(denom)
	val := 0
	switch {
	case ratio >= 3.0:
		val = 3
	case ratio >= 2.0:
		val = 2
	case ratio >= 1.0:
		val = 1
	}
	return Score{Dimension: "concreteness", Value: val, Hits: hits}
}

// bandFromHitCount maps a hit count onto a 0–3 score. 0 hits → 3, 1 → 2,
// 2-3 → 1, 4+ → 0.
func bandFromHitCount(n int) int {
	switch {
	case n == 0:
		return 3
	case n == 1:
		return 2
	case n <= 3:
		return 1
	default:
		return 0
	}
}

// ----------------------------------------------------------------------
// Italic balance — every * in briefing title and summary must be paired.
// ----------------------------------------------------------------------

// ItalicBalanced reports whether every `*` in s comes in matched pairs.
func ItalicBalanced(s string) bool {
	count := strings.Count(s, "*")
	return count%2 == 0
}

// ----------------------------------------------------------------------
// MUST cards — deterministic match by source + substring presence.
// ----------------------------------------------------------------------

// CheckMustCards returns one bool per fixture must-card indicating whether
// at least one Card in cs satisfies it (matching one of the permitted
// sources + at least one of title_contains or sub_contains substrings,
// case-insensitive).
func CheckMustCards(cs synth.CardSet, must []MustCard) []bool {
	out := make([]bool, len(must))
	for i, m := range must {
		for _, c := range cs.Cards {
			if !m.Sources.Accepts(c.Source) {
				continue
			}
			if substringHit(c.Title, m.TitleContains) || substringHit(c.Sub, m.SubContains) {
				out[i] = true
				break
			}
		}
	}
	return out
}

func substringHit(haystack string, needles []string) bool {
	if len(needles) == 0 {
		return true // no constraint = match
	}
	low := strings.ToLower(haystack)
	for _, n := range needles {
		if n != "" && strings.Contains(low, strings.ToLower(n)) {
			return true
		}
	}
	return false
}

// ----------------------------------------------------------------------
// Combined scoring
// ----------------------------------------------------------------------

// CombinedProse joins the briefing + card text into a single corpus the
// deterministic scorers run against.
func CombinedProse(b synth.Briefing, cs synth.CardSet) string {
	var sb strings.Builder
	sb.WriteString(b.Eyebrow)
	sb.WriteString("\n")
	sb.WriteString(b.Title)
	sb.WriteString("\n")
	sb.WriteString(b.Summary)
	sb.WriteString("\n")
	for _, c := range cs.Cards {
		sb.WriteString(c.Title)
		sb.WriteString("\n")
		sb.WriteString(c.Sub)
		sb.WriteString("\n")
	}
	return sb.String()
}

// ScoreAll runs the full deterministic scoring battery on one synth run.
// memory carries the fixture's seeded memory state (nil/empty when memory
// wasn't used) so the memory_grounding rubric can run its three checks
// against the same prose. actualState is the briefing's persisted State
// (empty for pre-V2.3 rows / reactive harness path); expectedState is the
// fixture's expected_state. Phase 0 wires the state_match rubric as an
// inert stub — see ScoreStateMatch.
//
// injectMode flips the cards schema validation from CardSetSchema
// (minItems=2) to InjectCardSetSchema (minItems=1). Inject fixtures
// emit exactly one card — validating them against the morning CardSet
// schema spuriously trips the cards_schema_ok flag and tanks the
// report's pass rate even when the model produced a perfect inject.
func ScoreAll(b synth.Briefing, cs synth.CardSet, must []MustCard, memory []FixtureMemoryFact, actualState, expectedState string, injectMode bool) Scoreboard {
	prose := CombinedProse(b, cs)
	stateMatch := ScoreStateMatch(actualState, expectedState)
	sb := Scoreboard{
		Calmness:        ScoreCalmness(prose),
		Decisiveness:    ScoreDecisiveness(prose),
		Concreteness:    ScoreConcreteness(prose),
		MemoryGrounding: ScoreMemoryGrounding(prose, memory),
		StateMatch:      stateMatch,
		// Tension band is keyed on the *actual* detected state — verifies
		// the model honored the register the detector chose, not the
		// fixture's hope. Empty/unknown actual → band is not applied.
		TensionInBand:  ScoreTensionInBand(b, stateMatch.Actual),
		ItalicBalanced: ItalicBalanced(b.Title) && ItalicBalanced(b.Summary),
	}

	// Schema validation against the same compiled validators the synth uses.
	briefingJSON, _ := jsonMarshal(b)
	if err := synth.ValidateJSON(synth.BriefingSchema(), briefingJSON); err != nil {
		sb.BriefingSchema = false
		sb.BriefingSchema_ = err.Error()
	} else {
		sb.BriefingSchema = true
	}
	cardsJSON, _ := jsonMarshal(cs)
	cardsSchema := synth.CardSetSchema()
	if injectMode {
		cardsSchema = synth.InjectCardSetSchema()
	}
	if err := synth.ValidateJSON(cardsSchema, cardsJSON); err != nil {
		sb.CardsSchema = false
		sb.CardsSchema_ = err.Error()
	} else {
		sb.CardsSchema = true
	}

	if len(must) > 0 {
		sb.MustCards = CheckMustCards(cs, must)
		sb.MustCardLabels = make([]string, len(must))
		for i, m := range must {
			sb.MustCardLabels[i] = m.Name
		}
	}
	return sb
}
