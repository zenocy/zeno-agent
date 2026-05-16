package synth

import "testing"

func TestState_IsValid(t *testing.T) {
	tests := []struct {
		name  string
		state State
		want  bool
	}{
		{"morning_calm", StateMorningCalm, true},
		{"pre_meeting", StatePreMeeting, true},
		{"deep_work", StateDeepWork, true},
		{"message_inject", StateMessageInject, true},
		{"end_of_day", StateEndOfDay, true},
		{"empty", State(""), false},
		{"unknown", State("weekend"), false},
		{"case_mismatch", State("Morning_Calm"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.state.IsValid(); got != tc.want {
				t.Fatalf("State(%q).IsValid() = %v, want %v", tc.state, got, tc.want)
			}
		})
	}
}

func TestState_String(t *testing.T) {
	if got := StatePreMeeting.String(); got != "pre_meeting" {
		t.Fatalf("StatePreMeeting.String() = %q, want pre_meeting", got)
	}
}
