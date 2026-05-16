package whatsapp

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestThrottle_NoWaitOnFirstSend(t *testing.T) {
	tr := NewThrottle()
	require.NoError(t, tr.Wait(context.Background(), "jid-a", time.Second))
}

func TestThrottle_WaitsAfterMarkSent(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	tr := NewThrottle()
	tr.SetClock(clock)
	slept := time.Duration(0)
	tr.SetSleep(func(d time.Duration) { slept = d })

	tr.MarkSent("jid-a")
	now = now.Add(time.Second) // 1s elapsed
	require.NoError(t, tr.Wait(context.Background(), "jid-a", 3*time.Second))
	require.Equal(t, 2*time.Second, slept, "should wait the remaining 2s")
}

func TestThrottle_NoWaitWhenIntervalElapsed(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	tr := NewThrottle()
	tr.SetClock(clock)
	called := false
	tr.SetSleep(func(d time.Duration) { called = true })

	tr.MarkSent("jid-a")
	now = now.Add(5 * time.Second)
	require.NoError(t, tr.Wait(context.Background(), "jid-a", 3*time.Second))
	require.False(t, called, "no sleep when interval already elapsed")
}

func TestThrottle_PerJIDIsolation(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	tr := NewThrottle()
	tr.SetClock(clock)
	called := false
	tr.SetSleep(func(d time.Duration) { called = true })

	tr.MarkSent("jid-a")
	require.NoError(t, tr.Wait(context.Background(), "jid-b", 3*time.Second))
	require.False(t, called, "send to jid-b should not be throttled by jid-a's recent send")
}

func TestThrottle_ContextCancel(t *testing.T) {
	tr := NewThrottle()
	tr.SetClock(time.Now)
	// Block forever to force the cancel path.
	tr.SetSleep(func(d time.Duration) { select {} })

	tr.MarkSent("jid-a")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := tr.Wait(ctx, "jid-a", time.Hour)
	require.ErrorIs(t, err, context.Canceled)
}

func TestThrottle_ZeroInterval(t *testing.T) {
	tr := NewThrottle()
	tr.MarkSent("jid-a")
	require.NoError(t, tr.Wait(context.Background(), "jid-a", 0))
}
