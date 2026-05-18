package api

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

// errBody is the canonical shape of an API error response: a single "error"
// field. Sites that need to attach additional context fields (e.g. the id of
// the missing row) inline a `map[string]string{...}` literal instead — those
// are the minority and carry no shared shape.
func errBody(msg string) map[string]string {
	return map[string]string{"error": msg}
}

// BadRequest returns a 400 with {"error": msg}.
func BadRequest(c echo.Context, msg string) error {
	return c.JSON(http.StatusBadRequest, errBody(msg))
}

// Unauthorized returns a 401 with {"error": msg}. The auth surface emits
// this directly; protected endpoints rely on the auth middleware to send
// 401s for them, so most handlers will not need this helper.
func Unauthorized(c echo.Context, msg string) error {
	return c.JSON(http.StatusUnauthorized, errBody(msg))
}

// NotFound returns a 404 with {"error": msg}.
func NotFound(c echo.Context, msg string) error {
	return c.JSON(http.StatusNotFound, errBody(msg))
}

// Conflict returns a 409 with {"error": msg}.
func Conflict(c echo.Context, msg string) error {
	return c.JSON(http.StatusConflict, errBody(msg))
}

// TooManyRequests returns a 429 with {"error": msg}.
func TooManyRequests(c echo.Context, msg string) error {
	return c.JSON(http.StatusTooManyRequests, errBody(msg))
}

// ServiceUnavailable returns a 503 with {"error": msg}.
func ServiceUnavailable(c echo.Context, msg string) error {
	return c.JSON(http.StatusServiceUnavailable, errBody(msg))
}

// GatewayTimeout returns a 504 with {"error": msg}.
func GatewayTimeout(c echo.Context, msg string) error {
	return c.JSON(http.StatusGatewayTimeout, errBody(msg))
}

// Internal returns a 500 with {"error": err.Error()}. Preserves the
// pre-existing leak of err.Error() into the response body so handler tests
// keep passing — callers that need to keep an error from clients should
// build their own response.
func Internal(c echo.Context, err error) error {
	return c.JSON(http.StatusInternalServerError, errBody(err.Error()))
}
