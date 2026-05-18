package system_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"testing"
	"time"

	"github.com/gorilla/sessions"
	echocontribsession "github.com/labstack/echo-contrib/session"
	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"github.com/wader/gormstore/v2"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/http/api"
	mw "github.com/zenocy/zeno-v2/internal/http/middleware"
)

// HarnessAuth captures the V2.14 cookie-auth knobs the system tests can
// toggle. Plaintext password — the harness hashes it internally and the
// hash never leaves the test process.
type HarnessAuth struct {
	Enabled  bool
	Username string
	Password string
	// SessionTTL defaults to 24h when zero so tests don't have to import
	// time everywhere just to pass a sentinel.
	SessionTTL time.Duration
}

const (
	harnessAuthCookie = "zeno_session_systest"
	harnessAuthSecret = "0123456789abcdef0123456789abcdef" // 32 bytes
)

// installAuth wires the AuthHandler + session middleware + Auth middleware
// onto the given Echo instance when cfg.Enabled is true. Returns the
// gormstore.Store (so tests can mutate the sessions table directly), or
// nil when auth is disabled.
//
// This mirrors what cmd/zeno/main.go does for the real binary. Kept in a
// helper so the harness shape stays unchanged for tests that don't care
// about auth.
func installAuth(t *testing.T, e *echo.Echo, db *gorm.DB, cfg HarnessAuth, logger *logrus.Logger) *gormstore.Store {
	t.Helper()
	if !cfg.Enabled {
		return nil
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(cfg.Password), bcrypt.MinCost)
	require.NoError(t, err)
	ttl := cfg.SessionTTL
	if ttl == 0 {
		ttl = 24 * time.Hour
	}
	store := gormstore.New(db, []byte(harnessAuthSecret))
	store.SessionOpts = &sessions.Options{
		Path:     "/",
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
	store.MaxAge(int(ttl.Seconds()))
	e.Use(echocontribsession.Middleware(store))
	e.Use(mw.Auth(mw.AuthConfig{
		Enabled:    true,
		Username:   cfg.Username,
		CookieName: harnessAuthCookie,
		SessionTTL: ttl,
	}))
	(&api.AuthHandler{
		Username:         cfg.Username,
		PasswordHash:     string(hash),
		CookieName:       harnessAuthCookie,
		FailedLoginDelay: 1 * time.Millisecond, // keep the suite snappy
		Log:              logger.WithField("c", "auth"),
	}).Register(e)
	return store
}

// LoginClient performs the login round-trip and returns an http.Client
// with the session cookie already in its jar. Use it to drive subsequent
// authenticated requests against h.Server.URL.
func (h *Harness) LoginClient(username, password string) *http.Client {
	h.t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(h.t, err)
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}

	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	req, err := http.NewRequest(http.MethodPost, h.Server.URL+"/api/auth/login", bytes.NewReader(body))
	require.NoError(h.t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	require.NoError(h.t, err)
	defer resp.Body.Close()
	require.Equal(h.t, http.StatusNoContent, resp.StatusCode, "login should succeed")
	return client
}

// GetWith hits a GET endpoint with the supplied client (so callers can use
// a logged-in client returned by LoginClient).
func (h *Harness) GetWith(client *http.Client, path string) (int, []byte) {
	h.t.Helper()
	resp, err := client.Get(h.Server.URL + path)
	require.NoError(h.t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// PostWith hits a POST endpoint with the supplied client and optional
// JSON payload.
func (h *Harness) PostWith(client *http.Client, path string, payload any) (int, []byte) {
	h.t.Helper()
	var bodyReader io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		require.NoError(h.t, err)
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(http.MethodPost, h.Server.URL+path, bodyReader)
	require.NoError(h.t, err)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	require.NoError(h.t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}
