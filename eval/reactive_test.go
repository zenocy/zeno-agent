package eval

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/synth"
)

func TestCheckReactiveExpect_NilExpect(t *testing.T) {
	// No expectations declared — every criterion trivially passes.
	got := CheckReactiveExpect(synth.Card{Title: "anything", Sub: "anything"}, nil, false)
	require.True(t, got.AllPass(), "nil expect should pass everything")
}

func TestCheckReactiveExpect_AllHits(t *testing.T) {
	card := synth.Card{
		Source: "ask",
		Rel:    "high",
		Title:  "Acuity Capital — Series B narrative",
		Sub:    "Saru, Lin, and Park at 11:00. The redline is the open question.",
	}
	expect := &ReactiveExpect{
		TitleContains:     []string{"acuity"},
		SubContains:       []string{"11:00"},
		SrcIn:             []string{"ask", "calendar"},
		RelIn:             []string{"high", "med"},
		MustNotBeDegraded: true,
	}
	got := CheckReactiveExpect(card, expect, false)
	require.True(t, got.AllPass(), "every criterion should pass; got %+v", got)
}

func TestCheckReactiveExpect_SrcMismatchFails(t *testing.T) {
	card := synth.Card{Source: "mail", Rel: "med", Title: "x", Sub: "y"}
	expect := &ReactiveExpect{SrcIn: []string{"ask", "calendar"}}
	got := CheckReactiveExpect(card, expect, false)
	require.False(t, got.Src, "src=mail not in [ask,calendar] should fail Src")
	require.True(t, got.Title, "empty TitleContains should pass Title")
	require.True(t, got.Rel, "empty RelIn should pass Rel")
	require.False(t, got.AllPass(), "AllPass should be false when Src fails")
}

func TestCheckReactiveExpect_DegradedFlagsNotDeg(t *testing.T) {
	card := synth.Card{Source: "ask", Rel: "low"}
	expect := &ReactiveExpect{MustNotBeDegraded: true}
	got := CheckReactiveExpect(card, expect, true) // degraded=true
	require.False(t, got.NotDeg, "MustNotBeDegraded=true + degraded=true → NotDeg fails")
	require.False(t, got.AllPass(), "AllPass should be false when NotDeg fails")
}

func TestCheckReactiveExpect_DegradedAllowedWhenNotRequired(t *testing.T) {
	card := synth.Card{Source: "ask", Rel: "low"}
	expect := &ReactiveExpect{} // MustNotBeDegraded false
	got := CheckReactiveExpect(card, expect, true)
	require.True(t, got.NotDeg, "MustNotBeDegraded=false → NotDeg always true regardless of degraded state")
	require.True(t, got.AllPass(), "all criteria empty → AllPass should hold")
}

func TestReactiveQueryTime_DefaultsTo10AM(t *testing.T) {
	f := &Fixture{Today: "2026-04-30", User: FixtureUser{TZ: "America/Los_Angeles"}}
	got, err := reactiveQueryTime(f)
	require.NoError(t, err)
	require.Equal(t, 10, got.Hour())
	require.Equal(t, 0, got.Minute())
}

func TestReactiveQueryTime_RespectsExplicit(t *testing.T) {
	f := &Fixture{Today: "2026-04-30", User: FixtureUser{TZ: "UTC"}, QueryTime: "14:30"}
	got, err := reactiveQueryTime(f)
	require.NoError(t, err)
	require.Equal(t, 14, got.Hour())
	require.Equal(t, 30, got.Minute())
}

// V2.7: speech_contains / speech_must_not_contain criteria for the
// WhatsApp register. Voice-register regression is the most likely
// failure mode (the model leaking Card-shaped headers into the chat
// reply); these tests guard against it.
// TestLoadFixture_WhatsAppCorpusFixturesParse exercises the JSON shape
// of the V2.7 corpus files. A typo in either the WhatsApp block or the
// new speech_* criteria would fail here long before someone runs the
// full LLM-driven eval.
func TestLoadFixture_WhatsAppCorpusFixturesParse(t *testing.T) {
	cases := []string{
		"corpus/whatsapp_dm_owner.json",
		"corpus/whatsapp_group_mention.json",
		"corpus/whatsapp_voice_register.json",
	}
	for _, path := range cases {
		t.Run(path, func(t *testing.T) {
			f, err := LoadFixture(path)
			require.NoError(t, err)
			require.Equal(t, "reactive", f.Kind)
			require.NotNil(t, f.WhatsApp, "WhatsApp block must be set in corpus fixture")
			require.NotNil(t, f.ReactiveExpect)
			require.NotEmpty(t, f.ReactiveExpect.SpeechMustNotContain,
				"every WhatsApp fixture must veto markdown header leakage")
		})
	}
}

// TestLoadFixture_WhatsAppSendFixtureParses smoke-tests the V2.12
// reactive_whatsapp_send fixture so a typo in the file would surface
// without running the full LLM pipeline.
func TestLoadFixture_WhatsAppSendFixtureParses(t *testing.T) {
	f, err := LoadFixture("corpus/reactive_whatsapp_send.json")
	require.NoError(t, err)
	require.Equal(t, "reactive", f.Kind)
	require.Contains(t, f.Query, "wife")
	require.NotEmpty(t, f.Memory, "fixture must seed at least one memory fact for the resolver")
}

func TestCheckReactiveExpect_SpeechContains(t *testing.T) {
	card := synth.Card{
		Source: "ask", Rel: "med", Title: "x", Sub: "y",
		Speech: "Standup at 10, design review at 3.",
	}
	expect := &ReactiveExpect{
		SpeechContains: []string{"standup", "design"},
	}
	got := CheckReactiveExpect(card, expect, false)
	require.True(t, got.Speech, "speech_contains should hit on 'standup'")
}

func TestCheckReactiveExpect_SpeechMustNotContainVeto(t *testing.T) {
	card := synth.Card{
		Source: "ask", Rel: "med", Title: "x", Sub: "y",
		Speech: "## Synthesis\n\nHere are the meetings",
	}
	expect := &ReactiveExpect{
		SpeechMustNotContain: []string{"## ", "Synthesis"},
	}
	got := CheckReactiveExpect(card, expect, false)
	require.False(t, got.SpeechClean, "veto on '## ' should fail SpeechClean")
	require.False(t, got.AllPass(), "AllPass must be false when SpeechClean fails")
}

func TestCheckReactiveExpect_AddressLeakInSpeech(t *testing.T) {
	cases := []struct {
		name   string
		speech string
	}{
		{"DM JID", "Sent it to 447700900111@s.whatsapp.net just now."},
		{"group JID", "The group chat is 120001@g.us."},
		{"plus phone", "Call her on +447700900111."},
		{"bare phone", "447700900111 will work."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			card := synth.Card{
				Source: "ask", Rel: "med", Title: "x", Sub: "y",
				Speech: tc.speech,
			}
			got := CheckReactiveExpect(card, &ReactiveExpect{}, false)
			require.False(t, got.SpeechClean,
				"address-shape pattern in Speech must fail SpeechClean even with empty SpeechMustNotContain")
		})
	}
}

func TestCheckReactiveExpect_AddressLeakInSub(t *testing.T) {
	card := synth.Card{
		Source: "ask", Rel: "med",
		Title: "x",
		Sub:   "Sam's WhatsApp is 447700900111@s.whatsapp.net.",
	}
	got := CheckReactiveExpect(card, &ReactiveExpect{}, false)
	require.False(t, got.Sub, "address-shape pattern in Sub must fail")
}

func TestCheckReactiveExpect_NoFalsePositiveOnDates(t *testing.T) {
	// Times like "20:30" and dates like "2026-05-10" must not trip the
	// regex — they have separators and are well under 10 contiguous digits.
	card := synth.Card{
		Source: "ask", Rel: "med",
		Title:  "Crew dinner — Petralona",
		Sub:    "20:30 at Petralona; the host's WhatsApp link is in your contacts.",
		Speech: "Tonight at 20:30 at Petralona.",
	}
	got := CheckReactiveExpect(card, &ReactiveExpect{}, false)
	require.True(t, got.SpeechClean, "20:30 must not be flagged as an address leak")
	require.True(t, got.Sub, "dates and times must not trip the leak detector")
}

func TestCheckReactiveExpect_SpeechMustNotContain_CaseInsensitive(t *testing.T) {
	card := synth.Card{Speech: "SYNTHESIS: today's signals", Source: "ask", Rel: "med"}
	expect := &ReactiveExpect{
		SpeechMustNotContain: []string{"synthesis"},
	}
	got := CheckReactiveExpect(card, expect, false)
	require.False(t, got.SpeechClean, "case-insensitive veto must catch SYNTHESIS")
}

func TestCheckReactiveExpect_NilExpectFillsSpeechCriteria(t *testing.T) {
	got := CheckReactiveExpect(synth.Card{}, nil, false)
	require.True(t, got.Speech, "nil expect must trivially pass Speech")
	require.True(t, got.SpeechClean, "nil expect must trivially pass SpeechClean")
	require.True(t, got.AllPass())
}

// Regression: a reactive single-card response must validate cleanly
// under both schemas — directly via CardSchema, and after wrapping in
// a CardSet (since CardSet's minItems dropped from 2 to 1). The
// reactive scoring path still overrides with CardSchema for semantic
// accuracy (a reactive response IS a single card, not a degenerate
// CardSet), but the override is no longer load-bearing for the
// single-card case.
func TestReactiveScoring_SingleCardValidatesUnderBothSchemas(t *testing.T) {
	card := synth.Card{
		ID:       "test-abcd",
		Date:     "2026-04-28",
		Source:   "ask",
		SrcLabel: "Generated",
		Rel:      "med",
		Title:    "Acuity at 11:00 with Saru, Lin, Park",
		Sub:      "Forty-five minutes on the Series B redline. Park is on cohort table.",
		Meta:     []string{},
		Actions:  []synth.Action{{Label: "Dismiss"}},
	}
	cardJSON, err := json.Marshal(card)
	require.NoError(t, err)
	require.NoError(t, synth.ValidateJSON(synth.CardSchema(), cardJSON),
		"valid Card must pass CardSchema validation")

	cs := synth.CardSet{Cards: []synth.Card{card}}
	csJSON, err := json.Marshal(cs)
	require.NoError(t, err)
	require.NoError(t, synth.ValidateJSON(synth.CardSetSchema(), csJSON),
		"single-card CardSet must pass CardSetSchema (minItems=1)")
}
