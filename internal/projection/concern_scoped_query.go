package projection

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/store"
)

// V2.5.0 Phase 3 — concern-scoped retrieval projection.
//
// QueryConcernEvidence is the "what's happening with construction?"
// answer engine. Given a concern_id, it returns a compact, ranked
// slice of evidence the model can use to compose a card:
//
//   - The N most-recent observations tagged to the concern.
//   - Their date / kind / title / 1-line preview.
//
// Phase 3 evidence is observation-only. Phase 5 may add memory facts
// whose subject overlaps the concern's name, plus run windows or
// other projection bleed; the structure here leaves room for those
// without forcing the model to consume them today.
//
// Terminal concerns (ended / merged) return `nil, nil` cleanly so a
// caller doesn't accidentally surface a tombstone. The reactive Ask
// path uses this directly via the ReadConcernEvidenceTool — the
// evidence prose is what lands in the user-facing card.

// EvidenceItem is one observation surfaced to the model.
type EvidenceItem struct {
	EventID string    `json:"event_id"`
	Date    time.Time `json:"date"`
	Kind    string    `json:"kind"` // "mail" | "cal" | "cal_changed"
	Title   string    `json:"title"`
	Sender  string    `json:"sender,omitempty"`  // mail only
	Preview string    `json:"preview,omitempty"` // 1-line excerpt; bounded
}

// Evidence is the runner's full report. ConcernID + ConcernName let
// the tool reflect them back to the model in the formatted prose so
// the card it composes can ground the title and sub copy.
type Evidence struct {
	ConcernID    string         `json:"concern_id"`
	ConcernName  string         `json:"concern_name"`
	Description  string         `json:"description,omitempty"`
	Observations []EvidenceItem `json:"observations"`
}

// QueryConcernEvidenceOpts tunes one query.
type QueryConcernEvidenceOpts struct {
	MaxObservations int           // 0 → 5
	Lookback        time.Duration // 0 → 30 days
	Now             time.Time     // 0 → time.Now
}

// QueryConcernEvidenceDeps wires the read-side projection. All four
// must be non-nil; otherwise the query returns `nil, nil`.
type QueryConcernEvidenceDeps struct {
	Concerns *store.ConcernRepo
	Tags     *store.ConcernObservationRepo
	Reader   log.Reader
}

// queryConcernEvidenceMaxObservations is the default cap on returned
// items. The tool's prose bound (≤600 chars) governs the upstream
// budget; this cap protects against runaway prompts when a concern
// has hundreds of tagged observations.
const queryConcernEvidenceMaxObservations = 5

// queryConcernEvidenceDefaultLookback is the default time window for
// the recent-observations slice. 30 days matches "what's happening"
// queries — older context is what retrospective tagging surfaces, not
// what the user is asking about right now.
const queryConcernEvidenceDefaultLookback = 30 * 24 * time.Hour

// QueryConcernEvidence assembles a compact evidence slice for the
// given concern. Returns `nil, nil` if the concern doesn't exist or
// is in a terminal state.
func QueryConcernEvidence(
	ctx context.Context,
	deps QueryConcernEvidenceDeps,
	concernID string,
	opts QueryConcernEvidenceOpts,
) (*Evidence, error) {
	if deps.Concerns == nil || deps.Tags == nil || deps.Reader == nil {
		return nil, nil
	}
	if strings.TrimSpace(concernID) == "" {
		return nil, nil
	}
	concern, err := deps.Concerns.GetByID(ctx, concernID)
	if err != nil {
		return nil, err
	}
	if concern == nil {
		return nil, nil
	}
	if concern.State == store.ConcernStateEnded || concern.State == store.ConcernStateMerged {
		return nil, nil
	}

	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	lookback := opts.Lookback
	if lookback <= 0 {
		lookback = queryConcernEvidenceDefaultLookback
	}
	maxObs := opts.MaxObservations
	if maxObs <= 0 {
		maxObs = queryConcernEvidenceMaxObservations
	}

	// Pull all visible tags ordered by tagged_at desc; cap at 4× the
	// requested count so we have headroom to drop tags whose underlying
	// event has aged out of the lookback window.
	tags, err := deps.Tags.ListByConcern(ctx, concernID, maxObs*4)
	if err != nil {
		return nil, err
	}
	if len(tags) == 0 {
		return &Evidence{
			ConcernID:    concern.ID,
			ConcernName:  concern.Name,
			Description:  concern.Description,
			Observations: []EvidenceItem{},
		}, nil
	}

	// Index events by ID by reading mail + cal kinds — we don't have a
	// "fetch by ID" Reader API, so a single ByKind scan is the cheapest
	// path.
	since := now.Add(-lookback)
	wantIDs := make(map[string]struct{}, len(tags))
	for _, t := range tags {
		wantIDs[t.EventID] = struct{}{}
	}
	mail, _ := deps.Reader.ByKind(ctx, log.KindMailReceived)
	calSeen, _ := deps.Reader.ByKind(ctx, log.KindCalEventSeen)
	calChanged, _ := deps.Reader.ByKind(ctx, log.KindCalEventChanged)

	items := make([]EvidenceItem, 0, len(tags))
	addItem := func(ev log.Event, kind string) {
		if _, ok := wantIDs[ev.ID]; !ok {
			return
		}
		if ev.TS.Before(since) {
			return
		}
		title, sender, preview := decodeEvidenceFields(ev, kind)
		items = append(items, EvidenceItem{
			EventID: ev.ID,
			Date:    ev.TS,
			Kind:    kind,
			Title:   title,
			Sender:  sender,
			Preview: preview,
		})
	}
	for _, e := range mail {
		addItem(e, "mail")
	}
	for _, e := range calSeen {
		addItem(e, "cal")
	}
	for _, e := range calChanged {
		addItem(e, "cal_changed")
	}

	// Most-recent first, then truncate to MaxObservations.
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].Date.After(items[j].Date)
	})
	if len(items) > maxObs {
		items = items[:maxObs]
	}

	return &Evidence{
		ConcernID:    concern.ID,
		ConcernName:  concern.Name,
		Description:  concern.Description,
		Observations: items,
	}, nil
}

// decodeEvidenceFields pulls out a presentable Title / Sender /
// Preview from an event's payload. Best-effort: a malformed payload
// degrades to empty strings rather than failing the whole query.
//
// Mail payloads carry `subject`, `from`, `body_preview`. Calendar
// payloads carry `title`. Other shapes are not surfaced because Phase
// 3 only ships mail + calendar evidence.
func decodeEvidenceFields(ev log.Event, kind string) (title, sender, preview string) {
	switch kind {
	case "mail":
		var p struct {
			Subject     string `json:"subject"`
			From        string `json:"from"`
			BodyPreview string `json:"body_preview"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		title = strings.TrimSpace(p.Subject)
		if title == "" {
			title = "(no subject)"
		}
		sender = strings.TrimSpace(p.From)
		preview = collapsePreview(p.BodyPreview, 120)
	case "cal", "cal_changed":
		var p struct {
			Title string `json:"title"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		title = strings.TrimSpace(p.Title)
		if title == "" {
			title = "(no title)"
		}
	}
	return title, sender, preview
}

// collapsePreview folds runs of whitespace and truncates to max chars.
func collapsePreview(s string, max int) string {
	cleaned := strings.Join(strings.Fields(s), " ")
	if max > 0 && len(cleaned) > max {
		cleaned = cleaned[:max] + "…"
	}
	return cleaned
}
