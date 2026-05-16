package projection

import (
	"context"
	"time"

	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/store"
)

// Concern is the projection-side shape injected into the cards / briefing /
// reactive prompts. It strips the persistence-only fields (created_at,
// merged_into_id, run IDs, soft-delete state) the synth doesn't need; the
// review surface uses the full store.Concern via the API directly.
//
// ObservationCount is filled from the concern_observations join when the
// projection is asked for it; it's separate from the concern row so a list
// query that doesn't need counts skips the per-row count subquery.
type Concern struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Description      string    `json:"description"`
	State            string    `json:"state"`
	Source           string    `json:"source"`
	Confidence       float64   `json:"confidence"`
	LastActiveAt     time.Time `json:"last_active_at"`
	ObservationCount int64     `json:"observation_count,omitempty"`
	// ReadyToRetire is computed on the fly from LastActiveAt against the
	// projection's AutoRetireDays threshold. Only `active` concerns can be
	// "ready to retire"; paused/ended/merged stay false. The flag is the
	// single source of truth the UI reads — recognition's audit log
	// captures the time-series; this field captures the present.
	ReadyToRetire bool `json:"ready_to_retire,omitempty"`
}

// ActiveConcernsConfig pins the per-projection cap. Briefing uses 5; cards
// uses 5 too (concerns relevant to today). Reactive uses 3.
type ActiveConcernsConfig struct {
	Limit         int
	IncludeCounts bool
	IncludePaused bool // Phase 5 may surface paused-but-recently-active concerns; default off.
	// AutoRetireDays gates the ReadyToRetire flag. 0 means default 90.
	// Phase 5 derives the flag from LastActiveAt against now-Days.
	AutoRetireDays int
	// Now is the clock the retirement check uses. Tests inject; production
	// leaves it nil (defaults to time.Now).
	Now func() time.Time
}

// ActiveConcerns wraps the ConcernRepo and surfaces the top-N active
// concerns ordered by last_active_at descending. Like MemoryFacts, the
// projection bypasses the event-log Reader — concern state lives in its
// own table — but implements Projection[T] for plug-symmetry.
type ActiveConcerns struct {
	Repo    *store.ConcernRepo
	TagRepo *store.ConcernObservationRepo // optional; only used when IncludeCounts is true
	Config  ActiveConcernsConfig
}

// Name returns the projection identifier.
func (p ActiveConcerns) Name() string { return "active_concerns" }

// Compute returns up to Config.Limit visible active concerns. Returns nil
// when the repo is unset so callers degrade gracefully.
func (p ActiveConcerns) Compute(ctx context.Context, _ log.Reader) ([]Concern, error) {
	if p.Repo == nil {
		return nil, nil
	}
	limit := p.Config.Limit
	if limit <= 0 {
		limit = 5
	}

	rows, err := p.Repo.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	if p.Config.IncludePaused {
		paused, err := p.Repo.ListByState(ctx, store.ConcernStatePaused)
		if err != nil {
			return nil, err
		}
		rows = append(rows, paused...)
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}

	retireDays := p.Config.AutoRetireDays
	if retireDays <= 0 {
		retireDays = DefaultAutoRetireDays
	}
	nowFn := p.Config.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	retireCutoff := nowFn().Add(-time.Duration(retireDays) * 24 * time.Hour)

	out := make([]Concern, 0, len(rows))
	for _, r := range rows {
		c := Concern{
			ID:           r.ID,
			Name:         r.Name,
			Description:  r.Description,
			State:        r.State,
			Source:       r.Source,
			Confidence:   r.Confidence,
			LastActiveAt: r.LastActiveAt,
		}
		if r.State == store.ConcernStateActive && !r.LastActiveAt.After(retireCutoff) {
			c.ReadyToRetire = true
		}
		if p.Config.IncludeCounts && p.TagRepo != nil {
			n, err := p.TagRepo.CountByConcern(ctx, r.ID)
			if err != nil {
				return nil, err
			}
			c.ObservationCount = n
		}
		out = append(out, c)
	}
	return out, nil
}

// DefaultAutoRetireDays is the projection-side fallback when
// ActiveConcernsConfig.AutoRetireDays is unset. Mirrors the config
// default in internal/config/config.go and the recognition retirement
// survey threshold.
const DefaultAutoRetireDays = 90

// ConcernsForEvent returns the visible concerns tagged to one event. Phase
// 3 uses it to propagate concern context to a card whose source observation
// is concern-tagged. Returns nil when either repo is unset.
//
// Not a Projection[T] — the input is an event ID, not the log reader.
func ConcernsForEvent(ctx context.Context, concernRepo *store.ConcernRepo, tagRepo *store.ConcernObservationRepo, eventID string) ([]Concern, error) {
	if concernRepo == nil || tagRepo == nil || eventID == "" {
		return nil, nil
	}
	tags, err := tagRepo.ListByEvent(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if len(tags) == 0 {
		return nil, nil
	}
	out := make([]Concern, 0, len(tags))
	for _, t := range tags {
		c, err := concernRepo.GetByID(ctx, t.ConcernID)
		if err != nil {
			return nil, err
		}
		if c == nil || (c.State != store.ConcernStateActive && c.State != store.ConcernStatePaused) {
			// Skip terminal concerns (ended, merged) — they don't add
			// surfacing context. The audit row stays in the join table
			// for history.
			continue
		}
		out = append(out, Concern{
			ID:           c.ID,
			Name:         c.Name,
			Description:  c.Description,
			State:        c.State,
			Source:       c.Source,
			Confidence:   c.Confidence,
			LastActiveAt: c.LastActiveAt,
		})
	}
	return out, nil
}
