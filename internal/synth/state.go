package synth

// State is the day-shape register Zeno speaks in. V2.3 Phase 0 introduces the
// type and the closed set of values; Phase 1 adds the detector that maps
// today's projections onto a State; Phase 2 makes the briefing voice and the
// cards-prompt bias actually fork on it.
//
// Unknown / empty State is treated as StateMorningCalm by callers — keeps
// pre-V2.3 briefing rows readable after the migration.
type State string

const (
	StateMorningCalm   State = "morning_calm"
	StatePreMeeting    State = "pre_meeting"
	StateDeepWork      State = "deep_work"
	StateMessageInject State = "message_inject"
	StateEndOfDay      State = "end_of_day"
)

// IsValid reports whether s is one of the five known states. Empty is not
// valid (callers default to StateMorningCalm explicitly so the choice is
// visible at the call site).
func (s State) IsValid() bool {
	switch s {
	case StateMorningCalm, StatePreMeeting, StateDeepWork, StateMessageInject, StateEndOfDay:
		return true
	}
	return false
}

// String implements fmt.Stringer.
func (s State) String() string { return string(s) }

// TensionBands carries the inclusive [low, high] tension range each state
// must produce. The eval harness uses the same bands in
// eval/scoring_tension.go to score the rubric; the briefing-synth repair
// path reads it to refuse out-of-band values from the model.
var TensionBands = map[State][2]int{
	StateMorningCalm:   {25, 45},
	StatePreMeeting:    {70, 100},
	StateDeepWork:      {15, 25},
	StateMessageInject: {80, 100},
	StateEndOfDay:      {35, 55},
}

// TensionInBand reports whether v falls within the band for state. Unknown
// states (including empty) are treated as morning_calm. Returns the band
// in use as a debug aid for callers building error messages.
func TensionInBand(state State, v int) (ok bool, band [2]int) {
	if !state.IsValid() {
		state = StateMorningCalm
	}
	band = TensionBands[state]
	return v >= band[0] && v <= band[1], band
}
