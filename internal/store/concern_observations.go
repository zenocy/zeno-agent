package store

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// ConcernObservation joins a concern to one event in the durable log. The
// composite primary key (concern_id, event_id) makes idempotent re-tagging
// free: retrospective tagging can re-run on the same concern and the same
// historical observations will land in the same join rows without
// duplication.
//
// Source distinguishes model-tagged rows (recognition pass, retrospective
// pass, post-tag classifier) from user-tagged rows (manual tag via the
// review surface). User rows are never overwritten by the model when the
// same (concern_id, event_id) shows up in a future pass.
//
// Soft-delete (DeletedAt) preserves untag history for audit. The recognition
// post-tag classifier sees soft-deleted rows via includeDeleted=true and
// skips re-tagging an observation a user has explicitly untagged.
type ConcernObservation struct {
	ConcernID  string         `gorm:"primaryKey;type:text"     json:"concern_id"`
	EventID    string         `gorm:"primaryKey;type:text"     json:"event_id"`
	Source     string         `gorm:"not null"                 json:"source"`
	Confidence float64        `gorm:"not null;default:0"       json:"confidence"`
	TaggedAt   time.Time      `                                json:"tagged_at"`
	DeletedAt  gorm.DeletedAt `gorm:"index"                    json:"-"`
}

// ConcernObservation tag sources.
const (
	ConcernTagSourceModel = "model"
	ConcernTagSourceUser  = "user"
)

// ConcernObservationRepo persists and reads ConcernObservation rows.
type ConcernObservationRepo struct {
	DB    *gorm.DB
	Table string // "concern_observations" or "concern_observations_replay"
}

// Migrate runs AutoMigrate and creates the per-column indexes used by
// ListByConcern (concern_id) and ListByEvent (event_id).
func (r *ConcernObservationRepo) Migrate() error {
	tbl := r.tableName()
	if err := r.DB.Table(tbl).AutoMigrate(&ConcernObservation{}); err != nil {
		return err
	}
	for _, idx := range []struct{ col, name string }{
		{"concern_id", fmt.Sprintf("idx_%s_concern", tbl)},
		{"event_id", fmt.Sprintf("idx_%s_event", tbl)},
	} {
		stmt := fmt.Sprintf(
			"CREATE INDEX IF NOT EXISTS %s ON %s (%s)",
			idx.name, tbl, idx.col,
		)
		if err := r.DB.Exec(stmt).Error; err != nil {
			return err
		}
	}
	return nil
}

// Tag inserts a (concern_id, event_id) row if absent, leaves it alone if
// present (visible). Idempotent under re-run via ON CONFLICT DO NOTHING.
//
// Soft-deleted rows are NOT resurrected by Tag — a user-untagged observation
// stays untagged unless the caller explicitly Restore()s it. Recognition's
// post-tag pass should call IsTaggedIncludingDeleted before tagging to
// honor the denylist semantics.
func (r *ConcernObservationRepo) Tag(ctx context.Context, t ConcernObservation) error {
	if t.TaggedAt.IsZero() {
		t.TaggedAt = time.Now()
	}
	return r.DB.WithContext(ctx).Table(r.tableName()).
		Clauses(onConflictDoNothing("concern_id", "event_id")).Create(&t).Error
}

// TagBatch inserts multiple tags in one round-trip with the same conflict
// semantics as Tag. Used by retrospective tagging which produces tens of
// rows per LLM call.
func (r *ConcernObservationRepo) TagBatch(ctx context.Context, tags []ConcernObservation) error {
	if len(tags) == 0 {
		return nil
	}
	now := time.Now()
	for i := range tags {
		if tags[i].TaggedAt.IsZero() {
			tags[i].TaggedAt = now
		}
	}
	return r.DB.WithContext(ctx).Table(r.tableName()).
		Clauses(onConflictDoNothing("concern_id", "event_id")).Create(&tags).Error
}

// Untag soft-deletes one (concern_id, event_id) row. The row stays in the
// table for audit; recognition's post-tag pass will skip re-tagging this
// pair via IsTaggedIncludingDeleted.
//
// We do an explicit UPDATE rather than relying on GORM's Delete + DeletedAt
// auto-soft-delete, because GORM's auto-soft-delete is unreliable on a
// composite-PK table when called via Table().Where().Delete(model). The
// explicit UPDATE makes the intent clear and eliminates the surprise.
func (r *ConcernObservationRepo) Untag(ctx context.Context, concernID, eventID string) error {
	return r.DB.WithContext(ctx).Table(r.tableName()).
		Where("concern_id = ? AND event_id = ? AND deleted_at IS NULL", concernID, eventID).
		Update("deleted_at", time.Now()).Error
}

// IsTagged reports whether (concern_id, event_id) has a visible row.
// Filters soft-deleted explicitly because Count() over .Table() doesn't
// register the gorm.DeletedAt soft-delete predicate.
func (r *ConcernObservationRepo) IsTagged(ctx context.Context, concernID, eventID string) (bool, error) {
	var n int64
	err := r.DB.WithContext(ctx).Table(r.tableName()).
		Where("concern_id = ? AND event_id = ? AND deleted_at IS NULL", concernID, eventID).
		Count(&n).Error
	return n > 0, err
}

// IsTaggedIncludingDeleted reports whether (concern_id, event_id) has any
// row, including soft-deleted ones. Used by the recognition post-tag pass
// to honor user untag decisions.
func (r *ConcernObservationRepo) IsTaggedIncludingDeleted(ctx context.Context, concernID, eventID string) (bool, error) {
	var n int64
	err := r.DB.WithContext(ctx).Table(r.tableName()).
		Unscoped().
		Where("concern_id = ? AND event_id = ?", concernID, eventID).
		Count(&n).Error
	return n > 0, err
}

// ListByConcern returns visible event IDs tagged to a concern, ordered by
// tagged_at descending. Used by the projection's RelevantToConcern path.
func (r *ConcernObservationRepo) ListByConcern(ctx context.Context, concernID string, limit int) ([]ConcernObservation, error) {
	var out []ConcernObservation
	q := r.DB.WithContext(ctx).Table(r.tableName()).
		Where("concern_id = ? AND deleted_at IS NULL", concernID).
		Order("tagged_at DESC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	err := q.Find(&out).Error
	return out, err
}

// ListByEvent returns the visible concerns tagged to one event. Used by
// cards.go to propagate concern context to a card whose source observation
// is tagged.
func (r *ConcernObservationRepo) ListByEvent(ctx context.Context, eventID string) ([]ConcernObservation, error) {
	var out []ConcernObservation
	err := r.DB.WithContext(ctx).Table(r.tableName()).
		Where("event_id = ? AND deleted_at IS NULL", eventID).
		Find(&out).Error
	return out, err
}

// CountByConcern returns the visible-tag count for a concern. Used by the
// review surface.
func (r *ConcernObservationRepo) CountByConcern(ctx context.Context, concernID string) (int64, error) {
	var n int64
	err := r.DB.WithContext(ctx).Table(r.tableName()).
		Where("concern_id = ? AND deleted_at IS NULL", concernID).
		Count(&n).Error
	return n, err
}

// LatestTaggedAt returns the most recent tagged_at for a concern, or zero
// time if none. Used by the auto-retire pass.
func (r *ConcernObservationRepo) LatestTaggedAt(ctx context.Context, concernID string) (time.Time, error) {
	var row struct{ TaggedAt time.Time }
	err := r.DB.WithContext(ctx).Table(r.tableName()).
		Select("tagged_at").
		Where("concern_id = ? AND deleted_at IS NULL", concernID).
		Order("tagged_at DESC").
		Limit(1).
		Scan(&row).Error
	return row.TaggedAt, err
}

// ReassignToConcern re-tags every visible row from sourceID to targetID,
// preserving source / confidence / tagged_at. Used by Merge: source's
// observations move to target. Idempotent — if (target, event_id) already
// exists, the source's row is just dropped (target's prior row, including
// any user-source override, wins).
//
// Implementation: select source rows, insert as target rows (DO NOTHING on
// conflict), soft-delete source rows via explicit UPDATE. Single transaction
// so the user never sees a partial state.
func (r *ConcernObservationRepo) ReassignToConcern(ctx context.Context, sourceID, targetID string) (int64, error) {
	if sourceID == targetID {
		return 0, nil
	}
	var moved int64
	err := r.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows []ConcernObservation
		if err := tx.Table(r.tableName()).
			Where("concern_id = ? AND deleted_at IS NULL", sourceID).
			Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		retagged := make([]ConcernObservation, len(rows))
		for i, row := range rows {
			retagged[i] = ConcernObservation{
				ConcernID:  targetID,
				EventID:    row.EventID,
				Source:     row.Source,
				Confidence: row.Confidence,
				TaggedAt:   row.TaggedAt,
			}
		}
		if err := tx.Table(r.tableName()).
			Clauses(onConflictDoNothing("concern_id", "event_id")).Create(&retagged).Error; err != nil {
			return err
		}
		// Explicit soft-delete UPDATE — see Untag for the rationale.
		if err := tx.Table(r.tableName()).
			Where("concern_id = ? AND deleted_at IS NULL", sourceID).
			Update("deleted_at", time.Now()).Error; err != nil {
			return err
		}
		moved = int64(len(rows))
		return nil
	})
	return moved, err
}

// PartitionToConcerns moves observations from sourceID to multiple target
// concerns according to assignment[event_id] = target_concern_id. Any event
// not present in the assignment stays tagged to source. Returns the number
// of rows moved per target concern. Used by Split.
func (r *ConcernObservationRepo) PartitionToConcerns(ctx context.Context, sourceID string, assignment map[string]string) (map[string]int64, error) {
	moved := map[string]int64{}
	if sourceID == "" || len(assignment) == 0 {
		return moved, nil
	}
	err := r.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows []ConcernObservation
		if err := tx.Table(r.tableName()).
			Where("concern_id = ? AND deleted_at IS NULL", sourceID).
			Find(&rows).Error; err != nil {
			return err
		}
		// Group reassignments by target.
		byTarget := map[string][]ConcernObservation{}
		var toDrop []string
		for _, row := range rows {
			tgt, ok := assignment[row.EventID]
			if !ok || tgt == "" || tgt == sourceID {
				continue
			}
			byTarget[tgt] = append(byTarget[tgt], ConcernObservation{
				ConcernID:  tgt,
				EventID:    row.EventID,
				Source:     row.Source,
				Confidence: row.Confidence,
				TaggedAt:   row.TaggedAt,
			})
			toDrop = append(toDrop, row.EventID)
		}
		for tgt, retagged := range byTarget {
			if err := tx.Table(r.tableName()).
				Clauses(onConflictDoNothing("concern_id", "event_id")).Create(&retagged).Error; err != nil {
				return err
			}
			moved[tgt] = int64(len(retagged))
		}
		if len(toDrop) > 0 {
			if err := tx.Table(r.tableName()).
				Where("concern_id = ? AND event_id IN ? AND deleted_at IS NULL", sourceID, toDrop).
				Update("deleted_at", time.Now()).Error; err != nil {
				return err
			}
		}
		return nil
	})
	return moved, err
}

// TableName returns the physical table name for this repo.
func (r *ConcernObservationRepo) TableName() string { return r.tableName() }

func (r *ConcernObservationRepo) tableName() string {
	if r.Table == "" {
		return "concern_observations"
	}
	return r.Table
}
