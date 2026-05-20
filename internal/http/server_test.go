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

// TestServiceTierMiddleware_StampsCtx pins the V2.x wiring that lets an
// operator route interactive HTTP requests to a non-default OpenRouter
// tier without each handler having to plumb config through. The
// middleware runs before any handler-level ctx wrapping (stream
// publishers, auth, etc.), so every downstream LLM call inherits the
// tier from the request ctx unless a per-call WithServiceTier overrides.
func TestServiceTierMiddleware_StampsCtx(t *testing.T) {
	var got string
	e := echo.New()
	e.Use(serviceTierMiddleware("priority"))
	e.GET("/probe", func(c echo.Context) error {
		got = llm.ServiceTierFromContext(c.Request().Context())
		return c.NoContent(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "priority", got,
		"handler ctx must carry the tier the middleware was configured with")
}

// TestServer_New_NoTierMeansNoMiddleware pins the safe-rollout promise:
// when ServiceTierInteractive is empty, the middleware must not be
// mounted at all — a downstream handler must see an unwrapped ctx so
// the LLM call falls through to the upstream default (i.e. no
// service_tier field sent at all, matching legacy behavior).
func TestServer_New_NoTierMeansNoMiddleware(t *testing.T) {
	var got string
	srv := newWithProbe(t, "", func(c echo.Context) error {
		got = llm.ServiceTierFromContext(c.Request().Context())
		return c.NoContent(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	srv.Echo.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "", got,
		"empty ServiceTierInteractive must leave the ctx untouched")
}

func TestServer_New_TierMountsMiddleware(t *testing.T) {
	var got string
	srv := newWithProbe(t, "flex", func(c echo.Context) error {
		got = llm.ServiceTierFromContext(c.Request().Context())
		return c.NoContent(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	srv.Echo.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "flex", got,
		"server.New must mount the tier middleware when ServiceTierInteractive is set")
}

// newWithProbe stands up a Server via New() so the full middleware
// chain — including the conditional serviceTierMiddleware — is wired
// the way production wires it. The probe handler captures whatever
// state the test cares about.
func newWithProbe(t *testing.T, tier string, probe echo.HandlerFunc) *Server {
	t.Helper()
	srv := New(ServerConfig{
		Bind:                   "127.0.0.1",
		Port:                   0,
		ServiceTierInteractive: tier,
	}, quietLogger())
	srv.Echo.GET("/probe", probe)
	return srv
}
