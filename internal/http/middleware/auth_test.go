package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/sessions"
	echocontribsession "github.com/labstack/echo-contrib/session"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

const (
	mwTestKey    = "0123456789abcdef0123456789abcdef"
	mwCookieName = "zeno_session_mw_test"
	mwUser       = "alice"
	mwBearer     = "lan-secret"
)

func newAuthEcho(t *testing.T, cfg AuthConfig) (*echo.Echo, *sessions.CookieStore) {
	t.Helper()
	store := sessions.NewCookieStore([]byte(mwTestKey))
	store.Options = &sessions.Options{Path: "/", MaxAge: 3600, HttpOnly: true}
	e := echo.New()
	e.Use(echocontribsession.Middleware(store))
	e.Use(Auth(cfg))
	e.GET("/api/protected", func(c echo.Context) error {
		user, _ := c.Get(AuthContextKey).(string)
		return c.JSON(http.StatusOK, map[string]string{"user": user})
	})
	e.GET("/api/health", func(c echo.Context) error {
		return c.NoContent(http.StatusOK)
	})
	e.GET("/api/auth/login", func(c echo.Context) error {
		return c.NoContent(http.StatusOK)
	})
	e.GET("/static", func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})
	return e, store
}

// issue logs the user in by directly setting cookie values via a one-off
// route, returning the resulting Set-Cookie header value. Mirrors what
// AuthHandler.login does in production.
func issueSessionForTest(t *testing.T, e *echo.Echo, username string, createdAt time.Time) string {
	t.Helper()
	e.POST("/test/issue", func(c echo.Context) error {
		sess, err := echocontribsession.Get(mwCookieName, c)
		require.NoError(t, err)
		sess.Values[SessionUsernameKey] = username
		sess.Values[SessionCreatedAtKey] = createdAt.Unix()
		require.NoError(t, sess.Save(c.Request(), c.Response()))
		return c.NoContent(http.StatusNoContent)
	})
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/test/issue", nil))
	require.Equal(t, http.StatusNoContent, rec.Code)
	raw := rec.Header().Get("Set-Cookie")
	require.NotEmpty(t, raw)
	return strings.SplitN(raw, ";", 2)[0]
}

func TestAuth_DisabledPassthrough(t *testing.T) {
	e, _ := newAuthEcho(t, AuthConfig{Enabled: false})

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/protected", nil))
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestAuth_NoCredsRejectedWithJSON(t *testing.T) {
	e, _ := newAuthEcho(t, AuthConfig{
		Enabled:    true,
		Username:   mwUser,
		CookieName: mwCookieName,
	})

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/protected", nil))
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Contains(t, rec.Body.String(), `"error":"unauthorized"`)
}

func TestAuth_CookieAccepted_SetsContextUser(t *testing.T) {
	e, _ := newAuthEcho(t, AuthConfig{
		Enabled:    true,
		Username:   mwUser,
		CookieName: mwCookieName,
		SessionTTL: 24 * time.Hour,
	})
	cookie := issueSessionForTest(t, e, mwUser, time.Now())

	req := httptest.NewRequest(http.MethodGet, "/api/protected", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), mwUser)
}

func TestAuth_BearerAccepted(t *testing.T) {
	e, _ := newAuthEcho(t, AuthConfig{
		Enabled:    true,
		Username:   mwUser,
		CookieName: mwCookieName,
		LANToken:   mwBearer,
	})

	req := httptest.NewRequest(http.MethodGet, "/api/protected", nil)
	req.Header.Set("Authorization", "Bearer "+mwBearer)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
}

func TestAuth_BadBearer_Rejected(t *testing.T) {
	e, _ := newAuthEcho(t, AuthConfig{
		Enabled:    true,
		Username:   mwUser,
		CookieName: mwCookieName,
		LANToken:   mwBearer,
	})

	req := httptest.NewRequest(http.MethodGet, "/api/protected", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAuth_ExpiredCookieRejected(t *testing.T) {
	e, _ := newAuthEcho(t, AuthConfig{
		Enabled:    true,
		Username:   mwUser,
		CookieName: mwCookieName,
		SessionTTL: 1 * time.Hour,
	})
	cookie := issueSessionForTest(t, e, mwUser, time.Now().Add(-2*time.Hour))

	req := httptest.NewRequest(http.MethodGet, "/api/protected", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAuth_UsernameMismatchRejected(t *testing.T) {
	e, _ := newAuthEcho(t, AuthConfig{
		Enabled:    true,
		Username:   "bob", // configured user
		CookieName: mwCookieName,
		SessionTTL: 24 * time.Hour,
	})
	cookie := issueSessionForTest(t, e, "alice", time.Now()) // stale session for prior user

	req := httptest.NewRequest(http.MethodGet, "/api/protected", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAuth_ExemptPaths(t *testing.T) {
	e, _ := newAuthEcho(t, AuthConfig{
		Enabled:    true,
		Username:   mwUser,
		CookieName: mwCookieName,
	})

	for _, p := range []string{"/api/health", "/api/auth/login"} {
		t.Run(p, func(t *testing.T) {
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
			require.Equal(t, http.StatusOK, rec.Code)
		})
	}
}

func TestAuth_NonAPIPathPassThrough(t *testing.T) {
	e, _ := newAuthEcho(t, AuthConfig{
		Enabled:    true,
		Username:   mwUser,
		CookieName: mwCookieName,
	})

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static", nil))
	require.Equal(t, http.StatusOK, rec.Code)
}
