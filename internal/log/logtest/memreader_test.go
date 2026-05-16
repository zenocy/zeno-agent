package logtest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/log"
)

func TestMemReader_Since(t *testing.T) {
	t0 := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	r := NewMemReader(
		MakeEvent("a", "src", t0.Add(-2*time.Hour), nil),
		MakeEvent("a", "src", t0.Add(2*time.Hour), nil),
		MakeEvent("a", "src", t0.Add(-1*time.Hour), nil),
		MakeEvent("a", "src", t0, nil),
	)

	// Out-of-order insertion: oldest-first ordering must still hold.
	got, err := r.Since(context.Background(), t0)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.True(t, got[0].TS.Before(got[1].TS), "oldest first")
	require.Equal(t, t0, got[0].TS)
	require.Equal(t, t0.Add(2*time.Hour), got[1].TS)
}

func TestMemReader_ByKind(t *testing.T) {
	t0 := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	r := NewMemReader(
		MakeEvent("mail.received", "imap", t0.Add(1*time.Hour), nil),
		MakeEvent("weather.snapshot", "weather", t0, nil),
		MakeEvent("mail.received", "imap", t0.Add(-1*time.Hour), nil),
	)

	got, err := r.ByKind(context.Background(), "mail.received")
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, t0.Add(-1*time.Hour), got[0].TS, "oldest first")

	all, err := r.ByKind(context.Background())
	require.NoError(t, err)
	require.Len(t, all, 3)

	multi, err := r.ByKind(context.Background(), "mail.received", "weather.snapshot")
	require.NoError(t, err)
	require.Len(t, multi, 3)

	none, err := r.ByKind(context.Background(), "nope")
	require.NoError(t, err)
	require.Empty(t, none)
}

func TestMemReader_Latest(t *testing.T) {
	t0 := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	r := NewMemReader(
		MakeEvent("imap.cursor", "imap", t0, nil),
		MakeEvent("imap.cursor", "imap", t0.Add(1*time.Hour), nil),
		MakeEvent("imap.cursor", "imap", t0.Add(-2*time.Hour), nil),
		MakeEvent("other", "x", t0.Add(99*time.Hour), nil),
	)

	got, err := r.Latest(context.Background(), "imap.cursor")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, t0.Add(1*time.Hour), got.TS)

	none, err := r.Latest(context.Background(), "missing")
	require.NoError(t, err)
	require.Nil(t, none)
}

func TestMemReader_AppendAfterConstruction(t *testing.T) {
	r := NewMemReader()
	r.AppendEvent(MakeEvent(log.KindWeatherSnapshot, "weather", time.Now(), map[string]int{"x": 1}))

	got, err := r.ByKind(context.Background(), log.KindWeatherSnapshot)
	require.NoError(t, err)
	require.Len(t, got, 1)
}

func TestMemReader_WriterAndStore(t *testing.T) {
	r := NewMemReader()
	ctx := context.Background()

	e, err := r.Append(ctx, log.KindMailReceived, "imap", map[string]any{"uid": 42})
	require.NoError(t, err)
	require.NotEmpty(t, e.ID)
	require.NotZero(t, e.TS)
	require.Equal(t, log.KindMailReceived, e.Kind)
	require.Contains(t, string(e.Payload), `"uid":42`)

	got, err := r.ByKind(ctx, log.KindMailReceived)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, e.ID, got[0].ID)
}
