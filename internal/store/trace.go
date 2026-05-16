package store

import (
	"context"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// Trace is the full record of one synth run's tool loop. Never overwritten —
// replays produce new rows so we can compare runs.
type Trace struct {
	ID        string         `gorm:"primaryKey;type:text" json:"trace_id"`
	RunID     string         `gorm:"type:text;index"      json:"run_id"`
	Date      string         `gorm:"type:text;index"      json:"date"`
	Stopped   string         `gorm:"type:text"            json:"stopped"`
	TotalMs   int64          `                            json:"total_ms"`
	Steps     datatypes.JSON `gorm:"type:text"            json:"steps"`
	CreatedAt time.Time      `                            json:"-"`
}

// TraceRepo persists and reads Trace rows.
type TraceRepo struct {
	DB    *gorm.DB
	Table string // "traces" or "traces_replay"
}

// Migrate runs AutoMigrate against the configured table.
func (r *TraceRepo) Migrate() error {
	return r.DB.Table(r.tableName()).AutoMigrate(&Trace{})
}

// Create inserts a new trace row.
func (r *TraceRepo) Create(ctx context.Context, t Trace) error {
	return r.DB.WithContext(ctx).Table(r.tableName()).Create(&t).Error
}

// Get returns the trace by ID, or nil if not found.
func (r *TraceRepo) Get(ctx context.Context, id string) (*Trace, error) {
	var t Trace
	return firstOrNil(r.DB.WithContext(ctx).Table(r.tableName()).Where("id = ?", id), &t)
}

// TableName returns the physical table name for this repo.
func (r *TraceRepo) TableName() string { return r.tableName() }

func (r *TraceRepo) tableName() string {
	if r.Table == "" {
		return "traces"
	}
	return r.Table
}
