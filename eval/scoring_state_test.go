package eval

import (
	"testing"

	"github.com/zenocy/zeno-v2/internal/synth"
)

// TestScoreStateMatch covers the real Phase 1 comparison. expected /
// actual normalize to morning_calm on empty/invalid; OK is true on
// match.
func TestScoreStateMatch(t *testing.T) {
	cases := []struct {
		name         string
		actualState  string
		expected     string
		wantExpected synth.State
		wantActual   synth.State
		wantOK       bool
	}{
		{
			name:         "expected empty defaults to morning_calm; actual matches",
			actualState:  "morning_calm",
			expected:     "",
			wantExpected: synth.StateMorningCalm,
			wantActual:   synth.StateMorningCalm,
			wantOK:       true,
		},
		{
			name:         "match on pre_meeting",
			actualState:  "pre_meeting",
			expected:     "pre_meeting",
			wantExpected: synth.StatePreMeeting,
			wantActual:   synth.StatePreMeeting,
			wantOK:       true,
		},
		{
			name:         "match on deep_work",
			actualState:  "deep_work",
			expected:     "deep_work",
			wantExpected: synth.StateDeepWork,
			wantActual:   synth.StateDeepWork,
			wantOK:       true,
		},
		{
			name:         "match on message_inject",
			actualState:  "message_inject",
			expected:     "message_inject",
			wantExpected: synth.StateMessageInject,
			wantActual:   synth.StateMessageInject,
			wantOK:       true,
		},
		{
			name:         "match on end_of_day",
			actualState:  "end_of_day",
			expected:     "end_of_day",
			wantExpected: synth.StateEndOfDay,
			wantActual:   synth.StateEndOfDay,
			wantOK:       true,
		},
		{
			name:         "actual empty defaults to morning_calm; mismatch with deep_work",
			actualState:  "",
			expected:     "deep_work",
			wantExpected: synth.StateDeepWork,
			wantActual:   synth.StateMorningCalm,
			wantOK:       false,
		},
		{
			name:         "actual invalid defaults to morning_calm",
			actualState:  "weekend",
			expected:     "morning_calm",
			wantExpected: synth.StateMorningCalm,
			wantActual:   synth.StateMorningCalm,
			wantOK:       true,
		},
		{
			name:         "mismatch — morning_calm but expected pre_meeting",
			actualState:  "morning_calm",
			expected:     "pre_meeting",
			wantExpected: synth.StatePreMeeting,
			wantActual:   synth.StateMorningCalm,
			wantOK:       false,
		},
		{
			name:         "expected invalid normalizes to morning_calm",
			actualState:  "morning_calm",
			expected:     "weekend",
			wantExpected: synth.StateMorningCalm,
			wantActual:   synth.StateMorningCalm,
			wantOK:       true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ScoreStateMatch(tc.actualState, tc.expected)
			if got.Expected != tc.wantExpected {
				t.Errorf("Expected = %q, want %q", got.Expected, tc.wantExpected)
			}
			if got.Actual != tc.wantActual {
				t.Errorf("Actual = %q, want %q", got.Actual, tc.wantActual)
			}
			if got.OK != tc.wantOK {
				t.Errorf("OK = %v, want %v", got.OK, tc.wantOK)
			}
			if got.Stub {
				t.Errorf("Stub = true, want false (P1 always non-stub)")
			}
		})
	}
}
