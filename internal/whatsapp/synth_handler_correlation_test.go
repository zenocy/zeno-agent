package whatsapp_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	zlog "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/store"
	"github.com/zenocy/zeno-v2/internal/synth"
	"github.com/zenocy/zeno-v2/internal/whatsapp"
	"github.com/zenocy/zeno-v2/internal/whatsapp/whatsapptest"
)

type captureNotifier struct {
	mu      sync.Mutex
	signals []whatsapp.ReplyReceivedSignal
}

func (c *captureNotifier) Notify(ctx context.Context, sig whatsapp.ReplyReceivedSignal) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.signals = append(c.signals, sig)
	return nil
}

func openCorrelationDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "store.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	return db
}

// TestSynthHandler_AssistantReplySuppressesAutoReply verifies V2.13.0
// correlation: an inbound DM matching an open ExpectedReply skips the
// reactive Ask path, fires the reply-received notifier, and emits a
// Suppressed audit event. The fake sender must NOT receive a message.
func TestSynthHandler_AssistantReplySuppressesAutoReply(t *testing.T) {
	db := openCorrelationDB(t)
	expectedRepo := &store.ExpectedReplyRepo{DB: db}
	require.NoError(t, expectedRepo.Migrate())

	chatJID := "447700900111@s.whatsapp.net"
	now := time.Date(2026, 5, 10, 20, 0, 0, 0, time.UTC)
	openRow := &store.ExpectedReply{
		ChatJID:       chatJID,
		SentAt:        now.Add(-30 * time.Minute),
		ExpiresAt:     now.Add(23 * time.Hour),
		ContextKind:   "event",
		ContextID:     "evt-dinner",
		RecipientName: "Dana",
		DraftBody:     "Hi Dana — Jamie asked me to confirm dinner tonight.",
	}
	require.NoError(t, expectedRepo.Insert(context.Background(), openRow))

	fake := whatsapptest.New()
	fake.SetSession("9@s.whatsapp.net", "Jamie")

	askCalled := false
	ask := func(ctx context.Context, q string, conv *synth.ConversationContext) (synth.Card, error) {
		askCalled = true
		return synth.Card{Speech: "should not be sent"}, nil
	}

	notifier := &captureNotifier{}
	w := &memWriter{}
	h := &whatsapp.SynthHandler{
		Ask:             ask,
		Client:          func() whatsapp.Client { return fake },
		EventLog:        w,
		Now:             func() time.Time { return now },
		Sleep:           func(time.Duration) {},
		SendBackoffs:    []time.Duration{0, time.Microsecond, time.Microsecond},
		SendDeadline:    time.Second,
		Logger:          quietLog(),
		ExpectedReplies: expectedRepo,
		ReplyReceived:   notifier,
	}

	dec := whatsapp.Decision{
		Action:     whatsapp.ActionProcess,
		ChatJID:    chatJID,
		SenderJID:  chatJID,
		SenderName: "Dana",
		IsDM:       true,
		Text:       "Yes, 7pm works",
		MessageID:  "INBOUND-1",
		Timestamp:  now,
	}
	require.NoError(t, h.Handle(context.Background(), dec))

	// 1. Reactive Ask path was NOT invoked.
	assert.False(t, askCalled, "Ask should not run when an ExpectedReply matches")
	// 2. Fake sender saw no outbound traffic.
	assert.Empty(t, fake.SentMessages(), "no auto-reply should be sent on correlated reply")
	// 3. Notifier got exactly one signal with the expected payload.
	require.Len(t, notifier.signals, 1)
	sig := notifier.signals[0]
	assert.Equal(t, chatJID, sig.ChatJID)
	assert.Equal(t, "Dana", sig.RecipientName)
	assert.Equal(t, "evt-dinner", sig.ContextID)
	assert.Equal(t, "Yes, 7pm works", sig.ReplyText)
	// 4. Audit log shows received + suppressed (no sent).
	kinds := w.kinds()
	assert.Equal(t, []string{zlog.KindWhatsAppMessageRecv, zlog.KindWhatsAppMessageSuppressed}, kinds)
	// 5. Row was marked resolved.
	open, err := expectedRepo.OpenForJID(context.Background(), chatJID, now.Add(time.Minute))
	require.NoError(t, err)
	assert.Nil(t, open, "row should be resolved")
}

// TestSynthHandler_NoCorrelationFallsThrough verifies that an inbound
// DM with no matching ExpectedReply still routes through the reactive
// Ask path (legacy V2.7 behavior).
func TestSynthHandler_NoCorrelationFallsThrough(t *testing.T) {
	db := openCorrelationDB(t)
	expectedRepo := &store.ExpectedReplyRepo{DB: db}
	require.NoError(t, expectedRepo.Migrate())

	fake := whatsapptest.New()
	fake.SetSession("9@s.whatsapp.net", "Jamie")

	askCalled := false
	ask := func(ctx context.Context, q string, conv *synth.ConversationContext) (synth.Card, error) {
		askCalled = true
		return synth.Card{
			Title:  "Quick acknowledgement",
			Sub:    "Got it.",
			Speech: "Got it, thanks.",
		}, nil
	}

	notifier := &captureNotifier{}
	w := &memWriter{}
	h := &whatsapp.SynthHandler{
		Ask:             ask,
		Client:          func() whatsapp.Client { return fake },
		EventLog:        w,
		Now:             time.Now,
		Sleep:           func(time.Duration) {},
		SendBackoffs:    []time.Duration{0, time.Microsecond, time.Microsecond},
		SendDeadline:    time.Second,
		Logger:          quietLog(),
		ExpectedReplies: expectedRepo,
		ReplyReceived:   notifier,
	}

	dec := whatsapp.Decision{
		Action:     whatsapp.ActionProcess,
		ChatJID:    "1@s.whatsapp.net",
		SenderJID:  "1@s.whatsapp.net",
		SenderName: "Random",
		IsDM:       true,
		Text:       "what's on today?",
		MessageID:  "abc",
		Timestamp:  time.Now(),
	}
	require.NoError(t, h.Handle(context.Background(), dec))

	assert.True(t, askCalled, "Ask should run when no ExpectedReply matches")
	assert.Len(t, fake.SentMessages(), 1, "auto-reply should be sent")
	assert.Empty(t, notifier.signals, "no reply-received signal in fall-through path")

	kinds := w.kinds()
	assert.Equal(t, []string{zlog.KindWhatsAppMessageRecv, zlog.KindWhatsAppMessageSent}, kinds)
}
