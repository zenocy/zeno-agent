package synth

import (
	"context"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/projection"
	"github.com/zenocy/zeno-v2/internal/store"
)

// V2.5.0 Phase 3 — card → concern inheritance.
//
// When a card's underlying observation is concern-tagged, the persisted
// card row carries that concern's ID. Inheritance is best-effort:
//
//   - Calendar cards: match SrcLabel substring against today's calendar
//     events, then look up concerns by `event.UID` (a stable handle).
//   - Mail cards: deferred. Phase 3 has no subject→event-IDs query
//     (the Thread projection summarizes by subject and exposes no IDs).
//     A subject-keyed concern-tag scan would touch the log; that's
//     Phase 5 territory.
//   - Other sources (personal, tasks, ask): no inheritance path. The
//     review surface remains the source of truth via direct API calls.
//
// `ConcernsForEvent` filters terminal concerns (ended/merged) so a
// merged tombstone never bleeds into a fresh card. Ambiguity (zero
// matches) leaves the field nil — surfacing is forgiving by design.

// ResolveCardConcern returns the concern ID a card should inherit, or
// nil when no concern resolves. Both repos must be non-nil; otherwise
// the function returns nil cleanly so callers can treat the inheritance
// pass as fully optional.
func ResolveCardConcern(
	ctx context.Context,
	concernRepo *store.ConcernRepo,
	tagRepo *store.ConcernObservationRepo,
	card Card,
	cal []projection.CalendarEvent,
	_ []projection.Thread,
	logger *logrus.Entry,
) *string {
	if concernRepo == nil || tagRepo == nil {
		return nil
	}
	src := strings.TrimSpace(card.SrcLabel)
	if src == "" {
		return nil
	}
	if card.Source != "calendar" {
		return nil
	}
	uid := matchCalendarEventUID(src, cal)
	if uid == "" {
		return nil
	}
	concerns, err := projection.ConcernsForEvent(ctx, concernRepo, tagRepo, uid)
	if err != nil {
		if logger != nil {
			logger.WithError(err).Debug("concern inheritance: ConcernsForEvent failed")
		}
		return nil
	}
	if len(concerns) == 0 {
		return nil
	}
	id := concerns[0].ID
	return &id
}

// matchCalendarEventUID finds the calendar event whose Title contains the
// card's SrcLabel (case-insensitive substring), and returns its UID.
// Returns "" when no match is found. The matching mirrors the card
// generation logic — the model picks an event by title, so the title
// is the natural matching key.
//
// Either-direction substring match handles the common abbreviation case
// where the model says "Acuity" for an event titled
// "Acuity Capital — Series B narrative review."
func matchCalendarEventUID(srcLabel string, cal []projection.CalendarEvent) string {
	srcLower := strings.ToLower(srcLabel)
	for _, ev := range cal {
		title := strings.ToLower(ev.Title)
		if strings.Contains(title, srcLower) || strings.Contains(srcLower, title) {
			return ev.UID
		}
	}
	return ""
}
