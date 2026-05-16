package embeddings

import (
	"context"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	return db
}

func TestEncodeDecodeVector_RoundTrip(t *testing.T) {
	in := []float32{0.0, 1.0, -1.0, 0.5, 0.25, 0.125}
	blob, err := EncodeVector(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(blob) != 4*len(in) {
		t.Fatalf("blob len mismatch: %d", len(blob))
	}
	out, err := DecodeVector(blob, len(in))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	for i := range in {
		if in[i] != out[i] {
			t.Fatalf("round-trip diff at %d: %v vs %v", i, in[i], out[i])
		}
	}
}

func TestEncodeVector_EmptyError(t *testing.T) {
	if _, err := EncodeVector(nil); err == nil {
		t.Fatal("expected error on empty")
	}
}

func TestDecodeVector_DimMismatch(t *testing.T) {
	blob := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	if _, err := DecodeVector(blob, 5); err == nil {
		t.Fatal("expected dim-mismatch error")
	}
}

func TestStore_MigrateUpsertLoad(t *testing.T) {
	db := newTestDB(t)
	store := &Store{DB: db, Table: "memory_embeddings"}
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	row := MemoryEmbedding{
		ID:          "f1",
		ContentHash: "abc",
		ModelID:     "test-model",
		Dims:        4,
		Vector:      mustEncode(t, []float32{0.1, 0.2, 0.3, 0.4}),
		UpdatedAt:   time.Now().UTC(),
	}
	ctx := context.Background()
	if err := store.Upsert(ctx, row); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := store.Load(ctx, "test-model")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 1 || got[0].ID != "f1" || got[0].ContentHash != "abc" {
		t.Fatalf("unexpected row: %+v", got)
	}
}

func TestStore_LoadFiltersByModelID(t *testing.T) {
	db := newTestDB(t)
	store := &Store{DB: db, Table: "memory_embeddings"}
	_ = store.Migrate()
	ctx := context.Background()
	_ = store.Upsert(ctx, MemoryEmbedding{ID: "a", ContentHash: "h", ModelID: "m1", Dims: 1, Vector: mustEncode(t, []float32{1})})
	_ = store.Upsert(ctx, MemoryEmbedding{ID: "b", ContentHash: "h", ModelID: "m2", Dims: 1, Vector: mustEncode(t, []float32{1})})

	rows, err := store.Load(ctx, "m1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "a" {
		t.Fatalf("expected only m1 row, got %+v", rows)
	}
}

func TestStore_DeleteByModelID(t *testing.T) {
	db := newTestDB(t)
	store := &Store{DB: db, Table: "memory_embeddings"}
	_ = store.Migrate()
	ctx := context.Background()
	_ = store.Upsert(ctx, MemoryEmbedding{ID: "a", ContentHash: "h", ModelID: "old", Dims: 1, Vector: mustEncode(t, []float32{1})})
	_ = store.Upsert(ctx, MemoryEmbedding{ID: "b", ContentHash: "h", ModelID: "current", Dims: 1, Vector: mustEncode(t, []float32{1})})

	deleted, err := store.DeleteByModelID(ctx, "current")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected 1 deletion, got %d", deleted)
	}
	rows, _ := store.Load(ctx, "current")
	if len(rows) != 1 || rows[0].ID != "b" {
		t.Fatalf("wrong survivor: %+v", rows)
	}
}

func TestStore_Delete(t *testing.T) {
	db := newTestDB(t)
	store := &Store{DB: db, Table: "memory_embeddings"}
	_ = store.Migrate()
	ctx := context.Background()
	_ = store.Upsert(ctx, MemoryEmbedding{ID: "a", ContentHash: "h", ModelID: "m", Dims: 1, Vector: mustEncode(t, []float32{1})})
	if err := store.Delete(ctx, "a"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	rows, _ := store.Load(ctx, "m")
	if len(rows) != 0 {
		t.Fatalf("expected empty, got %+v", rows)
	}
	// idempotent on missing
	if err := store.Delete(ctx, "a"); err != nil {
		t.Fatalf("expected idempotent delete, got %v", err)
	}
}

func mustEncode(t *testing.T, v []float32) []byte {
	t.Helper()
	b, err := EncodeVector(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return b
}
