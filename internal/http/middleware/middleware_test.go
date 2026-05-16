package middleware

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

func newEcho() *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	return e
}

func TestRequestID_GeneratesWhenMissing(t *testing.T) {
	e := newEcho()
	e.Use(RequestID())

	var captured string
	e.GET("/x", func(c echo.Context) error {
		captured = GetRequestID(c)
		return c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NotEmpty(t, captured, "context request_id must be set")
	require.Equal(t, captured, rec.Header().Get(RequestIDHeader),
		"response header must echo the generated id")
	// UUIDs from google/uuid include dashes; loose sanity check.
	require.Contains(t, captured, "-")
}

func TestRequestID_PreservesIncoming(t *testing.T) {
	e := newEcho()
	e.Use(RequestID())

	var captured string
	e.GET("/x", func(c echo.Context) error {
		captured = GetRequestID(c)
		return c.NoContent(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(RequestIDHeader, "incoming-12345")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, "incoming-12345", captured)
	require.Equal(t, "incoming-12345", rec.Header().Get(RequestIDHeader))
}

func TestRecovery_PanicReturns500(t *testing.T) {
	logger := logrus.New()
	logger.SetOutput(&bytes.Buffer{}) // silence
	logger.SetLevel(logrus.PanicLevel)

	e := newEcho()
	e.Use(RequestID(), Recovery(logger))
	e.GET("/boom", func(c echo.Context) error {
		panic("kaboom")
	})

	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	rec := httptest.NewRecorder()

	require.NotPanics(t, func() { e.ServeHTTP(rec, req) })

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Contains(t, rec.Body.String(), "internal server error")
}

func TestRecovery_LogsPanicWithRequestIDAndStack(t *testing.T) {
	var buf bytes.Buffer
	logger := logrus.New()
	logger.SetOutput(&buf)
	logger.SetFormatter(&logrus.JSONFormatter{})

	e := newEcho()
	e.Use(RequestID(), Recovery(logger))
	e.GET("/boom", func(c echo.Context) error {
		panic("blew up")
	})

	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	req.Header.Set(RequestIDHeader, "trace-7")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	out := buf.String()
	require.Contains(t, out, `"panic":"blew up"`)
	require.Contains(t, out, `"request_id":"trace-7"`)
	require.Contains(t, out, `"path":"/boom"`)
	require.Contains(t, out, `"stack":`)
}

func TestLogging_EmitsStructuredFields(t *testing.T) {
	var buf bytes.Buffer
	logger := logrus.New()
	logger.SetOutput(&buf)
	logger.SetLevel(logrus.DebugLevel)
	logger.SetFormatter(&logrus.JSONFormatter{})

	e := newEcho()
	e.Use(RequestID(), Logging(logger))
	e.GET("/hello", func(c echo.Context) error {
		return c.String(http.StatusOK, "hi")
	})

	req := httptest.NewRequest(http.MethodGet, "/hello", nil)
	req.Header.Set(RequestIDHeader, "req-42")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	out := buf.String()
	require.Contains(t, out, `"method":"GET"`)
	require.Contains(t, out, `"path":"/hello"`)
	require.Contains(t, out, `"status":200`)
	require.Contains(t, out, `"request_id":"req-42"`)
	require.Contains(t, out, `"duration_ms":`)
	require.Contains(t, out, `"msg":"request completed"`)
}

func TestLogging_LevelTracksStatusClass(t *testing.T) {
	cases := []struct {
		status   int
		levelStr string
	}{
		{http.StatusOK, `"level":"debug"`},                  // fast 2xx demoted to DEBUG
		{http.StatusBadRequest, `"level":"info"`},           // 4xx is INFO (client mistake, not alarm)
		{http.StatusInternalServerError, `"level":"error"`}, // 5xx is ERROR
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		logger := logrus.New()
		logger.SetOutput(&buf)
		logger.SetLevel(logrus.DebugLevel)
		logger.SetFormatter(&logrus.JSONFormatter{})

		e := newEcho()
		e.Use(Logging(logger))
		status := tc.status
		e.GET("/x", func(c echo.Context) error {
			return c.NoContent(status)
		})

		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		require.Truef(t, strings.Contains(buf.String(), tc.levelStr),
			"status %d expected %s in log; got %s", tc.status, tc.levelStr, buf.String())
	}
}

func TestLogging_SlowRequestPromotesToInfo(t *testing.T) {
	var buf bytes.Buffer
	logger := logrus.New()
	logger.SetOutput(&buf)
	logger.SetLevel(logrus.DebugLevel)
	logger.SetFormatter(&logrus.JSONFormatter{})

	e := newEcho()
	// Threshold low enough that any non-trivial handler beats it.
	e.Use(LoggingWithOptions(logger, LoggingOptions{SlowMs: 1}))
	e.GET("/slow", func(c echo.Context) error {
		time.Sleep(5 * time.Millisecond)
		return c.NoContent(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/slow", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	out := buf.String()
	require.Contains(t, out, `"level":"info"`)
	require.Contains(t, out, `"slow":true`)
	require.Contains(t, out, `"msg":"request completed (slow)"`)
}

func TestMetricsMiddleware_ObservesRouteTemplate(t *testing.T) {
	var (
		gotRoute  string
		gotStatus int
		gotDur    time.Duration
	)
	observe := func(route string, status int, dur time.Duration) {
		gotRoute, gotStatus, gotDur = route, status, dur
	}
	slowCalled := false
	slow := func() { slowCalled = true }

	e := newEcho()
	e.Use(Metrics(MetricsOptions{Observe: observe, SlowMs: 1, Slow: slow}))
	e.GET("/api/cards/:id", func(c echo.Context) error {
		time.Sleep(3 * time.Millisecond)
		return c.NoContent(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/cards/abc-123", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, "/api/cards/:id", gotRoute, "must collapse to route template, not raw URL")
	require.Equal(t, http.StatusOK, gotStatus)
	require.Greater(t, gotDur.Milliseconds(), int64(0))
	require.True(t, slowCalled, "slow marker should fire above threshold")
}

func TestMetricsMiddleware_UnmatchedRouteBucketed(t *testing.T) {
	var gotRoute string
	observe := func(route string, _ int, _ time.Duration) { gotRoute = route }

	e := newEcho()
	e.Use(Metrics(MetricsOptions{Observe: observe}))
	// No route registered for /missing.

	req := httptest.NewRequest(http.MethodGet, "/missing/secret-id", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, "unmatched", gotRoute, "unmatched routes must collapse to a single label")
}
