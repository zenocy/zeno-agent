package http

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/llm"
)

func quietLogger() *logrus.Logger {
	l := logrus.New()
	l.SetOutput(io.Discard)
	return l
}

// TestCallProfileMiddleware_StampsCtx pins the V2.x wiring that tags
// every inbound HTTP request as "interactive" so downstream LLM calls
// can pick a latency/cost knob per provider (OpenAI service_tier,
// Gemini thinkingLevel) without each handler plumbing config through.
// The middleware runs before any handler-level ctx wrapping (stream
// publishers, auth, etc.) so every downstream call inherits the
// profile unless a per-call WithServiceTier overrides.
func TestCallProfileMiddleware_StampsCtx(t *testing.T) {
	var got llm.CallProfile
	e := echo.New()
	e.Use(callProfileMiddleware())
	e.GET("/probe", func(c echo.Context) error {
		got = llm.CallProfileFromContext(c.Request().Context())
		return c.NoContent(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, llm.CallProfileInteractive, got,
		"handler ctx must carry CallProfileInteractive")
}

func TestServer_New_AlwaysMountsCallProfile(t *testing.T) {
	var got llm.CallProfile
	srv := newWithProbe(t, func(c echo.Context) error {
		got = llm.CallProfileFromContext(c.Request().Context())
		return c.NoContent(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	srv.Echo.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, llm.CallProfileInteractive, got,
		"server.New must mount the call-profile middleware unconditionally")
}

// newWithProbe stands up a Server via New() so the full middleware
// chain — including the call-profile middleware — is wired the way
// production wires it. The probe handler captures whatever state the
// test cares about.
func newWithProbe(t *testing.T, probe echo.HandlerFunc) *Server {
	t.Helper()
	srv := New(ServerConfig{
		Bind: "127.0.0.1",
		Port: 0,
	}, quietLogger())
	srv.Echo.GET("/probe", probe)
	return srv
}
