package whatsapptest_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/whatsapp"
	"github.com/zenocy/zeno-v2/internal/whatsapp/whatsapptest"
)

// TestFakeSeam exercises the interface contract: registration, Inject,
// SendText recording, error injection, and Logout cleanup. The Service
// tests later in Phase 1 build on these primitives.
func TestFakeSeam(t *testing.T) {
	f := whatsapptest.New()
	require.False(t, f.HasSession())

	got := []whatsapp.Event{}
	id := f.AddEventHandler(func(ev whatsapp.Event) { got = append(got, ev) })
	require.NotZero(t, id)

	f.InjectMessage(whatsapp.IncomingMessage{
		MessageID: "m1", SenderJID: "1@s.whatsapp.net", Text: "hi", Type: whatsapp.MessageTypeText,
	})
	require.Len(t, got, 1)
	require.IsType(t, whatsapp.EventIncomingMessage{}, got[0])

	f.SetSession("9@s.whatsapp.net", "Jamie")
	require.True(t, f.HasSession())
	assert.Equal(t, "9@s.whatsapp.net", f.OwnJID())
	assert.Equal(t, "Jamie", f.OwnPushName())

	require.NoError(t, f.Connect(context.Background()))
	assert.True(t, f.IsConnected())
	assert.True(t, f.IsLoggedIn())

	require.NoError(t, f.SendText(context.Background(), "10@s.whatsapp.net", "hello"))
	sent := f.SentMessages()
	require.Len(t, sent, 1)
	assert.Equal(t, "hello", sent[0].Text)

	want := errors.New("server unreachable")
	f.SetSendError(want)
	err := f.SendText(context.Background(), "10@s.whatsapp.net", "boom")
	assert.ErrorIs(t, err, want)
	assert.Len(t, f.SentMessages(), 1, "failed send must not be recorded")

	require.True(t, f.RemoveEventHandler(id))
	require.NoError(t, f.Logout(context.Background()))
	assert.False(t, f.HasSession())
	assert.False(t, f.IsConnected())
}
