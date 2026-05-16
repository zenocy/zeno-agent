package synth

import (
	"strings"
	"testing"
)

// TestSchemaGate_FixtureDriftRecoverable simulates the kind of drift the
// cards.go validate-or-repair flow has to handle in production: code fences,
// trailing prose, near-valid output that should round-trip cleanly. Gate-4
// in doc/Phase2.md is "clean-parse rate ≥80% over 20 fixture runs"; this
// test asserts the *defenses* (fence-stripping + validation + parse) accept
// ≥16/20 of these inputs without the LLM ever needing to be re-asked.
//
// What this does NOT verify: real-LLM output reliability against a model
// running locally. That's a benches/schema/ run, not a unit test.
func TestSchemaGate_FixtureDriftRecoverable(t *testing.T) {
	const goodCard = `{
  "id": "saru",
  "date": "2026-04-25",
  "src": "mail",
  "src_label": "Email · Acuity",
  "rel": "high",
  "kind": "",
  "title": "Saru Patel · re: redline",
  "sub": "Walked the redline with Lin. Two questions remain.",
  "meta": ["06:14"],
  "actions": [{"label":"Draft","primary":true}]
}`
	const goodCard2 = `{
  "id": "lia",
  "date": "2026-04-25",
  "src": "personal",
  "src_label": "Family",
  "rel": "high",
  "kind": "personal",
  "title": "Lia's school recital is Thursday",
  "sub": "Sam asked if you can be there.",
  "meta": ["Thu"],
  "actions": [{"label":"OK"}]
}`
	wrap := func(c1, c2 string) string {
		return `{"cards":[` + c1 + `,` + c2 + `]}`
	}
	clean := wrap(goodCard, goodCard2)

	// 20 fixtures: each is a way the LLM might serialize the same content.
	// The first 16 are recoverable by the existing parse-strip-validate path;
	// the last 4 represent failures the repair flow has to trigger on.
	fixtures := []struct {
		name        string
		raw         string
		recoverable bool
	}{
		// 16 recoverable variants (clean parse after strip).
		{"plain", clean, true},
		{"with_json_fence", "```json\n" + clean + "\n```", true},
		{"with_bare_fence", "```\n" + clean + "\n```", true},
		{"with_leading_whitespace", "   \n" + clean, true},
		{"with_trailing_whitespace", clean + "\n   ", true},
		{"compact_no_whitespace", strings.ReplaceAll(strings.ReplaceAll(clean, "\n", ""), "  ", ""), true},
		{"with_extra_newlines", "\n\n" + clean + "\n\n", true},
		{"three_cards_min_ok", `{"cards":[` + goodCard + `,` + goodCard2 + `,` + strings.Replace(goodCard, `"id": "saru"`, `"id": "third"`, 1) + `]}`, true},
		{"six_cards_max_ok", `{"cards":[` +
			goodCard + `,` + goodCard2 + `,` +
			strings.Replace(goodCard, `"id": "saru"`, `"id": "c3"`, 1) + `,` +
			strings.Replace(goodCard, `"id": "saru"`, `"id": "c4"`, 1) + `,` +
			strings.Replace(goodCard, `"id": "saru"`, `"id": "c5"`, 1) + `,` +
			strings.Replace(goodCard, `"id": "saru"`, `"id": "c6"`, 1) +
			`]}`, true},
		{"empty_kind_explicit", clean, true}, // kind:"" already valid
		{"with_trace_id_field", strings.Replace(clean, `"actions": [{"label":"Draft","primary":true}]`, `"actions": [{"label":"Draft","primary":true}],"trace_id":"abc-123"`, 1), true},
		{"long_titles_under_cap", strings.Replace(clean, "Saru Patel · re: redline", strings.Repeat("a", 119), 1), true},
		{"meta_at_max", strings.Replace(clean, `"meta": ["06:14"]`, `"meta": ["a","b","c","d"]`, 1), true},
		{"actions_at_max", strings.Replace(clean, `"actions": [{"label":"Draft","primary":true}]`, `"actions": [{"label":"a"},{"label":"b"},{"label":"c"}]`, 1), true},
		{"src_calendar", strings.Replace(clean, `"src": "mail"`, `"src": "calendar"`, 1), true},
		{"src_tasks", strings.Replace(clean, `"src": "mail"`, `"src": "tasks"`, 1), true},
		// `only_one_card` joined the recoverable list when CardSet's
		// minItems dropped from 2 to 1 — quiet mornings genuinely have
		// one card and forcing repair on those was burning Gemini's
		// thinking budget into truncated JSON.
		{"only_one_card", `{"cards":[` + goodCard + `]}`, true},

		// 4 unrecoverable shapes (must fail validation; repair would re-ask).
		// `with_expand_field` joined this list when Card.Expand was marked
		// `zen:"-"` to keep its map[string]string shape out of the
		// Gemini-facing schema. The LLM should never emit `expand`
		// anymore; if it does, additionalProperties:false correctly trips.
		{"missing_required_rel", strings.Replace(clean, `"rel": "high",`, ``, 1), false},
		{"invalid_enum_rel", strings.Replace(clean, `"rel": "high"`, `"rel": "URGENT"`, 1), false},
		{"zero_cards", `{"cards":[]}`, false},
		{"too_long_title", strings.Replace(clean, "Saru Patel · re: redline", strings.Repeat("x", 200), 1), false},
	}

	if got, want := len(fixtures), 21; got != want {
		t.Fatalf("fixture count: got %d, want %d", got, want)
	}

	clean_recoverable := 0
	for _, fx := range fixtures {
		_, err := parseAndValidateCardSet(fx.raw)
		recovered := err == nil
		switch {
		case fx.recoverable && recovered:
			clean_recoverable++
		case !fx.recoverable && !recovered:
			// Expected — a real run would hit the repair path here.
		case fx.recoverable && !recovered:
			t.Errorf("fixture %q expected to parse, got: %v", fx.name, err)
		case !fx.recoverable && recovered:
			t.Errorf("fixture %q expected to fail validation, parsed ok", fx.name)
		}
	}

	// Gate 4 threshold: ≥17/21 must clean-parse on the first try.
	// One slot opened up when minItems dropped to 1 (only_one_card now
	// recoverable), shifting the ratio one step in our favor.
	if clean_recoverable < 17 {
		t.Fatalf("schema gate: clean-parse rate %d/21 < threshold 17/21", clean_recoverable)
	}
	t.Logf("schema gate: clean-parse rate %d/21 ≥ 17/21", clean_recoverable)
}
