package projection

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/store"
)

func openActivityRepo(t *testing.T) *store.ExpectedReplyRepo {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "store.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	repo := &store.ExpectedReplyRepo{DB: db}
	require.NoError(t, repo.Migrate())
	return repo
}

func TestWhatsAppActivity_NilRepoEmpty(t *testing.T) {
	p := WhatsAppActivityProjection{Repo: nil}
	out, err := p.Compute(context.Background(), nil, 5)
	require.NoError(t, err)
	require.Empty(t, out)
}

func TestWhatsAppActivity_EmptyRepoEmpty(t *testing.T) {
	repo := openActivityRepo(t)
	p := WhatsAppActivityProjection{Repo: repo, Now: time.Now}
	out, err := p.Compute(context.Background(), nil, 5)
	require.NoError(t, err)
	require.Empty(t, out)
}

func TestWhatsAppActivity_AwaitingReply(t *testing.T) {
	repo := openActivityRepo(t)
	now := time.Date(2026, 5, 10, 18, 0, 0, 0, time.UTC)
	require.NoError(t, repo.Insert(context.Background(), &store.ExpectedReply{
		ChatJID:       "1@s.whatsapp.net",
		SentAt:        now.Add(-2 * time.Hour),
		ExpiresAt:     now.Add(22 * time.Hour),
		ContextKind:   "event",
		ContextID:     "evt-dinner",
		RecipientName: "Dana Lopez",
	}))

	p := WhatsAppActivityProjection{Repo: repo, Now: func() time.Time { return now }}
	out, err := p.Compute(context.Background(), nil, 5)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "Dana Lopez", out[0].RecipientName)
	assert.Equal(t, "awaiting_reply", out[0].Status)
	assert.Empty(t, out[0].ReplyBody)
	assert.Equal(t, "evt-dinner", out[0].EventUID)
	// EventTitle empty when no calendar match passed.
	assert.Empty(t, out[0].EventTitle)
}

func TestWhatsAppActivity_Replied_QuotesInbound(t *testing.T) {
	repo := openActivityRepo(t)
	now := time.Date(2026, 5, 10, 18, 0, 0, 0, time.UTC)
	resolved := now.Add(-30 * time.Minute)
	require.NoError(t, repo.Insert(context.Background(), &store.ExpectedReply{
		ChatJID:       "1@s.whatsapp.net",
		SentAt:        now.Add(-2 * time.Hour),
		ExpiresAt:     now.Add(22 * time.Hour),
		ContextKind:   "event",
		ContextID:     "evt-dinner",
		RecipientName: "Dana Lopez",
		ResolvedAt:    &resolved,
		InboundBody:   "Yes, 7pm works",
	}))

	p := WhatsAppActivityProjection{Repo: repo, Now: func() time.Time { return now }}
	out, err := p.Compute(context.Background(), nil, 5)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "replied", out[0].Status)
	assert.Equal(t, "Yes, 7pm works", out[0].ReplyBody)
	require.NotNil(t, out[0].ResolvedAt)
	assert.Equal(t, resolved.Unix(), out[0].ResolvedAt.Unix())
}

func TestWhatsAppActivity_Expired(t *testing.T) {
	repo := openActivityRepo(t)
	now := time.Date(2026, 5, 10, 18, 0, 0, 0, time.UTC)
	require.NoError(t, repo.Insert(context.Background(), &store.ExpectedReply{
		ChatJID:   "1@s.whatsapp.net",
		SentAt:    now.Add(-23 * time.Hour),
		ExpiresAt: now.Add(-1 * time.Hour), // past
		ContextID: "evt-old",
	}))

	p := WhatsAppActivityProjection{Repo: repo, Now: func() time.Time { return now }}
	out, err := p.Compute(context.Background(), nil, 5)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "expired", out[0].Status)
}

func TestWhatsAppActivity_TitleJoinFromCalendar(t *testing.T) {
	repo := openActivityRepo(t)
	now := time.Date(2026, 5, 10, 18, 0, 0, 0, time.UTC)
	require.NoError(t, repo.Insert(context.Background(), &store.ExpectedReply{
		ChatJID:       "1@s.whatsapp.net",
		SentAt:        now.Add(-2 * time.Hour),
		ExpiresAt:     now.Add(22 * time.Hour),
		ContextKind:   "event",
		ContextID:     "evt-dinner-uid",
		RecipientName: "Dana Lopez",
	}))

	cal := []CalendarEvent{
		{UID: "evt-dinner-uid", Title: "Dinner with Dana & Alex"},
		{UID: "evt-other", Title: "Standup"},
	}
	p := WhatsAppActivityProjection{Repo: repo, Now: func() time.Time { return now }}
	out, err := p.Compute(context.Background(), cal, 5)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "Dinner with Dana & Alex", out[0].EventTitle)
}

func TestWhatsAppActivity_Cap(t *testing.T) {
	repo := openActivityRepo(t)
	now := time.Date(2026, 5, 10, 18, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		require.NoError(t, repo.Insert(context.Background(), &store.ExpectedReply{
			ChatJID:   "1@s.whatsapp.net",
			SentAt:    now.Add(time.Duration(-i-1) * time.Hour),
			ExpiresAt: now.Add(time.Duration(23-i) * time.Hour),
			ContextID: "evt",
		}))
	}
	p := WhatsAppActivityProjection{Repo: repo, Now: func() time.Time { return now }}
	out, err := p.Compute(context.Background(), nil, 2)
	require.NoError(t, err)
	require.Len(t, out, 2, "limit honored")
}

func TestWhatsAppActivity_TruncatesLongReply(t *testing.T) {
	repo := openActivityRepo(t)
	now := time.Date(2026, 5, 10, 18, 0, 0, 0, time.UTC)
	resolved := now.Add(-15 * time.Minute)
	long := strings.Repeat("a", 500)
	require.NoError(t, repo.Insert(context.Background(), &store.ExpectedReply{
		ChatJID:       "1@s.whatsapp.net",
		SentAt:        now.Add(-1 * time.Hour),
		ExpiresAt:     now.Add(23 * time.Hour),
		ContextID:     "evt",
		RecipientName: "Sam",
		ResolvedAt:    &resolved,
		InboundBody:   long,
	}))

	p := WhatsAppActivityProjection{Repo: repo, Now: func() time.Time { return now }}
	out, err := p.Compute(context.Background(), nil, 5)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Less(t, len(out[0].ReplyBody), 300)
	assert.True(t, strings.HasSuffix(out[0].ReplyBody, "…"))
}

// TestWhatsAppActivity_TZConvertsToLocal verifies V2.13.3d: SentAt
// and ResolvedAt come out of the DB in UTC (GORM round-trip), so the
// projection must convert to the user's TZ before they reach the
// prompt template — otherwise `{{ .SentAt.Format "15:04" }}` shows
// the UTC clock and the model computes elapsed time wrong.
func TestWhatsAppActivity_TZConvertsToLocal(t *testing.T) {
	repo := openActivityRepo(t)
	athens, err := time.LoadLocation("Europe/Athens")
	require.NoError(t, err)

	// Sent at 14:25 UTC = 17:25 EEST (Athens), reply at 14:55 UTC = 17:55 EEST.
	sentUTC := time.Date(2026, 5, 10, 14, 25, 0, 0, time.UTC)
	resolvedUTC := time.Date(2026, 5, 10, 14, 55, 0, 0, time.UTC)
	require.NoError(t, repo.Insert(context.Background(), &store.ExpectedReply{
		ChatJID:       "1@s.whatsapp.net",
		SentAt:        sentUTC,
		ExpiresAt:     sentUTC.Add(24 * time.Hour),
		ContextID:     "evt-dinner",
		RecipientName: "Dana",
		ResolvedAt:    &resolvedUTC,
		InboundBody:   "yes 7pm",
	}))

	now := time.Date(2026, 5, 10, 15, 0, 0, 0, time.UTC) // 18:00 Athens
	p := WhatsAppActivityProjection{
		Repo: repo,
		Now:  func() time.Time { return now },
		TZ:   athens,
	}
	out, err := p.Compute(context.Background(), nil, 5)
	require.NoError(t, err)
	require.Len(t, out, 1)

	// SentAt's Location must be Athens — the prompt formatter uses Location.
	assert.Equal(t, "Europe/Athens", out[0].SentAt.Location().String())
	assert.Equal(t, "17:25", out[0].SentAt.Format("15:04"))
	require.NotNil(t, out[0].ResolvedAt)
	assert.Equal(t, "Europe/Athens", out[0].ResolvedAt.Location().String())
	assert.Equal(t, "17:55", out[0].ResolvedAt.Format("15:04"))
}

// TestWhatsAppActivity_NilTZFallsBackToUTC ensures eval/replay paths
// (no TZ set) keep working without panicking.
func TestWhatsAppActivity_NilTZFallsBackToUTC(t *testing.T) {
	repo := openActivityRepo(t)
	now := time.Date(2026, 5, 10, 15, 0, 0, 0, time.UTC)
	require.NoError(t, repo.Insert(context.Background(), &store.ExpectedReply{
		ChatJID:   "1@s.whatsapp.net",
		SentAt:    now.Add(-time.Hour),
		ExpiresAt: now.Add(23 * time.Hour),
		ContextID: "evt",
	}))
	p := WhatsAppActivityProjection{Repo: repo, Now: func() time.Time { return now }}
	out, err := p.Compute(context.Background(), nil, 5)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "UTC", out[0].SentAt.Location().String())
}

func TestWhatsAppActivity_OutsideWindowExcluded(t *testing.T) {
	repo := openActivityRepo(t)
	now := time.Date(2026, 5, 10, 18, 0, 0, 0, time.UTC)
	require.NoError(t, repo.Insert(context.Background(), &store.ExpectedReply{
		ChatJID:   "1@s.whatsapp.net",
		SentAt:    now.Add(-30 * time.Hour), // older than 24h window
		ExpiresAt: now.Add(-6 * time.Hour),
		ContextID: "evt-old",
	}))
	p := WhatsAppActivityProjection{Repo: repo, Now: func() time.Time { return now }}
	out, err := p.Compute(context.Background(), nil, 5)
	require.NoError(t, err)
	require.Empty(t, out)
}

func TestWhatsAppActivity_NoOperationalIdentifiers(t *testing.T) {
	// The projection's outputs must never carry JIDs, phone numbers, or
	// raw body bytes that include operational identifiers — V2.13's
	// privacy contract. Rendered fields are checked against the same
	// JID/phone shape regex used in eval/reactive.go.
	repo := openActivityRepo(t)
	now := time.Date(2026, 5, 10, 18, 0, 0, 0, time.UTC)
	resolved := now.Add(-5 * time.Minute)
	require.NoError(t, repo.Insert(context.Background(), &store.ExpectedReply{
		ChatJID:       "447700900111@s.whatsapp.net",
		OutboundMsgID: "WAMSG-77",
		SentAt:        now.Add(-1 * time.Hour),
		ExpiresAt:     now.Add(23 * time.Hour),
		ContextID:     "evt",
		RecipientName: "Dana Lopez",
		ResolvedAt:    &resolved,
		InboundBody:   "thanks!",
	}))

	p := WhatsAppActivityProjection{Repo: repo, Now: func() time.Time { return now }}
	out, err := p.Compute(context.Background(), nil, 5)
	require.NoError(t, err)
	require.Len(t, out, 1)
	for _, field := range []string{out[0].RecipientName, out[0].EventTitle, out[0].EventUID, out[0].ReplyBody, out[0].Status} {
		assert.NotContains(t, field, "@s.whatsapp.net", "JID must never appear in projection output")
		assert.NotContains(t, field, "447700", "phone digits must never appear")
		assert.NotContains(t, field, "WAMSG", "outbound msg ID must not leak")
	}
}
