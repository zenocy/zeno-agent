package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/store"
	"github.com/zenocy/zeno-v2/internal/synth"
)

func TestSeedMemoryReplay_TruncatesAndSeeds(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_journal_mode=WAL&_foreign_keys=on"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, synth.Migrate(db, true, true))

	fixture := filepath.Join(t.TempDir(), "memory.json")
	require.NoError(t, os.WriteFile(fixture, []byte(`[
		{"subject": "partner", "fact": "Partner is Sam.", "category": "relationship", "confidence": "high", "source": "user"},
		{"subject": "runs", "fact": "Tue/Thu mornings.", "category": "routine", "confidence": "med", "source": "synth"}
	]`), 0o644))

	ctx := context.Background()
	require.NoError(t, seedMemoryReplay(ctx, db, fixture))

	repo := &store.MemoryRepo{DB: db, Table: "memory_facts_replay"}
	rows, err := repo.ListTop(ctx, 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	// IDs are derived deterministically from subject+fact, so re-seeding the
	// same fixture overwrites in place rather than appending duplicates.
	require.NoError(t, seedMemoryReplay(ctx, db, fixture))
	rows, err = repo.ListTop(ctx, 10)
	require.NoError(t, err)
	require.Len(t, rows, 2, "re-seeding the same fixture must not duplicate rows")

	// Replacing the fixture with a smaller set must truncate first: a stale
	// row from the prior seed would otherwise survive.
	smaller := filepath.Join(t.TempDir(), "memory_smaller.json")
	require.NoError(t, os.WriteFile(smaller, []byte(`[
		{"subject": "anniversary", "fact": "Anniversary May 7.", "category": "date", "confidence": "high", "source": "user"}
	]`), 0o644))
	require.NoError(t, seedMemoryReplay(ctx, db, smaller))
	rows, err = repo.ListTop(ctx, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "anniversary", rows[0].Subject)

	// Prod table is untouched.
	prod := &store.MemoryRepo{DB: db, Table: "memory_facts"}
	prodRows, err := prod.ListTop(ctx, 10)
	require.NoError(t, err)
	require.Empty(t, prodRows, "seedMemoryReplay must not touch the prod table")
}

func TestSeedMemoryReplay_RejectsEmptyAndMissingFields(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, synth.Migrate(db, true, true))
	ctx := context.Background()

	empty := filepath.Join(t.TempDir(), "empty.json")
	require.NoError(t, os.WriteFile(empty, []byte(`[]`), 0o644))
	require.Error(t, seedMemoryReplay(ctx, db, empty))

	missingFact := filepath.Join(t.TempDir(), "bad.json")
	require.NoError(t, os.WriteFile(missingFact, []byte(`[{"subject": "x"}]`), 0o644))
	require.Error(t, seedMemoryReplay(ctx, db, missingFact))
}
