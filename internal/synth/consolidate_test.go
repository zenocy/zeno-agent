package synth

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/llm"
	"github.com/zenocy/zeno-v2/internal/store"
)

func openConsolidateDB(t *testing.T) *store.MemoryRepo {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_journal_mode=WAL&_foreign_keys=on"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	repo := &store.MemoryRepo{DB: db}
	require.NoError(t, repo.Migrate())
	return repo
}

func TestConsolidate_NewCandidate_InsertsAtLow(t *testing.T) {
	repo := openConsolidateDB(t)
	ctx := context.Background()

	delta, err := Consolidate(ctx, ConsolidateDeps{Repo: repo}, ConsolidateConfig{}, "run-1", []llm.MemoryCandidate{
		{Subject: "partner", Predicate: "Sam", Raw: "remember: partner: Sam"},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"partner"}, delta.Added)
	require.Empty(t, delta.Incremented)
	require.Empty(t, delta.Promoted)

	got, err := repo.GetBySubject(ctx, "partner", false)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "low", got.Confidence)
	require.Equal(t, 1, got.EvidenceCount)
	require.Equal(t, "synth", got.Source)
	require.Equal(t, "run-1", got.FirstSeenRunID)
	require.Equal(t, "run-1", got.LastSeenRunID)
	require.Equal(t, "Sam", got.Fact)
}

func TestConsolidate_ExistingSubject_Increments(t *testing.T) {
	repo := openConsolidateDB(t)
	ctx := context.Background()

	_, err := Consolidate(ctx, ConsolidateDeps{Repo: repo}, ConsolidateConfig{}, "run-1", []llm.MemoryCandidate{
		{Subject: "partner", Predicate: "Sam"},
	})
	require.NoError(t, err)

	// Different runID — should reinforce.
	delta, err := Consolidate(ctx, ConsolidateDeps{Repo: repo}, ConsolidateConfig{}, "run-2", []llm.MemoryCandidate{
		{Subject: "partner", Predicate: "Sam"},
	})
	require.NoError(t, err)
	require.Empty(t, delta.Added, "second observation must not insert again")
	require.Equal(t, []string{"partner"}, delta.Incremented)
	require.Empty(t, delta.Promoted, "evidence count = 2 is below promotion threshold")

	got, _ := repo.GetBySubject(ctx, "partner", false)
	require.Equal(t, 2, got.EvidenceCount)
	require.Equal(t, "low", got.Confidence)
	require.Equal(t, "run-2", got.LastSeenRunID)
}

func TestConsolidate_PromotesToMedAtThree(t *testing.T) {
	repo := openConsolidateDB(t)
	ctx := context.Background()

	for i, runID := range []string{"r1", "r2"} {
		_, err := Consolidate(ctx, ConsolidateDeps{Repo: repo}, ConsolidateConfig{}, runID, []llm.MemoryCandidate{
			{Subject: "runs", Predicate: "Tue/Thu"},
		})
		require.NoError(t, err, "iter %d", i)
	}
	got, _ := repo.GetBySubject(ctx, "runs", false)
	require.Equal(t, 2, got.EvidenceCount)
	require.Equal(t, "low", got.Confidence)

	// Third observation crosses the threshold.
	delta, err := Consolidate(ctx, ConsolidateDeps{Repo: repo}, ConsolidateConfig{}, "r3", []llm.MemoryCandidate{
		{Subject: "runs", Predicate: "Tue/Thu"},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"runs"}, delta.Promoted)

	got, _ = repo.GetBySubject(ctx, "runs", false)
	require.Equal(t, 3, got.EvidenceCount)
	require.Equal(t, "med", got.Confidence)
}

func TestConsolidate_PromotesToHighAtSeven(t *testing.T) {
	repo := openConsolidateDB(t)
	ctx := context.Background()

	for i := 1; i <= 7; i++ {
		runID := "r" + string(rune('0'+i))
		delta, err := Consolidate(ctx, ConsolidateDeps{Repo: repo}, ConsolidateConfig{}, runID, []llm.MemoryCandidate{
			{Subject: "anniversary", Predicate: "May 7"},
		})
		require.NoError(t, err)
		switch i {
		case 1:
			require.Equal(t, []string{"anniversary"}, delta.Added)
		case 3:
			require.Equal(t, []string{"anniversary"}, delta.Promoted, "promote to med on third evidence")
		case 7:
			require.Equal(t, []string{"anniversary"}, delta.Promoted, "promote to high on seventh evidence")
		}
	}
	got, _ := repo.GetBySubject(ctx, "anniversary", false)
	require.Equal(t, 7, got.EvidenceCount)
	require.Equal(t, "high", got.Confidence)
}

func TestConsolidate_DeniedSubject_Dropped(t *testing.T) {
	repo := openConsolidateDB(t)
	ctx := context.Background()

	// Insert and immediately soft-delete.
	now := time.Now()
	require.NoError(t, repo.Insert(ctx, store.MemoryFact{
		ID: "partner-x", Subject: "partner", Fact: "old", Category: "x",
		Confidence: "med", Source: "user", FirstSeen: now, LastReinforced: now,
	}))
	require.NoError(t, repo.SoftDelete(ctx, "partner-x"))

	delta, err := Consolidate(ctx, ConsolidateDeps{Repo: repo}, ConsolidateConfig{}, "run-1", []llm.MemoryCandidate{
		{Subject: "partner", Predicate: "Sam"},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"partner"}, delta.Skipped, "denylisted subject must not re-emerge")
	require.Empty(t, delta.Added)
	require.Empty(t, delta.Incremented)

	visible, _ := repo.GetBySubject(ctx, "partner", false)
	require.Nil(t, visible, "denylist still hides the row from default reads")
}

func TestConsolidate_IdempotentOnRunID(t *testing.T) {
	repo := openConsolidateDB(t)
	ctx := context.Background()

	cands := []llm.MemoryCandidate{{Subject: "runs", Predicate: "Tue/Thu"}}

	d1, err := Consolidate(ctx, ConsolidateDeps{Repo: repo}, ConsolidateConfig{}, "r1", cands)
	require.NoError(t, err)
	require.Equal(t, []string{"runs"}, d1.Added)

	// Same runID, same candidates → must skip without double-counting.
	d2, err := Consolidate(ctx, ConsolidateDeps{Repo: repo}, ConsolidateConfig{}, "r1", cands)
	require.NoError(t, err)
	require.Equal(t, []string{"runs"}, d2.SkippedSeen)
	require.Empty(t, d2.Incremented)
	require.Empty(t, d2.Added)

	got, _ := repo.GetBySubject(ctx, "runs", false)
	require.Equal(t, 1, got.EvidenceCount, "same-runID re-run must not double-count")
}

func TestConsolidate_DifferentRunID_DoesIncrement(t *testing.T) {
	repo := openConsolidateDB(t)
	ctx := context.Background()

	_, err := Consolidate(ctx, ConsolidateDeps{Repo: repo}, ConsolidateConfig{}, "r1", []llm.MemoryCandidate{
		{Subject: "commute", Predicate: "Bike when dry"},
	})
	require.NoError(t, err)
	d2, err := Consolidate(ctx, ConsolidateDeps{Repo: repo}, ConsolidateConfig{}, "r2", []llm.MemoryCandidate{
		{Subject: "commute", Predicate: "Bike when dry"},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"commute"}, d2.Incremented)

	got, _ := repo.GetBySubject(ctx, "commute", false)
	require.Equal(t, 2, got.EvidenceCount)
}

func TestConsolidate_EvictionAtCap_KeepsUserSource(t *testing.T) {
	repo := openConsolidateDB(t)
	ctx := context.Background()

	now := time.Now()
	// Two user-source rows (oldest by reinforcement) + two synth-source rows.
	require.NoError(t, repo.Upsert(ctx, []store.MemoryFact{
		{ID: "u1", Subject: "user-a", Fact: "x", Category: "x", Confidence: "high", Source: "user", FirstSeen: now, LastReinforced: now.Add(-72 * time.Hour)},
		{ID: "u2", Subject: "user-b", Fact: "x", Category: "x", Confidence: "high", Source: "user", FirstSeen: now, LastReinforced: now.Add(-48 * time.Hour)},
		{ID: "s1", Subject: "synth-a", Fact: "x", Category: "x", Confidence: "low", Source: "synth", FirstSeen: now, LastReinforced: now.Add(-24 * time.Hour)},
		{ID: "s2", Subject: "synth-b", Fact: "x", Category: "x", Confidence: "low", Source: "synth", FirstSeen: now, LastReinforced: now.Add(-12 * time.Hour)},
	}))

	// Insert one new candidate at cap=4 → 5 total → 1 must evict, must be a synth row.
	delta, err := Consolidate(ctx, ConsolidateDeps{Repo: repo}, ConsolidateConfig{MaxFacts: 4}, "run-1", []llm.MemoryCandidate{
		{Subject: "new", Predicate: "fresh"},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"new"}, delta.Added)
	require.Len(t, delta.Evicted, 1, "one row must be evicted to honor cap=4")
	require.Equal(t, "s1", delta.Evicted[0], "stalest synth row evicted first")

	// User rows still visible.
	for _, id := range []string{"u1", "u2"} {
		got, err := repo.GetByID(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, got, "user fact %s must survive eviction", id)
	}
}

func TestConsolidate_EvictionAtCap_DropsLowConfidenceFirst(t *testing.T) {
	repo := openConsolidateDB(t)
	ctx := context.Background()

	now := time.Now()
	require.NoError(t, repo.Upsert(ctx, []store.MemoryFact{
		{ID: "high-old", Subject: "high-old", Fact: "x", Category: "x", Confidence: "high", Source: "synth", FirstSeen: now, LastReinforced: now.Add(-72 * time.Hour)},
		{ID: "low-fresh", Subject: "low-fresh", Fact: "x", Category: "x", Confidence: "low", Source: "synth", FirstSeen: now, LastReinforced: now},
		{ID: "med-mid", Subject: "med-mid", Fact: "x", Category: "x", Confidence: "med", Source: "synth", FirstSeen: now, LastReinforced: now.Add(-24 * time.Hour)},
	}))
	delta, err := Consolidate(ctx, ConsolidateDeps{Repo: repo}, ConsolidateConfig{MaxFacts: 2}, "run-1", nil)
	require.NoError(t, err)
	require.Len(t, delta.Evicted, 1, "trimmed from 3 to cap=2 ⇒ one eviction")
	require.Equal(t, "low-fresh", delta.Evicted[0],
		"low-confidence is evicted before older but higher-confidence rows")
}

func TestConsolidate_DropsExcessCandidates(t *testing.T) {
	repo := openConsolidateDB(t)
	ctx := context.Background()

	cands := []llm.MemoryCandidate{
		{Subject: "a", Predicate: "1"},
		{Subject: "b", Predicate: "2"},
		{Subject: "c", Predicate: "3"},
		{Subject: "d", Predicate: "4"}, // beyond MaxPerRun=3
		{Subject: "e", Predicate: "5"},
	}
	delta, err := Consolidate(ctx, ConsolidateDeps{Repo: repo}, ConsolidateConfig{}, "run-1", cands)
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b", "c"}, delta.Added,
		"only first 3 candidates processed; rest dropped at consolidator boundary")
}

func TestConsolidate_NilRepoReturnsError(t *testing.T) {
	_, err := Consolidate(context.Background(), ConsolidateDeps{}, ConsolidateConfig{}, "run-1", nil)
	require.Error(t, err)
}
