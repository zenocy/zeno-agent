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
	return r.DB.Exec(stmt).Error
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
	return out, err
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
