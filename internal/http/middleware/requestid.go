package middleware

import (
	"github.com/labstack/echo/v4"
	"github.com/zenocy/zeno-v2/internal/idgen"
)

const (
	RequestIDHeader     = "X-Request-ID"
	RequestIDContextKey = "request_id"
)

// RequestID returns Echo middleware that ensures every request has a unique
// ID. If the inbound request carries X-Request-ID it's reused; otherwise a
// new UUID is generated. The ID lands in the Echo context and the response
// header.
func RequestID() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			reqID := c.Request().Header.Get(RequestIDHeader)
			if reqID == "" {
				reqID = idgen.New()
			}
			c.Set(RequestIDContextKey, reqID)
			c.Response().Header().Set(RequestIDHeader, reqID)
			return next(c)
		}
	}
}

// GetRequestID extracts the request ID from the Echo context.
func GetRequestID(c echo.Context) string {
	if id, ok := c.Get(RequestIDContextKey).(string); ok {
		return id
	}
	return ""
}
