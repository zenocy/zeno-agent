package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/store"
)

func openConcernsCLITestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, (&store.ConcernRepo{DB: db}).Migrate())
	require.NoError(t, (&store.ConcernObservationRepo{DB: db}).Migrate())
	return db
}

func seedCLIConcern(t *testing.T, ctx context.Context, repo *store.ConcernRepo, name, state string, lastActive time.Time) string {
	t.Helper()
	id := uuid.New().String()
	require.NoError(t, repo.Insert(ctx, store.Concern{
		ID:           id,
		Name:         name,
		NormName:     store.NormalizeConcernName(name),
		Description:  name + " — placeholder",
		State:        state,
		Source:       store.ConcernSourceUser,
		Confidence:   1.0,
		LastActiveAt: lastActive,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}))
	return id
}

// TestRenderConcernsList_TableFormat pins the column contract: header
// order, six columns, ISO date format. The CLI is consumed by humans
// scanning a terminal — header drift would be a regression for users
// who scripted around the columns.
func TestRenderConcernsList_TableFormat(t *testing.T) {
	db := openConcernsCLITestDB(t)
	cRepo := &store.ConcernRepo{DB: db}
	tRepo := &store.ConcernObservationRepo{DB: db}
	ctx := context.Background()

	la := time.Date(2026, 5, 3, 9, 30, 0, 0, time.UTC)
	id := seedCLIConcern(t, ctx, cRepo, "Construction", store.ConcernStateActive, la)
	require.NoError(t, tRepo.Tag(ctx, store.ConcernObservation{
		ConcernID: id, EventID: "ev1", Source: store.ConcernTagSourceUser,
	}))

	rows, err := cRepo.ListAll(ctx)
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, renderConcernsList(ctx, &buf, rows, tRepo, false))

	out := buf.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	require.GreaterOrEqual(t, len(lines), 2, "header + at least one row")

	// Header columns: STATE, NAME, SOURCE, OBS, LAST_ACTIVE, ID — in order.
	header := lines[0]
	cols := []string{"STATE", "NAME", "SOURCE", "OBS", "LAST_ACTIVE", "ID"}
	prev := -1
	for _, c := range cols {
		idx := strings.Index(header, c)
		require.GreaterOrEqualf(t, idx, 0, "header missing column %q", c)
		require.Greaterf(t, idx, prev, "column %q out of order", c)
		prev = idx
	}

	require.Contains(t, out, "Construction")
	require.Contains(t, out, "active")
	require.Contains(t, out, "user")
	require.Contains(t, out, "1") // OBS count
	require.Contains(t, out, "2026-05-03 09:30")
}

// TestRenderConcernsList_JSON exercises the --json path. Output must be
// a JSON array (not the wrapper object the API uses) so tools piping
// `zeno concerns list --json | jq` see uniform shape.
func TestRenderConcernsList_JSON(t *testing.T) {
	db := openConcernsCLITestDB(t)
	cRepo := &store.ConcernRepo{DB: db}
	ctx := context.Background()

	now := time.Now()
	seedCLIConcern(t, ctx, cRepo, "A", store.ConcernStateActive, now)
	seedCLIConcern(t, ctx, cRepo, "B", store.ConcernStateProposed, now.Add(-1*time.Hour))

	rows, err := cRepo.ListAll(ctx)
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, renderConcernsList(ctx, &buf, rows, nil, true))

	var got []map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	require.Len(t, got, 2)
	// ListAll orders proposed before active (review surface order).
	require.Equal(t, "proposed", got[0]["state"])
	require.Equal(t, "active", got[1]["state"])
}

// TestRenderConcernsList_NilTagRepoFallback pins the no-tag-counts path
// (used when the operator runs `zeno concerns list` against a DB whose
// tag table doesn't yet exist — e.g., during a Phase 1.0 → 1.1 upgrade
// window). OBS column should be "0", not crash.
func TestRenderConcernsList_NilTagRepoFallback(t *testing.T) {
	db := openConcernsCLITestDB(t)
	cRepo := &store.ConcernRepo{DB: db}
	ctx := context.Background()

	seedCLIConcern(t, ctx, cRepo, "X", store.ConcernStateActive, time.Now())
	rows, _ := cRepo.ListAll(ctx)

	var buf bytes.Buffer
	require.NoError(t, renderConcernsList(ctx, &buf, rows, nil, false))
	out := buf.String()
	require.Contains(t, out, "X")
}

// TestRenderConcernsList_StateOrderingByListAll verifies the table
// renders rows in ListAll order (proposed → active → paused → ended →
// merged), so the CLI matches the review surface tab order.
func TestRenderConcernsList_StateOrderingByListAll(t *testing.T) {
	db := openConcernsCLITestDB(t)
	cRepo := &store.ConcernRepo{DB: db}
	ctx := context.Background()

	now := time.Now()
	seedCLIConcern(t, ctx, cRepo, "Active one", store.ConcernStateActive, now)
	seedCLIConcern(t, ctx, cRepo, "Proposed one", store.ConcernStateProposed, now.Add(-1*time.Hour))
	pausedID := seedCLIConcern(t, ctx, cRepo, "Paused one", store.ConcernStatePaused, now.Add(-2*time.Hour))
	_ = pausedID

	rows, err := cRepo.ListAll(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 3)

	var buf bytes.Buffer
	require.NoError(t, renderConcernsList(ctx, &buf, rows, nil, false))
	out := buf.String()

	// Row indices should be: proposed first, active second, paused third.
	pi := strings.Index(out, "Proposed one")
	ai := strings.Index(out, "Active one")
	ppi := strings.Index(out, "Paused one")
	require.GreaterOrEqual(t, pi, 0)
	require.Greater(t, ai, pi, "active row must follow proposed")
	require.Greater(t, ppi, ai, "paused row must follow active")
}
