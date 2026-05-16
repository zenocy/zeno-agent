package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTraceRepo_Get_MissingReturnsNil(t *testing.T) {
	db := openTestDB(t)
	repo := &TraceRepo{DB: db}
	ctx := context.Background()

	got, err := repo.Get(ctx, "nope")
	require.NoError(t, err)
	require.Nil(t, got, "missing trace must surface as (nil, nil) not gorm.ErrRecordNotFound")
}

func TestTraceRepo_TableName_ReplaySplit(t *testing.T) {
	db := openTestDB(t)
	prod := &TraceRepo{DB: db}
	replay := &TraceRepo{DB: db, Table: "traces_replay"}
	require.NoError(t, replay.Migrate())

	require.Equal(t, "traces", prod.TableName())
	require.Equal(t, "traces_replay", replay.TableName())

	ctx := context.Background()
	require.NoError(t, prod.Create(ctx, Trace{ID: "p1", RunID: "p", Date: "2026-04-25"}))
	require.NoError(t, replay.Create(ctx, Trace{ID: "r1", RunID: "r", Date: "2026-04-25"}))

	// Each repo only sees its own table — the split is the whole point of Table.
	got, err := prod.Get(ctx, "r1")
	require.NoError(t, err)
	require.Nil(t, got, "prod repo must not see replay rows")

	got, err = replay.Get(ctx, "p1")
	require.NoError(t, err)
	require.Nil(t, got, "replay repo must not see prod rows")
}
