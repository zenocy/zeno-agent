package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/store"
)

func openSendsTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "store.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	return db
}

func sendsRequest(t *testing.T, e *echo.Echo, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func TestSendsHandler_ListsLast7Days(t *testing.T) {
	db := openSendsTestDB(t)
	repo := &store.ExpectedReplyRepo{DB: db}
	require.NoError(t, repo.Migrate())

	now := time.Date(2026, 5, 10, 18, 0, 0, 0, time.UTC)
	resolved := now.Add(-30 * time.Minute)
	rows := []*store.ExpectedReply{
		{ // open within window
			ChatJID:       "1@s.whatsapp.net",
			SentAt:        now.Add(-2 * time.Hour),
			ExpiresAt:     now.Add(22 * time.Hour),
			RecipientName: "Dana",
			ContextKind:   "event",
			ContextID:     "evt-dinner",
			DraftBody:     "Hi Dana…",
		},
		{ // resolved
			ChatJID:       "2@s.whatsapp.net",
			SentAt:        now.Add(-3 * time.Hour),
			ExpiresAt:     now.Add(21 * time.Hour),
			RecipientName: "Sam",
			ContextKind:   "event",
			ContextID:     "evt-brunch",
			ResolvedAt:    &resolved,
			InboundBody:   "yes 12:30",
			DraftBody:     "Hi Sam…",
		},
		{ // outside window (8 days ago)
			ChatJID:       "3@s.whatsapp.net",
			SentAt:        now.Add(-8 * 24 * time.Hour),
			ExpiresAt:     now.Add(-7 * 24 * time.Hour),
			RecipientName: "Old",
			ContextID:     "evt-gone",
		},
	}
	for _, r := range rows {
		require.NoError(t, repo.Insert(context.Background(), r))
	}

	e := echo.New()
	(&SendsHandler{
		Replies: repo,
		Now:     func() time.Time { return now },
	}).Register(e)

	rr := sendsRequest(t, e, "/api/sends")
	require.Equal(t, http.StatusOK, rr.Code)

	var resp sendsResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Len(t, resp.Sends, 2, "outside-window row excluded")
	// Newest first: open (-2h) before resolved (-3h)
	assert.Equal(t, "Dana", resp.Sends[0].RecipientName)
	assert.Equal(t, "awaiting_reply", resp.Sends[0].Status)
	assert.Equal(t, "Sam", resp.Sends[1].RecipientName)
	assert.Equal(t, "replied", resp.Sends[1].Status)
	assert.Equal(t, "yes 12:30", resp.Sends[1].ReplyBody)
}

func TestSendsHandler_NilRepo_EmptyResponse(t *testing.T) {
	e := echo.New()
	(&SendsHandler{Replies: nil}).Register(e)
	rr := sendsRequest(t, e, "/api/sends")
	require.Equal(t, http.StatusOK, rr.Code)
	var resp sendsResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Empty(t, resp.Sends)
}

func TestSendsHandler_StatusDerivation(t *testing.T) {
	now := time.Date(2026, 5, 10, 18, 0, 0, 0, time.UTC)

	t.Run("awaiting", func(t *testing.T) {
		s := sendStatus(store.ExpectedReply{
			SentAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour),
		}, now)
		assert.Equal(t, "awaiting_reply", s)
	})
	t.Run("replied", func(t *testing.T) {
		resolved := now.Add(-30 * time.Minute)
		s := sendStatus(store.ExpectedReply{
			SentAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour),
			ResolvedAt: &resolved,
		}, now)
		assert.Equal(t, "replied", s)
	})
	t.Run("expired", func(t *testing.T) {
		s := sendStatus(store.ExpectedReply{
			SentAt: now.Add(-30 * time.Hour), ExpiresAt: now.Add(-6 * time.Hour),
		}, now)
		assert.Equal(t, "expired", s)
	})
}
