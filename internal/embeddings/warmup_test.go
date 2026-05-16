package embeddings

import (
	"context"
	"testing"

	"github.com/sirupsen/logrus"
)

type stubLister struct{ rows []FactRow }

func (s *stubLister) ListAllFactRows(_ context.Context) ([]FactRow, error) { return s.rows, nil }

func TestWarmup_FreshDB_EmbedsAll(t *testing.T) {
	db := newTestDB(t)
	store := &Store{DB: db, Table: "memory_embeddings"}
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	idx := NewMemoryIndex(NewFakeEmbedder(32))
	lister := &stubLister{rows: []FactRow{
		{ID: "a", Fact: "alpha"},
		{ID: "b", Fact: "beta"},
	}}

	stats, err := Warmup(context.Background(), store, idx, lister, logrus.NewEntry(logrus.New()))
	if err != nil {
		t.Fatalf("warmup: %v", err)
	}
	if stats.Loaded != 0 {
		t.Errorf("expected loaded=0 on fresh db, got %d", stats.Loaded)
	}
	if stats.Reembedded != 2 {
		t.Errorf("expected reembedded=2, got %d", stats.Reembedded)
	}
	if idx.Size() != 2 {
		t.Errorf("expected index size=2, got %d", idx.Size())
	}

	// Persisted in store?
	rows, _ := store.Load(context.Background(), idx.Embedder().ModelID())
	if len(rows) != 2 {
		t.Errorf("expected 2 persisted rows, got %d", len(rows))
	}
}

func TestWarmup_CacheHitSecondRun(t *testing.T) {
	db := newTestDB(t)
	store := &Store{DB: db, Table: "memory_embeddings"}
	_ = store.Migrate()
	emb := &countingEmbedder{inner: NewFakeEmbedder(32)}
	idx := NewMemoryIndex(emb)
	lister := &stubLister{rows: []FactRow{{ID: "a", Fact: "alpha"}}}

	if _, err := Warmup(context.Background(), store, idx, lister, nil); err != nil {
		t.Fatal(err)
	}
	first := emb.calls

	// Second pass on a fresh index but same DB: should load from store, no re-embed.
	idx2 := NewMemoryIndex(emb)
	stats, err := Warmup(context.Background(), store, idx2, lister, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Loaded != 1 {
		t.Errorf("expected loaded=1 from cache, got %d", stats.Loaded)
	}
	if stats.Reembedded != 0 {
		t.Errorf("expected reembedded=0, got %d", stats.Reembedded)
	}
	if emb.calls != first {
		t.Errorf("expected embedder unused on cache hit, got %d total calls", emb.calls)
	}
}

func TestWarmup_ModelSwapEvictsStale(t *testing.T) {
	db := newTestDB(t)
	store := &Store{DB: db, Table: "memory_embeddings"}
	_ = store.Migrate()

	// Seed a row from a different model.
	_ = store.Upsert(context.Background(), MemoryEmbedding{
		ID:          "old",
		ContentHash: "h",
		ModelID:     "old-model",
		Dims:        32,
		Vector:      mustEncode(t, make([]float32, 32)),
	})

	idx := NewMemoryIndex(NewFakeEmbedder(32))
	lister := &stubLister{rows: []FactRow{{ID: "a", Fact: "alpha"}}}
	stats, err := Warmup(context.Background(), store, idx, lister, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Evicted != 1 {
		t.Errorf("expected evicted=1, got %d", stats.Evicted)
	}
	if idx.Has("old") {
		t.Error("old-model row should not be in index")
	}
}

func TestWarmup_ContentChangeReembeds(t *testing.T) {
	db := newTestDB(t)
	store := &Store{DB: db, Table: "memory_embeddings"}
	_ = store.Migrate()
	emb := &countingEmbedder{inner: NewFakeEmbedder(16)}
	idx := NewMemoryIndex(emb)

	// First pass: embed "alpha" for id a.
	lister := &stubLister{rows: []FactRow{{ID: "a", Fact: "alpha"}}}
	if _, err := Warmup(context.Background(), store, idx, lister, nil); err != nil {
		t.Fatal(err)
	}
	first := emb.calls

	// Second pass on a fresh index, same id but new content.
	idx2 := NewMemoryIndex(emb)
	lister2 := &stubLister{rows: []FactRow{{ID: "a", Fact: "different content"}}}
	stats, err := Warmup(context.Background(), store, idx2, lister2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Reembedded != 1 {
		t.Errorf("expected reembedded=1 on hash drift, got %d", stats.Reembedded)
	}
	if emb.calls <= first {
		t.Errorf("expected additional embed call, got %d", emb.calls)
	}
}
