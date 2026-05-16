package whatsapp_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/whatsapp"
	"github.com/zenocy/zeno-v2/internal/whatsapp/whatsapptest"
)

// quietLog returns a logger that swallows output during tests.
func quietLog() *logrus.Entry {
	l := logrus.New()
	l.SetLevel(logrus.PanicLevel)
	return logrus.NewEntry(l)
}

// fakeFactory wraps a *whatsapptest.FakeClient. Each call returns a
// fresh fake — mirrors the real adapter where Logout invalidates the
// underlying *whatsmeow.Client.
type fakeFactory struct {
	calls int32
	build func(int) *whatsapptest.FakeClient
	last  *whatsapptest.FakeClient
}

func newFactory(build func(int) *whatsapptest.FakeClient) *fakeFactory {
	return &fakeFactory{build: build}
}

func (f *fakeFactory) factory(_ context.Context) (whatsapp.Client, error) {
	n := atomic.AddInt32(&f.calls, 1)
	c := f.build(int(n))
	f.last = c
	return c, nil
}

func TestService_DisabledIsNoOp(t *testing.T) {
	f := newFactory(func(int) *whatsapptest.FakeClient { return whatsapptest.New() })
	svc := whatsapp.NewService(whatsapp.ServiceConfig{Enabled: false}, f.factory, quietLog())

	require.NoError(t, svc.Start(context.Background()))
	st := svc.Status()
	assert.False(t, st.Enabled)
	assert.False(t, st.Connected)
	assert.Equal(t, int32(0), atomic.LoadInt32(&f.calls), "factory must not run when disabled")

	require.NoError(t, svc.Stop(context.Background()))
}

func TestService_StartWithSessionConnects(t *testing.T) {
	f := newFactory(func(int) *whatsapptest.FakeClient {
		c := whatsapptest.New()
		c.SetSession("9@s.whatsapp.net", "Jamie")
		return c
	})
	svc := whatsapp.NewService(whatsapp.ServiceConfig{Enabled: true}, f.factory, quietLog())

	require.NoError(t, svc.Start(context.Background()))
	st := svc.Status()
	assert.True(t, st.Enabled)
	assert.True(t, st.HasSession)
	assert.True(t, st.Connected)
	assert.True(t, st.LoggedIn)
	assert.Equal(t, "9@s.whatsapp.net", st.OwnJID)
	assert.Equal(t, "Jamie", st.OwnPushName)
}

func TestService_StartWithoutSessionStaysIdle(t *testing.T) {
	f := newFactory(func(int) *whatsapptest.FakeClient { return whatsapptest.New() })
	svc := whatsapp.NewService(whatsapp.ServiceConfig{Enabled: true}, f.factory, quietLog())

	require.NoError(t, svc.Start(context.Background()))
	st := svc.Status()
	assert.False(t, st.HasSession)
	assert.False(t, st.Connected)
}

func TestService_BeginPairStreamsQRAndCompletes(t *testing.T) {
	fake := whatsapptest.New()
	f := newFactory(func(int) *whatsapptest.FakeClient { return fake })
	svc := whatsapp.NewService(whatsapp.ServiceConfig{Enabled: true}, f.factory, quietLog())
	require.NoError(t, svc.Start(context.Background()))

	ch, err := svc.BeginPair(context.Background())
	require.NoError(t, err)
	require.NotNil(t, ch)

	go func() {
		fake.InjectQR(whatsapp.QREvent{Event: "code", Code: "abc"})
		fake.InjectQR(whatsapp.QREvent{Event: "code", Code: "def"})
		fake.SetSession("12@s.whatsapp.net", "Jamie")
		fake.InjectQR(whatsapp.QREvent{Event: "success"})
		fake.CloseQR()
	}()

	codes := []string{}
	successSeen := false
	for ev := range ch {
		switch ev.Event {
		case "code":
			codes = append(codes, ev.Code)
		case "success":
			successSeen = true
		}
	}
	assert.Equal(t, []string{"abc", "def"}, codes)
	assert.True(t, successSeen)
}

func TestService_BeginPairRejectsWhenAlreadyPaired(t *testing.T) {
	f := newFactory(func(int) *whatsapptest.FakeClient {
		c := whatsapptest.New()
		c.SetSession("9@s.whatsapp.net", "Jamie")
		return c
	})
	svc := whatsapp.NewService(whatsapp.ServiceConfig{Enabled: true}, f.factory, quietLog())
	require.NoError(t, svc.Start(context.Background()))

	_, err := svc.BeginPair(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already paired")
}

func TestService_UnlinkAndRepairWithoutRestart(t *testing.T) {
	// Each factory call returns a fresh fake — mirrors real life where
	// Logout invalidates the underlying *whatsmeow.Client.
	f := newFactory(func(call int) *whatsapptest.FakeClient {
		c := whatsapptest.New()
		if call == 1 {
			// First instance is paired (resumed at boot).
			c.SetSession("9@s.whatsapp.net", "Jamie")
		}
		// Subsequent instances are fresh / unpaired.
		return c
	})
	svc := whatsapp.NewService(whatsapp.ServiceConfig{Enabled: true}, f.factory, quietLog())
	require.NoError(t, svc.Start(context.Background()))
	assert.True(t, svc.Status().HasSession)

	require.NoError(t, svc.Unlink(context.Background()))
	st := svc.Status()
	assert.False(t, st.HasSession, "unlink must clear session")
	assert.False(t, st.Connected)

	// Re-pair on the SAME Service instance — exercises the factory's
	// re-init path. BeginPair must succeed without a process restart.
	ch, err := svc.BeginPair(context.Background())
	require.NoError(t, err)
	require.NotNil(t, ch)

	require.GreaterOrEqual(t, atomic.LoadInt32(&f.calls), int32(2), "Unlink must produce a fresh client")
}

func TestService_LoggedOutFlipsStatus(t *testing.T) {
	fake := whatsapptest.New()
	fake.SetSession("9@s.whatsapp.net", "Jamie")
	f := newFactory(func(int) *whatsapptest.FakeClient { return fake })
	svc := whatsapp.NewService(whatsapp.ServiceConfig{Enabled: true}, f.factory, quietLog())
	require.NoError(t, svc.Start(context.Background()))

	fake.Inject(whatsapp.EventLoggedOut{Reason: "device_removed"})
	st := svc.Status()
	assert.False(t, st.LoggedIn)
	assert.Contains(t, st.LastError, "device_removed")
}

func TestService_DisconnectedRecordsError(t *testing.T) {
	fake := whatsapptest.New()
	fake.SetSession("9@s.whatsapp.net", "Jamie")
	f := newFactory(func(int) *whatsapptest.FakeClient { return fake })
	svc := whatsapp.NewService(whatsapp.ServiceConfig{Enabled: true}, f.factory, quietLog())
	require.NoError(t, svc.Start(context.Background()))

	fake.Inject(whatsapp.EventDisconnected{Err: errors.New("network gone")})
	st := svc.Status()
	assert.Equal(t, "network gone", st.LastError)
}

func TestService_PairSuccessRecordsTimestamp(t *testing.T) {
	fake := whatsapptest.New()
	f := newFactory(func(int) *whatsapptest.FakeClient { return fake })
	svc := whatsapp.NewService(whatsapp.ServiceConfig{Enabled: true}, f.factory, quietLog())

	frozen := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return frozen })
	require.NoError(t, svc.Start(context.Background()))

	fake.Inject(whatsapp.EventPairSuccess{JID: "9@s.whatsapp.net"})
	st := svc.Status()
	assert.Equal(t, frozen, st.PairedAt)
	assert.Equal(t, "9@s.whatsapp.net", st.OwnJID)
}

// TestService_BeginPairCancelsPrior guards the React-StrictMode dev
// double-effect: a second BeginPair while one is already running must
// pre-empt the first (cancel its pairCtx, wait for its goroutine to
// release the slot) and return a fresh stream — not 409 the caller.
func TestService_BeginPairCancelsPrior(t *testing.T) {
	fake := whatsapptest.New()
	f := newFactory(func(int) *whatsapptest.FakeClient { return fake })
	svc := whatsapp.NewService(whatsapp.ServiceConfig{Enabled: true}, f.factory, quietLog())
	require.NoError(t, svc.Start(context.Background()))

	first, err := svc.BeginPair(context.Background())
	require.NoError(t, err)
	require.NotNil(t, first)

	// Second call: pre-empts the first and returns its own stream.
	second, err := svc.BeginPair(context.Background())
	require.NoError(t, err)
	require.NotNil(t, second)

	// First stream must be drained — the second's pre-empt cancelled
	// its pairCtx, the goroutine exited, the stream closed.
	for range first {
	}

	// Drain the second so its defers run cleanly.
	fake.CloseQR()
	for range second {
	}
}

// TestService_UnlinkCancelsInFlightPair: an Unlink during pairing must
// stop the pair flow first (so the goroutine releases its reference to
// the *Client) and then proceed with logout. No error.
func TestService_UnlinkCancelsInFlightPair(t *testing.T) {
	fake := whatsapptest.New()
	f := newFactory(func(int) *whatsapptest.FakeClient { return fake })
	svc := whatsapp.NewService(whatsapp.ServiceConfig{Enabled: true}, f.factory, quietLog())
	require.NoError(t, svc.Start(context.Background()))

	ch, err := svc.BeginPair(context.Background())
	require.NoError(t, err)
	require.NotNil(t, ch)

	require.NoError(t, svc.Unlink(context.Background()))

	// The pair stream must have been closed by the pre-empt; ranging
	// over it must terminate without us injecting CloseQR. Bound the
	// drain with a timeout so a hang reads as a test failure rather
	// than a deadlock.
	drained := make(chan struct{})
	go func() {
		for range ch {
		}
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("pair stream did not close after Unlink")
	}
}
