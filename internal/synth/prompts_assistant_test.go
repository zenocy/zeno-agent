package synth

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/projection"
)

// TestLoadPrompts_AssistantRegisterParsed asserts the V2.13.0
// `## Register: assistant` block is loaded into PromptSet.AssistantRegister.
func TestLoadPrompts_AssistantRegisterParsed(t *testing.T) {
	prompts, err := LoadPrompts("")
	require.NoError(t, err)
	require.NotEmpty(t, prompts.AssistantRegister, "register block must be parsed")
	assert.Contains(t, prompts.AssistantRegister, "third person")
	assert.Contains(t, prompts.AssistantRegister, "assistant name")
	// The block must NOT include the `## Register:` heading itself —
	// only its body. (The trim happens in parseAssistantRegister.)
	assert.NotContains(t, prompts.AssistantRegister, "## Register:")
}

// TestCardsSystem_RendersAttendees asserts that calendar events with
// Attendees populate the prompt's calendar list with a "with X, Y"
// suffix — feeds the model for assistant-mode "text Dana to confirm"
// proposal cards.
func TestCardsSystem_RendersAttendees(t *testing.T) {
	prompts, err := LoadPrompts("")
	require.NoError(t, err)

	cal := []projection.CalendarEvent{
		{
			UID:       "evt-dinner",
			Title:     "Dinner with Dana & Alex",
			Start:     time.Date(2026, 5, 10, 20, 0, 0, 0, time.UTC),
			End:       time.Date(2026, 5, 10, 22, 0, 0, 0, time.UTC),
			Tag:       "personal",
			Attendees: []string{"Dana Smith", "Alex Doe"},
		},
	}

	var buf bytes.Buffer
	err = prompts.CardsSystem.Execute(&buf, map[string]any{
		"VoiceShort": "voice",
		"State":      "morning_calm",
		"StateBias":  prompts.StateBias[StateMorningCalm],
		"Date":       "2026-05-10",
		"TZ":         "UTC",
		"Calendar":   cal,
	})
	require.NoError(t, err)
	out := buf.String()

	assert.Contains(t, out, "with Dana Smith, Alex Doe",
		"attendee list must render via the join template func")
	assert.Contains(t, out, "uid=evt-dinner")
}

// TestCardsSystem_RendersProposalSuppress asserts that already-handled
// event UIDs are surfaced to the model as a do-not-propose list.
func TestCardsSystem_RendersProposalSuppress(t *testing.T) {
	prompts, err := LoadPrompts("")
	require.NoError(t, err)

	var buf bytes.Buffer
	err = prompts.CardsSystem.Execute(&buf, map[string]any{
		"VoiceShort":       "voice",
		"State":            "morning_calm",
		"StateBias":        prompts.StateBias[StateMorningCalm],
		"Date":             "2026-05-10",
		"TZ":               "UTC",
		"Calendar":         []projection.CalendarEvent{},
		"ProposalSuppress": []string{"evt-dinner", "evt-lunch"},
	})
	require.NoError(t, err)
	out := buf.String()

	assert.Contains(t, out, "Already-handled events")
	assert.Contains(t, out, "evt-dinner")
	assert.Contains(t, out, "evt-lunch")
}

// TestCardsSystem_OmitsSuppressBlockWhenEmpty asserts the prompt stays
// byte-equal to V2.12 when no suppression is active.
func TestCardsSystem_OmitsSuppressBlockWhenEmpty(t *testing.T) {
	prompts, err := LoadPrompts("")
	require.NoError(t, err)

	var buf bytes.Buffer
	err = prompts.CardsSystem.Execute(&buf, map[string]any{
		"VoiceShort": "voice",
		"State":      "morning_calm",
		"StateBias":  prompts.StateBias[StateMorningCalm],
		"Date":       "2026-05-10",
		"TZ":         "UTC",
		"Calendar":   []projection.CalendarEvent{},
	})
	require.NoError(t, err)
	assert.NotContains(t, buf.String(), "Already-handled events")
}
