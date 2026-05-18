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

// TestCardsHandler_Archive seeds a mix of card flavours on one date and
// asserts /api/cards/archive returns every one of them (including
// dismissed and expired-ask rows that ListByDate would filter out),
// newest-first.
func TestCardsHandler_Archive(t *testing.T) {
	db := openHandlerTestDB(t)
	cards := &store.CardRepo{DB: db}
	traces := &store.TraceRepo{DB: db}
	ctx := context.Background()

	t0 := time.Date(2026, 5, 18, 6, 0, 0, 0, time.UTC)
	past := t0.Add(-1 * time.Hour)
	require.NoError(t, cards.Upsert(ctx, []store.Card{
		{
			ID: "morning", Date: "2026-05-18", Source: "mail", Kind: "", Rel: "high",
			SrcLabel: "Email", Title: "morning", Sub: "body",
			Meta:    datatypes.JSON([]byte(`[]`)),
			Actions: datatypes.JSON([]byte(`[]`)),
			RunID:   "tr-1", CreatedAt: t0,
		},
		{
			ID: "dismissed", Date: "2026-05-18", Source: "calendar", Rel: "med",
			SrcLabel: "Calendar", Title: "dismissed", Sub: "x",
			Meta:    datatypes.JSON([]byte(`[]`)),
			Actions: datatypes.JSON([]byte(`[]`)),
			RunID:   "tr-1", Dismissed: true, CreatedAt: t0.Add(time.Millisecond),
		},
		{
			ID: "ask-expired", Date: "2026-05-18", Source: "ask", Origin: "ask", Rel: "med",
			SrcLabel: "Generated", Title: "old answer", Sub: "y",
			Meta:      datatypes.JSON([]byte(`[]`)),
			Actions:   datatypes.JSON([]byte(`[]`)),
			RunID:     "tr-2", CreatedAt: t0.Add(2 * time.Millisecond),
			ExpiresAt: &past,
		},
	}))
	require.NoError(t, traces.Create(ctx, store.Trace{
		ID: "tr-1", RunID: "tr-1", Date: "2026-05-18", Stopped: "ok", TotalMs: 100,
		Steps: datatypes.JSON([]byte(`[]`)), CreatedAt: t0,
	}))

	e := echo.New()
	(&CardsHandler{
		Cards: cards, Traces: traces, TZ: tzUTC,
		Now: func() time.Time { return t0 },
		Log: quietHandlerEntry(),
	}).Register(e)

	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/cards/archive?date=2026-05-18", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	var resp cardsListResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Equal(t, "2026-05-18", resp.Date)
	require.Len(t, resp.Cards, 3, "archive returns dismissed + expired-ask + morning, all three")

	ids := make([]string, 0, len(resp.Cards))
	for _, c := range resp.Cards {
		ids = append(ids, c.ID)
	}
	require.Equal(t, []string{"ask-expired", "dismissed", "morning"}, ids,
		"archive is ordered CreatedAt DESC")
}

// Default ?date= → today in the handler's TZ.
func TestCardsHandler_Archive_DefaultsToToday(t *testing.T) {
	db := openHandlerTestDB(t)
	cards := &store.CardRepo{DB: db}
	traces := &store.TraceRepo{DB: db}
	ctx := context.Background()

	require.NoError(t, cards.Upsert(ctx, []store.Card{
		{
			ID: "today-card", Date: "2026-05-18", Source: "mail", Rel: "high",
			SrcLabel: "Email", Title: "today", Sub: "x",
			Meta: datatypes.JSON([]byte(`[]`)), Actions: datatypes.JSON([]byte(`[]`)),
			RunID: "tr-1", CreatedAt: time.Now(),
		},
	}))

	e := echo.New()
	(&CardsHandler{
		Cards: cards, Traces: traces, TZ: tzUTC,
		Now: func() time.Time { return time.Date(2026, 5, 18, 8, 0, 0, 0, time.UTC) },
		Log: quietHandlerEntry(),
	}).Register(e)

	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/cards/archive", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	var resp cardsListResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Equal(t, "2026-05-18", resp.Date)
	require.Len(t, resp.Cards, 1)
	require.Equal(t, "today-card", resp.Cards[0].ID)
}

// A date with no cards must return an empty array, not null — the UI
// expects to iterate over `cards` unconditionally.
func TestCardsHandler_Archive_EmptyDay(t *testing.T) {
	db := openHandlerTestDB(t)
	cards := &store.CardRepo{DB: db}
	traces := &store.TraceRepo{DB: db}

	e := echo.New()
	(&CardsHandler{
		Cards: cards, Traces: traces, TZ: tzUTC,
		Now: func() time.Time { return time.Date(2026, 5, 18, 8, 0, 0, 0, time.UTC) },
		Log: quietHandlerEntry(),
	}).Register(e)

	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/cards/archive?date=2024-01-01", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	// Body must parse with cards as an empty array, not null.
	require.Contains(t, rr.Body.String(), `"cards":[]`)
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
