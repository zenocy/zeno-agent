package jina

import (
	"context"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	s := &Store{DB: db}
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s
}

func TestStore_PutGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, hit, err := s.Get(ctx, "read", "https://example.com/x")
	if err != nil {
		t.Fatalf("get miss: %v", err)
	}
	if hit {
		t.Fatalf("expected miss on empty store")
	}

	body := []byte(`{"title":"x"}`)
	if err := s.Put(ctx, "read", "https://example.com/x", time.Hour, body); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, hit, err := s.Get(ctx, "read", "https://example.com/x")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !hit {
		t.Fatalf("expected hit after put")
	}
	if string(got.Response) != string(body) {
		t.Fatalf("response mismatch: %q", string(got.Response))
	}
	if got.Kind != "read" {
		t.Errorf("kind=%q", got.Kind)
	}
}

func TestStore_Expiry(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.Put(ctx, "search", SearchKey("q", "", 5), time.Millisecond, []byte("[]")); err != nil {
		t.Fatalf("put: %v", err)
	}
	time.Sleep(5 * time.Millisecond)

	_, hit, err := s.Get(ctx, "search", SearchKey("q", "", 5))
	if err != nil {
		t.Fatalf("get expired: %v", err)
	}
	if hit {
		t.Fatalf("expected miss on expired entry")
	}
}

func TestStore_Upsert(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.Put(ctx, "read", "u", time.Hour, []byte("v1")); err != nil {
		t.Fatalf("put1: %v", err)
	}
	if err := s.Put(ctx, "read", "u", time.Hour, []byte("v2")); err != nil {
		t.Fatalf("put2: %v", err)
	}
	got, hit, _ := s.Get(ctx, "read", "u")
	if !hit || string(got.Response) != "v2" {
		t.Fatalf("expected v2, got hit=%v body=%q", hit, string(got.Response))
	}

	// Sanity: only one row.
	var count int64
	s.DB.Table(s.tableName()).Count(&count)
	if count != 1 {
		t.Fatalf("expected 1 row after upsert, got %d", count)
	}
}

func TestStore_KindSeparation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.Put(ctx, "read", "same", time.Hour, []byte("R"))
	_ = s.Put(ctx, "search", "same", time.Hour, []byte("S"))
	r, _, _ := s.Get(ctx, "read", "same")
	q, _, _ := s.Get(ctx, "search", "same")
	if string(r.Response) != "R" || string(q.Response) != "S" {
		t.Fatalf("kind separation failed: read=%q search=%q", string(r.Response), string(q.Response))
	}
}

func TestSearchKey_Normalization(t *testing.T) {
	a := SearchKey("  Hello World  ", "Go.Dev", 5)
	b := SearchKey("hello world", "go.dev", 5)
	if a != b {
		t.Errorf("expected case+whitespace insensitive: %q vs %q", a, b)
	}
}

func TestReadKey_UsesNormalizeURL(t *testing.T) {
	if got, want := ReadKey("https://Example.COM/x/?utm_source=a"), "https://example.com/x"; got != want {
		t.Errorf("ReadKey=%q want %q", got, want)
	}
}
