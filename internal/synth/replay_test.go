package synth

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/log"
)

func openReplayLog(t *testing.T) log.Store {
	t.Helper()
	_, store, err := log.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	return store
}

func TestSliceReader_DropsEventsPastUntil(t *testing.T) {
	inner := openReplayLog(t)
	ctx := context.Background()

	// Append three mail events; the second's append-time is "now" (test time)
	// so we use an explicit Until cutoff between them.
	for i := 0; i < 3; i++ {
		_, err := inner.Append(ctx, log.KindMailReceived, "imap", map[string]any{"n": i})
		require.NoError(t, err)
		time.Sleep(2 * time.Millisecond)
	}

	all, err := inner.ByKind(ctx, log.KindMailReceived)
	require.NoError(t, err)
	require.Len(t, all, 3)

	until := all[1].TS // include events at-or-before the second one
	reader := &SliceReader{Inner: inner, Until: until}

	clamped, err := reader.ByKind(ctx, log.KindMailReceived)
	require.NoError(t, err)
	require.Len(t, clamped, 2, "third event must be dropped")

	since, err := reader.Since(ctx, all[0].TS)
	require.NoError(t, err)
	require.Len(t, since, 2)

	latest, err := reader.Latest(ctx, log.KindMailReceived)
	require.NoError(t, err)
	require.NotNil(t, latest)
	require.Equal(t, all[1].ID, latest.ID, "latest under cutoff = second event")
}

func TestSliceReader_ZeroUntilPassesThrough(t *testing.T) {
	inner := openReplayLog(t)
	ctx := context.Background()

	_, err := inner.Append(ctx, log.KindMailReceived, "imap", map[string]any{})
	require.NoError(t, err)
	_, err = inner.Append(ctx, log.KindMailReceived, "imap", map[string]any{})
	require.NoError(t, err)

	reader := &SliceReader{Inner: inner} // Until is zero — no clamping
	out, err := reader.ByKind(ctx, log.KindMailReceived)
	require.NoError(t, err)
	require.Len(t, out, 2)
}

func TestSliceReader_LatestNoneReturnsNil(t *testing.T) {
	reader := &SliceReader{Inner: openReplayLog(t), Until: time.Now()}
	out, err := reader.Latest(context.Background(), log.KindMailReceived)
	require.NoError(t, err)
	require.Nil(t, out)
}
