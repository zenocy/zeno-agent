package log

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStore_AppendAndQuery(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	_, store, err := Open(dbPath)
	require.NoError(t, err)

	ctx := context.Background()

	e1, err := store.Append(ctx, "test.thing", "unit", map[string]any{"n": 1})
	require.NoError(t, err)
	require.NotEmpty(t, e1.ID)
	require.NotZero(t, e1.TS)

	time.Sleep(2 * time.Millisecond)
	_, err = store.Append(ctx, "test.thing", "unit", map[string]any{"n": 2})
	require.NoError(t, err)
	_, err = store.Append(ctx, "other.kind", "unit", nil)
	require.NoError(t, err)

	all, err := store.ByKind(ctx)
	require.NoError(t, err)
	require.Len(t, all, 3)

	thingies, err := store.ByKind(ctx, "test.thing")
	require.NoError(t, err)
	require.Len(t, thingies, 2)

	latest, err := store.Latest(ctx, "test.thing")
	require.NoError(t, err)
	require.NotNil(t, latest)
	require.Contains(t, string(latest.Payload), `"n":2`)

	missing, err := store.Latest(ctx, "nope")
	require.NoError(t, err)
	require.Nil(t, missing)

	since, err := store.Since(ctx, e1.TS.Add(time.Microsecond))
	require.NoError(t, err)
	require.Len(t, since, 2)
}
