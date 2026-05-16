package jina

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/zenocy/zeno-v2/internal/config"
	jinaclient "github.com/zenocy/zeno-v2/internal/jina"
	zlog "github.com/zenocy/zeno-v2/internal/log"
)

type stubClient struct {
	calls int
	fn    func(ctx context.Context, q string, opts jinaclient.SearchOpts) ([]jinaclient.Result, error)
}

func (s *stubClient) Search(ctx context.Context, q string, opts jinaclient.SearchOpts) ([]jinaclient.Result, error) {
	s.calls++
	return s.fn(ctx, q, opts)
}

func newCache(t *testing.T) *jinaclient.Store {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("gorm open: %v", err)
	}
	s := &jinaclient.Store{DB: db}
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate cache: %v", err)
	}
	return s
}

func newLogStore(t *testing.T) zlog.Store {
	t.Helper()
	_, store, err := zlog.Open(t.TempDir() + "/log.db")
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	return store
}

func TestSync_AppendsObservation(t *testing.T) {
	stub := &stubClient{
		fn: func(_ context.Context, _ string, _ jinaclient.SearchOpts) ([]jinaclient.Result, error) {
			return []jinaclient.Result{{Title: "T", URL: "u", Description: "d"}}, nil
		},
	}
	logStore := newLogStore(t)
	s := New(
		[]config.SavedSearch{{Name: "go-news", Query: "golang weekly", Site: "go.dev"}},
		stub, newCache(t), logStore, time.Hour, 5,
		logrus.NewEntry(logrus.New()),
	)
	if err := s.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	events, err := logStore.ByKind(context.Background(), zlog.KindWebSearchResult)
	if err != nil {
		t.Fatalf("byKind: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 observation, got %d", len(events))
	}
}

func TestSync_CacheHitNoObservation(t *testing.T) {
	stub := &stubClient{
		fn: func(_ context.Context, _ string, _ jinaclient.SearchOpts) ([]jinaclient.Result, error) {
			return []jinaclient.Result{{Title: "X", URL: "u"}}, nil
		},
	}
	logStore := newLogStore(t)
	cache := newCache(t)
	s := New(
		[]config.SavedSearch{{Name: "n", Query: "q", Site: ""}},
		stub, cache, logStore, time.Hour, 5,
		logrus.NewEntry(logrus.New()),
	)

	if err := s.Sync(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if err := s.Sync(context.Background()); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if stub.calls != 1 {
		t.Fatalf("expected 1 Search call (second served from cache), got %d", stub.calls)
	}
	events, _ := logStore.ByKind(context.Background(), zlog.KindWebSearchResult)
	if len(events) != 1 {
		t.Fatalf("expected 1 observation despite 2 syncs, got %d", len(events))
	}
}

func TestSync_PartialFailureContinues(t *testing.T) {
	stub := &stubClient{
		fn: func(_ context.Context, q string, _ jinaclient.SearchOpts) ([]jinaclient.Result, error) {
			if q == "boom" {
				return nil, errors.New("network down")
			}
			return []jinaclient.Result{{Title: "ok", URL: "u"}}, nil
		},
	}
	logStore := newLogStore(t)
	s := New(
		[]config.SavedSearch{
			{Name: "fail", Query: "boom"},
			{Name: "ok", Query: "good"},
		},
		stub, newCache(t), logStore, time.Hour, 5,
		logrus.NewEntry(logrus.New()),
	)
	err := s.Sync(context.Background())
	if err == nil {
		t.Fatalf("expected first-error returned")
	}
	events, _ := logStore.ByKind(context.Background(), zlog.KindWebSearchResult)
	if len(events) != 1 {
		t.Fatalf("expected 1 observation from successful search, got %d", len(events))
	}
}

func TestSync_RateLimitSoftError(t *testing.T) {
	stub := &stubClient{
		fn: func(_ context.Context, _ string, _ jinaclient.SearchOpts) ([]jinaclient.Result, error) {
			return nil, jinaclient.ErrRateLimit
		},
	}
	logStore := newLogStore(t)
	s := New(
		[]config.SavedSearch{{Name: "x", Query: "q"}},
		stub, newCache(t), logStore, time.Hour, 5,
		logrus.NewEntry(logrus.New()),
	)
	if err := s.Sync(context.Background()); err == nil {
		t.Fatalf("expected error")
	}
	events, _ := logStore.ByKind(context.Background(), zlog.KindWebSearchResult)
	if len(events) != 0 {
		t.Fatalf("expected no observation on rate-limit, got %d", len(events))
	}
}

func TestSync_EmptySearchesIsNoop(t *testing.T) {
	stub := &stubClient{
		fn: func(_ context.Context, _ string, _ jinaclient.SearchOpts) ([]jinaclient.Result, error) {
			t.Fatalf("Search should not be called")
			return nil, nil
		},
	}
	logStore := newLogStore(t)
	s := New(nil, stub, newCache(t), logStore, time.Hour, 5, logrus.NewEntry(logrus.New()))
	if err := s.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if stub.calls != 0 {
		t.Fatalf("expected 0 calls, got %d", stub.calls)
	}
}
