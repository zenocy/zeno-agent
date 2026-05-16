package embeddings

import (
	"context"
	"testing"
)

func TestMemoryIndex_EmptyReturnsNil(t *testing.T) {
	idx := NewMemoryIndex(NewFakeEmbedder(16))
	matches, err := idx.Search(context.Background(), "anything", 5, -1)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if matches != nil {
		t.Fatalf("expected nil, got %v", matches)
	}
}

func TestMemoryIndex_UpsertSearch(t *testing.T) {
	idx := NewMemoryIndex(NewFakeEmbedder(64))
	ctx := context.Background()
	_ = idx.Upsert(ctx, "f1", "partner is Sam")
	_ = idx.Upsert(ctx, "f2", "anniversary is May 7")
	_ = idx.Upsert(ctx, "f3", "runs Tuesdays and Thursdays")

	matches, err := idx.Search(ctx, "anniversary is May 7", 2, -1)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(matches))
	}
	if matches[0].ID != "f2" {
		t.Fatalf("expected f2 first (exact text match), got %s", matches[0].ID)
	}
}

func TestMemoryIndex_UpsertCacheHit(t *testing.T) {
	emb := &countingEmbedder{inner: NewFakeEmbedder(16)}
	idx := NewMemoryIndex(emb)
	ctx := context.Background()
	if err := idx.Upsert(ctx, "f1", "hello world"); err != nil {
		t.Fatal(err)
	}
	first := emb.calls
	if err := idx.Upsert(ctx, "f1", "hello world"); err != nil {
		t.Fatal(err)
	}
	if emb.calls != first {
		t.Fatalf("expected cache hit, embedder was called again")
	}
	if err := idx.Upsert(ctx, "f1", "different content"); err != nil {
		t.Fatal(err)
	}
	if emb.calls != first+1 {
		t.Fatalf("expected re-embed on content change, got %d calls", emb.calls)
	}
}

func TestMemoryIndex_UpsertEmptyContentRemoves(t *testing.T) {
	idx := NewMemoryIndex(NewFakeEmbedder(16))
	ctx := context.Background()
	_ = idx.Upsert(ctx, "f1", "something")
	if !idx.Has("f1") {
		t.Fatal("expected f1 indexed")
	}
	_ = idx.Upsert(ctx, "f1", "")
	if idx.Has("f1") {
		t.Fatal("expected f1 removed by empty content")
	}
}

func TestMemoryIndex_UpsertVectorDimMismatch(t *testing.T) {
	idx := NewMemoryIndex(NewFakeEmbedder(16))
	if err := idx.UpsertVector("f1", "h", []float32{1, 2, 3}); err == nil {
		t.Fatal("expected dim mismatch error")
	}
}

func TestMemoryIndex_SearchKLimit(t *testing.T) {
	idx := NewMemoryIndex(NewFakeEmbedder(32))
	ctx := context.Background()
	for i, txt := range []string{"alpha", "bravo", "charlie", "delta", "echo"} {
		_ = idx.Upsert(ctx, string(rune('a'+i)), txt)
	}
	matches, _ := idx.Search(ctx, "alpha", 3, -1)
	if len(matches) != 3 {
		t.Fatalf("expected k=3 results, got %d", len(matches))
	}
}

func TestMemoryIndex_SearchMinScoreFilter(t *testing.T) {
	idx := NewMemoryIndex(NewFakeEmbedder(32))
	ctx := context.Background()
	_ = idx.Upsert(ctx, "f1", "alpha")
	_ = idx.Upsert(ctx, "f2", "totally unrelated content here")
	matches, _ := idx.Search(ctx, "alpha", 0, 0.99)
	for _, m := range matches {
		if m.Score < 0.99 {
			t.Fatalf("minScore filter failed: %v", m)
		}
	}
}

func TestMemoryIndex_SearchDeterministicTiebreak(t *testing.T) {
	idx := NewMemoryIndex(NewFakeEmbedder(8))
	v := []float32{0.5, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5}
	l2Normalize(v)
	if err := idx.UpsertVector("zzz", "h1", append([]float32(nil), v...)); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertVector("aaa", "h2", append([]float32(nil), v...)); err != nil {
		t.Fatal(err)
	}
	matches, _ := idx.SearchVector(v, 0, -1)
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(matches))
	}
	if matches[0].ID != "aaa" || matches[1].ID != "zzz" {
		t.Fatalf("expected ID-asc tiebreak, got %v", matches)
	}
}

func TestMemoryIndex_RemoveAndIDs(t *testing.T) {
	idx := NewMemoryIndex(NewFakeEmbedder(16))
	ctx := context.Background()
	_ = idx.Upsert(ctx, "b", "beta")
	_ = idx.Upsert(ctx, "a", "alpha")
	ids := idx.IDs()
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("expected lex order, got %v", ids)
	}
	idx.Remove("a")
	if idx.Has("a") {
		t.Fatal("expected a removed")
	}
	if idx.Size() != 1 {
		t.Fatalf("expected size=1, got %d", idx.Size())
	}
}

type countingEmbedder struct {
	inner Embedder
	calls int
}

func (c *countingEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	c.calls++
	return c.inner.Embed(ctx, text)
}
func (c *countingEmbedder) Dims() int       { return c.inner.Dims() }
func (c *countingEmbedder) ModelID() string { return c.inner.ModelID() }
