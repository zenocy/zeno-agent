package whatsapp_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	zlog "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/synth"
	"github.com/zenocy/zeno-v2/internal/whatsapp"
	"github.com/zenocy/zeno-v2/internal/whatsapp/whatsapptest"
)

// memWriter is a minimal log.Writer for tests; it appends Events into a
// slice we can assert on.
type memWriter struct {
	mu     sync.Mutex
	events []zlog.Event
}

func (w *memWriter) Append(ctx context.Context, kind, source string, payload any) (zlog.Event, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	ev := zlog.Event{Kind: kind, Source: source}
	w.events = append(w.events, ev)
	return ev, nil
}

func (w *memWriter) kinds() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, len(w.events))
	for i, e := range w.events {
		out[i] = e.Kind
	}
	return out
}

func makeHandler(t *testing.T, fake *whatsapptest.FakeClient, ask whatsapp.AskFunc) (*whatsapp.SynthHandler, *memWriter) {
	t.Helper()
	w := &memWriter{}
	h := &whatsapp.SynthHandler{
		Ask:          ask,
		Client:       func() whatsapp.Client { return fake },
		EventLog:     w,
		Now:          time.Now,
		Sleep:        func(time.Duration) {}, // skip real waits
		SendBackoffs: []time.Duration{0, time.Microsecond, time.Microsecond},
		SendDeadline: time.Second,
		Logger:       quietLog(),
	}
	return h, w
}

func TestSynthHandler_DMHappyPath(t *testing.T) {
	fake := whatsapptest.New()
	fake.SetSession("9@s.whatsapp.net", "Jamie")
	ask := func(ctx context.Context, q string, conv *synth.ConversationContext) (synth.Card, error) {
		require.NotNil(t, conv)
		assert.True(t, conv.IsDM)
		assert.Equal(t, "Jamie", conv.SenderName)
		return synth.Card{
			Title: "Today is busy",
			Sub:   "Three meetings starting at 10:00.",
			Speech: "You've got three meetings today, kicking off at 10am.",
		}, nil
	}
	h, w := makeHandler(t, fake, ask)

	dec := whatsapp.Decision{
		Action:     whatsapp.ActionProcess,
		ChatJID:    "1@s.whatsapp.net",
		SenderJID:  "1@s.whatsapp.net",
		SenderName: "Jamie",
		IsDM:       true,
		Text:       "what's on today?",
		MessageID:  "abc",
		Timestamp:  time.Now(),
	}
	require.NoError(t, h.Handle(context.Background(), dec))

	sent := fake.SentMessages()
	require.Len(t, sent, 1)
	assert.Equal(t, "1@s.whatsapp.net", sent[0].To)
	assert.Equal(t, "You've got three meetings today, kicking off at 10am.", sent[0].Text)

	kinds := w.kinds()
	assert.Equal(t, []string{zlog.KindWhatsAppMessageRecv, zlog.KindWhatsAppMessageSent}, kinds)
}

func TestSynthHandler_PreCannedReplySkipsAsk(t *testing.T) {
	fake := whatsapptest.New()
	fake.SetSession("9@s.whatsapp.net", "Jamie")
	askCalled := false
	ask := func(ctx context.Context, q string, conv *synth.ConversationContext) (synth.Card, error) {
		askCalled = true
		return synth.Card{}, nil
	}
	h, _ := makeHandler(t, fake, ask)

	dec := whatsapp.Decision{
		Action:         whatsapp.ActionProcess,
		ChatJID:        "1@s.whatsapp.net",
		SenderJID:      "1@s.whatsapp.net",
		IsDM:           true,
		PreCannedReply: "I can't see images yet.",
		MessageID:      "img1",
		Timestamp:      time.Now(),
	}
	require.NoError(t, h.Handle(context.Background(), dec))
	assert.False(t, askCalled, "pre-canned reply must bypass synth")

	sent := fake.SentMessages()
	require.Len(t, sent, 1)
	assert.Equal(t, "I can't see images yet.", sent[0].Text)
}

func TestSynthHandler_FallsBackToSubWhenSpeechEmpty(t *testing.T) {
	fake := whatsapptest.New()
	fake.SetSession("9@s.whatsapp.net", "Jamie")
	ask := func(ctx context.Context, q string, conv *synth.ConversationContext) (synth.Card, error) {
		return synth.Card{
			Title: "Title only",
			Sub:   "Three meetings starting at 10:00.",
			// Speech intentionally empty
		}, nil
	}
	h, _ := makeHandler(t, fake, ask)

	dec := whatsapp.Decision{
		Action:    whatsapp.ActionProcess,
		ChatJID:   "1@s.whatsapp.net",
		SenderJID: "1@s.whatsapp.net",
		IsDM:      true,
		Text:      "what's on?",
		MessageID: "abc",
	}
	require.NoError(t, h.Handle(context.Background(), dec))

	sent := fake.SentMessages()
	require.Len(t, sent, 1)
	assert.Equal(t, "Three meetings starting at 10:00.", sent[0].Text)
}

func TestSynthHandler_RetriesOnTransientSendFailure(t *testing.T) {
	fake := whatsapptest.New()
	fake.SetSession("9@s.whatsapp.net", "Jamie")

	var attempts int
	fake.SetSendHook(func(to, text string) error {
		attempts++
		if attempts < 3 {
			return errors.New("transient")
		}
		return nil
	})

	ask := func(ctx context.Context, q string, conv *synth.ConversationContext) (synth.Card, error) {
		return synth.Card{Speech: "ok"}, nil
	}
	h, w := makeHandler(t, fake, ask)

	dec := whatsapp.Decision{
		Action: whatsapp.ActionProcess, ChatJID: "1@s.whatsapp.net", SenderJID: "1@s.whatsapp.net", IsDM: true,
		Text: "ping", MessageID: "abc",
	}
	require.NoError(t, h.Handle(context.Background(), dec))
	assert.Equal(t, 3, attempts, "must retry until success")
	assert.Contains(t, w.kinds(), zlog.KindWhatsAppMessageSent)
}

func TestSynthHandler_RetryExhaustionRecordsFailure(t *testing.T) {
	fake := whatsapptest.New()
	fake.SetSession("9@s.whatsapp.net", "Jamie")
	fake.SetSendError(errors.New("offline"))

	ask := func(ctx context.Context, q string, conv *synth.ConversationContext) (synth.Card, error) {
		return synth.Card{Speech: "ok"}, nil
	}
	h, w := makeHandler(t, fake, ask)

	dec := whatsapp.Decision{
		Action: whatsapp.ActionProcess, ChatJID: "1@s.whatsapp.net", SenderJID: "1@s.whatsapp.net", IsDM: true,
		Text: "ping", MessageID: "abc",
	}
	err := h.Handle(context.Background(), dec)
	require.Error(t, err)

	kinds := w.kinds()
	assert.Contains(t, kinds, zlog.KindWhatsAppMessageRecv)
	assert.Contains(t, kinds, zlog.KindWhatsAppMessageFailed)
	assert.NotContains(t, kinds, zlog.KindWhatsAppMessageSent)
}

func TestSynthHandler_AskErrorRecordsFailureNoSend(t *testing.T) {
	fake := whatsapptest.New()
	fake.SetSession("9@s.whatsapp.net", "Jamie")
	ask := func(ctx context.Context, q string, conv *synth.ConversationContext) (synth.Card, error) {
		return synth.Card{}, fmt.Errorf("llm down")
	}
	h, w := makeHandler(t, fake, ask)

	dec := whatsapp.Decision{
		Action: whatsapp.ActionProcess, ChatJID: "1@s.whatsapp.net", SenderJID: "1@s.whatsapp.net", IsDM: true,
		Text: "ping", MessageID: "abc",
	}
	require.Error(t, h.Handle(context.Background(), dec))

	kinds := w.kinds()
	assert.Contains(t, kinds, zlog.KindWhatsAppMessageFailed)
	assert.Empty(t, fake.SentMessages())
}

func TestSynthHandler_DropDecisionIsNoOp(t *testing.T) {
	fake := whatsapptest.New()
	fake.SetSession("9@s.whatsapp.net", "Jamie")
	called := false
	ask := func(ctx context.Context, q string, conv *synth.ConversationContext) (synth.Card, error) {
		called = true
		return synth.Card{}, nil
	}
	h, w := makeHandler(t, fake, ask)

	dec := whatsapp.Decision{Action: whatsapp.ActionDrop, ChatJID: "x"}
	require.NoError(t, h.Handle(context.Background(), dec))
	assert.False(t, called)
	assert.Empty(t, fake.SentMessages())
	assert.Empty(t, w.kinds())
}

func TestSynthHandler_GroupConversationContextThreaded(t *testing.T) {
	fake := whatsapptest.New()
	fake.SetSession("9@s.whatsapp.net", "Jamie")

	var capturedConv *synth.ConversationContext
	ask := func(ctx context.Context, q string, conv *synth.ConversationContext) (synth.Card, error) {
		capturedConv = conv
		return synth.Card{Speech: "team meets at 3pm"}, nil
	}
	h, _ := makeHandler(t, fake, ask)

	dec := whatsapp.Decision{
		Action:     whatsapp.ActionProcess,
		ChatJID:    "g@g.us",
		SenderJID:  "5@s.whatsapp.net",
		SenderName: "Bob",
		GroupName:  "Crew",
		IsDM:       false,
		IsMention:  true,
		Text:       "@zeno when do we meet?",
		MessageID:  "abc",
	}
	require.NoError(t, h.Handle(context.Background(), dec))
	require.NotNil(t, capturedConv)
	assert.False(t, capturedConv.IsDM)
	assert.True(t, capturedConv.IsMention)
	assert.Equal(t, "Crew", capturedConv.GroupName)
	assert.Equal(t, "Bob", capturedConv.SenderName)
}
