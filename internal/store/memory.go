package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// MemoryFact is one durable, human-readable line of "what Zeno thinks it knows
// about you" — `partner: Sam`, `runs Tue/Thu mornings`. Synth derives them via
// the `remember:` sentinel; the user can also add, edit, or soft-delete them
// from the UI. Soft-deleted rows act as a denylist so user-removed facts don't
// re-emerge from a future synth run.
//
// Subject is the consolidator's dedupe key: lowercased and trimmed by the
// consolidator before insert/lookup. The full Fact text stays free-form for
// human readability.
type MemoryFact struct {
	ID             string         `gorm:"primaryKey;type:text"       json:"id"`
	Subject        string         `gorm:"index;not null"             json:"subject"`
	Fact           string         `gorm:"not null"                   json:"fact"`
	Category       string         `gorm:"index;not null"             json:"category"`
	Confidence     string         `gorm:"index;not null;default:low" json:"confidence"`
	Source         string         `gorm:"not null;default:synth"     json:"source"`
	EvidenceCount  int            `gorm:"not null;default:1"         json:"evidence_count"`
	FirstSeenRunID string         `gorm:"type:text"                  json:"first_seen_run_id,omitempty"`
	LastSeenRunID  string         `gorm:"type:text"                  json:"last_seen_run_id,omitempty"`
	FirstSeen      time.Time      `                                  json:"first_seen"`
	LastReinforced time.Time      `                                  json:"last_reinforced"`
	CreatedAt      time.Time      `                                  json:"-"`
	UpdatedAt      time.Time      `                                  json:"updated_at"`
	DeletedAt      gorm.DeletedAt `gorm:"index"                      json:"-"`
}

// MemoryRepo persists and reads MemoryFact rows. The Table field controls
// which physical table is used so the same code path serves prod and replay.
type MemoryRepo struct {
	DB    *gorm.DB
	Table string // "memory_facts" or "memory_facts_replay"
}

// Migrate runs AutoMigrate against the configured table and creates the
// (subject, confidence, last_reinforced) compound index used by ListTop and
// the consolidator's GetBySubject lookup.
func (r *MemoryRepo) Migrate() error {
	tbl := r.tableName()
	if err := r.DB.Table(tbl).AutoMigrate(&MemoryFact{}); err != nil {
		return err
	}
	idx := fmt.Sprintf("idx_%s_subject_conf", tbl)
	stmt := fmt.Sprintf(
		"CREATE INDEX IF NOT EXISTS %s ON %s (subject, confidence, last_reinforced)",
		idx, tbl,
	)
	return r.DB.Exec(stmt).Error
}

// Upsert inserts or replaces facts keyed by ID. The consolidator (Phase 2)
// uses this for batch writes — re-running on the same set of IDs overwrites
// in place.
func (r *MemoryRepo) Upsert(ctx context.Context, items []MemoryFact) error {
	if len(items) == 0 {
		return nil
	}
	return r.DB.WithContext(ctx).Table(r.tableName()).
		Clauses(onConflictUpdateAll("id")).Create(&items).Error
}

// Insert writes one fact. Used by the manual-add API (Phase 3) where the
// caller wants an explicit error if the ID already exists rather than silent
// replacement.
func (r *MemoryRepo) Insert(ctx context.Context, m MemoryFact) error {
	return r.DB.WithContext(ctx).Table(r.tableName()).Create(&m).Error
}

// GetByID returns one fact or nil if not found. Soft-deleted rows are hidden.
func (r *MemoryRepo) GetByID(ctx context.Context, id string) (*MemoryFact, error) {
	var m MemoryFact
	return firstOrNil(r.DB.WithContext(ctx).Table(r.tableName()).Where("id = ?", id), &m)
}

// GetBySubject returns the fact with the given subject (case-insensitive).
// When includeDeleted is true the lookup runs Unscoped so the consolidator
// can see denylisted subjects and skip re-inserting them. Returns nil if no
// row matches.
func (r *MemoryRepo) GetBySubject(ctx context.Context, subject string, includeDeleted bool) (*MemoryFact, error) {
	q := r.DB.WithContext(ctx).Table(r.tableName())
	if includeDeleted {
		q = q.Unscoped()
	}
	var m MemoryFact
	return firstOrNil(q.Where("LOWER(subject) = ?", lowerTrim(subject)), &m)
}

// ListTop returns up to limit visible facts ordered by confidence
// (high→med→low) then by recency of reinforcement. Used by the projection
// (Phase 1) to seed the cards/reactive prompts.
func (r *MemoryRepo) ListTop(ctx context.Context, limit int) ([]MemoryFact, error) {
	var out []MemoryFact
	err := r.DB.WithContext(ctx).Table(r.tableName()).
		Order("CASE confidence WHEN 'high' THEN 0 WHEN 'med' THEN 1 WHEN 'low' THEN 2 ELSE 3 END ASC, last_reinforced DESC").
		Limit(limit).
		Find(&out).Error
	return out, err
}

// ListAllVisible returns every non-soft-deleted fact ordered by ID asc.
// Used by the embedding-index warmup walker to diff the live fact set
// against the persistent vector cache. Bounded by the V2.2 50-fact total
// cap, so a full table scan is cheap.
func (r *MemoryRepo) ListAllVisible(ctx context.Context) ([]MemoryFact, error) {
	var out []MemoryFact
	err := r.DB.WithContext(ctx).Table(r.tableName()).
		Order("id ASC").
		Find(&out).Error
	return out, err
}

// UpdateFact replaces the human-readable text on an existing fact. Used by
// the manual-edit API (Phase 3).
func (r *MemoryRepo) UpdateFact(ctx context.Context, id, fact string) error {
	return r.DB.WithContext(ctx).Table(r.tableName()).
		Where("id = ?", id).
		Update("fact", fact).Error
}

// UpdateCategory changes the category tag on an existing fact. Phase 3's
// manual-edit UI exposes both fact and category via the same PATCH; the
// handler calls these in turn so partial-failure semantics surface cleanly
// per field.
func (r *MemoryRepo) UpdateCategory(ctx context.Context, id, category string) error {
	return r.DB.WithContext(ctx).Table(r.tableName()).
		Where("id = ?", id).
		Update("category", category).Error
}

// SoftDelete marks a fact as deleted. The row stays in the table (the
// gorm.DeletedAt column is set) so the consolidator can read it back via
// GetBySubject(includeDeleted=true) and avoid re-emerging the same subject
// from synth-derived candidates.
func (r *MemoryRepo) SoftDelete(ctx context.Context, id string) error {
	return r.DB.WithContext(ctx).Table(r.tableName()).
		Where("id = ?", id).
		Delete(&MemoryFact{}).Error
}

// IncrementEvidence bumps EvidenceCount and updates LastSeenRunID /
// LastReinforced for one fact. Idempotent under same-day re-runs: if the
// fact's last_seen_run_id already equals runID the call is a no-op. This is
// the load-bearing mechanism that lets morning synth re-run on the same day
// (manual trigger, container restart) without double-counting evidence.
func (r *MemoryRepo) IncrementEvidence(ctx context.Context, id, runID string) error {
	now := time.Now()
	return r.DB.WithContext(ctx).Table(r.tableName()).
		Where("id = ? AND (last_seen_run_id IS NULL OR last_seen_run_id <> ?)", id, runID).
		Updates(map[string]any{
			"evidence_count":   gorm.Expr("evidence_count + 1"),
			"last_seen_run_id": runID,
			"last_reinforced":  now,
		}).Error
}

// PromoteConfidence sets the confidence tier on one fact. The consolidator
// (Phase 2) calls this when EvidenceCount crosses the 3× / 7× thresholds.
func (r *MemoryRepo) PromoteConfidence(ctx context.Context, id, conf string) error {
	return r.DB.WithContext(ctx).Table(r.tableName()).
		Where("id = ?", id).
		Update("confidence", conf).Error
}

// EvictExcess soft-deletes enough rows to bring the visible fact count down
// to capN, picking the worst non-user rows first: low confidence before high,
// stalest-by-last_reinforced before fresh. Source=user rows are exempt — they
// were typed in by the user and only user-delete can remove them. Returns
// the deleted IDs so the consolidator can include them in the
// memory.consolidated event.
func (r *MemoryRepo) EvictExcess(ctx context.Context, capN int) ([]string, error) {
	var total int64
	if err := r.DB.WithContext(ctx).Table(r.tableName()).Count(&total).Error; err != nil {
		return nil, err
	}
	excess := int(total) - capN
	if excess <= 0 {
		return nil, nil
	}
	type idRow struct{ ID string }
	var victims []idRow
	if err := r.DB.WithContext(ctx).Table(r.tableName()).
		Select("id").
		Where("source <> ?", "user").
		Order("CASE confidence WHEN 'low' THEN 0 WHEN 'med' THEN 1 WHEN 'high' THEN 2 ELSE 3 END ASC, last_reinforced ASC").
		Limit(excess).
		Find(&victims).Error; err != nil {
		return nil, err
	}
	if len(victims) == 0 {
		return nil, nil
	}
	deleted := make([]string, 0, len(victims))
	for _, v := range victims {
		deleted = append(deleted, v.ID)
	}
	if err := r.DB.WithContext(ctx).Table(r.tableName()).
		Where("id IN ?", deleted).
		Delete(&MemoryFact{}).Error; err != nil {
		return nil, err
	}
	return deleted, nil
}

// TableName returns the physical table name for this repo.
func (r *MemoryRepo) TableName() string { return r.tableName() }

func (r *MemoryRepo) tableName() string {
	if r.Table == "" {
		return "memory_facts"
	}
	return r.Table
}

// lowerTrim normalizes a subject for case-insensitive lookup. Mirrors the
// constraint enforced by the cards prompt convention: `remember: <subject>:
// <predicate>` with subject lowercased and trimmed. Uses Unicode-aware
// folding so accented subjects round-trip predictably.
func lowerTrim(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
