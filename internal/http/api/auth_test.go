package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/sessions"
	echocontribsession "github.com/labstack/echo-contrib/session"
	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	mw "github.com/zenocy/zeno-v2/internal/http/middleware"
)

const (
	testUser    = "alice"
	testPass    = "s3cret"
	testCookie  = "zeno_session_test"
	authKeyPair = "0123456789abcdef0123456789abcdef" // 32 bytes; HMAC for cookie store
)

// newAuthTestServer mounts an Echo with the session middleware backed by a
// pure in-memory cookie store. AuthHandler hangs off it. Returns the
// router (callers use httptest.NewRecorder directly).
func newAuthTestServer(t *testing.T, delay time.Duration) (*echo.Echo, *AuthHandler) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(testPass), bcrypt.MinCost)
	require.NoError(t, err)
	store := sessions.NewCookieStore([]byte(authKeyPair))
	store.Options = &sessions.Options{Path: "/", MaxAge: 3600, HttpOnly: true}
	e := echo.New()
	e.Use(echocontribsession.Middleware(store))
	handler := &AuthHandler{
		Username:         testUser,
		PasswordHash:     string(hash),
		CookieName:       testCookie,
		FailedLoginDelay: delay,
		Log:              logrus.New().WithField("c", "auth-test"),
	}
	handler.Register(e)
	return e, handler
}

func loginBody(username, password string) io.Reader {
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	return bytes.NewReader(body)
}

func TestLogin_Valid_SetsCookieAndReturns204(t *testing.T) {
	e, _ := newAuthTestServer(t, 0)

	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", loginBody(testUser, testPass))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code)
	setCookie := rec.Header().Get("Set-Cookie")
	require.NotEmpty(t, setCookie, "login must set a session cookie")
	require.True(t, strings.HasPrefix(setCookie, testCookie+"="), "cookie name = %s", setCookie)
	require.Contains(t, strings.ToLower(setCookie), "httponly")
}

func TestLogin_BadPassword_Returns401AndDelays(t *testing.T) {
	e, _ := newAuthTestServer(t, 50*time.Millisecond)

	start := time.Now()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", loginBody(testUser, "wrong"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.GreaterOrEqual(t, elapsed, 40*time.Millisecond, "rate-guard delay should be applied")
	require.Empty(t, rec.Header().Get("Set-Cookie"), "no cookie on failed login")
}

func TestLogin_BadUsername_Returns401(t *testing.T) {
	e, _ := newAuthTestServer(t, 0)

	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", loginBody("bob", testPass))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestLogin_MalformedJSON_Returns400(t *testing.T) {
	e, _ := newAuthTestServer(t, 0)

	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestLogin_MissingFields_Returns400(t *testing.T) {
	e, _ := newAuthTestServer(t, 0)

	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", loginBody("", ""))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestMe_UnauthenticatedReturns401(t *testing.T) {
	e, _ := newAuthTestServer(t, 0)

	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestMe_AfterLoginReturnsUsername(t *testing.T) {
	e, _ := newAuthTestServer(t, 0)

	loginReq := httptest.NewRequest(http.MethodPost, "/api/auth/login", loginBody(testUser, testPass))
	loginReq.Header.Set("Content-Type", "application/json")
	loginRec := httptest.NewRecorder()
	e.ServeHTTP(loginRec, loginReq)
	require.Equal(t, http.StatusNoContent, loginRec.Code)
	cookie := loginRec.Header().Get("Set-Cookie")
	require.NotEmpty(t, cookie)

	meReq := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	meReq.Header.Set("Cookie", strings.SplitN(cookie, ";", 2)[0])
	meRec := httptest.NewRecorder()
	e.ServeHTTP(meRec, meReq)

	require.Equal(t, http.StatusOK, meRec.Code)
	var body meResp
	require.NoError(t, json.Unmarshal(meRec.Body.Bytes(), &body))
	require.Equal(t, testUser, body.Username)
}

func TestLogout_TombstonesCookie(t *testing.T) {
	e, _ := newAuthTestServer(t, 0)

	loginReq := httptest.NewRequest(http.MethodPost, "/api/auth/login", loginBody(testUser, testPass))
	loginReq.Header.Set("Content-Type", "application/json")
	loginRec := httptest.NewRecorder()
	e.ServeHTTP(loginRec, loginReq)
	require.Equal(t, http.StatusNoContent, loginRec.Code)
	cookie := strings.SplitN(loginRec.Header().Get("Set-Cookie"), ";", 2)[0]

	logoutReq := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	logoutReq.Header.Set("Cookie", cookie)
	logoutRec := httptest.NewRecorder()
	e.ServeHTTP(logoutRec, logoutReq)

	require.Equal(t, http.StatusNoContent, logoutRec.Code)
	cleared := logoutRec.Header().Get("Set-Cookie")
	require.NotEmpty(t, cleared)
	require.Contains(t, strings.ToLower(cleared), "max-age=0",
		"logout must tombstone the cookie via Max-Age=0")
	// Server-side invalidation (so a stolen cookie can't be replayed) is
	// covered by the system test against the real gormstore — the cookie
	// store used here is intentionally stateless.
}

func TestLogout_NoCookie_IsIdempotent(t *testing.T) {
	e, _ := newAuthTestServer(t, 0)

	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code)
}

func TestIssueAndClearSession_RoundTrip(t *testing.T) {
	store := sessions.NewCookieStore([]byte(authKeyPair))
	store.Options = &sessions.Options{Path: "/", MaxAge: 3600, HttpOnly: true}
	e := echo.New()
	e.Use(echocontribsession.Middleware(store))
	e.POST("/issue", func(c echo.Context) error {
		sess, err := echocontribsession.Get(testCookie, c)
		require.NoError(t, err)
		require.NoError(t, mw.IssueSession(c, sess, "alice"))
		return c.NoContent(http.StatusNoContent)
	})
	e.POST("/clear", func(c echo.Context) error {
		sess, err := echocontribsession.Get(testCookie, c)
		require.NoError(t, err)
		require.NoError(t, mw.ClearSession(c, sess))
		return c.NoContent(http.StatusNoContent)
	})

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/issue", nil))
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.NotEmpty(t, rec.Header().Get("Set-Cookie"))

	rec2 := httptest.NewRecorder()
	clearReq := httptest.NewRequest(http.MethodPost, "/clear", nil)
	clearReq.Header.Set("Cookie", strings.SplitN(rec.Header().Get("Set-Cookie"), ";", 2)[0])
	e.ServeHTTP(rec2, clearReq)
	require.Equal(t, http.StatusNoContent, rec2.Code)
	require.Contains(t, strings.ToLower(rec2.Header().Get("Set-Cookie")), "max-age=0")
}
