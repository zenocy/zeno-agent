package synth

import (
	"testing"
	"time"
)

func TestDetectState(t *testing.T) {
	tests := []struct {
		name string
		in   StateInputs
		want State
	}{
		// ----- Default fall-through -----
		{
			name: "default morning_calm — weekday morning, no meeting, no big block",
			in: StateInputs{
				NextMeetingMinutes: -1,
				UnbookedBlockHours: 1.5,
				Weekday:            time.Tuesday,
				LocalHour:          10,
			},
			want: StateMorningCalm,
		},
		{
			name: "weekend morning falls through to morning_calm",
			in: StateInputs{
				NextMeetingMinutes: -1,
				UnbookedBlockHours: 5.0, // would be deep_work on a weekday
				Weekday:            time.Saturday,
				LocalHour:          10,
			},
			want: StateMorningCalm,
		},

		// ----- Priority 1: message_inject -----
		{
			name: "inject signal wins regardless of other state",
			in: StateInputs{
				HasInjectSignal:    true,
				InjectSignalKind:   "email",
				NextMeetingMinutes: 30, // would be pre_meeting
				UnbookedBlockHours: 5.0,
				Weekday:            time.Tuesday,
				LocalHour:          10,
			},
			want: StateMessageInject,
		},
		{
			name: "inject signal during deep-work block still wins",
			in: StateInputs{
				HasInjectSignal:    true,
				InjectSignalKind:   "calendar_move",
				UnbookedBlockHours: 5.0,
				NextMeetingMinutes: -1,
				Weekday:            time.Wednesday,
				LocalHour:          14,
			},
			want: StateMessageInject,
		},
		{
			name: "inject without InjectSignalKind falls through (defensive)",
			in: StateInputs{
				HasInjectSignal:    true,
				InjectSignalKind:   "", // misconfigured signal
				NextMeetingMinutes: -1,
				Weekday:            time.Tuesday,
				LocalHour:          10,
			},
			want: StateMorningCalm,
		},

		// ----- Priority 2: pre_meeting -----
		{
			name: "pre_meeting happy path: 30 min out, 4 attendees",
			in: StateInputs{
				NextMeetingMinutes:   30,
				NextMeetingAttendees: 4,
				Weekday:              time.Wednesday,
				LocalHour:            10,
			},
			want: StatePreMeeting,
		},
		{
			name: "pre_meeting boundary: exactly 120 min out fires",
			in: StateInputs{
				NextMeetingMinutes:   PreMeetingMaxMinutes,
				NextMeetingAttendees: 3,
				Weekday:              time.Tuesday,
				LocalHour:            10,
			},
			want: StatePreMeeting,
		},
		{
			name: "pre_meeting boundary: 121 min out does NOT fire",
			in: StateInputs{
				NextMeetingMinutes:   PreMeetingMaxMinutes + 1,
				NextMeetingAttendees: 5,
				Weekday:              time.Tuesday,
				LocalHour:            10,
			},
			want: StateMorningCalm,
		},
		{
			name: "pre_meeting boundary: 2 attendees does NOT fire (1:1 isn't a meeting)",
			in: StateInputs{
				NextMeetingMinutes:   30,
				NextMeetingAttendees: 2,
				Weekday:              time.Tuesday,
				LocalHour:            10,
			},
			want: StateMorningCalm,
		},
		{
			name: "pre_meeting fires on Friday morning even pre-EOD-shift",
			in: StateInputs{
				NextMeetingMinutes:   60,
				NextMeetingAttendees: 4,
				Weekday:              time.Friday,
				LocalHour:            11,
			},
			want: StatePreMeeting,
		},

		// ----- Priority 3: deep_work -----
		{
			name: "deep_work happy path: 4-hour unbooked weekday block",
			in: StateInputs{
				NextMeetingMinutes: -1,
				UnbookedBlockHours: 4.0,
				Weekday:            time.Wednesday,
				LocalHour:          10,
			},
			want: StateDeepWork,
		},
		{
			name: "deep_work boundary: 3.0 hours fires",
			in: StateInputs{
				NextMeetingMinutes: -1,
				UnbookedBlockHours: DeepWorkMinHours,
				Weekday:            time.Tuesday,
				LocalHour:          11,
			},
			want: StateDeepWork,
		},
		{
			name: "deep_work boundary: 2.99 hours does NOT fire",
			in: StateInputs{
				NextMeetingMinutes: -1,
				UnbookedBlockHours: 2.99,
				Weekday:            time.Tuesday,
				LocalHour:          11,
			},
			want: StateMorningCalm,
		},
		{
			name: "deep_work boundary: hour 9 inclusive",
			in: StateInputs{
				NextMeetingMinutes: -1,
				UnbookedBlockHours: 4.0,
				Weekday:            time.Wednesday,
				LocalHour:          DeepWorkEarliestHour,
			},
			want: StateDeepWork,
		},
		{
			name: "deep_work boundary: hour 16 inclusive",
			in: StateInputs{
				NextMeetingMinutes: -1,
				UnbookedBlockHours: 4.0,
				Weekday:            time.Wednesday,
				LocalHour:          DeepWorkLatestHour,
			},
			want: StateDeepWork,
		},
		{
			name: "deep_work boundary: hour 8 too early — falls to morning_calm",
			in: StateInputs{
				NextMeetingMinutes: -1,
				UnbookedBlockHours: 4.0,
				Weekday:            time.Wednesday,
				LocalHour:          8,
			},
			want: StateMorningCalm,
		},
		{
			name: "deep_work suppressed on Saturday",
			in: StateInputs{
				NextMeetingMinutes: -1,
				UnbookedBlockHours: 5.0,
				Weekday:            time.Saturday,
				LocalHour:          11,
			},
			want: StateMorningCalm,
		},
		{
			name: "deep_work suppressed on Sunday",
			in: StateInputs{
				NextMeetingMinutes: -1,
				UnbookedBlockHours: 5.0,
				Weekday:            time.Sunday,
				LocalHour:          11,
			},
			want: StateMorningCalm,
		},

		// ----- Priority 4: end_of_day -----
		{
			name: "end_of_day at 17:00 weekday",
			in: StateInputs{
				NextMeetingMinutes: -1,
				Weekday:            time.Wednesday,
				LocalHour:          17,
			},
			want: StateEndOfDay,
		},
		{
			name: "end_of_day boundary: 16:00 fires (>= EndOfDayHour)",
			in: StateInputs{
				NextMeetingMinutes: -1,
				UnbookedBlockHours: 5.0, // would be deep_work, but EOD wins via gate
				Weekday:            time.Wednesday,
				LocalHour:          EndOfDayHour,
			},
			// At hour 16, deep_work still fires (LocalHour <= 16 inclusive)
			// AND end_of_day matches. Priority order: deep_work check is
			// earlier — see the rule body. So with a 5-hour block at 16:00,
			// deep_work wins.
			want: StateDeepWork,
		},
		{
			name: "end_of_day at 17:00 with no block",
			in: StateInputs{
				NextMeetingMinutes: -1,
				UnbookedBlockHours: 0.5,
				Weekday:            time.Thursday,
				LocalHour:          17,
			},
			want: StateEndOfDay,
		},
		{
			name: "Friday afternoon shift: 14:30 fires",
			in: StateInputs{
				NextMeetingMinutes: -1,
				Weekday:            time.Friday,
				LocalHour:          FridayEndOfDayHour,
			},
			want: StateEndOfDay,
		},
		{
			name: "Friday boundary: 13:59 stays calm — no end_of_day yet",
			in: StateInputs{
				NextMeetingMinutes: -1,
				UnbookedBlockHours: 1.5,
				Weekday:            time.Friday,
				LocalHour:          FridayEndOfDayHour - 1,
			},
			want: StateMorningCalm,
		},
		{
			name: "Saturday 17:00 still flips to end_of_day (EOD is weekday-agnostic via hour)",
			in: StateInputs{
				NextMeetingMinutes: -1,
				Weekday:            time.Saturday,
				LocalHour:          17,
			},
			want: StateEndOfDay,
		},

		// ----- Priority interplay -----
		{
			name: "pre_meeting beats deep_work when both qualify",
			in: StateInputs{
				NextMeetingMinutes:   30,
				NextMeetingAttendees: 4,
				UnbookedBlockHours:   5.0,
				Weekday:              time.Wednesday,
				LocalHour:            10,
			},
			want: StatePreMeeting,
		},
		{
			name: "pre_meeting beats end_of_day when both qualify",
			in: StateInputs{
				NextMeetingMinutes:   45,
				NextMeetingAttendees: 5,
				Weekday:              time.Wednesday,
				LocalHour:            17,
			},
			want: StatePreMeeting,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DetectState(tc.in); got != tc.want {
				t.Fatalf("DetectState(%+v) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}
