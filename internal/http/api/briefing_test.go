package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/store"
)

func mountBriefing(t *testing.T, repo *store.BriefingRepo, now time.Time) *echo.Echo {
	t.Helper()
	e := echo.New()
	(&BriefingHandler{
		Repo: repo,
		TZ:   tzUTC,
		Now:  func() time.Time { return now },
		Log:  quietHandlerEntry(),
	}).Register(e)
	return e
}

func TestBriefingHandler_OK_ReturnsTodaysMorningRow(t *testing.T) {
	db := openHandlerTestDB(t)
	repo := &store.BriefingRepo{DB: db}
	ctx := context.Background()

	require.NoError(t, repo.UpsertMorning(ctx, store.Briefing{
		Date:    "2026-04-25",
		Eyebrow: "calm",
		Title:   "A quiet morning",
		Summary: "One paragraph.",
		Tension: 1,
		State:   "morning_calm",
	}))

	now := time.Date(2026, 4, 25, 8, 0, 0, 0, time.UTC)
	e := mountBriefing(t, repo, now)
	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/briefing/today", nil))
	require.Equal(t, http.StatusOK, rr.Code)

	var b store.Briefing
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &b))
	require.Equal(t, "A quiet morning", b.Title)
	require.Equal(t, "morning_calm", b.State)
}

// The ?date= query parameter overrides the Now-derived date so the eval
// harness and historical-day inspection in the UI both work.
func TestBriefingHandler_DateQueryParamOverridesNow(t *testing.T) {
	db := openHandlerTestDB(t)
	repo := &store.BriefingRepo{DB: db}
	ctx := context.Background()

	require.NoError(t, repo.UpsertMorning(ctx, store.Briefing{
		Date: "2026-04-20", Title: "Five days ago",
	}))

	now := time.Date(2026, 4, 25, 8, 0, 0, 0, time.UTC)
	e := mountBriefing(t, repo, now)
	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/briefing/today?date=2026-04-20", nil))
	require.Equal(t, http.StatusOK, rr.Code)

	var b store.Briefing
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &b))
	require.Equal(t, "Five days ago", b.Title)
}

// No row for the requested date → 404. The body carries {"error", "date"}
// so the UI can render a stable "no briefing yet" message.
func TestBriefingHandler_MissingDateReturns404(t *testing.T) {
	db := openHandlerTestDB(t)
	repo := &store.BriefingRepo{DB: db}

	now := time.Date(2026, 4, 25, 8, 0, 0, 0, time.UTC)
	e := mountBriefing(t, repo, now)
	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/briefing/today", nil))
	require.Equal(t, http.StatusNotFound, rr.Code)

	var resp map[string]string
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Contains(t, resp["error"], "no briefing")
	require.Equal(t, "2026-04-25", resp["date"])
}
