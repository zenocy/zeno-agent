package synth

import (
	"regexp"
	"strings"

	"github.com/zenocy/zeno-v2/internal/projection"
)

// resolveEntityKey derives a stable, date-independent handle for the
// underlying entity a card is about. It is the V2.x continuity anchor:
// when a refresh or next-day run produces a card for the same entity, the
// key is reused as the card ID so the Upsert overwrites in place rather
// than minting a near-duplicate row (the root cause of card repetition).
//
// Deny-by-default: returns "" when nothing confidently matches, in which
// case postProcessCards falls back to the title-slug ID (legacy behavior).
// Matching reuses the existing conservative matchers (matchCalendarEventUID,
// bestMatchingThread, cardMatchesText) so a card only anchors to an entity
// it shares distinctive tokens with — false negatives (a card stays
// title-keyed) are fine; false-positive merges are not.
//
// Key namespaces:
//
//	propose-confirm:<uid>  send_whatsapp confirmation proposal (V2.13 scheme,
//	                       preserved verbatim so existing rows keep identity)
//	digest:<date>          the day's rolled-up low-signal digest card
//	cal:<uid>              a calendar/personal event
//	thread:<subj-slug>     an email/tasks thread (re:/fwd: stripped)
//	ticker:<SYM>           a markets watchlist card
func resolveEntityKey(c *Card, date string, cal []projection.CalendarEvent, threads []projection.Thread) string {
	// Proposal confirmation cards: keyed on the target event UID. Folded
	// in from the old stableCardID special-case so there is one anchor
	// path. Kept on the legacy "propose-confirm-" prefix so V2.13 rows
	// upsert in place instead of forking a duplicate.
	if len(c.Actions) > 0 {
		a := c.Actions[0]
		if a.Intent == "send_whatsapp" && a.Target != nil {
			kind, _ := a.Target["context_kind"].(string)
			id, _ := a.Target["context_id"].(string)
			if kind == "event" && strings.TrimSpace(id) != "" {
				return "propose-confirm-" + slugifyUID(id)
			}
		}
	}

	if c.Kind == "digest" {
		return "digest:" + date
	}

	switch c.Source {
	case "calendar", "personal":
		if uid := matchEventUID(c, cal); uid != "" {
			return EventEntityKey(uid)
		}
	case "mail", "tasks":
		if t := bestMatchingThread(c, threads); t != nil {
			return ThreadEntityKey(t.Subject)
		}
	case "markets":
		if tk := tickerFromSrcLabel(c.SrcLabel); tk != "" {
			return TickerEntityKey(tk)
		}
	case "ask":
		// Cross-source dedup: an ask card about an event/thread that also
		// has a morning card anchors to the same entity so the ListByDate
		// fold collapses the pair. Ask cards with no entity match (web
		// queries, general questions) stay title-keyed.
		if uid := matchEventUID(c, cal); uid != "" {
			return EventEntityKey(uid)
		}
		if t := bestMatchingThread(c, threads); t != nil {
			return ThreadEntityKey(t.Subject)
		}
	}
	return ""
}

// EventEntityKey, ThreadEntityKey, and TickerEntityKey are the canonical
// entity-key constructors. They are exported so the inject detector
// (internal/sensor) can derive the same key for a projection thread/event
// and check whether it already has a card — the basis for the V2.x
// update-in-place reactive path. Keep these the single source of truth for
// key format so the detector and the cards post-process never drift.
func EventEntityKey(uid string) string { return "cal:" + slugifyUID(uid) }
func ThreadEntityKey(subject string) string {
	return "thread:" + slugifyUID(normalizeSubject(subject))
}
func TickerEntityKey(sym string) string { return "ticker:" + strings.ToUpper(strings.TrimSpace(sym)) }

// matchEventUID returns the UID of the calendar event this card is about,
// or "". It prefers the SrcLabel abbreviation match the model tends to use
// ("Acuity" → "Acuity Capital — Series B"), then falls back to a
// distinctive-token overlap between the card's title+sub and an event title
// — which is what catches personal cards whose SrcLabel ("Family · Sam")
// shares nothing with the event title.
func matchEventUID(c *Card, cal []projection.CalendarEvent) string {
	if uid := matchCalendarEventUID(c.SrcLabel, cal); uid != "" {
		return uid
	}
	for _, ev := range cal {
		if cardMatchesText(c, ev.Title) {
			return ev.UID
		}
	}
	return ""
}

var rePrefixRE = regexp.MustCompile(`(?i)^\s*((re|fwd|fw)\s*:\s*)+`)

// normalizeSubject strips leading reply/forward prefixes (re:, fwd:, fw:,
// possibly stacked) and surrounding whitespace so "Re: redline" and
// "redline" produce the same thread entity key. slugifyUID lowercases and
// cleans the rest.
func normalizeSubject(s string) string {
	return strings.TrimSpace(rePrefixRE.ReplaceAllString(s, ""))
}

// tickerFromSrcLabel extracts the ticker from a markets SrcLabel of the
// form "Markets · AAPL". Returns "" when the trailing token doesn't look
// like a ticker (1–6 chars, uppercase letters/digits/dot) so a malformed
// label falls through to the title slug rather than minting a junk key.
func tickerFromSrcLabel(label string) string {
	parts := strings.Split(label, "·")
	cand := strings.ToUpper(strings.TrimSpace(parts[len(parts)-1]))
	if cand == "" || len(cand) > 6 {
		return ""
	}
	for _, r := range cand {
		if !((r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.') {
			return ""
		}
	}
	return cand
}
