package synth

import (
	"strings"
	"testing"
)

const allStatesVoice = `# Zeno voice — rules

(some preamble)

---

## Tension meter

Some prose about tension.

---

## State: morning_calm

The default register. Tension 25–45.

## State: pre_meeting

Pre-meeting register. Tension 70+.

## State: deep_work

Deep work register. Tension 15–25.

## State: end_of_day

End of day register. Tension 35–55.

## State: message_inject

Inject register. Tension 75–90.

## Cards bias: morning_calm

Mix work + personal.

## Cards bias: pre_meeting

Meeting-prep first.

## Cards bias: deep_work

Fewer cards, protected window.

## Cards bias: end_of_day

Retro framing.

## Cards bias: message_inject

Single card.

---

## Memory facts are scaffolding, not script

Trailing section after the state blocks.
`

func TestParseStateBlocks_AllPresent(t *testing.T) {
	stateVoice, stateBias, err := parseStateBlocks(allStatesVoice)
	if err != nil {
		t.Fatalf("parseStateBlocks error: %v", err)
	}
	if len(stateVoice) != 5 {
		t.Errorf("len(stateVoice)=%d want 5; keys=%v", len(stateVoice), keys(stateVoice))
	}
	if len(stateBias) != 5 {
		t.Errorf("len(stateBias)=%d want 5; keys=%v", len(stateBias), keys(stateBias))
	}
	for _, s := range []State{StateMorningCalm, StatePreMeeting, StateDeepWork, StateEndOfDay, StateMessageInject} {
		if v, ok := stateVoice[s]; !ok || strings.TrimSpace(v) == "" {
			t.Errorf("stateVoice[%s] missing/empty", s)
		}
		if v, ok := stateBias[s]; !ok || strings.TrimSpace(v) == "" {
			t.Errorf("stateBias[%s] missing/empty", s)
		}
	}
	// Trailing `## Memory facts...` heading must NOT have leaked into the
	// last state-block body.
	if strings.Contains(stateBias[StateMessageInject], "Memory facts") {
		t.Errorf("stateBias[message_inject] leaked trailing content: %q", stateBias[StateMessageInject])
	}
	// Body should be trimmed and not contain the heading itself.
	if strings.Contains(stateVoice[StateMorningCalm], "## State:") {
		t.Errorf("stateVoice body contains heading: %q", stateVoice[StateMorningCalm])
	}
}

func TestParseStateBlocks_MissingFallback(t *testing.T) {
	// Drop pre_meeting; morning_calm exists so the parser should fall back.
	v := strings.Replace(allStatesVoice, `## State: pre_meeting

Pre-meeting register. Tension 70+.

`, "", 1)
	stateVoice, stateBias, err := parseStateBlocks(v)
	if err != nil {
		t.Fatalf("parseStateBlocks error: %v", err)
	}
	got := stateVoice[StatePreMeeting]
	want := stateVoice[StateMorningCalm]
	if got != want {
		t.Errorf("missing pre_meeting voice did not fall back to morning_calm:\n got=%q\n want=%q", got, want)
	}
	// Bias for pre_meeting still present (we only dropped voice).
	if stateBias[StatePreMeeting] == "" {
		t.Errorf("stateBias[pre_meeting] empty after dropping only voice block")
	}
}

func TestParseStateBlocks_MissingMorningCalmHardError(t *testing.T) {
	v := strings.Replace(allStatesVoice, `## State: morning_calm

The default register. Tension 25–45.

`, "", 1)
	if _, _, err := parseStateBlocks(v); err == nil {
		t.Fatal("expected hard error when morning_calm voice block is missing; got nil")
	}
}

func TestParseStateBlocks_MalformedHeadingsIgnored(t *testing.T) {
	// `## State morning_calm` (no colon) and `## State: ` (no name) should
	// not match the regex. The other valid blocks should still parse.
	v := allStatesVoice + `

## State morning_calm

Should be ignored — missing colon.

## State:

Empty name — ignored.

## State: not_a_real_state

Unknown name — ignored.
`
	stateVoice, _, err := parseStateBlocks(v)
	if err != nil {
		t.Fatalf("parseStateBlocks error: %v", err)
	}
	for _, s := range []State{StateMorningCalm, StatePreMeeting, StateDeepWork, StateEndOfDay, StateMessageInject} {
		if _, ok := stateVoice[s]; !ok {
			t.Errorf("stateVoice[%s] missing after malformed-heading test", s)
		}
	}
	// We can't assert "not_a_real_state" is absent because it wasn't in the
	// known set anyway, but we can assert the count is still 5 (no extra).
	if len(stateVoice) != 5 {
		t.Errorf("len(stateVoice)=%d want 5 after malformed headings; keys=%v", len(stateVoice), keys(stateVoice))
	}
}

func TestParseStateBlocks_BodyIncludesH3Subsections(t *testing.T) {
	// A `### subsection` line inside a block body must NOT terminate the
	// body. Only `## ` (level-2 headings) or `---` separators terminate.
	v := `## State: morning_calm

Some prose.
### A subsection

More prose.

## Cards bias: morning_calm

Bias prose.
`
	stateVoice, _, err := parseStateBlocks(v)
	if err != nil {
		t.Fatalf("parseStateBlocks error: %v", err)
	}
	body := stateVoice[StateMorningCalm]
	if !strings.Contains(body, "### A subsection") {
		t.Errorf("h3 subsection lost from body: %q", body)
	}
	if !strings.Contains(body, "More prose.") {
		t.Errorf("body terminated early: %q", body)
	}
}

func TestParseStateBlocks_LoadPromptsRoundTrip(t *testing.T) {
	// Exercise the real embedded _voice.md through LoadPrompts. The repo's
	// canonical voice file must always parse cleanly with all five blocks.
	ps, err := LoadPrompts("")
	if err != nil {
		t.Fatalf("LoadPrompts error: %v", err)
	}
	if ps.StateVoice == nil || len(ps.StateVoice) != 5 {
		t.Errorf("PromptSet.StateVoice not fully populated: %v", keys(ps.StateVoice))
	}
	if ps.StateBias == nil || len(ps.StateBias) != 5 {
		t.Errorf("PromptSet.StateBias not fully populated: %v", keys(ps.StateBias))
	}
	for _, s := range []State{StateMorningCalm, StatePreMeeting, StateDeepWork, StateEndOfDay, StateMessageInject} {
		if v := ps.StateVoice[s]; strings.TrimSpace(v) == "" {
			t.Errorf("StateVoice[%s] empty in canonical _voice.md", s)
		}
		if v := ps.StateBias[s]; strings.TrimSpace(v) == "" {
			t.Errorf("StateBias[%s] empty in canonical _voice.md", s)
		}
	}
}

func keys(m map[State]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, string(k))
	}
	return out
}
