package middleware

import (
	"crypto/subtle"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/sessions"
	"github.com/labstack/echo-contrib/session"
	"github.com/labstack/echo/v4"
)

// AuthContextKey is where authenticated requests record the username so
// downstream handlers can read it via `c.Get(AuthContextKey)`.
const AuthContextKey = "auth.user"

// SessionUsernameKey is the gorilla-session Values key holding the
// authenticated username. Handlers writing to the session at login should
// use the same constant.
const SessionUsernameKey = "username"

// SessionCreatedAtKey records when the session was issued. Used for the
// TTL check below — gorilla/sessions ages the cookie, but gormstore deletes
// the row on time, so this is a belt-and-braces guard in case the DB row
// somehow outlives MaxAge.
const SessionCreatedAtKey = "created_at"

// AuthConfig is the runtime view of config.AuthConfig needed by the
// middleware. The HTTP package keeps its own view to avoid an import cycle
// against the higher-level config package.
type AuthConfig struct {
	Enabled    bool
	Username   string
	CookieName string
	LANToken   string
	SessionTTL time.Duration
}

// Auth returns an Echo middleware that gates /api/* requests behind either
// a valid session cookie or an Authorization: Bearer <lan_token> header.
//
// Layout:
//   - When cfg.Enabled is false, the middleware passes everything through
//     (preserves the pre-auth loopback behavior; emergency rollback path).
//   - Non-/api/* paths pass through — the SPA bundle is served openly and
//     the frontend bounces to /login on a 401 from /api/auth/me.
//   - /api/auth/login, /api/auth/logout, /api/auth/me, and /api/health are
//     exempt so the login flow and container healthcheck always reach the
//     handler.
//   - A valid cookie sets c.Get(AuthContextKey) = username and proceeds.
//   - A valid LANToken bearer (when configured) proceeds without setting
//     a username — preserving the existing CLI/CI bearer usage.
//   - Otherwise: 401 {"error":"unauthorized"}.
func Auth(cfg AuthConfig) echo.MiddlewareFunc {
	exempt := map[string]struct{}{
		"/api/auth/login":  {},
		"/api/auth/logout": {},
		"/api/auth/me":     {},
		"/api/health":      {},
	}
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if !cfg.Enabled {
				return next(c)
			}
			path := c.Request().URL.Path
			if !strings.HasPrefix(path, "/api/") {
				return next(c)
			}
			if _, ok := exempt[path]; ok {
				return next(c)
			}

			if username, ok := authenticatedUsername(c, cfg); ok {
				c.Set(AuthContextKey, username)
				return next(c)
			}

			if cfg.LANToken != "" {
				want := "Bearer " + cfg.LANToken
				got := c.Request().Header.Get("Authorization")
				if subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1 {
					return next(c)
				}
			}

			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		}
	}
}

// authenticatedUsername returns the username stored on the request's
// session cookie, if any, and whether that session is still within its
// configured TTL. The session middleware (echo-contrib/session) must be
// mounted upstream for this to work.
func authenticatedUsername(c echo.Context, cfg AuthConfig) (string, bool) {
	sess, err := session.Get(cfg.CookieName, c)
	if err != nil || sess == nil {
		return "", false
	}
	username, _ := sess.Values[SessionUsernameKey].(string)
	if username == "" {
		return "", false
	}
	// Defense-in-depth TTL check: gorilla manages cookie MaxAge and
	// gormstore deletes expired DB rows, but a created_at value lets us
	// reject sessions issued before a TTL shortening config change.
	if cfg.SessionTTL > 0 {
		if createdUnix, ok := sess.Values[SessionCreatedAtKey].(int64); ok {
			created := time.Unix(createdUnix, 0)
			if time.Since(created) > cfg.SessionTTL {
				return "", false
			}
		}
	}
	// Match the configured username explicitly; rotating auth.username in
	// config.yaml then must invalidate live sessions issued for the old
	// account.
	if cfg.Username != "" && username != cfg.Username {
		return "", false
	}
	return username, true
}

// IssueSession writes the username + issued-at into the given session and
// saves it. Centralized so login handlers and tests share one code path.
func IssueSession(c echo.Context, sess *sessions.Session, username string) error {
	sess.Values[SessionUsernameKey] = username
	sess.Values[SessionCreatedAtKey] = time.Now().Unix()
	return sess.Save(c.Request(), c.Response())
}

// ClearSession marks the gorilla session for deletion. gormstore reads
// MaxAge<0 and removes the DB row + sets a tombstone cookie.
func ClearSession(c echo.Context, sess *sessions.Session) error {
	sess.Options.MaxAge = -1
	delete(sess.Values, SessionUsernameKey)
	delete(sess.Values, SessionCreatedAtKey)
	return sess.Save(c.Request(), c.Response())
}
