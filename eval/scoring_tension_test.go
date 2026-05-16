package eval

import (
	"testing"

	"github.com/zenocy/zeno-v2/internal/synth"
)

func TestScoreTensionInBand_HappyPaths(t *testing.T) {
	cases := []struct {
		name    string
		state   synth.State
		tension int
		wantOK  bool
	}{
		// In-band cases
		{"morning_calm low edge", synth.StateMorningCalm, 25, true},
		{"morning_calm mid", synth.StateMorningCalm, 35, true},
		{"morning_calm high edge", synth.StateMorningCalm, 45, true},
		{"pre_meeting low edge", synth.StatePreMeeting, 70, true},
		{"pre_meeting high edge", synth.StatePreMeeting, 100, true},
		{"deep_work low edge", synth.StateDeepWork, 15, true},
		{"deep_work high edge", synth.StateDeepWork, 25, true},
		{"end_of_day low edge", synth.StateEndOfDay, 35, true},
		{"end_of_day high edge", synth.StateEndOfDay, 55, true},
		{"message_inject low edge", synth.StateMessageInject, 80, true},
		{"message_inject high edge", synth.StateMessageInject, 100, true},

		// Out-of-band cases
		{"morning_calm too low", synth.StateMorningCalm, 24, false},
		{"morning_calm too high", synth.StateMorningCalm, 46, false},
		{"pre_meeting too low", synth.StatePreMeeting, 69, false},
		{"deep_work too low", synth.StateDeepWork, 14, false},
		{"deep_work too high", synth.StateDeepWork, 26, false},
		{"end_of_day too low", synth.StateEndOfDay, 34, false},
		{"end_of_day too high", synth.StateEndOfDay, 56, false},
		{"message_inject too low", synth.StateMessageInject, 79, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ScoreTensionInBand(synth.Briefing{Tension: tc.tension}, tc.state)
			if got.OK != tc.wantOK {
				t.Errorf("ScoreTensionInBand(tension=%d, %s) OK=%v want %v", tc.tension, tc.state, got.OK, tc.wantOK)
			}
			if !got.Gated {
				t.Errorf("ScoreTensionInBand(%s) Gated=false; expected true for known state", tc.state)
			}
		})
	}
}

func TestScoreTensionInBand_UnknownStateUngated(t *testing.T) {
	// Unknown / empty state → no band applied. OK=true so pre-V2.3 corpora
	// don't regress; Gated=false so the report can distinguish "not gated"
	// from "passed."
	got := ScoreTensionInBand(synth.Briefing{Tension: 200}, synth.State(""))
	if !got.OK {
		t.Errorf("unknown state should pass (OK=true), got %+v", got)
	}
	if got.Gated {
		t.Errorf("unknown state should NOT be gated, got %+v", got)
	}
}

// Pathological tension values (negative, > 100) should still gate cleanly
// for known states — the rubric is "in-band or not"; out-of-domain just
// fails the band check rather than panicking.
func TestScoreTensionInBand_PathologicalValues(t *testing.T) {
	cases := []struct {
		name    string
		state   synth.State
		tension int
	}{
		{"zero on morning_calm", synth.StateMorningCalm, 0},
		{"negative on deep_work", synth.StateDeepWork, -50},
		{"over-100 on pre_meeting", synth.StatePreMeeting, 250},
		{"zero on pre_meeting", synth.StatePreMeeting, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ScoreTensionInBand(synth.Briefing{Tension: tc.tension}, tc.state)
			if got.OK {
				t.Errorf("pathological tension %d on %s must fail the band, got OK=true", tc.tension, tc.state)
			}
			if !got.Gated {
				t.Errorf("known state must remain gated even on pathological values, got Gated=false")
			}
			if got.Tension != tc.tension {
				t.Errorf("Tension field must echo the input even when out of range, got %d", got.Tension)
			}
		})
	}
}

func TestFixture_MustCardsForState(t *testing.T) {
	flat := []MustCard{{Name: "flat A", Sources: CardSources{"mail"}}}
	preMeetingExtra := []MustCard{{Name: "attendee context", Sources: CardSources{"calendar"}}}
	deepWorkExtra := []MustCard{{Name: "window framing", Sources: CardSources{"tasks"}}}
	f := Fixture{
		MustCards:           flat,
		MustCardsPreMeeting: preMeetingExtra,
		MustCardsDeepWork:   deepWorkExtra,
	}

	// morning_calm — no per-state list configured → only flat list.
	got := f.MustCardsForState(synth.StateMorningCalm)
	if len(got) != 1 || got[0].Name != "flat A" {
		t.Errorf("morning_calm should yield just the flat list, got %+v", got)
	}

	// pre_meeting — flat + per-state with suffix.
	got = f.MustCardsForState(synth.StatePreMeeting)
	if len(got) != 2 {
		t.Fatalf("pre_meeting len=%d want 2; got=%+v", len(got), got)
	}
	if got[0].Name != "flat A" {
		t.Errorf("flat must come first, got %q", got[0].Name)
	}
	if got[1].Name != "attendee context [pre_meeting]" {
		t.Errorf("per-state Name should be tagged with [pre_meeting], got %q", got[1].Name)
	}
	// Original slice is not mutated.
	if preMeetingExtra[0].Name != "attendee context" {
		t.Errorf("source slice was mutated; expected unchanged, got %q", preMeetingExtra[0].Name)
	}

	// deep_work — flat + deep_work suffix.
	got = f.MustCardsForState(synth.StateDeepWork)
	if len(got) != 2 || got[1].Name != "window framing [deep_work]" {
		t.Errorf("deep_work tagging incorrect: %+v", got)
	}

	// State with no per-state list (end_of_day) — only flat.
	got = f.MustCardsForState(synth.StateEndOfDay)
	if len(got) != 1 || got[0].Name != "flat A" {
		t.Errorf("end_of_day should be flat-only, got %+v", got)
	}
}
