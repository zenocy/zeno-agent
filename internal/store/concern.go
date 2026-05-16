package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// Concern is one named, long-running situation Zeno tracks across emails,
// calendar events, and (later) other sensors. *Construction at the house.*
// *Frankfurt trip in summer.* *Hiring search for the engineering lead.*
//
// Concerns are not projects. The naming is load-bearing: nothing in this
// codebase, schema, or UI uses "project," "kanban," "milestone," or // allow-pm-language
// "deliverable." A concern with a status badge or completion percentage // allow-pm-language
// is broken.
//
// Lifecycle:
//
//	proposed ─approve─→ active ─pause─→ paused ─resume─→ active
//	         ─dismiss─→ (soft-delete + 90-day denylist on norm_name)
//	         (active|paused) ─end──→ ended    (terminal at user level)
//	         (active|paused) ─merge─→ merged  (terminal; merged_into_id set)
//
// Split is *terminal-as-ended on source* + new concerns at active with
// split_from_id pointing back; the source row keeps its provenance for
// audit but contributes nothing to current surfaces.
type Concern struct {
	ID             string         `gorm:"primaryKey;type:text"              json:"id"`
	Name           string         `gorm:"type:text;not null"                json:"name"`
	NormName       string         `gorm:"index;not null"                    json:"-"`
	Description    string         `gorm:"type:text;not null"                json:"description"`
	State          string         `gorm:"index;not null;default:'proposed'" json:"state"`
	Source         string         `gorm:"not null"                          json:"source"`
	MergedIntoID   *string        `gorm:"type:text;index"                   json:"merged_into_id,omitempty"`
	SplitFromID    *string        `gorm:"type:text;index"                   json:"split_from_id,omitempty"`
	Confidence     float64        `gorm:"not null;default:0"                json:"confidence"`
	LastActiveAt   time.Time      `                                         json:"last_active_at"`
	EndedAt        *time.Time     `                                         json:"ended_at,omitempty"`
	FirstSeenRunID string         `gorm:"type:text"                         json:"first_seen_run_id,omitempty"`
	CreatedAt      time.Time      `                                         json:"created_at"`
	UpdatedAt      time.Time      `                                         json:"updated_at"`
	DeletedAt      gorm.DeletedAt `gorm:"index"                             json:"-"`
}

// Concern lifecycle states.
const (
	ConcernStateProposed = "proposed"
	ConcernStateActive   = "active"
	ConcernStatePaused   = "paused"
	ConcernStateEnded    = "ended"
	ConcernStateMerged   = "merged"
)

// Concern source values. "model" = recognition pass; "user" = declared via
// /synth.Ask or POSTed manually.
const (
	ConcernSourceModel = "model"
	ConcernSourceUser  = "user"
)

// validConcernTransitions enforces the lifecycle. Any transition not listed
// here is rejected by Transition(). Re-open from ended (ended → active) is
// exposed as a separate method (Reopen) so the back-door is explicit at the
// call site.
var validConcernTransitions = map[string]map[string]bool{
	ConcernStateProposed: {ConcernStateActive: true, ConcernStateEnded: true},
	ConcernStateActive:   {ConcernStatePaused: true, ConcernStateEnded: true, ConcernStateMerged: true},
	ConcernStatePaused:   {ConcernStateActive: true, ConcernStateEnded: true, ConcernStateMerged: true},
	ConcernStateEnded:    {},
	ConcernStateMerged:   {},
}

// IsValidConcernState reports whether s is one of the five known states.
func IsValidConcernState(s string) bool {
	switch s {
	case ConcernStateProposed, ConcernStateActive, ConcernStatePaused, ConcernStateEnded, ConcernStateMerged:
		return true
	}
	return false
}

// ErrInvalidConcernTransition is returned by Transition when the requested
// move is not in the lifecycle table.
var ErrInvalidConcernTransition = errors.New("invalid concern state transition")

// ErrConcernNotFound is a sentinel for callers that want to disambiguate
// a missing row from a transport error.
var ErrConcernNotFound = errors.New("concern not found")

// ConcernRepo persists and reads Concern rows. The Table field controls
// which physical table is used so the same code path serves prod and replay.
type ConcernRepo struct {
	DB    *gorm.DB
	Table string // "concerns" or "concerns_replay"
}

// Migrate runs AutoMigrate against the configured table and creates the
// (norm_name, state) compound index used by the recognition idempotency
// check (GetByNormName) and the projection's ListByState path.
func (r *ConcernRepo) Migrate() error {
	tbl := r.tableName()
	if err := r.DB.Table(tbl).AutoMigrate(&Concern{}); err != nil {
		return err
	}
	idx := fmt.Sprintf("idx_%s_norm_state", tbl)
	stmt := fmt.Sprintf(
		"CREATE INDEX IF NOT EXISTS %s ON %s (norm_name, state)",
		idx, tbl,
	)
	return r.DB.Exec(stmt).Error
}

// Insert writes one concern. The caller is responsible for assigning a
// unique ID (UUID via uuid.New()) and a normalized name (NormalizeConcernName).
func (r *ConcernRepo) Insert(ctx context.Context, c Concern) error {
	return r.DB.WithContext(ctx).Table(r.tableName()).Create(&c).Error
}

// GetByID returns one concern or nil if not found. Soft-deleted rows are
// hidden — recognition uses GetByNormName(includeDeleted=true) to see the
// denylist.
func (r *ConcernRepo) GetByID(ctx context.Context, id string) (*Concern, error) {
	var c Concern
	return firstOrNil(r.DB.WithContext(ctx).Table(r.tableName()).Where("id = ?", id), &c)
}

// GetByNormName returns the concern whose normalized name matches.
// includeDeleted=true uses Unscoped so recognition can see soft-deleted
// rows and apply the 90-day denylist.
func (r *ConcernRepo) GetByNormName(ctx context.Context, norm string, includeDeleted bool) (*Concern, error) {
	q := r.DB.WithContext(ctx).Table(r.tableName())
	if includeDeleted {
		q = q.Unscoped()
	}
	var c Concern
	return firstOrNil(q.Where("norm_name = ?", norm), &c)
}

// ListByState returns visible concerns in the given state, ordered by
// last_active_at descending.
func (r *ConcernRepo) ListByState(ctx context.Context, state string) ([]Concern, error) {
	var out []Concern
	err := r.DB.WithContext(ctx).Table(r.tableName()).
		Where("state = ?", state).
		Order("last_active_at DESC").
		Find(&out).Error
	return out, err
}

// ListActive is a thin alias for ListByState(active).
func (r *ConcernRepo) ListActive(ctx context.Context) ([]Concern, error) {
	return r.ListByState(ctx, ConcernStateActive)
}

// ListAll returns every visible concern (excluding soft-deleted), ordered
// by state group (proposed → active → paused → ended → merged) then by
// last_active_at descending. Used by the review surface.
func (r *ConcernRepo) ListAll(ctx context.Context) ([]Concern, error) {
	var out []Concern
	err := r.DB.WithContext(ctx).Table(r.tableName()).
		Order("CASE state WHEN 'proposed' THEN 0 WHEN 'active' THEN 1 WHEN 'paused' THEN 2 WHEN 'ended' THEN 3 WHEN 'merged' THEN 4 ELSE 5 END ASC, last_active_at DESC").
		Find(&out).Error
	return out, err
}

// ListInactiveSince returns active concerns whose last_active_at is older
// than `since`. Used by the auto-retire pass in Phase 5.
func (r *ConcernRepo) ListInactiveSince(ctx context.Context, since time.Time) ([]Concern, error) {
	var out []Concern
	err := r.DB.WithContext(ctx).Table(r.tableName()).
		Where("state = ? AND last_active_at < ?", ConcernStateActive, since).
		Order("last_active_at ASC").
		Find(&out).Error
	return out, err
}

// UpdateName changes the human-readable name and re-derives the normalized
// form. Returns the updated concern.
func (r *ConcernRepo) UpdateName(ctx context.Context, id, name string) error {
	norm := NormalizeConcernName(name)
	return r.DB.WithContext(ctx).Table(r.tableName()).
		Where("id = ?", id).
		Updates(map[string]any{
			"name":      name,
			"norm_name": norm,
		}).Error
}

// UpdateDescription replaces the Zeno-voiced prose on a concern.
func (r *ConcernRepo) UpdateDescription(ctx context.Context, id, desc string) error {
	return r.DB.WithContext(ctx).Table(r.tableName()).
		Where("id = ?", id).
		Update("description", desc).Error
}

// Transition moves a concern from its current state to `to`. The transition
// must appear in validConcernTransitions; otherwise ErrInvalidConcernTransition
// is returned and the row is unchanged. Concurrent writers are serialized via
// the WHERE-on-state guard so a stale transition fails noisily rather than
// silently overwriting.
//
// Side effects per transition:
//   - any → ended:  EndedAt = now()
//   - any → merged: handled by Merge (transition + merged_into_id atomically);
//     callers should use Merge, not Transition.
//   - paused → active or proposed → active: bumps LastActiveAt = now()
func (r *ConcernRepo) Transition(ctx context.Context, id, to string) error {
	if !IsValidConcernState(to) {
		return fmt.Errorf("%w: target %q is not a known state", ErrInvalidConcernTransition, to)
	}
	cur, err := r.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if cur == nil {
		return ErrConcernNotFound
	}
	allowed, ok := validConcernTransitions[cur.State]
	if !ok || !allowed[to] {
		return fmt.Errorf("%w: %s → %s", ErrInvalidConcernTransition, cur.State, to)
	}
	now := time.Now()
	updates := map[string]any{"state": to}
	switch to {
	case ConcernStateEnded:
		updates["ended_at"] = now
	case ConcernStateActive:
		updates["last_active_at"] = now
	}
	res := r.DB.WithContext(ctx).Table(r.tableName()).
		Where("id = ? AND state = ?", id, cur.State).
		Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		// Concurrent writer beat us; surface as an invalid-transition so the
		// caller can re-fetch and retry.
		return fmt.Errorf("%w: concurrent state change on %s", ErrInvalidConcernTransition, id)
	}
	return nil
}

// Reopen takes an ended concern back to active. Back-door for recovery from
// accidental ends; not exposed in the V2.5 UI. Resets EndedAt and bumps
// LastActiveAt.
func (r *ConcernRepo) Reopen(ctx context.Context, id string) error {
	now := time.Now()
	res := r.DB.WithContext(ctx).Table(r.tableName()).
		Where("id = ? AND state = ?", id, ConcernStateEnded).
		Updates(map[string]any{
			"state":          ConcernStateActive,
			"ended_at":       nil,
			"last_active_at": now,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrConcernNotFound
	}
	return nil
}

// Merge moves source to state=merged with merged_into_id=targetID. The caller
// is responsible for re-tagging observations via the ConcernObservation repo.
// Both rows must exist; the source must be in active or paused; the target
// must not itself be merged or ended.
func (r *ConcernRepo) Merge(ctx context.Context, sourceID, targetID string) error {
	if sourceID == targetID {
		return errors.New("cannot merge a concern into itself")
	}
	src, err := r.GetByID(ctx, sourceID)
	if err != nil {
		return err
	}
	if src == nil {
		return fmt.Errorf("%w: source %s", ErrConcernNotFound, sourceID)
	}
	allowed, ok := validConcernTransitions[src.State]
	if !ok || !allowed[ConcernStateMerged] {
		return fmt.Errorf("%w: %s → merged", ErrInvalidConcernTransition, src.State)
	}
	tgt, err := r.GetByID(ctx, targetID)
	if err != nil {
		return err
	}
	if tgt == nil {
		return fmt.Errorf("%w: target %s", ErrConcernNotFound, targetID)
	}
	if tgt.State == ConcernStateMerged || tgt.State == ConcernStateEnded {
		return fmt.Errorf("%w: target is %s", ErrInvalidConcernTransition, tgt.State)
	}
	now := time.Now()
	return r.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Table(r.tableName()).
			Where("id = ? AND state = ?", sourceID, src.State).
			Updates(map[string]any{
				"state":          ConcernStateMerged,
				"merged_into_id": targetID,
				"ended_at":       now,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("%w: concurrent state change on %s", ErrInvalidConcernTransition, sourceID)
		}
		return tx.Table(r.tableName()).
			Where("id = ?", targetID).
			Update("last_active_at", now).Error
	})
}

// BumpLastActive sets last_active_at = t. Called by the tag repo after
// successful tagging so the recognition / retire passes have an accurate
// activity signal.
func (r *ConcernRepo) BumpLastActive(ctx context.Context, id string, t time.Time) error {
	return r.DB.WithContext(ctx).Table(r.tableName()).
		Where("id = ?", id).
		Update("last_active_at", t).Error
}

// SoftDelete marks a proposed concern as dismissed. Recognition checks the
// soft-deleted row via GetByNormName(includeDeleted=true) and skips
// re-proposing the same norm_name within the denylist window. The row stays
// in the table for audit (gorm.DeletedAt).
func (r *ConcernRepo) SoftDelete(ctx context.Context, id string) error {
	return r.DB.WithContext(ctx).Table(r.tableName()).
		Where("id = ?", id).
		Delete(&Concern{}).Error
}

// UpsertReplay supports the replay tooling: insert or replace by ID without
// invoking the lifecycle guards. Mirror of MemoryRepo.Upsert.
func (r *ConcernRepo) UpsertReplay(ctx context.Context, items []Concern) error {
	if len(items) == 0 {
		return nil
	}
	return r.DB.WithContext(ctx).Table(r.tableName()).
		Clauses(onConflictUpdateAll("id")).Create(&items).Error
}

// TableName returns the physical table name for this repo.
func (r *ConcernRepo) TableName() string { return r.tableName() }

func (r *ConcernRepo) tableName() string {
	if r.Table == "" {
		return "concerns"
	}
	return r.Table
}

// NormalizeConcernName lowercases, trims, and collapses internal whitespace
// for stable dedup keys. Recognition uses this to detect "frankfurt trip"
// already exists before proposing it again.
func NormalizeConcernName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.Join(strings.Fields(s), " ")
}
