package synth

import "time"

// PreMeetingHoldDuration is how long a freshly-detected morning_calm is
// held back as pre_meeting after the prior briefing was pre_meeting. The
// hold prevents the cron firing 5 minutes after a meeting ends and
// flipping the register mid-conversation.
//
// Only pre_meeting → morning_calm is held. Other transitions flip
// immediately because the cost of a wrong-direction flip is small (e.g.
// deep_work → morning_calm just removes the protected-window framing,
// which is fine if the block actually broke).
const PreMeetingHoldDuration = 15 * time.Minute

// ApplyHysteresis returns the effective state given the previously
// persisted state for the same date and the freshly detected state.
//
// Currently:
//   - prev=pre_meeting, curr=morning_calm, age < PreMeetingHoldDuration →
//     hold pre_meeting.
//   - any other transition → curr (immediate).
//
// prev empty (no prior briefing on this date) is treated as "no
// hysteresis applies" — curr is returned verbatim. prevAt zero value
// likewise short-circuits the hold (treated as "infinitely old").
func ApplyHysteresis(prev, curr State, prevAt, now time.Time) State {
	if prev != StatePreMeeting || curr != StateMorningCalm {
		return curr
	}
	if prevAt.IsZero() {
		return curr
	}
	if now.Sub(prevAt) < PreMeetingHoldDuration {
		return StatePreMeeting
	}
	return curr
}
