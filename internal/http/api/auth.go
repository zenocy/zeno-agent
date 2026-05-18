package api

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"time"

	"github.com/labstack/echo-contrib/session"
	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/bcrypt"

	mw "github.com/zenocy/zeno-v2/internal/http/middleware"
)

// AuthHandler implements the cookie-based login surface used by the
// browser UI. It is intentionally tiny — there is one user, configured in
// YAML; we only persist a server-side session row (via the session
// middleware's store) so a logout invalidates the cookie immediately.
//
// FailedLoginDelay is the wall-clock penalty applied to a rejected login
// to slow online brute-force attempts. Tests can shorten it to keep the
// suite snappy; production keeps the default.
type AuthHandler struct {
	Username         string
	PasswordHash     string
	CookieName       string
	FailedLoginDelay time.Duration
	Log              *logrus.Entry
}

// Register wires the routes onto the Echo instance. These three routes
// MUST be in the auth middleware's exempt list — otherwise a logged-out
// user can never reach /login to authenticate.
func (h *AuthHandler) Register(e *echo.Echo) {
	e.POST("/api/auth/login", h.login)
	e.POST("/api/auth/logout", h.logout)
	e.GET("/api/auth/me", h.me)
}

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (h *AuthHandler) login(c echo.Context) error {
	var req loginReq
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "invalid request body")
	}
	if req.Username == "" || req.Password == "" {
		return BadRequest(c, "username and password are required")
	}

	if !h.credentialsValid(req.Username, req.Password) {
		h.sleepFailedDelay()
		if h.Log != nil {
			h.Log.WithField("username", req.Username).Warn("auth: login failed")
		}
		return Unauthorized(c, "invalid credentials")
	}

	sess, err := session.Get(h.CookieName, c)
	if err != nil {
		return Internal(c, err)
	}
	if err := mw.IssueSession(c, sess, req.Username); err != nil {
		return Internal(c, err)
	}
	if h.Log != nil {
		h.Log.WithField("username", req.Username).Info("auth: login")
	}
	return c.NoContent(http.StatusNoContent)
}

func (h *AuthHandler) logout(c echo.Context) error {
	sess, err := session.Get(h.CookieName, c)
	if err != nil {
		// Treat a missing cookie as a successful logout — no session, no
		// state to clear. Match the idempotent behavior most apps expose.
		return c.NoContent(http.StatusNoContent)
	}
	if err := mw.ClearSession(c, sess); err != nil {
		return Internal(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

type meResp struct {
	Username string `json:"username"`
}

func (h *AuthHandler) me(c echo.Context) error {
	sess, err := session.Get(h.CookieName, c)
	if err != nil {
		return Unauthorized(c, "unauthorized")
	}
	username, _ := sess.Values[mw.SessionUsernameKey].(string)
	if username == "" || username != h.Username {
		return Unauthorized(c, "unauthorized")
	}
	return c.JSON(http.StatusOK, meResp{Username: username})
}

// credentialsValid is the constant-time comparison of submitted creds
// against the configured user. bcrypt.CompareHashAndPassword already runs
// in constant time per its docs; the username check uses subtle to avoid
// leaking whether the username was wrong vs. the password.
func (h *AuthHandler) credentialsValid(username, password string) bool {
	userMatches := subtle.ConstantTimeCompare([]byte(username), []byte(h.Username)) == 1
	hashErr := bcrypt.CompareHashAndPassword([]byte(h.PasswordHash), []byte(password))
	if !userMatches {
		return false
	}
	if errors.Is(hashErr, bcrypt.ErrMismatchedHashAndPassword) {
		return false
	}
	return hashErr == nil
}

func (h *AuthHandler) sleepFailedDelay() {
	d := h.FailedLoginDelay
	if d == 0 {
		d = 250 * time.Millisecond
	}
	if d < 0 {
		return
	}
	time.Sleep(d)
}
