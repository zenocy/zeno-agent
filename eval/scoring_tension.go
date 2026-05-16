package eval

import (
	"github.com/zenocy/zeno-v2/internal/synth"
)

// TensionBand is the inclusive [low, high] band the briefing's tension is
// expected to land in for a given State. Bands are derived from the voice
// canon's tension scale (`prompts/_voice.md`'s "Tension meter" + per-state
// blocks) and may be tuned in V2.3.x once we have telemetry on the model's
// actual distribution per state.
type TensionBand [2]int

// tensionBands maps each V2.3 register to its tension band. Unknown / empty
// states aren't gated — the rubric returns OK=true with InBand=true so
// pre-V2.3 fixtures don't regress.
var tensionBands = map[synth.State]TensionBand{
	synth.StateMorningCalm:   {25, 45},
	synth.StatePreMeeting:    {70, 100},
	synth.StateDeepWork:      {15, 25},
	synth.StateMessageInject: {80, 100},
	synth.StateEndOfDay:      {35, 55},
}

// TensionInBand records whether the briefing's tension landed in the band
// expected for the actual detected state. Persisted on Scoreboard so the
// HTML report can show "tension=42 in [25,45] ✓".
type TensionInBand struct {
	State   synth.State `json:"state"`
	Tension int         `json:"tension"`
	Low     int         `json:"low"`
	High    int         `json:"high"`
	OK      bool        `json:"ok"`
	// Gated reports whether a band was applied. False when the state is
	// unknown/empty (no band configured) — distinguishes "passed because the
	// rubric didn't apply" from "passed because tension matched."
	Gated bool `json:"gated"`
}

// ScoreTensionInBand checks whether the briefing's tension lies in the band
// configured for the actual detected state. Unknown / empty state returns
// Gated=false, OK=true (no regression for pre-V2.3 corpora).
func ScoreTensionInBand(b synth.Briefing, state synth.State) TensionInBand {
	band, ok := tensionBands[state]
	if !ok {
		return TensionInBand{State: state, Tension: b.Tension, OK: true, Gated: false}
	}
	return TensionInBand{
		State:   state,
		Tension: b.Tension,
		Low:     band[0],
		High:    band[1],
		Gated:   true,
		OK:      b.Tension >= band[0] && b.Tension <= band[1],
	}
}
