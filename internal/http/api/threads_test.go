package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	zlog "github.com/zenocy/zeno-v2/internal/log"
)

func newThreadsStore(t *testing.T) zlog.Store {
	t.Helper()
	_, store, err := zlog.Open(t.TempDir() + "/zeno.db")
	require.NoError(t, err)
	return store
}

func buildThreadsHandler(t *testing.T) (*echo.Echo, zlog.Store) {
	t.Helper()
	e := echo.New()
	store := newThreadsStore(t)
	(&ThreadsHandler{
		Reader: store,
		Now:    func() time.Time { return time.Date(2026, 5, 18, 9, 0, 0, 0, time.UTC) },
	}).Register(e)
	return e, store
}

func appendMail(t *testing.T, w zlog.Writer, subject, from, body string, when time.Time) {
	t.Helper()
	_, err := w.Append(context.Background(), zlog.KindMailReceived, "imap", map[string]any{
		"subject":      subject,
		"from":         from,
		"date":         when.Format(time.RFC3339),
		"body_preview": body,
	})
	require.NoError(t, err)
}

func TestThreadsPreview_ReturnsLatestMatchVerbatim(t *testing.T) {
	e, store := buildThreadsHandler(t)
	appendMail(t, store, "Homework Week of 18 May", "Miss Despoina <despoina@school>",
		"Monday\nGreek: page 57\n", time.Date(2026, 5, 17, 10, 0, 0, 0, time.UTC))

	req := httptest.NewRequest(http.MethodGet, "/api/threads/preview?hint=homework", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out threadPreviewResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, "Homework Week of 18 May", out.Subject)
	require.Equal(t, "Miss Despoina <despoina@school>", out.From)
	require.True(t, strings.Contains(out.Body, "Monday"))
	require.True(t, strings.Contains(out.Body, "page 57"))
}

func TestThreadsPreview_PrefersMostRecent(t *testing.T) {
	e, store := buildThreadsHandler(t)
	appendMail(t, store, "Homework Week of 11 May", "Miss Despoina", "older content",
		time.Date(2026, 5, 11, 10, 0, 0, 0, time.UTC))
	appendMail(t, store, "Homework Week of 18 May", "Miss Despoina", "newer content",
		time.Date(2026, 5, 17, 10, 0, 0, 0, time.UTC))

	req := httptest.NewRequest(http.MethodGet, "/api/threads/preview?hint=homework", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var out threadPreviewResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, "newer content", out.Body)
}

func TestThreadsPreview_404WhenNoMatch(t *testing.T) {
	e, store := buildThreadsHandler(t)
	appendMail(t, store, "Unrelated subject", "x@y", "body", time.Date(2026, 5, 17, 10, 0, 0, 0, time.UTC))

	req := httptest.NewRequest(http.MethodGet, "/api/threads/preview?hint=homework", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

func TestThreadsPreview_400WhenHintMissing(t *testing.T) {
	e, _ := buildThreadsHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/api/threads/preview", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}
