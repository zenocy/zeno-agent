package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/log/logtest"
	"github.com/zenocy/zeno-v2/internal/projection"
	"github.com/zenocy/zeno-v2/internal/store"
)

func quietEntry() *logrus.Entry {
	l := logrus.New()
	l.Out = io.Discard
	return l.WithField("c", "api-test")
}

func mustTZ(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	require.NoError(t, err)
	return loc
}

func newProjCfg(now time.Time, tz *time.Location) projection.Config {
	return projection.Config{
		TZ:                    tz,
		LookbackDays:          14,
		RunWindowMinMinutes:   45,
		RunWindowMaxWindKmh:   25,
		RunWindowEarliestHour: 6,
		RunWindowLatestHour:   20,
		OpenThreadsMax:        20,
		Now:                   func() time.Time { return now },
	}
}

func mountProjections(reader log.Reader, cfg projection.Config) *echo.Echo {
	return mountProjectionsWith(reader, cfg, nil)
}

func mountProjectionsWith(reader log.Reader, cfg projection.Config, tickers projection.TickerSource) *echo.Echo {
	return mountProjectionsWithTasks(reader, nil, cfg, tickers)
}

// mountProjectionsWithTasks lets the V2.11 task-projection tests inject
// a real TaskRepo while leaving the rest of the projection routes
// reading from the event log.
func mountProjectionsWithTasks(reader log.Reader, tasks *store.TaskRepo, cfg projection.Config, tickers projection.TickerSource) *echo.Echo {
	e := echo.New()
	(&ProjectionsHandler{Reader: reader, Tasks: tasks, Cfg: cfg, Tickers: tickers, Log: quietEntry()}).Register(e)
	return e
}

// newTaskProjectionRepo returns a fresh in-memory TaskRepo for the
// V2.11 tasks-projection tests.
func newTaskProjectionRepo(t *testing.T) *store.TaskRepo {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_journal_mode=WAL&_foreign_keys=on"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	repo := &store.TaskRepo{DB: db}
	require.NoError(t, repo.Migrate())
	return repo
}

type stubTickerSource struct {
	tickers []string
	ok      bool
}

func (s stubTickerSource) StockConfig() ([]string, float64, bool, bool) {
	return s.tickers, 0, false, s.ok
}

func TestProjectionsHandler_CalendarToday_OK(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 8, 0, 0, 0, tz)

	mem := logtest.NewMemReader()
	for i, h := range []int{14, 9, 11} {
		s := time.Date(2026, 4, 25, h, 0, 0, 0, tz)
		mem.AppendEvent(logtest.MakeEvent(log.KindCalEventSeen, "caldav",
			now.Add(time.Duration(i)*time.Minute),
			map[string]any{"uid": "e" + string(rune('0'+i)), "title": "T", "start": s, "end": s.Add(time.Hour)}))
	}

	e := mountProjections(mem, newProjCfg(now, tz))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/projections/calendar/today", nil)
	e.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var out []projection.CalendarEvent
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	require.Len(t, out, 3)
	require.True(t, out[0].Start.Before(out[1].Start))
}

func TestProjectionsHandler_CalendarToday_Empty(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	mem := logtest.NewMemReader()
	e := mountProjections(mem, newProjCfg(time.Now().In(tz), tz))

	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/projections/calendar/today", nil))

	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, "[]\n", rr.Body.String(), "empty array, never null")
}

func TestProjectionsHandler_EmailOpen_OK(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 9, 0, 0, 0, tz)

	mem := logtest.NewMemReader()
	mem.AppendEvent(logtest.MakeEvent(log.KindMailReceived, "imap", now,
		map[string]any{"folder": "INBOX", "uid": 1, "from": "a@x.test", "subject": "Hi", "date": now}))

	e := mountProjections(mem, newProjCfg(now, tz))
	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/projections/email/open", nil))

	require.Equal(t, http.StatusOK, rr.Code)
	var out []projection.Thread
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	require.Len(t, out, 1)
	require.Equal(t, "Hi", out[0].Subject)
}

func TestProjectionsHandler_RunWindow_HasWindow(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 6, 0, 0, 0, tz)

	mem := logtest.NewMemReader()
	type hp struct {
		Time     time.Time `json:"time"`
		Code     int       `json:"code"`
		WindKmh  float64   `json:"wind_kmh"`
		PrecipMM float64   `json:"precip_mm"`
	}
	hours := []hp{}
	for h := 6; h < 20; h++ {
		hours = append(hours, hp{Time: time.Date(2026, 4, 25, h, 0, 0, 0, tz), Code: 1, WindKmh: 8})
	}
	mem.AppendEvent(logtest.MakeEvent(log.KindWeatherSnapshot, "weather", now,
		map[string]any{"captured_at": now, "timezone": tz.String(), "hourly": hours}))

	e := mountProjections(mem, newProjCfg(now, tz))
	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/projections/run-window", nil))

	require.Equal(t, http.StatusOK, rr.Code)
	var w projection.Window
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &w))
	require.False(t, w.Start.IsZero())
	require.NotEmpty(t, w.Condition)
}

func TestProjectionsHandler_Weather_OK(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 16, 0, 0, 0, tz)

	type hp struct {
		Time     time.Time `json:"time"`
		TempC    float64   `json:"temp_c"`
		Code     int       `json:"code"`
		WindKmh  float64   `json:"wind_kmh"`
		PrecipMM float64   `json:"precip_mm"`
		Label    string    `json:"label,omitempty"`
	}
	hours := []hp{}
	for h := 13; h < 19; h++ {
		hours = append(hours, hp{
			Time:  time.Date(2026, 4, 25, h, 0, 0, 0, tz),
			TempC: float64(13 + (h - 13)), Code: 0, WindKmh: 8, Label: "Clear sky",
		})
	}
	current := hp{Time: now, TempC: 17, Code: 0, WindKmh: 8, Label: "Clear sky"}

	mem := logtest.NewMemReader()
	mem.AppendEvent(logtest.MakeEvent(log.KindWeatherSnapshot, "weather", now,
		map[string]any{
			"captured_at": now, "timezone": tz.String(), "location": "San Francisco",
			"current": current, "hourly": hours,
		}))

	e := mountProjections(mem, newProjCfg(now, tz))
	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/projections/weather", nil))

	require.Equal(t, http.StatusOK, rr.Code)
	var out projection.WeatherView
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	require.Equal(t, "San Francisco", out.Location)
	require.Equal(t, 17.0, out.Current.TempC)
	require.Len(t, out.Hourly, 6)
	require.Equal(t, 3, out.NowIndex)
}

func TestProjectionsHandler_Weather_Null(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	mem := logtest.NewMemReader()
	e := mountProjections(mem, newProjCfg(time.Now().In(tz), tz))

	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/projections/weather", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, "null\n", rr.Body.String())
}

func TestProjectionsHandler_RunWindow_Null(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	mem := logtest.NewMemReader()
	e := mountProjections(mem, newProjCfg(time.Now().In(tz), tz))

	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/projections/run-window", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, "null\n", rr.Body.String())
}

// brokenReader returns an error from every method to verify error mapping.
type brokenReader struct{}

func (brokenReader) Since(context.Context, time.Time) ([]log.Event, error) {
	return nil, errors.New("db down")
}
func (brokenReader) ByKind(context.Context, ...string) ([]log.Event, error) {
	return nil, errors.New("db down")
}
func (brokenReader) Latest(context.Context, string) (*log.Event, error) {
	return nil, errors.New("db down")
}

func TestProjectionsHandler_TasksOpen_Empty(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	mem := logtest.NewMemReader()
	repo := newTaskProjectionRepo(t)
	e := mountProjectionsWithTasks(mem, repo, newProjCfg(time.Now().In(tz), tz), nil)

	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/projections/tasks/open", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, "[]\n", rr.Body.String(), "empty array, never null")
}

func TestProjectionsHandler_TasksOpen_OK(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 5, 5, 9, 0, 0, 0, tz)

	repo := newTaskProjectionRepo(t)
	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: "a", Title: "Ship V2.6", DueDate: "2026-05-10", Priority: "high"}))
	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: "b", Title: "Reply legal", Priority: "med"}))

	e := mountProjectionsWithTasks(logtest.NewMemReader(), repo, newProjCfg(now, tz), nil)
	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/projections/tasks/open", nil))

	require.Equal(t, http.StatusOK, rr.Code)
	var out []projection.OpenTasksTask
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	require.Len(t, out, 2)
}

func TestProjectionsHandler_TasksOpen_OrderingIsByBand(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 5, 5, 9, 0, 0, 0, tz)

	repo := newTaskProjectionRepo(t)
	// Insert in a deliberately-shuffled order so the test verifies the
	// projection's sort, not the input order.
	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: "nd", Title: "no date", Priority: "med"}))
	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: "od", Title: "overdue", DueDate: "2026-05-01", Priority: "med"}))
	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: "td", Title: "today", DueDate: "2026-05-05", Priority: "med"}))

	e := mountProjectionsWithTasks(logtest.NewMemReader(), repo, newProjCfg(now, tz), nil)
	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/projections/tasks/open", nil))
	require.Equal(t, http.StatusOK, rr.Code)

	var out []projection.OpenTasksTask
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	require.Len(t, out, 3)
	require.Equal(t, "od", out[0].UID, "overdue should come first")
	require.Equal(t, "td", out[1].UID, "today should come second")
	require.Equal(t, "nd", out[2].UID, "no-date should come last")
}

// TestProjectionsHandler_TasksOpen_NoRepoConfigured surfaces the
// projection's missing-deps error path: the handler 500s if the repo
// is nil. Production wiring always sets the repo; this catches a
// future wiring-regression.
func TestProjectionsHandler_TasksOpen_NoRepoConfigured(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	e := mountProjectionsWithTasks(logtest.NewMemReader(), nil, newProjCfg(time.Now().In(tz), tz), nil)

	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/projections/tasks/open", nil))
	require.Equal(t, http.StatusInternalServerError, rr.Code)
	require.Contains(t, rr.Body.String(), "TaskRepo")
}

func TestProjectionsHandler_StoreError(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	e := mountProjections(brokenReader{}, newProjCfg(time.Now().In(tz), tz))

	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/projections/calendar/today", nil))
	require.Equal(t, http.StatusInternalServerError, rr.Code)
	require.Contains(t, rr.Body.String(), "db down")
}

func TestProjectionsHandler_Stock_NoTickers_Null(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	mem := logtest.NewMemReader()
	e := mountProjectionsWith(mem, newProjCfg(time.Now().In(tz), tz), stubTickerSource{ok: false})

	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/projections/stock", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, "null\n", rr.Body.String())
}

func TestProjectionsHandler_Stock_OK(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 5, 5, 14, 0, 0, 0, tz)

	mem := logtest.NewMemReader()
	mem.AppendEvent(logtest.MakeEvent(log.KindStockSnapshot, "stock", now, map[string]any{
		"ticker": "AAPL", "price": 200.5, "prev_close": 195.0,
		"currency": "USD", "change_pct": 2.82, "as_of": now,
	}))

	cfg := newProjCfg(now, tz)
	e := mountProjectionsWith(mem, cfg, stubTickerSource{tickers: []string{"AAPL"}, ok: true})

	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/projections/stock", nil))
	require.Equal(t, http.StatusOK, rr.Code)

	var out projection.StockView
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	require.Len(t, out.Quotes, 1)
	require.Equal(t, "AAPL", out.Quotes[0].Ticker)
	require.InDelta(t, 200.5, out.Quotes[0].Price, 1e-6)
}
