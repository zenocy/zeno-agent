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

	zlog "github.com/zenocy/zeno-v2/internal/log"
)

// stubReader is a minimal log.Reader that returns canned events keyed by kind.
type stubReader struct {
	latest map[string]*zlog.Event
}

func (s *stubReader) Since(context.Context, time.Time) ([]zlog.Event, error)  { return nil, nil }
func (s *stubReader) ByKind(context.Context, ...string) ([]zlog.Event, error) { return nil, nil }
func (s *stubReader) Latest(_ context.Context, kind string) (*zlog.Event, error) {
	if e, ok := s.latest[kind]; ok {
		return e, nil
	}
	return nil, nil
}

func TestHealth_Shape(t *testing.T) {
	synthAt := time.Date(2026, 4, 25, 7, 0, 5, 0, time.UTC)
	syncAt := time.Date(2026, 4, 25, 8, 30, 0, 0, time.UTC)
	reader := &stubReader{latest: map[string]*zlog.Event{
		zlog.KindSynthRunCompleted: {Kind: zlog.KindSynthRunCompleted, TS: synthAt},
		zlog.KindSyncCompleted:     {Kind: zlog.KindSyncCompleted, TS: syncAt},
	}}
	h := &HealthHandler{
		Reader:    reader,
		StartedAt: time.Now().Add(-2 * time.Minute),
	}
	e := echo.New()
	h.Register(e)

	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var body HealthResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))

	require.NotNil(t, body.LastSynthAt)
	require.True(t, body.LastSynthAt.Equal(synthAt), "last_synth_at = %v want %v", body.LastSynthAt, synthAt)
	require.NotNil(t, body.LastSyncAt)
	require.True(t, body.LastSyncAt.Equal(syncAt))
	require.NotEmpty(t, body.Uptime)
	// No DB / LLM wired → both false → ok=false. We're testing the shape, not the policy.
}

func TestHealth_NoLatestEventsOmitsTimestamps(t *testing.T) {
	h := &HealthHandler{
		Reader:    &stubReader{latest: map[string]*zlog.Event{}},
		StartedAt: time.Now(),
	}
	e := echo.New()
	h.Register(e)

	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NotContains(t, rec.Body.String(), "last_synth_at",
		"omitempty must drop the field when there's no event yet")
	require.NotContains(t, rec.Body.String(), "last_sync_at")
}

func TestHealth_NilReaderDoesNotCrash(t *testing.T) {
	h := &HealthHandler{StartedAt: time.Now()} // no Reader, no DB, no LLM
	e := echo.New()
	h.Register(e)

	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	rec := httptest.NewRecorder()
	require.NotPanics(t, func() { e.ServeHTTP(rec, req) })
	require.Equal(t, http.StatusOK, rec.Code)
}
