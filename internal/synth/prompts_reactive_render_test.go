package synth

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// renderReactive renders the Reactive template with a minimal context
// (just enough to satisfy the template's range/with blocks). Returns
// the rendered string for content assertions.
func renderReactive(t *testing.T, conversation *ConversationContext) string {
	t.Helper()
	prompts, err := LoadPrompts("")
	require.NoError(t, err)
	var buf bytes.Buffer
	err = prompts.Reactive.Execute(&buf, map[string]any{
		"VoiceShort":       "voice",
		"State":            "",
		"Date":             "2026-05-20",
		"TZ":               "UTC",
		"Now":              "Wed 09:00",
		"Query":            "what's the latest in the war of iran?",
		"Calendar":         nil,
		"CalendarTomorrow": nil,
		"CalendarWeek":     nil,
		"Threads":          nil,
		"RunWindow":        nil,
		"Memory":           nil,
		"Concerns":         nil,
		"Conversation":     conversation,
		"WiredIntents":     nil,
		"WhatsAppActivity": nil,
	})
	require.NoError(t, err)
	return buf.String()
}

// TestReactiveTemplate_InAppBodyBlockPresent pins the in-app surface
// branch: when Conversation is nil (the in-app text chat) the prompt
// must include the "In-app body" section instructing the model to
// populate multi-paragraph elaboration, and the JSON spec example
// must list the optional `body` field.
func TestReactiveTemplate_InAppBodyBlockPresent(t *testing.T) {
	out := renderReactive(t, nil)

	assert.Contains(t, out, "In-app body",
		"in-app surface (Conversation==nil) must render the In-app body section header")
	assert.Contains(t, out, "2 to 4 short paragraphs",
		"in-app surface must instruct the model on body paragraph count")
	assert.Contains(t, out, `"body"`,
		"JSON spec example must mention the body field for the in-app surface")

	// And the WhatsApp register heading must be absent on the in-app
	// surface. (The phrase "WhatsApp register" also appears as a
	// cross-reference in the Messaging rules block above, so we anchor
	// on the markdown header which only renders inside the gated block.)
	assert.NotContains(t, out, "# WhatsApp register",
		"WhatsApp register heading must not render when Conversation is nil")
}

// TestReactiveTemplate_WhatsAppSuppressesBody pins the WhatsApp branch:
// when Conversation is non-nil the In-app body section must NOT render
// (the multi-paragraph elaboration is only for the in-app surface),
// and the WhatsApp register must instruct the model to leave body
// empty so an accidental leak from the JSON spec example doesn't end
// up sent over WhatsApp.
func TestReactiveTemplate_WhatsAppSuppressesBody(t *testing.T) {
	out := renderReactive(t, &ConversationContext{
		IsDM:       true,
		SenderName: "Andreas",
	})

	assert.NotContains(t, out, "In-app body",
		"WhatsApp surface must not render the In-app body section")
	assert.NotContains(t, out, "2 to 4 short paragraphs",
		"WhatsApp surface must not carry the body paragraph instruction")

	// WhatsApp register still renders.
	assert.Contains(t, out, "# WhatsApp register")
	// And it explicitly tells the model to leave body empty.
	assert.Contains(t, out, "Leave `body` empty",
		"WhatsApp register must instruct the model to leave body empty so it can't leak into the verbatim send path")
}
