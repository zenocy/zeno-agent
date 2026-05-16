package synth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/zenocy/zeno-v2/internal/jina"
)

// stubJinaClient implements JinaClient for tests. ReadFn / SearchFn drive
// behavior; nil means "return error". Calls counter validates cache hits.
type stubJinaClient struct {
	ReadFn      func(ctx context.Context, url string, opts jina.ReadOpts) (jina.Document, error)
	SearchFn    func(ctx context.Context, q string, opts jina.SearchOpts) ([]jina.Result, error)
	ReadCalls   int
	SearchCalls int
}

func (s *stubJinaClient) Read(ctx context.Context, url string, opts jina.ReadOpts) (jina.Document, error) {
	s.ReadCalls++
	if s.ReadFn == nil {
		return jina.Document{}, errors.New("ReadFn not set")
	}
	return s.ReadFn(ctx, url, opts)
}
func (s *stubJinaClient) Search(ctx context.Context, q string, opts jina.SearchOpts) ([]jina.Result, error) {
	s.SearchCalls++
	if s.SearchFn == nil {
		return nil, errors.New("SearchFn not set")
	}
	return s.SearchFn(ctx, q, opts)
}

func newJinaTestStore(t *testing.T) *jina.Store {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s := &jina.Store{DB: db}
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s
}

func TestSearchWebTool_HappyPath(t *testing.T) {
	stub := &stubJinaClient{
		SearchFn: func(_ context.Context, q string, _ jina.SearchOpts) ([]jina.Result, error) {
			return []jina.Result{
				{Title: "T1", URL: "https://a.example/1", Description: "first description"},
				{Title: "T2", URL: "https://a.example/2", Description: "second description"},
			}, nil
		},
	}
	tool := &SearchWebTool{Client: stub, Cache: newJinaTestStore(t), TTLs: jinaTTLs{Search: time.Hour}}
	out, err := tool.Execute(context.Background(), map[string]any{"query": "anything"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "T1") || !strings.Contains(out, "https://a.example/2") {
		t.Fatalf("missing result: %q", out)
	}
}

func TestSearchWebTool_CacheHit(t *testing.T) {
	stub := &stubJinaClient{
		SearchFn: func(_ context.Context, q string, _ jina.SearchOpts) ([]jina.Result, error) {
			return []jina.Result{{Title: "C", URL: "u"}}, nil
		},
	}
	tool := &SearchWebTool{Client: stub, Cache: newJinaTestStore(t), TTLs: jinaTTLs{Search: time.Hour}}
	args := map[string]any{"query": "same", "max_results": 3}
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("second: %v", err)
	}
	if stub.SearchCalls != 1 {
		t.Fatalf("expected 1 search call (second served from cache), got %d", stub.SearchCalls)
	}
}

func TestSearchWebTool_RateLimit(t *testing.T) {
	stub := &stubJinaClient{
		SearchFn: func(_ context.Context, _ string, _ jina.SearchOpts) ([]jina.Result, error) {
			return nil, jina.ErrRateLimit
		},
	}
	tool := &SearchWebTool{Client: stub, Cache: newJinaTestStore(t)}
	out, err := tool.Execute(context.Background(), map[string]any{"query": "x"})
	if err != nil {
		t.Fatalf("rate-limit should be soft-handled, got err: %v", err)
	}
	if !strings.Contains(out, "rate-limited") {
		t.Fatalf("expected rate-limited prose, got %q", out)
	}
}

func TestSearchWebTool_AuthErrorIsAlsoSoft(t *testing.T) {
	stub := &stubJinaClient{
		SearchFn: func(_ context.Context, _ string, _ jina.SearchOpts) ([]jina.Result, error) {
			return nil, jina.ErrAuth
		},
	}
	tool := &SearchWebTool{Client: stub}
	out, err := tool.Execute(context.Background(), map[string]any{"query": "x"})
	if err != nil {
		t.Fatalf("auth should be soft-handled, got err: %v", err)
	}
	if !strings.Contains(out, "rate-limited") {
		t.Fatalf("expected fixed prose, got %q", out)
	}
}

func TestSearchWebTool_RequiredQuery(t *testing.T) {
	tool := &SearchWebTool{Client: &stubJinaClient{}}
	if _, err := tool.Execute(context.Background(), map[string]any{}); err == nil {
		t.Fatalf("expected error on missing query")
	}
}

func TestReadURLTool_HappyPathAndCache(t *testing.T) {
	stub := &stubJinaClient{
		ReadFn: func(_ context.Context, url string, _ jina.ReadOpts) (jina.Document, error) {
			return jina.Document{URL: url, Title: "Page", Content: "Body content here. " + strings.Repeat("words ", 100)}, nil
		},
	}
	tool := &ReadURLTool{Client: stub, Cache: newJinaTestStore(t), TTLs: jinaTTLs{Read: time.Hour}}
	args := map[string]any{"url": "https://example.com/Path"}
	out1, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if !strings.Contains(out1, "Page") || !strings.Contains(out1, "Body content") {
		t.Fatalf("output missing fields: %q", out1)
	}
	// Equivalent URL with tracking params should hit cache.
	args2 := map[string]any{"url": "https://Example.com/Path/?utm_source=foo"}
	if _, err := tool.Execute(context.Background(), args2); err != nil {
		t.Fatalf("second: %v", err)
	}
	if stub.ReadCalls != 1 {
		t.Fatalf("expected 1 Read call (cache hit on normalized URL), got %d", stub.ReadCalls)
	}
}

func TestReadURLTool_RejectRelative(t *testing.T) {
	tool := &ReadURLTool{Client: &stubJinaClient{}}
	if _, err := tool.Execute(context.Background(), map[string]any{"url": "/relative"}); err == nil {
		t.Fatalf("expected error on relative URL")
	}
}

func TestReadURLTool_MaxCharsEnforced(t *testing.T) {
	stub := &stubJinaClient{
		ReadFn: func(_ context.Context, url string, _ jina.ReadOpts) (jina.Document, error) {
			return jina.Document{URL: url, Title: "T", Content: strings.Repeat("a", 50000)}, nil
		},
	}
	tool := &ReadURLTool{Client: stub, Cache: newJinaTestStore(t)}
	out, err := tool.Execute(context.Background(), map[string]any{"url": "https://example.com", "max_chars": 100})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "truncated") {
		t.Fatalf("expected truncation marker, got: %q", out[:200])
	}
	// Hard ceiling: requesting 999999 should still be capped at 12000.
	out2, _ := tool.Execute(context.Background(), map[string]any{"url": "https://example.com", "max_chars": 999999})
	// out2's body portion should be ≤ 12000 + a few overhead chars (title/url lines).
	if len(out2) > 12500 {
		t.Fatalf("hard ceiling not enforced; len=%d", len(out2))
	}
}

func TestArgInt_Variants(t *testing.T) {
	cases := []struct {
		v    any
		want int
	}{
		{float64(3), 3},
		{int(4), 4},
		{int64(5), 5},
		{"6", 6},
		{nil, 0},
	}
	for _, c := range cases {
		got := argInt(map[string]any{"k": c.v}, "k")
		if got != c.want {
			t.Errorf("argInt(%v)=%d want %d", c.v, got, c.want)
		}
	}
	if got := argInt(nil, "k"); got != 0 {
		t.Errorf("argInt(nil)=%d", got)
	}
}
