package projection

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/store"
)

func openMemoryDB(t *testing.T) (*store.MemoryRepo, context.Context) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_journal_mode=WAL&_foreign_keys=on"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	repo := &store.MemoryRepo{DB: db}
	require.NoError(t, repo.Migrate())
	return repo, context.Background()
}

func TestMemoryFacts_NilRepoIsEmpty(t *testing.T) {
	p := MemoryFacts{Repo: nil, Config: MemoryFactsConfig{Limit: 10}}
	out, err := p.Compute(context.Background(), nil)
	require.NoError(t, err)
	require.Empty(t, out)
}

func TestMemoryFacts_OrderingAndLimit(t *testing.T) {
	repo, ctx := openMemoryDB(t)
	now := time.Now()
	require.NoError(t, repo.Upsert(ctx, []store.MemoryFact{
		{ID: "low-old", Subject: "low-old", Fact: "x", Category: "x", Confidence: "low", Source: "synth", FirstSeen: now, LastReinforced: now.Add(-time.Hour)},
		{ID: "high-recent", Subject: "high-recent", Fact: "x", Category: "x", Confidence: "high", Source: "synth", FirstSeen: now, LastReinforced: now},
		{ID: "med-recent", Subject: "med-recent", Fact: "x", Category: "x", Confidence: "med", Source: "synth", FirstSeen: now, LastReinforced: now},
		{ID: "high-old", Subject: "high-old", Fact: "x", Category: "x", Confidence: "high", Source: "synth", FirstSeen: now, LastReinforced: now.Add(-24 * time.Hour)},
	}))

	out, err := MemoryFacts{Repo: repo, Config: MemoryFactsConfig{Limit: 10}}.Compute(ctx, nil)
	require.NoError(t, err)
	require.Len(t, out, 4)
	require.Equal(t, "high-recent", out[0].Subject)
	require.Equal(t, "high-old", out[1].Subject)
	require.Equal(t, "med-recent", out[2].Subject)
	require.Equal(t, "low-old", out[3].Subject)

	// Limit honored.
	limited, err := MemoryFacts{Repo: repo, Config: MemoryFactsConfig{Limit: 2}}.Compute(ctx, nil)
	require.NoError(t, err)
	require.Len(t, limited, 2)
	require.Equal(t, "high-recent", limited[0].Subject)
	require.Equal(t, "high-old", limited[1].Subject)
}

func TestMemoryFacts_DefaultLimit(t *testing.T) {
	repo, ctx := openMemoryDB(t)
	now := time.Now()
	rows := make([]store.MemoryFact, 25)
	for i := range rows {
		rows[i] = store.MemoryFact{
			ID:             "f" + string(rune('a'+i)),
			Subject:        "subj-" + string(rune('a'+i)),
			Fact:           "x",
			Category:       "x",
			Confidence:     "med",
			Source:         "synth",
			FirstSeen:      now,
			LastReinforced: now,
		}
	}
	require.NoError(t, repo.Upsert(ctx, rows))

	// Limit 0 → default 20.
	out, err := MemoryFacts{Repo: repo}.Compute(ctx, nil)
	require.NoError(t, err)
	require.Len(t, out, 20)
}
