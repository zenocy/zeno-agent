package synth

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/projection"
)

// TestReactiveTemplate_WhatsAppActivityRendered verifies V2.13.2:
// the recent-activity block surfaces canonical recipient name, event
// title, status, and reply quote without leaking operational
// identifiers.
func TestReactiveTemplate_WhatsAppActivityRendered(t *testing.T) {
	prompts, err := LoadPrompts("")
	require.NoError(t, err)

	resolved := time.Date(2026, 5, 10, 17, 30, 0, 0, time.UTC)
	activity := []projection.WhatsAppActivity{
		{
			SentAt:        time.Date(2026, 5, 10, 14, 25, 0, 0, time.UTC),
			RecipientName: "Dana Lopez",
			EventTitle:    "Dinner with Dana & Alex",
			EventUID:      "evt-dinner",
			Status:        "awaiting_reply",
		},
		{
			SentAt:        time.Date(2026, 5, 10, 13, 0, 0, 0, time.UTC),
			RecipientName: "Sam Carter",
			EventTitle:    "Brunch with Sam",
			EventUID:      "evt-brunch",
			Status:        "replied",
			ResolvedAt:    &resolved,
			ReplyBody:     "Yes, see you at noon",
		},
	}

	var buf bytes.Buffer
	err = prompts.Reactive.Execute(&buf, map[string]any{
		"VoiceShort":       "voice",
		"State":            "morning_calm",
		"Date":             "2026-05-10",
		"TZ":               "UTC",
		"Query":            "has Dana replied yet?",
		"Calendar":         []projection.CalendarEvent{},
		"CalendarTomorrow": []projection.CalendarEvent{},
		"CalendarWeek":     []projection.CalendarEvent{},
		"Threads":          []projection.Thread{},
		"Memory":           []projection.MemoryFact{},
		"WhatsAppActivity": activity,
		"WiredIntents":     []WiredIntent{{Intent: "send_whatsapp", Description: "send a WhatsApp message"}},
	})
	require.NoError(t, err)
	out := buf.String()

	// Block heading + per-row contents.
	assert.Contains(t, out, "Recent WhatsApp activity")
	assert.Contains(t, out, "Dana Lopez")
	assert.Contains(t, out, `re: "Dinner with Dana & Alex"`)
	assert.Contains(t, out, "awaiting_reply")
	assert.Contains(t, out, "14:25")

	// Replied row carries the inbound text in quotes.
	assert.Contains(t, out, "Sam Carter")
	assert.Contains(t, out, "replied")
	assert.Contains(t, out, `"Yes, see you at noon"`)

	// Instruction line teaches the model how to use the block.
	assert.Contains(t, out, "look here first")

	// No operational identifiers leak.
	assert.NotContains(t, out, "@s.whatsapp.net")
	assert.NotContains(t, out, "WAMSG")
}

func TestReactiveTemplate_WhatsAppActivityEmptyState(t *testing.T) {
	prompts, err := LoadPrompts("")
	require.NoError(t, err)

	var buf bytes.Buffer
	err = prompts.Reactive.Execute(&buf, map[string]any{
		"VoiceShort":       "voice",
		"State":            "morning_calm",
		"Date":             "2026-05-10",
		"TZ":               "UTC",
		"Query":            "anything?",
		"Calendar":         []projection.CalendarEvent{},
		"CalendarTomorrow": []projection.CalendarEvent{},
		"CalendarWeek":     []projection.CalendarEvent{},
		"Threads":          []projection.Thread{},
		"Memory":           []projection.MemoryFact{},
		"WhatsAppActivity": []projection.WhatsAppActivity{},
		"WiredIntents":     []WiredIntent{},
	})
	require.NoError(t, err)
	out := buf.String()
	assert.Contains(t, out, "(no recent assistant-mode messages)")
}

func TestConverseTemplate_WhatsAppActivityRendered(t *testing.T) {
	prompts, err := LoadPrompts("")
	require.NoError(t, err)

	activity := []projection.WhatsAppActivity{
		{
			SentAt:        time.Date(2026, 5, 10, 14, 25, 0, 0, time.UTC),
			RecipientName: "Dana Lopez",
			EventTitle:    "Dinner with Dana & Alex",
			EventUID:      "evt-dinner",
			Status:        "awaiting_reply",
		},
	}

	var buf bytes.Buffer
	err = prompts.Converse.Execute(&buf, map[string]any{
		"VoiceShort":       "voice",
		"State":            "morning_calm",
		"Date":             "2026-05-10",
		"TZ":               "UTC",
		"Query":            "did Dana reply?",
		"Calendar":         []projection.CalendarEvent{},
		"Threads":          []projection.Thread{},
		"Memory":           []projection.MemoryFact{},
		"Card":             PinnedCard{Title: "Dinner", SrcLabel: "Calendar"},
		"PriorTurns":       []PriorTurn{},
		"WhatsAppActivity": activity,
		"WiredIntents":     []WiredIntent{{Intent: "send_whatsapp", Description: "send a WhatsApp message"}},
	})
	require.NoError(t, err)
	out := buf.String()
	assert.Contains(t, out, "Recent WhatsApp activity")
	assert.Contains(t, out, "Dana Lopez")
	assert.Contains(t, out, "awaiting_reply")
}
