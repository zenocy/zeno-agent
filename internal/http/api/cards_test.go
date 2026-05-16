package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/store"
)

func openHandlerTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_journal_mode=WAL&_foreign_keys=on"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, (&store.CardRepo{DB: db}).Migrate())
	require.NoError(t, (&store.BriefingRepo{DB: db}).Migrate())
	require.NoError(t, (&store.TraceRepo{DB: db}).Migrate())
	require.NoError(t, (&store.MemoryRepo{DB: db}).Migrate())
	require.NoError(t, (&store.ConcernRepo{DB: db}).Migrate())
	require.NoError(t, (&store.ConcernObservationRepo{DB: db}).Migrate())
	require.NoError(t, (&store.ConversationRepo{DB: db}).Migrate())
	return db
}

// tzUTC is a func() *time.Location returning UTC, suitable for the
// handlers' new live-TZ getter pattern in tests where the timezone
// is fixed and doesn't need to change mid-test.
func tzUTC() *time.Location { return time.UTC }

func quietHandlerEntry() *logrus.Entry {
	l := logrus.New()
	l.Out = io.Discard
	return l.WithField("c", "test")
}

func TestCardsHandler_ListAndTrace(t *testing.T) {
	db := openHandlerTestDB(t)
	cards := &store.CardRepo{DB: db}
	traces := &store.TraceRepo{DB: db}
	ctx := context.Background()

	require.NoError(t, cards.Upsert(ctx, []store.Card{
		{
			ID: "saru", Date: "2026-04-25", Source: "mail", Kind: "", Rel: "high",
			SrcLabel: "Email", Title: "Saru", Sub: "body",
			Meta:    datatypes.JSON([]byte(`["06:14"]`)),
			Actions: datatypes.JSON([]byte(`[{"label":"Reply","primary":true}]`)),
			TraceID: "tr-1", RunID: "tr-1", CreatedAt: time.Now(),
		},
		{
			ID: "lia", Date: "2026-04-25", Source: "personal", Kind: "personal", Rel: "low",
			SrcLabel: "Family", Title: "Lia", Sub: "body",
			Meta:    datatypes.JSON([]byte(`[]`)),
			Actions: datatypes.JSON([]byte(`[{"label":"OK"}]`)),
			TraceID: "tr-1", RunID: "tr-1", CreatedAt: time.Now(),
		},
	}))
	require.NoError(t, traces.Create(ctx, store.Trace{
		ID: "tr-1", RunID: "tr-1", Date: "2026-04-25", Stopped: "ok", TotalMs: 100,
		Steps:     datatypes.JSON([]byte(`[{"kind":"tool","op":"READ"}]`)),
		CreatedAt: time.Now(),
	}))

	e := echo.New()
	(&CardsHandler{
		Cards: cards, Traces: traces, TZ: tzUTC,
		Now: func() time.Time { return time.Date(2026, 4, 25, 8, 0, 0, 0, time.UTC) },
		Log: quietHandlerEntry(),
	}).Register(e)

	// /api/cards
	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/cards", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	var listResp cardsListResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &listResp))
	require.Equal(t, "2026-04-25", listResp.Date)
	require.Len(t, listResp.Cards, 2)
	require.Equal(t, "high", listResp.Cards[0].Rel) // ordering
	require.Equal(t, "saru", listResp.Cards[0].ID)
	require.NotEmpty(t, listResp.Cards[0].Actions)

	// /api/cards/:id/trace
	rr = httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/cards/saru/trace", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	var trResp store.Trace
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &trResp))
	require.Equal(t, "ok", trResp.Stopped)

	// /api/traces/:id — direct trace lookup (used by reactive cards which
	// aren't persisted in the cards table).
	rr = httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/traces/tr-1", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	var trDirect store.Trace
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &trDirect))
	require.Equal(t, "ok", trDirect.Stopped)

	// /api/traces/:id with unknown id → 404.
	rr = httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/traces/missing", nil))
	require.Equal(t, http.StatusNotFound, rr.Code)
}

func TestBriefingHandler_TodayAndQuery(t *testing.T) {
	db := openHandlerTestDB(t)
	repo := &store.BriefingRepo{DB: db}
	ctx := context.Background()

	require.NoError(t, repo.UpsertMorning(ctx, store.Briefing{
		Date: "2026-04-25", Eyebrow: "e", Title: "A *calm* start.", Summary: "s", Tension: 38, CreatedAt: time.Now(),
	}))

	e := echo.New()
	(&BriefingHandler{
		Repo: repo, TZ: tzUTC,
		Now: func() time.Time { return time.Date(2026, 4, 25, 8, 0, 0, 0, time.UTC) },
		Log: quietHandlerEntry(),
	}).Register(e)

	// Default: today.
	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/briefing/today", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	var b store.Briefing
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &b))
	require.Equal(t, 38, b.Tension)
	require.Contains(t, b.Title, "*")

	// Explicit ?date= for missing date returns 404.
	rr = httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/briefing/today?date=1999-01-01", nil))
	require.Equal(t, http.StatusNotFound, rr.Code)
}
