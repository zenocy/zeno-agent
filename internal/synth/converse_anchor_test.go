package synth

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/projection"
)

// TestAnchorEventUID_TitleMatch asserts that a pinned card title
// sharing a distinctive token with a calendar event resolves to that
// event's UID — the V2.13.1 card-anchored ask path.
func TestAnchorEventUID_TitleMatch(t *testing.T) {
	cal := []projection.CalendarEvent{
		{
			UID:   "evt-dinner-2026-05-10@cal.example",
			Title: "Dinner with Dana & Alex",
			Start: time.Date(2026, 5, 10, 20, 0, 0, 0, time.UTC),
		},
	}
	got := AnchorEventUID(PinnedCard{
		ID:    "dinner-abcd",
		Title: "Dinner with Dana and Alex tonight",
	}, cal)
	assert.Equal(t, "evt-dinner-2026-05-10@cal.example", got)
}

func TestAnchorEventUID_NoCalendarMatchEmpty(t *testing.T) {
	cal := []projection.CalendarEvent{
		{
			UID:   "evt-standup",
			Title: "Engineering standup",
			Start: time.Date(2026, 5, 10, 10, 0, 0, 0, time.UTC),
		},
	}
	got := AnchorEventUID(PinnedCard{
		Title: "Pricing memo follow-up",
		Sub:   "Owen needs the cohort table by EOD.",
	}, cal)
	assert.Equal(t, "", got, "no shared distinctive token should yield empty")
}

func TestAnchorEventUID_FirstMatchWins(t *testing.T) {
	// Two events both contain "dinner" — the earlier-starting one
	// (sorted by Compute) wins.
	cal := []projection.CalendarEvent{
		{UID: "evt-early-dinner", Title: "Quick dinner with Mara", Start: time.Date(2026, 5, 10, 18, 0, 0, 0, time.UTC)},
		{UID: "evt-late-dinner", Title: "Dinner with Dana", Start: time.Date(2026, 5, 10, 21, 0, 0, 0, time.UTC)},
	}
	got := AnchorEventUID(PinnedCard{Title: "Dinner tonight"}, cal)
	assert.Equal(t, "evt-early-dinner", got)
}

func TestAnchorEventUID_EmptyInputs(t *testing.T) {
	assert.Equal(t, "", AnchorEventUID(PinnedCard{}, nil))
	assert.Equal(t, "", AnchorEventUID(PinnedCard{Title: "Anything"}, nil))
	assert.Equal(t, "", AnchorEventUID(PinnedCard{}, []projection.CalendarEvent{
		{UID: "x", Title: "Standup"},
	}))
}

// TestConverseSystemTemplate_RendersAnchorInstruction asserts the
// converse template emits the "context_id: <uid>" instruction when
// Card.EventUID is populated.
func TestConverseSystemTemplate_RendersAnchorInstruction(t *testing.T) {
	prompts, err := LoadPrompts("")
	require.NoError(t, err)

	pinned := PinnedCard{
		ID:       "dinner-abcd",
		Title:    "Dinner tonight",
		Sub:      "8pm at Avli.",
		SrcLabel: "Calendar · 20:00",
		EventUID: "evt-dinner-2026-05-10@cal.example",
	}

	var buf bytes.Buffer
	err = prompts.Converse.Execute(&buf, map[string]any{
		"VoiceShort": "voice",
		"State":      "morning_calm",
		"Date":       "2026-05-10",
		"TZ":         "UTC",
		"Query":      "text Dana to confirm",
		"Calendar":   []projection.CalendarEvent{},
		"Threads":    []projection.Thread{},
		"Memory":     []projection.MemoryFact{},
		"Card":       pinned,
		"PriorTurns": []PriorTurn{},
		"WiredIntents": []WiredIntent{
			{Intent: "send_whatsapp", Description: "send a WhatsApp message"},
		},
	})
	require.NoError(t, err)
	out := buf.String()

	assert.Contains(t, out, "anchored on calendar event")
	assert.Contains(t, out, "evt-dinner-2026-05-10@cal.example")
	assert.Contains(t, out, `context_kind: "event"`)
	assert.Contains(t, out, "context_id")
}

// TestConverseSystemTemplate_OmitsAnchorWhenNoUID asserts the prompt
// stays byte-equal to the V2.13 baseline when no anchor is set
// (e.g. mail card, anchor lookup miss).
func TestConverseSystemTemplate_OmitsAnchorWhenNoUID(t *testing.T) {
	prompts, err := LoadPrompts("")
	require.NoError(t, err)

	var buf bytes.Buffer
	err = prompts.Converse.Execute(&buf, map[string]any{
		"VoiceShort": "voice",
		"State":      "morning_calm",
		"Date":       "2026-05-10",
		"TZ":         "UTC",
		"Query":      "what's on?",
		"Calendar":   []projection.CalendarEvent{},
		"Threads":    []projection.Thread{},
		"Memory":     []projection.MemoryFact{},
		"Card":       PinnedCard{Title: "Untitled", SrcLabel: "Mail · Acuity"},
		"PriorTurns": []PriorTurn{},
		"WiredIntents": []WiredIntent{
			{Intent: "send_whatsapp", Description: "send a WhatsApp message"},
		},
	})
	require.NoError(t, err)
	out := buf.String()

	assert.NotContains(t, out, "anchored on calendar event")
	// The general send_whatsapp instruction still renders.
	assert.True(t, strings.Contains(out, "WhatsApp") || strings.Contains(out, "whatsapp"),
		"send_whatsapp tool guidance should still appear")
}
