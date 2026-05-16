package synth

import (
	"testing"
	"time"
)

func TestApplyHysteresis(t *testing.T) {
	now := time.Date(2026, 4, 28, 11, 0, 0, 0, time.UTC)

	tests := []struct {
		name   string
		prev   State
		curr   State
		prevAt time.Time
		want   State
	}{
		{
			name:   "hold pre_meeting when meeting just ended (10 min ago)",
			prev:   StatePreMeeting,
			curr:   StateMorningCalm,
			prevAt: now.Add(-10 * time.Minute),
			want:   StatePreMeeting,
		},
		{
			name:   "release pre_meeting after hold expires (20 min ago)",
			prev:   StatePreMeeting,
			curr:   StateMorningCalm,
			prevAt: now.Add(-20 * time.Minute),
			want:   StateMorningCalm,
		},
		{
			name:   "boundary: exactly PreMeetingHoldDuration ago — release",
			prev:   StatePreMeeting,
			curr:   StateMorningCalm,
			prevAt: now.Add(-PreMeetingHoldDuration),
			want:   StateMorningCalm,
		},
		{
			name:   "boundary: 1 second under hold — still held",
			prev:   StatePreMeeting,
			curr:   StateMorningCalm,
			prevAt: now.Add(-PreMeetingHoldDuration + time.Second),
			want:   StatePreMeeting,
		},
		{
			name:   "pre_meeting → deep_work: no hysteresis (only ->morning_calm is held)",
			prev:   StatePreMeeting,
			curr:   StateDeepWork,
			prevAt: now.Add(-2 * time.Minute),
			want:   StateDeepWork,
		},
		{
			name:   "morning_calm → pre_meeting: no inverse hysteresis, flip immediately",
			prev:   StateMorningCalm,
			curr:   StatePreMeeting,
			prevAt: now.Add(-2 * time.Minute),
			want:   StatePreMeeting,
		},
		{
			name:   "deep_work → morning_calm: no hysteresis (only pre_meeting holds)",
			prev:   StateDeepWork,
			curr:   StateMorningCalm,
			prevAt: now.Add(-1 * time.Minute),
			want:   StateMorningCalm,
		},
		{
			name:   "empty prev (first run today) — return curr",
			prev:   State(""),
			curr:   StateMorningCalm,
			prevAt: now.Add(-1 * time.Minute),
			want:   StateMorningCalm,
		},
		{
			name:   "zero prevAt — short-circuit, return curr",
			prev:   StatePreMeeting,
			curr:   StateMorningCalm,
			prevAt: time.Time{},
			want:   StateMorningCalm,
		},
		{
			name:   "prev == curr: returns curr (no-op)",
			prev:   StatePreMeeting,
			curr:   StatePreMeeting,
			prevAt: now.Add(-1 * time.Minute),
			want:   StatePreMeeting,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ApplyHysteresis(tc.prev, tc.curr, tc.prevAt, now); got != tc.want {
				t.Fatalf("ApplyHysteresis(prev=%q, curr=%q, age=%v) = %q, want %q",
					tc.prev, tc.curr, now.Sub(tc.prevAt), got, tc.want)
			}
		})
	}
}
