// Package projection — WhatsAppActivity surfaces recent assistant-mode
// outbound WhatsApp activity into the reactive + converse prompts so
// Zeno can answer follow-up questions like "has Dana replied yet?"
// without losing track of what it sent on the user's behalf. V2.13.2.
package projection

import (
	"context"
	"strings"
	"time"

	"github.com/zenocy/zeno-v2/internal/store"
)

// activityWindow is the lookback for "recent" outbound activity.
// 24h covers same-day plus late-evening sends from the day before
// (a "did Sam reply?" asked Tuesday morning still surfaces a Monday-
// night send).
const activityWindow = 24 * time.Hour

// activityReplyMaxLen caps the inbound body rendered into the prompt.
// Mirrors the V2.13 reply-card composer's truncation so the same text
// the user sees on the rail is what the model sees in prompt context.
const activityReplyMaxLen = 280

// WhatsAppActivity is one entry in the recent-activity list rendered
// into the reactive and converse prompts. Operational identifiers
// (JIDs, phones) never leak — only canonical contact name + event
// title + status + (optional) reply text.
type WhatsAppActivity struct {
	SentAt        time.Time
	RecipientName string
	EventTitle    string
	EventUID      string
	Status        string // "awaiting_reply" | "replied" | "expired"
	ResolvedAt    *time.Time
	ReplyBody     string
}

// WhatsAppActivityProjection reads ExpectedReply rows from the last
// activityWindow, joins ContextID against today's calendar for human
// titles, and returns up to maxItems most-recent entries.
//
// V2.13.3d: TZ is the user's local Location. SentAt and ResolvedAt
// are read from the DB in UTC (GORM's default round-trip), so they
// must be converted before the prompt renders `{{ .SentAt.Format
// "15:04" }}` — otherwise "Dana replied 14:25" reads as 14:25 UTC
// in the model's clock and the elapsed-time calculation skews by the
// TZ offset (e.g. 3h in EEST). Nil TZ → UTC (matches eval/replay).
type WhatsAppActivityProjection struct {
	Repo *store.ExpectedReplyRepo
	Now  func() time.Time
	TZ   *time.Location
}

// Compute returns the activity slice, newest-first. Nil-safe: a nil
// repo or empty result both yield an empty slice (the prompt block
// then renders the "(no recent assistant-mode messages)" branch).
//
// The cal slice is the same []CalendarEvent the reactive/converse
// pipelines have already computed — passed in to avoid a second
// projection pass and to share its TZ semantics.
func (p WhatsAppActivityProjection) Compute(ctx context.Context, cal []CalendarEvent, maxItems int) ([]WhatsAppActivity, error) {
	if p.Repo == nil {
		return nil, nil
	}
	if maxItems <= 0 {
		maxItems = 5
	}
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	tz := p.TZ
	if tz == nil {
		tz = time.UTC
	}
	since := now().Add(-activityWindow)

	rows, err := p.Repo.ListRecent(ctx, since, maxItems)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}

	titleByUID := make(map[string]string, len(cal))
	for _, ev := range cal {
		if strings.TrimSpace(ev.UID) != "" {
			titleByUID[ev.UID] = ev.Title
		}
	}

	out := make([]WhatsAppActivity, 0, len(rows))
	nowTime := now()
	for _, r := range rows {
		entry := WhatsAppActivity{
			SentAt:        r.SentAt.In(tz),
			RecipientName: r.RecipientName,
			EventUID:      r.ContextID,
			Status:        statusFor(r, nowTime),
			ReplyBody:     truncateReply(r.InboundBody),
		}
		if r.ResolvedAt != nil {
			localResolved := r.ResolvedAt.In(tz)
			entry.ResolvedAt = &localResolved
		}
		if r.ContextKind == "event" {
			if title, ok := titleByUID[r.ContextID]; ok {
				entry.EventTitle = title
			}
		}
		out = append(out, entry)
	}
	return out, nil
}

// statusFor derives the prompt-facing status string from the row.
//   - resolved (ResolvedAt set)              → "replied"
//   - past expiry, not resolved              → "expired"
//   - otherwise                              → "awaiting_reply"
func statusFor(r store.ExpectedReply, now time.Time) string {
	if r.ResolvedAt != nil && !r.ResolvedAt.IsZero() {
		return "replied"
	}
	if !r.ExpiresAt.IsZero() && r.ExpiresAt.Before(now) {
		return "expired"
	}
	return "awaiting_reply"
}

// truncateReply caps the inbound body at activityReplyMaxLen runes
// and appends an ellipsis when truncated. Empty in returns empty out.
func truncateReply(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) <= activityReplyMaxLen {
		return s
	}
	return s[:activityReplyMaxLen-1] + "…"
}
