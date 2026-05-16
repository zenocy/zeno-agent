package action

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	logp "github.com/zenocy/zeno-v2/internal/log"
)

type fakeResolver struct {
	contact WhatsAppContact
	err     error
}

func (f *fakeResolver) Resolve(ctx context.Context, query string) (WhatsAppContact, error) {
	if f.err != nil {
		return WhatsAppContact{}, f.err
	}
	return f.contact, nil
}

type fakeSender struct {
	gotJID  string
	gotText string
	msgID   string
	err     error
}

func (f *fakeSender) SendText(ctx context.Context, to, text string) error {
	_, err := f.SendTextWithID(ctx, to, text)
	return err
}

func (f *fakeSender) SendTextWithID(ctx context.Context, to, text string) (string, error) {
	f.gotJID = to
	f.gotText = text
	if f.err != nil {
		return "", f.err
	}
	if f.msgID == "" {
		return "fake-msg-id", nil
	}
	return f.msgID, nil
}

type stubThrottle struct {
	waited   bool
	markedAt string
	err      error
}

func (s *stubThrottle) Wait(ctx context.Context, jid string, dur time.Duration) error {
	s.waited = true
	return s.err
}
func (s *stubThrottle) MarkSent(jid string) { s.markedAt = jid }

func openLogStore(t *testing.T) logp.Store {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "log.db")
	_, store, err := logp.OpenWith(dsn, nil)
	require.NoError(t, err)
	return store
}

func TestSendWhatsAppExec_PreviewShape(t *testing.T) {
	resolver := &fakeResolver{contact: WhatsAppContact{
		Name: "Sam Carter", JID: "447700900111@s.whatsapp.net", FactID: "fact-sam", CardDAVUID: "vc-sam",
	}}
	sender := &fakeSender{}
	exec := &SendWhatsAppExec{Deps: WhatsAppDeps{
		Resolver: resolver,
		Sender:   func() WhatsAppSender { return sender },
	}}

	res, err := exec.Execute(context.Background(), ExecCtx{
		Intent: "send_whatsapp",
		Target: map[string]any{
			"recipient": "Sam Carter",
			"message":   "Hello there.",
		},
		Confirm: false,
		Now:     time.Now(),
	})
	require.NoError(t, err)
	require.True(t, res.OK)
	require.True(t, res.NeedsConfirm)
	require.Equal(t, "Sam Carter", res.Preview["to_name"])
	require.Equal(t, false, res.Preview["is_group"])
	require.Equal(t, "Hello there.", res.Preview["body"])
	require.NotContains(t, res.Preview, "chat_jid", "preview must not leak the JID to the UI")
	require.Empty(t, sender.gotText, "preview must not call SendText")
}

func TestSendWhatsAppExec_CommitSendsAndAudits(t *testing.T) {
	store := openLogStore(t)
	resolver := &fakeResolver{contact: WhatsAppContact{
		Name: "Sam Carter", JID: "447700900111@s.whatsapp.net", FactID: "fact-sam",
	}}
	sender := &fakeSender{}
	throttle := &stubThrottle{}
	exec := &SendWhatsAppExec{Deps: WhatsAppDeps{
		Resolver: resolver,
		Sender:   func() WhatsAppSender { return sender },
		Throttle: throttle,
		EventLog: store,
		Reader:   store,
		Logger:   logrus.NewEntry(logrus.New()),
	}}

	res, err := exec.Execute(context.Background(), ExecCtx{
		Intent:   "send_whatsapp",
		Target:   map[string]any{"recipient": "Sam Carter", "message": "Hi."},
		Confirm:  true,
		Now:      time.Now(),
		EventLog: store,
	})
	require.NoError(t, err)
	require.True(t, res.OK)
	require.Equal(t, "447700900111@s.whatsapp.net", sender.gotJID)
	require.Equal(t, "Hi.", sender.gotText)
	require.True(t, throttle.waited)
	require.Equal(t, "447700900111@s.whatsapp.net", throttle.markedAt)
	require.Equal(t, logp.KindWhatsAppMessageSent, res.EventKind)
	require.Equal(t, true, res.EventPayload["proactive"])
	require.Equal(t, "Sam Carter", res.EventPayload["to_name"])
}

func TestSendWhatsAppExec_GroupSendPayload(t *testing.T) {
	store := openLogStore(t)
	resolver := &fakeResolver{contact: WhatsAppContact{
		Name: "Family", JID: "120001@g.us", IsGroup: true, FactID: "fact-fam",
	}}
	sender := &fakeSender{}
	exec := &SendWhatsAppExec{Deps: WhatsAppDeps{
		Resolver: resolver,
		Sender:   func() WhatsAppSender { return sender },
		EventLog: store,
		Reader:   store,
	}}

	res, err := exec.Execute(context.Background(), ExecCtx{
		Target:  map[string]any{"recipient": "Family", "message": "Tonight at 7"},
		Confirm: true,
		Now:     time.Now(),
	})
	require.NoError(t, err)
	require.True(t, res.OK)
	require.Equal(t, true, res.EventPayload["is_group"])
	require.Contains(t, res.Toast, "group Family")
}

func TestSendWhatsAppExec_AmbiguousResolveSurfacesCandidates(t *testing.T) {
	resolver := &fakeResolver{err: &ResolveErrAmbiguous{Query: "sam", Candidates: []string{"Sam Carter", "Sam Other"}}}
	exec := &SendWhatsAppExec{Deps: WhatsAppDeps{
		Resolver: resolver,
		Sender:   func() WhatsAppSender { return &fakeSender{} },
	}}
	res, err := exec.Execute(context.Background(), ExecCtx{
		Target:  map[string]any{"recipient": "sam"},
		Confirm: false,
	})
	require.NoError(t, err)
	require.False(t, res.OK)
	require.Contains(t, res.Toast, "Sam Carter, Sam Other")
}

func TestSendWhatsAppExec_NotFoundResolveBlocksSend(t *testing.T) {
	resolver := &fakeResolver{err: &ResolveErrNotFound{Query: "ghost"}}
	sender := &fakeSender{}
	exec := &SendWhatsAppExec{Deps: WhatsAppDeps{
		Resolver: resolver,
		Sender:   func() WhatsAppSender { return sender },
	}}
	res, err := exec.Execute(context.Background(), ExecCtx{
		Target:  map[string]any{"recipient": "ghost"},
		Confirm: true,
	})
	require.NoError(t, err)
	require.False(t, res.OK)
	require.Empty(t, sender.gotText, "no send when resolver fails")
}

func TestSendWhatsAppExec_SendFailureLogsFailedEvent(t *testing.T) {
	store := openLogStore(t)
	resolver := &fakeResolver{contact: WhatsAppContact{
		Name: "Sam", JID: "1@s.whatsapp.net",
	}}
	sender := &fakeSender{err: errors.New("transport down")}
	exec := &SendWhatsAppExec{Deps: WhatsAppDeps{
		Resolver: resolver,
		Sender:   func() WhatsAppSender { return sender },
		EventLog: store,
		Reader:   store,
	}}

	res, err := exec.Execute(context.Background(), ExecCtx{
		Target:  map[string]any{"recipient": "Sam", "message": "Hi"},
		Confirm: true,
		Now:     time.Now(),
	})
	require.Error(t, err)
	require.False(t, res.OK)

	// The failure event should have been appended.
	failed, _ := store.ByKind(context.Background(), logp.KindWhatsAppMessageFailed)
	require.Len(t, failed, 1)
}

func TestSendWhatsAppExec_DailyCapBlocks(t *testing.T) {
	store := openLogStore(t)
	now := time.Date(2026, 5, 10, 14, 0, 0, 0, time.UTC)

	// Seed three proactive sends today.
	for i := 0; i < 3; i++ {
		_, err := store.Append(context.Background(), logp.KindWhatsAppMessageSent, "send_whatsapp", map[string]any{
			"proactive": true, "to_name": "x",
		})
		require.NoError(t, err)
	}

	resolver := &fakeResolver{contact: WhatsAppContact{Name: "Sam", JID: "1@s.whatsapp.net"}}
	sender := &fakeSender{}
	exec := &SendWhatsAppExec{Deps: WhatsAppDeps{
		Resolver:     resolver,
		Sender:       func() WhatsAppSender { return sender },
		EventLog:     store,
		Reader:       store,
		DailySendCap: 3,
	}}
	res, err := exec.Execute(context.Background(), ExecCtx{
		Target:  map[string]any{"recipient": "Sam", "message": "Hi"},
		Confirm: true,
		Now:     now,
		TZ:      time.UTC,
	})
	require.NoError(t, err)
	require.False(t, res.OK)
	require.Contains(t, res.Toast, "cap (3)")
	require.Empty(t, sender.gotText)
}

func TestSendWhatsAppExec_DailyCapIgnoresReactiveSends(t *testing.T) {
	store := openLogStore(t)
	// Seed reactive (proactive=false) sends — should NOT count.
	for i := 0; i < 5; i++ {
		_, err := store.Append(context.Background(), logp.KindWhatsAppMessageSent, "whatsapp", map[string]any{
			"proactive": false,
		})
		require.NoError(t, err)
	}
	resolver := &fakeResolver{contact: WhatsAppContact{Name: "Sam", JID: "1@s.whatsapp.net"}}
	sender := &fakeSender{}
	exec := &SendWhatsAppExec{Deps: WhatsAppDeps{
		Resolver:     resolver,
		Sender:       func() WhatsAppSender { return sender },
		EventLog:     store,
		Reader:       store,
		DailySendCap: 3,
	}}
	res, err := exec.Execute(context.Background(), ExecCtx{
		Target:  map[string]any{"recipient": "Sam", "message": "Hi"},
		Confirm: true,
		Now:     time.Now(),
		TZ:      time.UTC,
	})
	require.NoError(t, err)
	require.True(t, res.OK)
}

func TestSendWhatsAppExec_MissingRecipient(t *testing.T) {
	resolver := &fakeResolver{}
	exec := &SendWhatsAppExec{Deps: WhatsAppDeps{
		Resolver: resolver,
		Sender:   func() WhatsAppSender { return &fakeSender{} },
	}}
	res, _ := exec.Execute(context.Background(), ExecCtx{Target: map[string]any{}})
	require.False(t, res.OK)
	require.Contains(t, res.Toast, "who to message")
}

func TestSendWhatsAppExec_EmptyComposedFalls(t *testing.T) {
	// LLM = nil → fallback path is exercised; recipient with no event,
	// no mail, no steer falls all the way to "Quick message from Zeno."
	resolver := &fakeResolver{contact: WhatsAppContact{Name: "Sam", JID: "1@s.whatsapp.net"}}
	exec := &SendWhatsAppExec{Deps: WhatsAppDeps{
		Resolver: resolver,
		Sender:   func() WhatsAppSender { return &fakeSender{} },
	}}
	res, err := exec.Execute(context.Background(), ExecCtx{
		Target:  map[string]any{"recipient": "Sam"},
		Confirm: false,
	})
	require.NoError(t, err)
	require.True(t, res.OK)
	require.True(t, res.NeedsConfirm)
	require.NotEmpty(t, res.Preview["body"])
}
