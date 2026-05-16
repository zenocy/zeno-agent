package embeddings

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MemoryEmbedding is the GORM row mirroring memory_facts.id. The
// (id, content_hash, model_id) triple is the cache key: a content change
// flips the hash, a model swap flips model_id, and either invalidates the
// stored vector. dims is denormalised so DecodeVector can validate without
// loading the embedder.
type MemoryEmbedding struct {
	ID          string    `gorm:"primaryKey;type:text" json:"id"`
	ContentHash string    `gorm:"index;not null"       json:"content_hash"`
	ModelID     string    `gorm:"index;not null"       json:"model_id"`
	Dims        int       `gorm:"not null"             json:"dims"`
	Vector      []byte    `gorm:"type:blob;not null"   json:"-"`
	UpdatedAt   time.Time `                            json:"updated_at"`
}

// Store persists MemoryEmbedding rows for one logical scope (prod or replay).
// Table controls the physical table name so the same code path serves both.
type Store struct {
	DB    *gorm.DB
	Table string
}

func (s *Store) tableName() string {
	if s.Table == "" {
		return "memory_embeddings"
	}
	return s.Table
}

// Migrate runs AutoMigrate against the configured table and creates the
// model_id index used by warmup's stale-model eviction.
func (s *Store) Migrate() error {
	if s == nil || s.DB == nil {
		return errors.New("embeddings: Store.Migrate on nil store")
	}
	tbl := s.tableName()
	if err := s.DB.Table(tbl).AutoMigrate(&MemoryEmbedding{}); err != nil {
		return err
	}
	idx := fmt.Sprintf("idx_%s_model", tbl)
	stmt := fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s (model_id)", idx, tbl)
	return s.DB.Exec(stmt).Error
}

// Load returns every cached row whose model_id matches the supplied id.
// Rows for other models are intentionally skipped — DeleteByModelID is the
// caller's job and lets warmup count evictions separately from loads.
func (s *Store) Load(ctx context.Context, modelID string) ([]MemoryEmbedding, error) {
	if s == nil || s.DB == nil {
		return nil, errors.New("embeddings: Store.Load on nil store")
	}
	var out []MemoryEmbedding
	err := s.DB.WithContext(ctx).Table(s.tableName()).
		Where("model_id = ?", modelID).
		Find(&out).Error
	return out, err
}

// Upsert inserts or replaces one row keyed by id. UpdatedAt is set by the
// caller; we don't override it so tests can set deterministic timestamps.
func (s *Store) Upsert(ctx context.Context, row MemoryEmbedding) error {
	if s == nil || s.DB == nil {
		return errors.New("embeddings: Store.Upsert on nil store")
	}
	if row.ID == "" {
		return errors.New("embeddings: Store.Upsert with empty id")
	}
	if row.UpdatedAt.IsZero() {
		row.UpdatedAt = time.Now().UTC()
	}
	return s.DB.WithContext(ctx).Table(s.tableName()).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		UpdateAll: true,
	}).Create(&row).Error
}

// Delete removes one row by id. Idempotent.
func (s *Store) Delete(ctx context.Context, id string) error {
	if s == nil || s.DB == nil {
		return nil
	}
	return s.DB.WithContext(ctx).Table(s.tableName()).
		Where("id = ?", id).
		Delete(&MemoryEmbedding{}).Error
}

// DeleteByModelID removes every row whose model_id is *not* the supplied
// id. Used by warmup on startup so a new embedder model evicts stale
// vectors before repopulation. Returns the number of rows deleted.
func (s *Store) DeleteByModelID(ctx context.Context, keepModelID string) (int64, error) {
	if s == nil || s.DB == nil {
		return 0, nil
	}
	res := s.DB.WithContext(ctx).Table(s.tableName()).
		Where("model_id <> ?", keepModelID).
		Delete(&MemoryEmbedding{})
	return res.RowsAffected, res.Error
}

// EncodeVector packs a float32 slice into a little-endian byte slice.
// Returns an error if vec is empty.
func EncodeVector(vec []float32) ([]byte, error) {
	if len(vec) == 0 {
		return nil, errors.New("embeddings: cannot encode empty vector")
	}
	out := make([]byte, 4*len(vec))
	for i, v := range vec {
		binary.LittleEndian.PutUint32(out[i*4:(i+1)*4], math.Float32bits(v))
	}
	return out, nil
}

// DecodeVector unpacks a little-endian byte slice back into a float32 slice.
// dims is the expected component count; mismatch returns an error.
func DecodeVector(blob []byte, dims int) ([]float32, error) {
	if dims <= 0 {
		return nil, fmt.Errorf("embeddings: dims must be > 0, got %d", dims)
	}
	if len(blob) != dims*4 {
		return nil, fmt.Errorf("embeddings: blob length %d does not match dims %d*4", len(blob), dims)
	}
	out := make([]float32, dims)
	for i := range dims {
		u := binary.LittleEndian.Uint32(blob[i*4 : (i+1)*4])
		out[i] = math.Float32frombits(u)
	}
	return out, nil
}
