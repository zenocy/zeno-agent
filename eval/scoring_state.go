package eval

import (
	"github.com/zenocy/zeno-v2/internal/synth"
)

// StateMatch is the rubric dimension that compares the briefing's
// persisted State against the fixture's declared expected_state. V2.3.0
// Phase 1 wires the real comparison; the Stub field stays on the struct
// for back-compat with P0-era serialized scoreboards (always false from
// P1 onward).
type StateMatch struct {
	Expected synth.State `json:"expected"`
	Actual   synth.State `json:"actual"`
	OK       bool        `json:"ok"`
	// Stub was true while the detector was not yet wired (Phase 0). Phase
	// 1 always sets it to false. Kept for back-compat with previously
	// serialized scoreboards.
	Stub bool `json:"stub,omitempty"`
}

// ScoreStateMatch returns the state-match rubric result for one briefing.
// actualState is the briefing's persisted State column (empty for
// pre-V2.3 rows); expectedState is the fixture's expected_state (empty
// treated as morning_calm). OK is true when actual matches expected
// after both have been normalized to morning_calm on empty/invalid.
func ScoreStateMatch(actualState, expectedState string) StateMatch {
	if expectedState == "" {
		expectedState = string(synth.StateMorningCalm)
	}
	actual := synth.State(actualState)
	if !actual.IsValid() {
		actual = synth.StateMorningCalm
	}
	expected := synth.State(expectedState)
	if !expected.IsValid() {
		expected = synth.StateMorningCalm
	}
	return StateMatch{
		Expected: expected,
		Actual:   actual,
		OK:       expected == actual,
	}
}
