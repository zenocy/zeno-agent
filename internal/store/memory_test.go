package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMemoryRepo_UpsertByID(t *testing.T) {
	db := openTestDB(t)
	repo := &MemoryRepo{DB: db}
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, repo.Upsert(ctx, []MemoryFact{
		{ID: "partner", Subject: "partner", Fact: "Partner is Sam.", Category: "relationship", Confidence: "high", Source: "user", EvidenceCount: 1, FirstSeen: now, LastReinforced: now},
		{ID: "runs", Subject: "runs", Fact: "Runs Tue/Thu mornings.", Category: "routine", Confidence: "med", Source: "synth", EvidenceCount: 3, FirstSeen: now, LastReinforced: now},
	}))
	rows, err := repo.ListTop(ctx, 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	// Re-upsert with same ID overwrites in place.
	require.NoError(t, repo.Upsert(ctx, []MemoryFact{
		{ID: "partner", Subject: "partner", Fact: "Partner is Sam (updated).", Category: "relationship", Confidence: "high", Source: "user", EvidenceCount: 1, FirstSeen: now, LastReinforced: now},
	}))
	got, err := repo.GetByID(ctx, "partner")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "Partner is Sam (updated).", got.Fact)

	rows, err = repo.ListTop(ctx, 10)
	require.NoError(t, err)
	require.Len(t, rows, 2, "same-id upsert must replace not duplicate")
}

func TestMemoryRepo_ListTopOrdering(t *testing.T) {
	db := openTestDB(t)
	repo := &MemoryRepo{DB: db}
	ctx := context.Background()

	now := time.Now()
	require.NoError(t, repo.Upsert(ctx, []MemoryFact{
		{ID: "low-recent", Subject: "low-recent", Fact: "x", Category: "misc", Confidence: "low", Source: "synth", FirstSeen: now, LastReinforced: now.Add(time.Hour)},
		{ID: "high-old", Subject: "high-old", Fact: "x", Category: "misc", Confidence: "high", Source: "synth", FirstSeen: now, LastReinforced: now.Add(-24 * time.Hour)},
		{ID: "med-mid", Subject: "med-mid", Fact: "x", Category: "misc", Confidence: "med", Source: "synth", FirstSeen: now, LastReinforced: now},
		{ID: "high-recent", Subject: "high-recent", Fact: "x", Category: "misc", Confidence: "high", Source: "synth", FirstSeen: now, LastReinforced: now.Add(time.Minute)},
	}))
	rows, err := repo.ListTop(ctx, 10)
	require.NoError(t, err)
	require.Len(t, rows, 4)
	// high-recent (high, newest) > high-old (high, older) > med-mid > low-recent.
	require.Equal(t, "high-recent", rows[0].ID)
	require.Equal(t, "high-old", rows[1].ID)
	require.Equal(t, "med-mid", rows[2].ID)
	require.Equal(t, "low-recent", rows[3].ID)

	// Limit honored.
	rows, err = repo.ListTop(ctx, 2)
	require.NoError(t, err)
	require.Len(t, rows, 2)
}

func TestMemoryRepo_SoftDeleteHidesFromList(t *testing.T) {
	db := openTestDB(t)
	repo := &MemoryRepo{DB: db}
	ctx := context.Background()

	now := time.Now()
	require.NoError(t, repo.Insert(ctx, MemoryFact{
		ID: "anniversary", Subject: "anniversary", Fact: "Anniversary May 7.",
		Category: "date", Confidence: "high", Source: "user", FirstSeen: now, LastReinforced: now,
	}))

	require.NoError(t, repo.SoftDelete(ctx, "anniversary"))

	// Default list excludes soft-deleted rows.
	rows, err := repo.ListTop(ctx, 10)
	require.NoError(t, err)
	require.Empty(t, rows)

	// GetByID also hides soft-deleted by default.
	got, err := repo.GetByID(ctx, "anniversary")
	require.NoError(t, err)
	require.Nil(t, got)

	// GetBySubject without includeDeleted also hides.
	hidden, err := repo.GetBySubject(ctx, "anniversary", false)
	require.NoError(t, err)
	require.Nil(t, hidden)

	// GetBySubject(includeDeleted=true) sees the denylist row.
	denylisted, err := repo.GetBySubject(ctx, "anniversary", true)
	require.NoError(t, err)
	require.NotNil(t, denylisted)
	require.Equal(t, "anniversary", denylisted.ID)
	require.True(t, denylisted.DeletedAt.Valid, "DeletedAt should be set for soft-deleted row")
}

func TestMemoryRepo_IncrementEvidenceIdempotent(t *testing.T) {
	db := openTestDB(t)
	repo := &MemoryRepo{DB: db}
	ctx := context.Background()

	now := time.Now()
	require.NoError(t, repo.Insert(ctx, MemoryFact{
		ID: "runs", Subject: "runs", Fact: "Runs Tue/Thu.",
		Category: "routine", Confidence: "low", Source: "synth", EvidenceCount: 1,
		FirstSeen: now, LastReinforced: now,
	}))

	// First increment with run-1: count goes 1→2.
	require.NoError(t, repo.IncrementEvidence(ctx, "runs", "run-1"))
	got, err := repo.GetByID(ctx, "runs")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, 2, got.EvidenceCount)
	require.Equal(t, "run-1", got.LastSeenRunID)

	// Second call with the same run-1 is a no-op (same-day re-run guarantee).
	require.NoError(t, repo.IncrementEvidence(ctx, "runs", "run-1"))
	got, err = repo.GetByID(ctx, "runs")
	require.NoError(t, err)
	require.Equal(t, 2, got.EvidenceCount, "same runID must not double-count")

	// New runID increments.
	require.NoError(t, repo.IncrementEvidence(ctx, "runs", "run-2"))
	got, err = repo.GetByID(ctx, "runs")
	require.NoError(t, err)
	require.Equal(t, 3, got.EvidenceCount)
	require.Equal(t, "run-2", got.LastSeenRunID)
}

func TestMemoryRepo_EvictExcessSkipsUserSource(t *testing.T) {
	db := openTestDB(t)
	repo := &MemoryRepo{DB: db}
	ctx := context.Background()

	now := time.Now()
	// 3 user-source rows (oldest by reinforcement) + 3 synth-source rows.
	// capN=4 means 2 must be evicted; only synth rows should go.
	require.NoError(t, repo.Upsert(ctx, []MemoryFact{
		{ID: "u1", Subject: "u1", Fact: "user old", Category: "x", Confidence: "high", Source: "user", FirstSeen: now, LastReinforced: now.Add(-72 * time.Hour)},
		{ID: "u2", Subject: "u2", Fact: "user mid", Category: "x", Confidence: "high", Source: "user", FirstSeen: now, LastReinforced: now.Add(-48 * time.Hour)},
		{ID: "u3", Subject: "u3", Fact: "user new", Category: "x", Confidence: "high", Source: "user", FirstSeen: now, LastReinforced: now.Add(-24 * time.Hour)},
		{ID: "s1-low-old", Subject: "s1", Fact: "synth", Category: "x", Confidence: "low", Source: "synth", FirstSeen: now, LastReinforced: now.Add(-12 * time.Hour)},
		{ID: "s2-low-mid", Subject: "s2", Fact: "synth", Category: "x", Confidence: "low", Source: "synth", FirstSeen: now, LastReinforced: now.Add(-6 * time.Hour)},
		{ID: "s3-high-recent", Subject: "s3", Fact: "synth", Category: "x", Confidence: "high", Source: "synth", FirstSeen: now, LastReinforced: now},
	}))

	deleted, err := repo.EvictExcess(ctx, 4)
	require.NoError(t, err)
	// Two synth rows should be evicted: the lowest-confidence + oldest by reinforcement.
	require.Len(t, deleted, 2)
	require.Contains(t, deleted, "s1-low-old")
	require.Contains(t, deleted, "s2-low-mid")

	// All three user rows remain visible.
	for _, id := range []string{"u1", "u2", "u3"} {
		got, err := repo.GetByID(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, got, "user fact %s must survive eviction", id)
	}
	// s3 (high-confidence synth) survives.
	got, err := repo.GetByID(ctx, "s3-high-recent")
	require.NoError(t, err)
	require.NotNil(t, got)
}

// Subjects with non-ASCII characters must round-trip through GetBySubject's
// case-insensitive lookup. SQLite's default LOWER() only handles ASCII; the
// repo lowercases the query string in code before the WHERE so callers don't
// need to think about it, but the stored Subject must also be lowercased
// upstream by the consolidator. This test confirms a lowercased non-ASCII
// subject finds the row regardless of the query's case.
func TestMemoryRepo_GetBySubjectNonASCII(t *testing.T) {
	db := openTestDB(t)
	repo := &MemoryRepo{DB: db}
	ctx := context.Background()

	now := time.Now()
	require.NoError(t, repo.Insert(ctx, MemoryFact{
		ID: "andre", Subject: "andré", Fact: "Friend andré likes single malts.",
		Category: "relationship", Confidence: "med", Source: "user", FirstSeen: now, LastReinforced: now,
	}))

	for _, query := range []string{"andré", "  andré  ", "andré\t"} {
		got, err := repo.GetBySubject(ctx, query, false)
		require.NoError(t, err)
		require.NotNil(t, got, "lookup %q must find the row", query)
		require.Equal(t, "andre", got.ID)
	}
}
