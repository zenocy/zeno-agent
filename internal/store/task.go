package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// Task is one row in the V2.11 unified tasks table. Replaces the V2.6
// Markdown sensor model and the V2.8.1 reminders table — a "reminder"
// is now a task with FireAt set; a plain todo has FireAt nil.
//
// Lifecycle:
//
//	insert ──→ open ──complete──→ completed
//	                 ──delete────→ soft-deleted (DeletedAt set)
//
// Alarm lifecycle (orthogonal to completion):
//
//	FireAt set ─sweeper fires─→ FiredAt set (one-shot; row stays alive)
//
// Source-of-truth: this row. There is no Markdown side, no other store.
// The user edits tasks via the UI / LLM tools / action surface, never
// by hand-editing a file.
type Task struct {
	ID    string `gorm:"primaryKey;type:text" json:"uid"`
	Title string `gorm:"type:text;not null"   json:"title"`
	Body  string `gorm:"type:text"            json:"body,omitempty"`

	Completed   bool       `gorm:"index;not null;default:false" json:"completed"`
	CompletedAt *time.Time `                                    json:"done_date,omitempty"`

	// DueDate is the user-facing date string (YYYY-MM-DD). Kept as text
	// rather than a time.Time because tasks may have a date with no time
	// (a "due Tuesday" todo) and the existing UI / projection / LLM
	// surfaces all expect the YYYY-MM-DD shape.
	DueDate string `gorm:"type:text;index" json:"due_date,omitempty"`

	Priority string         `gorm:"type:text;not null;default:'med'" json:"priority"`
	Tags     datatypes.JSON `gorm:"type:json"                        json:"-"`

	// FireAt + FiredAt model the alarm. Both nullable. The 60s sweeper
	// queries `FireAt IS NOT NULL AND FireAt <= now AND FiredAt IS NULL`
	// then sets FiredAt to bound the dispatch to once.
	FireAt  *time.Time `gorm:"index"           json:"fire_at,omitempty"`
	FiredAt *time.Time `gorm:"index"           json:"fired_at,omitempty"`

	// SourceCardID linkbacks a task created from a briefing card action
	// (e.g. "set reminder on this card"). Optional.
	SourceCardID string `gorm:"type:text;index" json:"source_card_id,omitempty"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}

// Priority constants. Default is PriorityMed; values outside this set
// are coerced to med on insert.
const (
	TaskPriorityLow  = "low"
	TaskPriorityMed  = "med"
	TaskPriorityHigh = "high"
)

// IsValidPriority reports whether s is one of the three known priorities.
func IsValidPriority(s string) bool {
	switch s {
	case TaskPriorityLow, TaskPriorityMed, TaskPriorityHigh:
		return true
	}
	return false
}

// ErrTaskNotFound is the sentinel for callers that want to disambiguate
// a missing row from a transport error.
var ErrTaskNotFound = errors.New("task not found")

// TaskRepo persists and reads Task rows. Mirrors the shape of the
// other repos in this package (CardRepo, ConcernRepo, etc.).
type TaskRepo struct {
	DB    *gorm.DB
	Table string // "tasks" or "tasks_replay"
}

// Migrate runs AutoMigrate against the configured table.
func (r *TaskRepo) Migrate() error {
	return r.DB.Table(r.tableName()).AutoMigrate(&Task{})
}

// Insert writes one task. The caller assigns the UUID via idgen.New()
// (or any unique string). CreatedAt / UpdatedAt are stamped by GORM.
func (r *TaskRepo) Insert(ctx context.Context, t Task) error {
	if !IsValidPriority(t.Priority) {
		t.Priority = TaskPriorityMed
	}
	return r.DB.WithContext(ctx).Table(r.tableName()).Create(&t).Error
}

// Get returns one task by UID, or nil if not found / soft-deleted.
func (r *TaskRepo) Get(ctx context.Context, uid string) (*Task, error) {
	var t Task
	return firstOrNil(r.DB.WithContext(ctx).Table(r.tableName()).Where("id = ?", uid), &t)
}

// TaskFilter narrows a List() call. Zero-value fields are ignored so
// callers can build queries declaratively.
type TaskFilter struct {
	// Status: "open" (default), "due_today", "overdue",
	// "completed_today", "has_alarm", "all".
	Status string

	// Tag: case-insensitive tag (no leading "#"). Empty matches any.
	Tag string

	// SourceCardID: if non-empty, only return tasks created from this
	// card. Used by the rail to show "tasks from this briefing".
	SourceCardID string

	// Today is the YYYY-MM-DD anchor for due_today / overdue /
	// completed_today filters. Zero → time.Now().Format(...).
	Today string

	// Limit caps the result; 0 → no cap.
	Limit int
}

// List returns tasks matching the filter, ordered by:
//
//  1. has-alarm-due-soon first (FireAt ascending, NULLs last)
//  2. overdue tasks
//  3. due today
//  4. due soon (date ascending)
//  5. no-date tasks
//  6. completed-today
//
// Priority breaks ties (high → med → low). Soft-deleted rows are
// excluded.
func (r *TaskRepo) List(ctx context.Context, f TaskFilter) ([]Task, error) {
	q := r.DB.WithContext(ctx).Table(r.tableName())

	today := f.Today
	if today == "" {
		today = time.Now().UTC().Format("2006-01-02")
	}

	switch strings.ToLower(strings.TrimSpace(f.Status)) {
	case "", "open":
		q = q.Where("completed = ?", false)
	case "due_today":
		q = q.Where("completed = ? AND due_date = ?", false, today)
	case "overdue":
		q = q.Where("completed = ? AND due_date != '' AND due_date < ?", false, today)
	case "completed_today":
		q = q.Where("completed = ? AND date(completed_at) = ?", true, today)
	case "has_alarm":
		q = q.Where("fire_at IS NOT NULL AND fired_at IS NULL")
	case "all":
		// no completion filter
	default:
		return nil, fmt.Errorf("unknown task status filter: %q", f.Status)
	}

	if tag := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(f.Tag), "#")); tag != "" {
		// SQLite JSON containment via LIKE on the serialized array. Tags
		// are stored lowercased so a literal substring match suffices for
		// the small tag-set sizes we expect.
		q = q.Where("LOWER(tags) LIKE ?", `%"`+tag+`"%`)
	}
	if f.SourceCardID != "" {
		q = q.Where("source_card_id = ?", f.SourceCardID)
	}

	q = q.Order(
		"CASE WHEN fire_at IS NOT NULL AND fired_at IS NULL THEN 0 " +
			"WHEN due_date != '' AND due_date < '" + today + "' THEN 1 " +
			"WHEN due_date = '" + today + "' THEN 2 " +
			"WHEN due_date != '' THEN 3 " +
			"WHEN completed = 1 THEN 5 " +
			"ELSE 4 END ASC, " +
			"fire_at ASC, due_date ASC, " +
			"CASE priority WHEN 'high' THEN 0 WHEN 'med' THEN 1 WHEN 'low' THEN 2 ELSE 3 END ASC, " +
			"created_at ASC")

	if f.Limit > 0 {
		q = q.Limit(f.Limit)
	}

	var out []Task
	if err := q.Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// Update applies the given column changes to the row identified by uid.
// The map is gorm-style (column → value); typical callers use SetFireAt
// / Complete / Delete instead. Returns ErrTaskNotFound if no row matched.
func (r *TaskRepo) Update(ctx context.Context, uid string, updates map[string]any) error {
	if len(updates) == 0 {
		return nil
	}
	res := r.DB.WithContext(ctx).Table(r.tableName()).
		Where("id = ?", uid).
		Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrTaskNotFound
	}
	return nil
}

// SetFireAt sets (or clears, when t is nil) the alarm on a task. Also
// resets FiredAt so an alarm cleared and re-set will fire again. Returns
// ErrTaskNotFound if no row matched.
func (r *TaskRepo) SetFireAt(ctx context.Context, uid string, t *time.Time) error {
	updates := map[string]any{
		"fire_at":  t,
		"fired_at": nil,
	}
	res := r.DB.WithContext(ctx).Table(r.tableName()).
		Where("id = ?", uid).
		Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrTaskNotFound
	}
	return nil
}

// Complete marks a task done at the given timestamp. Idempotent: a
// second call is a no-op (RowsAffected may be 0; that is not an error).
// Returns ErrTaskNotFound only if the row never existed.
func (r *TaskRepo) Complete(ctx context.Context, uid string, at time.Time) error {
	// Fast path: check existence first so re-completing a completed
	// task can return nil without surfacing as ErrTaskNotFound (the
	// row is there, the column is just already true).
	cur, err := r.Get(ctx, uid)
	if err != nil {
		return err
	}
	if cur == nil {
		return ErrTaskNotFound
	}
	if cur.Completed {
		return nil
	}
	return r.DB.WithContext(ctx).Table(r.tableName()).
		Where("id = ?", uid).
		Updates(map[string]any{
			"completed":    true,
			"completed_at": at,
		}).Error
}

// Delete soft-deletes the task. Returns ErrTaskNotFound if no row matched.
func (r *TaskRepo) Delete(ctx context.Context, uid string) error {
	res := r.DB.WithContext(ctx).Table(r.tableName()).
		Where("id = ?", uid).
		Delete(&Task{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrTaskNotFound
	}
	return nil
}

// DueBefore returns up to limit tasks whose alarm should fire by `at`
// (FireAt <= at AND FiredAt IS NULL), oldest-first. Used by the
// schedule.ReminderSweeper to drain due alarms each tick.
func (r *TaskRepo) DueBefore(ctx context.Context, at time.Time, limit int) ([]Task, error) {
	if limit <= 0 {
		limit = 10
	}
	var out []Task
	err := r.DB.WithContext(ctx).Table(r.tableName()).
		Where("fire_at IS NOT NULL AND fire_at <= ? AND fired_at IS NULL", at).
		Order("fire_at ASC").
		Limit(limit).
		Find(&out).Error
	return out, err
}

// MarkFired stamps FiredAt on the task. The WHERE clause is guarded on
// `fire_at IS NOT NULL AND fired_at IS NULL` so a clear-while-firing
// race resolves to "didn't fire" — the caller checks RowsAffected to
// decide whether to dispatch the alarm.
//
// Returns the rows-affected count (0 → another writer cleared the
// alarm or already fired it; do not dispatch).
func (r *TaskRepo) MarkFired(ctx context.Context, uid string, at time.Time) (int64, error) {
	res := r.DB.WithContext(ctx).Table(r.tableName()).
		Where("id = ? AND fire_at IS NOT NULL AND fired_at IS NULL", uid).
		Update("fired_at", at)
	if res.Error != nil {
		return 0, res.Error
	}
	return res.RowsAffected, nil
}

func (r *TaskRepo) tableName() string {
	if r.Table == "" {
		return "tasks"
	}
	return r.Table
}

// ParseRelative parses a relative offset like "+2h" or "+30m" or "+1d"
// into a duration. Returns 0, error on bad input. Originally lived in
// the V2.8.1 reminders package; moved here in V2.11 since both
// add_task and set_reminder use it for fire_at parsing.
func ParseRelative(s string) (time.Duration, error) {
	if len(s) < 2 || s[0] != '+' {
		return 0, fmt.Errorf("expected +<N>(m|h|d): %q", s)
	}
	body := s[1:]
	// Convert "Xd" → "24*Xh" so stdlib time.ParseDuration handles it.
	if n := len(body); n > 1 && body[n-1] == 'd' {
		var days int
		if _, err := fmt.Sscanf(body[:n-1], "%d", &days); err != nil {
			return 0, fmt.Errorf("parse days %q: %w", body, err)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(body)
}
