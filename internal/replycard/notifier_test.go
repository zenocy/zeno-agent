package replycard

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/store"
	"github.com/zenocy/zeno-v2/internal/whatsapp"
)

func openCardRepo(t *testing.T) *store.CardRepo {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "store.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	repo := &store.CardRepo{DB: db, Table: "cards"}
	require.NoError(t, repo.Migrate())
	return repo
}

func TestNotifier_DeterministicCardComposition(t *testing.T) {
	cards := openCardRepo(t)
	notifier := &Notifier{
		Cards: cards,
		Now:   func() time.Time { return time.Date(2026, 5, 10, 21, 14, 0, 0, time.UTC) },
		Log:   logrus.NewEntry(logrus.New()),
	}

	sig := whatsapp.ReplyReceivedSignal{
		ChatJID:       "447700900111@s.whatsapp.net",
		RecipientName: "Dana",
		ContextKind:   "event",
		ContextID:     "evt-dinner",
		DraftBody:     "Hi Dana — Jamie asked me to confirm dinner tonight.",
		ReplyText:     "Yes, 7pm works",
		ReceivedAt:    time.Date(2026, 5, 10, 21, 14, 0, 0, time.UTC),
	}
	require.NoError(t, notifier.Notify(context.Background(), sig))

	rows, err := cards.ListByDate(context.Background(), "2026-05-10")
	require.NoError(t, err)
	require.Len(t, rows, 1)

	c := rows[0]
	assert.Equal(t, "reply_received", c.Kind)
	assert.Equal(t, "personal", c.Source)
	assert.Equal(t, "WhatsApp · Dana", c.SrcLabel)
	assert.Equal(t, "Dana replied", c.Title)
	assert.Contains(t, c.Sub, "Yes, 7pm works")
	assert.Contains(t, c.Sub, "Dana")
	assert.True(t, strings.HasPrefix(c.ID, "reply-"))
}

func TestNotifier_NoLeakOfPII(t *testing.T) {
	cards := openCardRepo(t)
	notifier := &Notifier{Cards: cards}
	sig := whatsapp.ReplyReceivedSignal{
		ChatJID:       "447700900111@s.whatsapp.net",
		RecipientName: "Dana",
		ReplyText:     "thanks",
	}
	require.NoError(t, notifier.Notify(context.Background(), sig))

	rows, _ := cards.ListByDate(context.Background(), time.Now().Format("2006-01-02"))
	require.Len(t, rows, 1)
	c := rows[0]
	// No JID-shaped strings or phone numbers in any user-facing field.
	for _, field := range []string{c.Title, c.Sub, c.SrcLabel} {
		assert.NotContains(t, field, "@s.whatsapp.net", "JID must not leak into card text")
		assert.NotContains(t, field, "447700", "phone digits must not leak")
	}
	// Actions also clean.
	var actions []map[string]any
	require.NoError(t, json.Unmarshal(c.Actions, &actions))
	for _, a := range actions {
		body, _ := json.Marshal(a)
		assert.NotContains(t, string(body), "@s.whatsapp.net")
		assert.NotContains(t, string(body), "447700")
	}
}

func TestNotifier_TruncatesLongReplies(t *testing.T) {
	cards := openCardRepo(t)
	notifier := &Notifier{Cards: cards}
	long := strings.Repeat("a", 500)
	require.NoError(t, notifier.Notify(context.Background(), whatsapp.ReplyReceivedSignal{
		ChatJID:       "1@s.whatsapp.net",
		RecipientName: "Sam",
		ReplyText:     long,
	}))

	rows, _ := cards.ListByDate(context.Background(), time.Now().Format("2006-01-02"))
	require.Len(t, rows, 1)
	assert.Less(t, len(rows[0].Sub), 400, "long inbound bodies must be truncated")
	assert.True(t, strings.Contains(rows[0].Sub, "…"))
}
