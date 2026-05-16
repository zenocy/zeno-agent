package log

import (
	"context"
	"fmt"
	"time"

	"github.com/zenocy/zeno-v2/internal/idgen"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// Reader reads from the observation log.
type Reader interface {
	Since(ctx context.Context, t time.Time) ([]Event, error)
	ByKind(ctx context.Context, kinds ...string) ([]Event, error)
	Latest(ctx context.Context, kind string) (*Event, error)
}

// Writer appends to the observation log.
type Writer interface {
	Append(ctx context.Context, kind, source string, payload any) (Event, error)
}

// Store combines Reader and Writer.
type Store interface {
	Reader
	Writer
}

// gormStore is the GORM-backed implementation.
type gormStore struct {
	db *gorm.DB
}

// Open opens (and creates if needed) the SQLite database, runs migrations,
// and returns a ready-to-use Store. WAL is enabled and busy_timeout is set
// to 5s. Pass nil for cfg to use GORM defaults.
func Open(path string) (*gorm.DB, Store, error) {
	return OpenWith(path, nil)
}

// OpenWith is like Open but lets the caller supply a custom *gorm.Config —
// used by cmd/zeno/main.go to wire the slow-query logger.
func OpenWith(path string, cfg *gorm.Config) (*gorm.DB, Store, error) {
	dsn := path + "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on"
	if cfg == nil {
		cfg = &gorm.Config{}
	}
	db, err := gorm.Open(sqlite.Open(dsn), cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	if err := db.AutoMigrate(&Event{}); err != nil {
		return nil, nil, fmt.Errorf("migrate events: %w", err)
	}
	return db, &gormStore{db: db}, nil
}

// Append writes a new event. The payload is marshaled to JSON.
func (s *gormStore) Append(ctx context.Context, kind, source string, payload any) (Event, error) {
	js, err := marshalJSON(payload)
	if err != nil {
		return Event{}, fmt.Errorf("marshal payload: %w", err)
	}
	e := Event{
		ID:      idgen.New(),
		TS:      time.Now().UTC(),
		Kind:    kind,
		Source:  source,
		Payload: js,
	}
	if err := s.db.WithContext(ctx).Create(&e).Error; err != nil {
		return Event{}, fmt.Errorf("create event: %w", err)
	}
	return e, nil
}

// Since returns all events at or after t, oldest first.
func (s *gormStore) Since(ctx context.Context, t time.Time) ([]Event, error) {
	var out []Event
	err := s.db.WithContext(ctx).
		Where("ts >= ?", t.UTC()).
		Order("ts ASC").
		Find(&out).Error
	return out, err
}

// ByKind returns all events whose kind matches one of the given kinds, oldest
// first. Pass no arguments to get everything.
func (s *gormStore) ByKind(ctx context.Context, kinds ...string) ([]Event, error) {
	var out []Event
	tx := s.db.WithContext(ctx).Order("ts ASC")
	if len(kinds) > 0 {
		tx = tx.Where("kind IN ?", kinds)
	}
	err := tx.Find(&out).Error
	return out, err
}

// Latest returns the most recent event of the given kind, or nil if none.
func (s *gormStore) Latest(ctx context.Context, kind string) (*Event, error) {
	var e Event
	err := s.db.WithContext(ctx).
		Where("kind = ?", kind).
		Order("ts DESC").
		Limit(1).
		First(&e).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func marshalJSON(v any) (datatypes.JSON, error) {
	if v == nil {
		return datatypes.JSON([]byte("null")), nil
	}
	b, err := jsonMarshal(v)
	if err != nil {
		return nil, err
	}
	return datatypes.JSON(b), nil
}
