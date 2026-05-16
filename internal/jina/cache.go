package jina

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// CacheEntry is one row in jina_cache. The (kind, key_hash) pair is
// unique; OnConflict upsert keeps the latest fetch.
//
// Kind is one of "search" or "read". RequestKey is the human-readable
// pre-image of KeyHash, kept for forensics — never used for lookups.
// Response is the raw JSON-marshaled wire value (Document for read,
// []Result for search) so callers don't pay the cost of re-marshaling
// on hit.
type CacheEntry struct {
	ID         string    `gorm:"primaryKey;size:64"`
	Kind       string    `gorm:"size:16;index:idx_jina_cache_lookup,unique,priority:1"`
	KeyHash    string    `gorm:"size:64;index:idx_jina_cache_lookup,unique,priority:2"`
	RequestKey string    `gorm:"size:512"`
	Response   []byte    `gorm:"type:blob"`
	FetchedAt  time.Time `gorm:"index"`
	ExpiresAt  time.Time
}

// TableName tracks Store.Table at runtime through gorm.
func (CacheEntry) TableName() string { return "jina_cache" }

// Store is a GORM-backed cache for Read and Search responses.
//
// Pattern mirrors internal/embeddings/store.go: one Store value per
// process, the Table field overrides the default name (used by the
// _replay variant only — not yet used for jina but kept for symmetry).
type Store struct {
	DB    *gorm.DB
	Table string
}

func (s *Store) tableName() string {
	if s.Table == "" {
		return "jina_cache"
	}
	return s.Table
}

// Migrate runs AutoMigrate and creates the (kind, key_hash) unique index.
func (s *Store) Migrate() error {
	if s == nil || s.DB == nil {
		return errors.New("jina: Store.Migrate on nil store")
	}
	tbl := s.tableName()
	if err := s.DB.Table(tbl).AutoMigrate(&CacheEntry{}); err != nil {
		return fmt.Errorf("jina: migrate %s: %w", tbl, err)
	}
	return nil
}

// Get returns the cache entry for (kind, key) if present AND unexpired.
// Returns hit=false on miss or expired; expired rows are deleted lazily.
func (s *Store) Get(ctx context.Context, kind, key string) (CacheEntry, bool, error) {
	if s == nil || s.DB == nil {
		return CacheEntry{}, false, nil
	}
	if kind == "" || key == "" {
		return CacheEntry{}, false, nil
	}
	id := entryID(kind, key)
	var row CacheEntry
	err := s.DB.WithContext(ctx).Table(s.tableName()).
		Where("id = ?", id).
		Limit(1).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return CacheEntry{}, false, nil
	}
	if err != nil {
		return CacheEntry{}, false, err
	}
	if !row.ExpiresAt.IsZero() && time.Now().UTC().After(row.ExpiresAt) {
		// Lazy GC.
		_ = s.DB.WithContext(ctx).Table(s.tableName()).
			Where("id = ?", id).
			Delete(&CacheEntry{}).Error
		return CacheEntry{}, false, nil
	}
	return row, true, nil
}

// Put writes or replaces a cache entry. ttl=0 caches forever (not
// recommended); negative ttl is rejected.
func (s *Store) Put(ctx context.Context, kind, key string, ttl time.Duration, response []byte) error {
	if s == nil || s.DB == nil {
		return nil
	}
	if kind == "" || key == "" {
		return errors.New("jina: cache Put with empty kind/key")
	}
	if ttl < 0 {
		return errors.New("jina: cache Put with negative ttl")
	}
	now := time.Now().UTC()
	exp := time.Time{}
	if ttl > 0 {
		exp = now.Add(ttl)
	}
	row := CacheEntry{
		ID:         entryID(kind, key),
		Kind:       kind,
		KeyHash:    keyHash(key),
		RequestKey: truncateKey(key, 512),
		Response:   response,
		FetchedAt:  now,
		ExpiresAt:  exp,
	}
	return s.DB.WithContext(ctx).Table(s.tableName()).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		UpdateAll: true,
	}).Create(&row).Error
}

// SearchKey builds the canonical cache key for a Search call.
func SearchKey(query, site string, maxResults int) string {
	q := strings.ToLower(strings.TrimSpace(query))
	site = strings.ToLower(strings.TrimSpace(site))
	return fmt.Sprintf("%s|%s|%d", q, site, maxResults)
}

// ReadKey builds the canonical cache key for a Read call. The URL is
// run through NormalizeURL so equivalent URLs share an entry.
func ReadKey(target string) string {
	return NormalizeURL(target)
}

func entryID(kind, key string) string {
	h := sha256.Sum256([]byte(kind + "|" + key))
	return hex.EncodeToString(h[:])
}

func keyHash(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

func truncateKey(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
