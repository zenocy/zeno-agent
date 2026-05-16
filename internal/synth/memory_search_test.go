package synth

import (
	"context"
	"testing"
	"time"

	"github.com/zenocy/zeno-v2/internal/embeddings"
	"github.com/zenocy/zeno-v2/internal/projection"
)

func newRankerIndex(t *testing.T, dims int) *embeddings.MemoryIndex {
	t.Helper()
	return embeddings.NewMemoryIndex(embeddings.NewFakeEmbedder(dims))
}

func seedPool(t *testing.T, idx *embeddings.MemoryIndex, facts []projection.MemoryFact) {
	t.Helper()
	for _, f := range facts {
		if f.ID == "" {
			continue
		}
		if err := idx.Upsert(context.Background(), f.ID, f.Fact); err != nil {
			t.Fatalf("seed upsert: %v", err)
		}
	}
}

func TestRanker_NilRankerTruncates(t *testing.T) {
	pool := []projection.MemoryFact{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	var r *MemoryRanker
	got := r.Rank(context.Background(), "x", pool, 2)
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("expected pool truncated, got %+v", got)
	}
}

func TestRanker_NilIndexTruncates(t *testing.T) {
	pool := []projection.MemoryFact{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	r := &MemoryRanker{}
	got := r.Rank(context.Background(), "x", pool, 2)
	if len(got) != 2 {
		t.Fatalf("expected truncated pool, got %d", len(got))
	}
}

func TestRanker_EmptyQueryTruncates(t *testing.T) {
	idx := newRankerIndex(t, 16)
	pool := []projection.MemoryFact{{ID: "a", Fact: "alpha"}, {ID: "b", Fact: "beta"}}
	seedPool(t, idx, pool)
	r := &MemoryRanker{Index: idx, WSim: 1, WConf: 0.3}
	got := r.Rank(context.Background(), "   ", pool, 1)
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("expected pool[:1], got %+v", got)
	}
}

func TestRanker_EmptyPoolNil(t *testing.T) {
	idx := newRankerIndex(t, 16)
	r := &MemoryRanker{Index: idx}
	got := r.Rank(context.Background(), "x", nil, 5)
	if got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

func TestRanker_ExactMatchSurfacesFirst(t *testing.T) {
	idx := newRankerIndex(t, 64)
	pool := []projection.MemoryFact{
		{ID: "f1", Fact: "runs Tuesdays and Thursdays", Confidence: "high"},
		{ID: "f2", Fact: "anniversary is May 7", Confidence: "low"},
		{ID: "f3", Fact: "coffee at 8am", Confidence: "high"},
	}
	seedPool(t, idx, pool)
	r := &MemoryRanker{Index: idx, WSim: 1.0, WConf: 0.3}
	got := r.Rank(context.Background(), "anniversary is May 7", pool, 1)
	if len(got) != 1 || got[0].ID != "f2" {
		t.Fatalf("expected f2 first (exact match dominates conf bias), got %+v", got)
	}
}

func TestRanker_ConfidenceBreaksUnrelatedTie(t *testing.T) {
	idx := newRankerIndex(t, 64)
	// Same content for two facts → identical cosine to the query → confidence
	// must decide. (Hash short-circuit means both facts share one vector by
	// hash; we set unique IDs to force two index entries.)
	pool := []projection.MemoryFact{
		{ID: "low", Fact: "alpha", Confidence: "low"},
		{ID: "high", Fact: "alpha", Confidence: "high"},
	}
	seedPool(t, idx, pool)
	r := &MemoryRanker{Index: idx, WSim: 1.0, WConf: 0.3}
	got := r.Rank(context.Background(), "alpha", pool, 1)
	if got[0].ID != "high" {
		t.Fatalf("expected high-confidence fact first, got %s", got[0].ID)
	}
}

func TestRanker_AllBelowMinScoreFallsThrough(t *testing.T) {
	idx := newRankerIndex(t, 64)
	pool := []projection.MemoryFact{
		{ID: "a", Fact: "alpha", Confidence: "low"},
		{ID: "b", Fact: "beta", Confidence: "low"},
	}
	seedPool(t, idx, pool)
	r := &MemoryRanker{Index: idx, WSim: 1.0, WConf: 0.3, MinScore: 1.5} // unreachable
	got := r.Rank(context.Background(), "totally different", pool, 2)
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("expected pool truncated as fallback, got %+v", got)
	}
}

func TestRanker_FactWithoutIDStillReturned(t *testing.T) {
	// A fact missing its ID can't be looked up in the vector index, so
	// it is scored on confidence alone. It must still survive the rank
	// step rather than being silently dropped — otherwise stale projection
	// rows would disappear from the prompt entirely.
	idx := newRankerIndex(t, 64)
	pool := []projection.MemoryFact{
		{ID: "", Fact: "no id here", Confidence: "high"},
		{ID: "b", Fact: "totally unrelated content", Confidence: "low"},
	}
	seedPool(t, idx, pool)
	r := &MemoryRanker{Index: idx, WSim: 1.0, WConf: 0.3, MinScore: -1}
	got := r.Rank(context.Background(), "no id here", pool, 2)
	if len(got) != 2 {
		t.Fatalf("expected both facts, got %d", len(got))
	}
}

func TestRanker_StableTiebreakOnEqualScore(t *testing.T) {
	idx := newRankerIndex(t, 16)
	pool := []projection.MemoryFact{
		{ID: "first", Fact: "hello", Confidence: "high"},
		{ID: "second", Fact: "hello", Confidence: "high"},
	}
	seedPool(t, idx, pool)
	r := &MemoryRanker{Index: idx, WSim: 1.0, WConf: 0.3}
	got := r.Rank(context.Background(), "hello", pool, 2)
	if got[0].ID != "first" || got[1].ID != "second" {
		t.Fatalf("expected pool order preserved on tie, got %+v", got)
	}
}

func TestCardsSyntheticQuery_DeterministicOrder(t *testing.T) {
	t1, _ := time.Parse(time.RFC3339, "2026-04-26T10:00:00Z")
	t2, _ := time.Parse(time.RFC3339, "2026-04-26T11:00:00Z")
	cal := []projection.CalendarEvent{
		{Title: "B Standup", Start: t2},
		{Title: "A Board call", Start: t1},
	}
	threads := []projection.Thread{
		{Subject: "weekly review"},
		{Subject: "OKRs draft"},
	}
	q := CardsSyntheticQuery(cal, threads)
	want := "A Board call B Standup OKRs draft weekly review"
	if q != want {
		t.Fatalf("expected %q, got %q", want, q)
	}
}

func TestCardsSyntheticQuery_EmptyInputs(t *testing.T) {
	if got := CardsSyntheticQuery(nil, nil); got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

func TestCardsSyntheticQuery_TrimsBlanks(t *testing.T) {
	cal := []projection.CalendarEvent{{Title: "  "}}
	threads := []projection.Thread{{Subject: "  hello  "}}
	got := CardsSyntheticQuery(cal, threads)
	if got != "hello" {
		t.Fatalf("expected 'hello', got %q", got)
	}
}
