package projection

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/store"
)

func openConcernTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "p.db") + "?_journal_mode=WAL&_foreign_keys=on"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, (&store.ConcernRepo{DB: db}).Migrate())
	require.NoError(t, (&store.ConcernObservationRepo{DB: db}).Migrate())
	return db
}

func seedConcern(t *testing.T, ctx context.Context, repo *store.ConcernRepo, name, state string, lastActive time.Time) string {
	t.Helper()
	id := uuid.New().String()
	require.NoError(t, repo.Insert(ctx, store.Concern{
		ID:           id,
		Name:         name,
		NormName:     store.NormalizeConcernName(name),
		Description:  name + " — desc",
		State:        state,
		Source:       store.ConcernSourceUser,
		Confidence:   1.0,
		LastActiveAt: lastActive,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}))
	return id
}

func TestActiveConcerns_TopByLastActive(t *testing.T) {
	db := openConcernTestDB(t)
	cRepo := &store.ConcernRepo{DB: db}
	ctx := context.Background()

	now := time.Now()
	seedConcern(t, ctx, cRepo, "Old", store.ConcernStateActive, now.Add(-3*time.Hour))
	seedConcern(t, ctx, cRepo, "Newest", store.ConcernStateActive, now)
	seedConcern(t, ctx, cRepo, "Middle", store.ConcernStateActive, now.Add(-1*time.Hour))
	seedConcern(t, ctx, cRepo, "Paused (excluded by default)", store.ConcernStatePaused, now.Add(-30*time.Minute))

	p := ActiveConcerns{Repo: cRepo, Config: ActiveConcernsConfig{Limit: 2}}
	out, err := p.Compute(ctx, nil)
	require.NoError(t, err)
	require.Len(t, out, 2)
	require.Equal(t, "Newest", out[0].Name, "ordered by last_active_at DESC")
	require.Equal(t, "Middle", out[1].Name)
}

func TestActiveConcerns_IncludesCountsWhenAsked(t *testing.T) {
	db := openConcernTestDB(t)
	cRepo := &store.ConcernRepo{DB: db}
	tRepo := &store.ConcernObservationRepo{DB: db}
	ctx := context.Background()

	cID := seedConcern(t, ctx, cRepo, "Counts test", store.ConcernStateActive, time.Now())
	for i := 0; i < 3; i++ {
		require.NoError(t, tRepo.Tag(ctx, store.ConcernObservation{
			ConcernID: cID, EventID: uuid.New().String(),
			Source: store.ConcernTagSourceModel, TaggedAt: time.Now(),
		}))
	}

	p := ActiveConcerns{
		Repo: cRepo, TagRepo: tRepo,
		Config: ActiveConcernsConfig{Limit: 5, IncludeCounts: true},
	}
	out, err := p.Compute(ctx, nil)
	require.NoError(t, err)
	require.Len(t, out, 1)
	require.EqualValues(t, 3, out[0].ObservationCount)
}

func TestActiveConcerns_IncludePausedAppendsAfterActive(t *testing.T) {
	db := openConcernTestDB(t)
	cRepo := &store.ConcernRepo{DB: db}
	ctx := context.Background()

	now := time.Now()
	seedConcern(t, ctx, cRepo, "Active 1", store.ConcernStateActive, now)
	seedConcern(t, ctx, cRepo, "Active 2", store.ConcernStateActive, now.Add(-1*time.Hour))
	seedConcern(t, ctx, cRepo, "Paused 1", store.ConcernStatePaused, now.Add(-2*time.Hour))

	// IncludePaused=true picks up the paused row; default Limit=5 keeps all.
	p := ActiveConcerns{Repo: cRepo, Config: ActiveConcernsConfig{IncludePaused: true}}
	out, err := p.Compute(ctx, nil)
	require.NoError(t, err)
	require.Len(t, out, 3)

	// Limit truncates after the append.
	p.Config.Limit = 2
	out, err = p.Compute(ctx, nil)
	require.NoError(t, err)
	require.Len(t, out, 2)
	// First two should be the active ones (paused appended after).
	for _, c := range out {
		require.Equal(t, store.ConcernStateActive, c.State)
	}
}

func TestActiveConcerns_NilRepoIsSilent(t *testing.T) {
	p := ActiveConcerns{Config: ActiveConcernsConfig{Limit: 5}}
	out, err := p.Compute(context.Background(), nil)
	require.NoError(t, err)
	require.Nil(t, out)
}

// V2.5.0 Phase 5: ReadyToRetire derivation. Boundary day is the
// AutoRetireDays threshold; >= that many days idle marks the concern
// ready to retire. Only `active` qualifies.
func TestActiveConcerns_ReadyToRetire_BoundaryAndOnlyActive(t *testing.T) {
	db := openConcernTestDB(t)
	cRepo := &store.ConcernRepo{DB: db}
	ctx := context.Background()

	now := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	// Below threshold (89 days idle) — false.
	seedConcern(t, ctx, cRepo, "Recently Idle", store.ConcernStateActive,
		now.Add(-89*24*time.Hour))
	// At threshold (90 days idle exact) — true.
	seedConcern(t, ctx, cRepo, "At Threshold", store.ConcernStateActive,
		now.Add(-90*24*time.Hour))
	// Past threshold — true.
	seedConcern(t, ctx, cRepo, "Long Idle", store.ConcernStateActive,
		now.Add(-100*24*time.Hour))
	// Paused but ancient — must stay false (only active qualifies).
	seedConcern(t, ctx, cRepo, "Paused Ancient", store.ConcernStatePaused,
		now.Add(-200*24*time.Hour))

	p := ActiveConcerns{
		Repo: cRepo,
		Config: ActiveConcernsConfig{
			Limit:          10,
			IncludePaused:  true,
			AutoRetireDays: 90,
			Now:            func() time.Time { return now },
		},
	}
	out, err := p.Compute(ctx, nil)
	require.NoError(t, err)
	require.Len(t, out, 4)

	got := map[string]bool{}
	for _, c := range out {
		got[c.Name] = c.ReadyToRetire
	}
	require.False(t, got["Recently Idle"], "89 days < threshold")
	require.True(t, got["At Threshold"], "exactly 90 days >= threshold")
	require.True(t, got["Long Idle"], "100 days past threshold")
	require.False(t, got["Paused Ancient"], "paused never qualifies")
}

func TestConcernsForEvent_FiltersTerminalAndOrders(t *testing.T) {
	db := openConcernTestDB(t)
	cRepo := &store.ConcernRepo{DB: db}
	tRepo := &store.ConcernObservationRepo{DB: db}
	ctx := context.Background()

	now := time.Now()
	active := seedConcern(t, ctx, cRepo, "Active", store.ConcernStateActive, now)
	paused := seedConcern(t, ctx, cRepo, "Paused", store.ConcernStatePaused, now)
	ended := seedConcern(t, ctx, cRepo, "Ended", store.ConcernStateActive, now)
	require.NoError(t, cRepo.Transition(ctx, ended, store.ConcernStateEnded))

	evID := uuid.New().String()
	require.NoError(t, tRepo.Tag(ctx, store.ConcernObservation{ConcernID: active, EventID: evID, Source: store.ConcernTagSourceModel}))
	require.NoError(t, tRepo.Tag(ctx, store.ConcernObservation{ConcernID: paused, EventID: evID, Source: store.ConcernTagSourceModel}))
	require.NoError(t, tRepo.Tag(ctx, store.ConcernObservation{ConcernID: ended, EventID: evID, Source: store.ConcernTagSourceModel}))

	out, err := ConcernsForEvent(ctx, cRepo, tRepo, evID)
	require.NoError(t, err)
	require.Len(t, out, 2, "ended concerns must be filtered out")
	names := []string{out[0].Name, out[1].Name}
	require.Contains(t, names, "Active")
	require.Contains(t, names, "Paused")
}
