package synth

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestStableCardID_ProposalCardDeterministic asserts the V2.13.0
// proposal-card ID rule: a `send_whatsapp` action with `context_kind=event`
// and a non-empty `context_id` produces a `propose-confirm-<uid-slug>`
// ID, so cards-loop re-runs Upsert in place rather than creating dupes.
func TestStableCardID_ProposalCardDeterministic(t *testing.T) {
	c := &Card{
		Title: "Text Dana to confirm dinner",
		Actions: []Action{
			{
				Label:   "Text Dana to confirm",
				Primary: true,
				Intent:  "send_whatsapp",
				Target: map[string]any{
					"recipient":    "Dana",
					"context_kind": "event",
					"context_id":   "evt-dinner-2026-05-10@cal.example",
				},
			},
		},
	}
	id1 := stableCardID(c)
	id2 := stableCardID(c)
	assert.Equal(t, id1, id2, "deterministic across calls")
	assert.True(t, strings.HasPrefix(id1, "propose-confirm-"), "id %q must use propose-confirm prefix", id1)
}

// TestStableCardID_NormalCardUsesSlug asserts non-proposal cards still
// route through slugFromTitle.
func TestStableCardID_NormalCardUsesSlug(t *testing.T) {
	c := &Card{
		Title: "Dinner tonight at 8",
		Actions: []Action{
			{Label: "Dismiss", Intent: "dismiss"},
		},
	}
	id := stableCardID(c)
	assert.False(t, strings.HasPrefix(id, "propose-confirm-"))
}

// TestStableCardID_NoContextIDFallsBackToSlug asserts a send_whatsapp
// action without a context_id (e.g. a free-form recipient request) does
// NOT collide on a deterministic propose-confirm ID — falls back to
// slugFromTitle so multiple distinct sends don't overwrite each other.
func TestStableCardID_NoContextIDFallsBackToSlug(t *testing.T) {
	c := &Card{
		Title: "Quick note to Sam",
		Actions: []Action{
			{
				Label:  "Send",
				Intent: "send_whatsapp",
				Target: map[string]any{
					"recipient":    "Sam",
					"context_kind": "event",
				},
			},
		},
	}
	id := stableCardID(c)
	assert.False(t, strings.HasPrefix(id, "propose-confirm-"))
}
