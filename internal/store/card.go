// Package store holds the GORM models and repositories for synthesized
// outputs: Card, Briefing, Trace. The same models are routed to either the
// production tables (cards, briefings, traces) or the replay tables
// (cards_replay, briefings_replay, traces_replay) via Table(name).
package store

import (
	"context"
	"fmt"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// Card is one persisted card row. Uniqueness is by ID alone (the primary
// key, generated server-side from the title via slugFromTitle with a hash
// suffix). A non-unique compound index on (date, kind, source) supports the
// date-scoped queries; it does not collapse cards that share a source.
//
// Origin distinguishes cards produced by the morning synth (default empty
// or "morning") from V2.3 message_inject cards ("inject"). The UI uses it
// to badge inject cards without reshuffling the morning grid.
type Card struct {
	ID          string         `gorm:"primaryKey;type:text"         json:"id"`
	Date        string         `gorm:"index;not null"               json:"date"`
	Kind        string         `gorm:"index;not null;default:''"    json:"kind"`
	Source      string         `gorm:"index;not null"               json:"src"`
	SrcLabel    string         `gorm:"type:text"                    json:"src_label"`
	Rel         string         `gorm:"type:text;index"              json:"rel"`
	Origin      string         `gorm:"type:text;index;default:''"   json:"origin,omitempty"`
	Title       string         `gorm:"type:text"                    json:"title"`
	Sub         string         `gorm:"type:text"                    json:"sub"`
	Meta        datatypes.JSON `gorm:"type:text"                    json:"meta"`
	Actions     datatypes.JSON `gorm:"type:text"                    json:"actions"`
	Expand      datatypes.JSON `gorm:"type:text"                    json:"expand,omitempty"`
	TraceID     string         `gorm:"type:text;index"              json:"trace_id,omitempty"`
	RunID       string         `gorm:"type:text;index"              json:"-"`
	Dismissed   bool           `gorm:"default:false"                json:"-"`
	SnoozedDate string         `gorm:"type:text;default:''"         json:"-"` // "YYYY-MM-DD" while snoozed
	// V2.8.1: pinned cards survive day boundaries — ListByDate
	// returns them regardless of the requested date until unpinned.
	Pinned    bool      `gorm:"index;default:false"          json:"pinned,omitempty"`
	CreatedAt time.Time `                                    json:"-"`

	// V2.5.0 Phase 3: nullable FK to concerns.id when this card's
	// underlying observation is concern-tagged. Populated server-side by
	// `synth.ResolveCardConcern` from the (calendar UID | thread subject)
	// match against the concern_observations join. Existing rows back-fill
	// nil; AutoMigrate adds the column on next boot.
	ConcernID *string `gorm:"type:text;index"              json:"concern_id,omitempty"`

	// ExpiresAt is set for reactive ask cards so ListByDate hides them
	// from the main rail after the configured TTL while leaving the row
	// in the table for the archive view. NULL means "never on the main
	// rail" (legacy ask rows from before this field existed) for ask
	// cards, and is irrelevant for non-ask sources (which the filter
	// admits unconditionally).
	ExpiresAt *time.Time `gorm:"index" json:"expires_at,omitempty"`

	// Body holds multi-paragraph elaboration produced by the reactive
	// Ask flow for in-app text-chat queries. Empty on WhatsApp-origin
	// cards and on morning cards. AutoMigrate adds the column on next
	// boot; existing rows back-fill with the default empty string.
	Body string `gorm:"type:text;default:''" json:"body,omitempty"`

	// Sources is the JSON-marshaled list of web citations the model
	// emitted when answering. Shape matches synth.Source ({t, u}).
	// Null on cards that didn't use the web tools; the cardDTO
	// unmarshal handles both null and empty array.
	Sources datatypes.JSON `gorm:"type:text" json:"sources,omitempty"`

	// EntityKey is the V2.x continuity anchor — a stable, date-independent
	// handle for the underlying entity ("cal:<uid>", "thread:<slug>",
	// "ticker:AAPL", "digest:<date>", "propose:<id>"). Derived server-side
	// by synth.resolveEntityKey and used AS the card ID for anchored cards,
	// so a refresh or next-day run Upserts the same entity in place rather
	// than minting a duplicate. Empty for legacy/unanchored rows, which
	// keep title-slug identity. AutoMigrate adds the column; existing rows
	// backfill "".
	EntityKey string `gorm:"type:text;index" json:"-"`

	// LastMaterialAt is the last time this card's material content
	// (Title/Sub) actually changed — distinct from CreatedAt, which moves
	// on every idempotent re-upsert. Powers the "still open since…" UI
	// affordance and the "surface only if changed" prompt hint. Nil until
	// the first material write under the new code.
	LastMaterialAt *time.Time `gorm:"index" json:"-"`

	// FirstShownDate is the earliest date this entity surfaced ("YYYY-MM-DD").
	// Set once on first insert and preserved across re-upserts so the UI can
	// show how long a thread/event has been open. Empty on legacy rows.
	FirstShownDate string `gorm:"type:text;default:''" json:"-"`

	// Items is the JSON-marshaled list of rolled-up children for a
	// kind="digest" card. Shape matches synth.DigestItem. Null on ordinary
	// cards; the cardDTO unmarshal handles both null and empty array.
	Items datatypes.JSON `gorm:"type:text" json:"items,omitempty"`

	// Live is the JSON-marshaled list of serve-time data bindings the model
	// declared. Shape matches synth.LiveField. The HTTP layer resolves each
	// against the latest projection on GET. Null on cards with no volatile
	// data.
	Live datatypes.JSON `gorm:"type:text" json:"live,omitempty"`
}

// CardRepo persists and reads Card rows. The Table field controls which
// physical table is used so the same code path serves prod and replay.
type CardRepo struct {
	DB    *gorm.DB
	Table string // "cards" or "cards_replay"
}

// Migrate runs AutoMigrate against the configured table. Drops any legacy
// unique (date, kind, source) index from prior versions (which collapsed
// multiple cards sharing src/kind into one) and creates a non-unique
// compound index in its place to keep date-scoped queries fast.
//
// Two legacy names are dropped: idx_card_dks (V2.0 singular) and
// idx_<tbl>_dks (V2.1 first attempt). Either form is a unique index that
// would block multi-source persistence; both must go.
func (r *CardRepo) Migrate() error {
	tbl := r.tableName()
	if err := r.DB.Table(tbl).AutoMigrate(&Card{}); err != nil {
		return err
	}
	for _, idx := range []string{
		"idx_card_dks",
		fmt.Sprintf("idx_%s_dks", tbl),
	} {
		if err := r.DB.Exec(fmt.Sprintf("DROP INDEX IF EXISTS %s", idx)).Error; err != nil {
			return err
		}
	}
	newIdx := fmt.Sprintf("idx_%s_dks_query", tbl)
	stmt := fmt.Sprintf(
		"CREATE INDEX IF NOT EXISTS %s ON %s (date, kind, source)",
		newIdx, tbl,
	)
	if err := r.DB.Exec(stmt).Error; err != nil {
		return err
	}
	// V2.x: entity-key index backs ListRecentEntities and the
	// cross-source dedup fold in ListByDate. Non-unique — an entity can
	// have a row per source until the fold collapses them at read time.
	ekIdx := fmt.Sprintf("idx_%s_entity_key", tbl)
	ekStmt := fmt.Sprintf(
		"CREATE INDEX IF NOT EXISTS %s ON %s (entity_key)",
		ekIdx, tbl,
	)
	return r.DB.Exec(ekStmt).Error
}

// Upsert inserts or replaces cards keyed by ID. IDs are slugFromTitle-derived
// upstream, so two cards with distinct titles get distinct IDs and both
// persist; an idempotent re-run with the same titles overwrites in place.
// All cards in one call should share the same RunID so the caller can sweep
// stale rows from prior runs (see DeleteStale).
func (r *CardRepo) Upsert(ctx context.Context, cards []Card) error {
	if len(cards) == 0 {
		return nil
	}
	return r.DB.WithContext(ctx).Table(r.tableName()).
		Clauses(onConflictUpdateAll("id")).Create(&cards).Error
}

// UpsertWithContinuity is the V2.x entity-aware upsert. For each incoming
// card it loads the existing row (by ID — which for anchored cards is the
// entity key) and carries forward the fields that must survive an
// idempotent re-run or a next-day refresh of the same entity:
//
//   - FirstShownDate — set once on first insert, preserved thereafter so the
//     UI can show how long a thread/event has been open.
//   - LastMaterialAt — bumped to `now` only when the card's material content
//     (Title/Sub) actually changed; otherwise the prior value is kept so a
//     no-op re-run doesn't look like fresh activity.
//   - Dismissed / SnoozedDate / Pinned — user actions stick. Because IDs are
//     now stable across days, a naive overwrite would resurrect a card the
//     user dismissed yesterday; preserving these keeps dismissal durable.
//
// The carry-forward values are written onto the row before the base Upsert
// (which overwrites all columns), so the net effect is that only synth-owned
// content fields move and user/continuity state is retained.
func (r *CardRepo) UpsertWithContinuity(ctx context.Context, cards []Card, now time.Time) error {
	if len(cards) == 0 {
		return nil
	}
	for i := range cards {
		c := &cards[i]
		existing, err := r.GetByID(ctx, c.ID)
		if err != nil {
			return err
		}
		if existing == nil {
			if c.FirstShownDate == "" {
				c.FirstShownDate = c.Date
			}
			if c.LastMaterialAt == nil {
				t := now
				c.LastMaterialAt = &t
			}
			continue
		}
		// Preserve first-shown + user actions.
		if existing.FirstShownDate != "" {
			c.FirstShownDate = existing.FirstShownDate
		} else if c.FirstShownDate == "" {
			c.FirstShownDate = c.Date
		}
		c.Dismissed = existing.Dismissed
		c.SnoozedDate = existing.SnoozedDate
		c.Pinned = existing.Pinned
		// Bump LastMaterialAt only on a real content change.
		if existing.Title != c.Title || existing.Sub != c.Sub {
			t := now
			c.LastMaterialAt = &t
		} else if existing.LastMaterialAt != nil {
			c.LastMaterialAt = existing.LastMaterialAt
		} else {
			t := now
			c.LastMaterialAt = &t
		}
	}
	return r.Upsert(ctx, cards)
}

// DeleteStale removes any card row for the given date that wasn't part of
// the latest run (i.e. carries a different RunID). Optional sweep — without
// it, orphaned cards remain in the table but are invisible to date-scoped
// queries that filter by RunID. We currently leave them in place so a
// subsequent re-run can roll back to a prior set if needed.
//
// V2.3.0 P3: inject cards (Origin="inject") are NEVER swept by this
// function. They were produced by an out-of-band cron tick with their own
// RunID; the morning re-run must not destroy them. The UI renders them
// alongside morning cards until the next-day boundary clears them via
// the date filter on ListByDate.
func (r *CardRepo) DeleteStale(ctx context.Context, date, runID string) error {
	return r.DB.WithContext(ctx).Table(r.tableName()).
		Where("date = ? AND run_id <> ? AND origin <> 'inject'", date, runID).
		Delete(&Card{}).Error
}

// ListByDate returns visible cards for the given date: excludes dismissed
// cards, cards snoozed on this date, and ask cards whose ExpiresAt has
// passed (or that have no ExpiresAt at all, which means a legacy ask row
// from before the TTL field existed — treat as already expired). Ordered
// high→med→low by rel then by created_at as tie-breaker.
func (r *CardRepo) ListByDate(ctx context.Context, date string) ([]Card, error) {
	var out []Card
	// V2.8.1: pinned cards from any date are folded in alongside today's
	// rows. Snoozed pinned cards still respect the snooze for the day —
	// pin doesn't override snooze, just date scope.
	err := r.DB.WithContext(ctx).Table(r.tableName()).
		Where(
			"(source != 'ask' OR (source = 'ask' AND expires_at IS NOT NULL AND expires_at > ?)) "+
				"AND dismissed = false "+
				"AND (snoozed_date = '' OR snoozed_date != ?) "+
				"AND (date = ? OR pinned = true)",
			time.Now(), date, date,
		).
		Order("pinned DESC, CASE rel WHEN 'high' THEN 0 WHEN 'med' THEN 1 WHEN 'low' THEN 2 ELSE 3 END ASC, created_at ASC").
		Find(&out).Error
	if err != nil {
		return nil, err
	}
	return foldByEntityKey(out), nil
}

// foldByEntityKey collapses rows that share a non-empty EntityKey down to
// one — the V2.x cross-source dedup that stops an ask/inject card and a
// morning card about the same thread/event from both showing. Rows with an
// empty EntityKey (legacy/unanchored) are never folded. Within a group the
// winner is pinned-first, then freshest CreatedAt, then higher rel — so the
// user sees the latest take on the entity. Input order is otherwise
// preserved (the winner keeps its original slot).
func foldByEntityKey(rows []Card) []Card {
	best := make(map[string]int, len(rows)) // entity key → index of current winner in out
	out := make([]Card, 0, len(rows))
	for i := range rows {
		c := rows[i]
		if c.EntityKey == "" {
			out = append(out, c)
			continue
		}
		if j, seen := best[c.EntityKey]; seen {
			if cardBetterForFold(c, out[j]) {
				out[j] = c
			}
			continue
		}
		best[c.EntityKey] = len(out)
		out = append(out, c)
	}
	return out
}

// cardBetterForFold reports whether candidate a should replace the current
// fold winner b for the same entity key.
func cardBetterForFold(a, b Card) bool {
	if a.Pinned != b.Pinned {
		return a.Pinned
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.After(b.CreatedAt)
	}
	return relRank(a.Rel) < relRank(b.Rel)
}

func relRank(rel string) int {
	switch rel {
	case "high":
		return 0
	case "med":
		return 1
	case "low":
		return 2
	default:
		return 3
	}
}

// RecentEntity is a compact digest of an entity that surfaced recently —
// fed into the cards prompt so the synthesizer can UPDATE or DROP an entity
// it already showed instead of regenerating a near-duplicate. One row per
// distinct EntityKey (the most recent).
type RecentEntity struct {
	EntityKey      string
	Title          string
	Source         string
	Date           string
	FirstShownDate string
	LastMaterialAt *time.Time
}

// ListRecentEntities returns the most-recent row for each anchored entity
// (non-empty EntityKey) seen on or after sinceDate, excluding dismissed
// rows. Backs the "already surfaced recently" prompt block. Ordered most
// recently shown first.
func (r *CardRepo) ListRecentEntities(ctx context.Context, sinceDate string) ([]RecentEntity, error) {
	var rows []Card
	err := r.DB.WithContext(ctx).Table(r.tableName()).
		Where("entity_key != '' AND date >= ? AND dismissed = false", sinceDate).
		Order("date DESC, created_at DESC").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(rows))
	out := make([]RecentEntity, 0, len(rows))
	for i := range rows {
		c := rows[i]
		if _, dup := seen[c.EntityKey]; dup {
			continue
		}
		seen[c.EntityKey] = struct{}{}
		out = append(out, RecentEntity{
			EntityKey:      c.EntityKey,
			Title:          c.Title,
			Source:         c.Source,
			Date:           c.Date,
			FirstShownDate: c.FirstShownDate,
			LastMaterialAt: c.LastMaterialAt,
		})
	}
	return out, nil
}

// ListAllByDate returns every card row for the given date with no
// visibility filters — dismissed, snoozed, and expired ask cards all
// come back. Backs the archive view, which is intentionally a
// complete record of what ever appeared on a given day. Ordered by
// CreatedAt DESC so the most recently produced cards appear first.
func (r *CardRepo) ListAllByDate(ctx context.Context, date string) ([]Card, error) {
	var out []Card
	err := r.DB.WithContext(ctx).Table(r.tableName()).
		Where("date = ?", date).
		Order("created_at DESC").
		Find(&out).Error
	return out, err
}

// SetDismissed marks a card as permanently hidden from the morning list.
func (r *CardRepo) SetDismissed(ctx context.Context, id string) error {
	return r.DB.WithContext(ctx).Table(r.tableName()).
		Where("id = ?", id).
		Update("dismissed", true).Error
}

// SetSnoozed hides a card for the given date (format "YYYY-MM-DD").
func (r *CardRepo) SetSnoozed(ctx context.Context, id, date string) error {
	return r.DB.WithContext(ctx).Table(r.tableName()).
		Where("id = ?", id).
		Update("snoozed_date", date).Error
}

// SetPinned toggles the V2.8.1 pinned state. Pinned cards survive
// across day boundaries until unpinned.
func (r *CardRepo) SetPinned(ctx context.Context, id string, pinned bool) error {
	return r.DB.WithContext(ctx).Table(r.tableName()).
		Where("id = ?", id).
		Update("pinned", pinned).Error
}

// GetByID returns one card or nil if not found.
func (r *CardRepo) GetByID(ctx context.Context, id string) (*Card, error) {
	var c Card
	return firstOrNil(r.DB.WithContext(ctx).Table(r.tableName()).Where("id = ?", id), &c)
}

// TableName returns the physical table name for this repo.
func (r *CardRepo) TableName() string { return r.tableName() }

func (r *CardRepo) tableName() string {
	if r.Table == "" {
		return "cards"
	}
	return r.Table
}
