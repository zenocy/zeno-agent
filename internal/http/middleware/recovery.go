package middleware

import (
	"fmt"
	"net/http"
	"runtime"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
)

// Recovery returns Echo middleware that recovers from panics and logs the
// stack trace using the provided logger.
func Recovery(logger *logrus.Logger) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			defer func() {
				if r := recover(); r != nil {
					buf := make([]byte, 4096)
					n := runtime.Stack(buf, false)

					logger.WithFields(logrus.Fields{
						"panic":      fmt.Sprintf("%v", r),
						"stack":      string(buf[:n]),
						"request_id": GetRequestID(c),
						"method":     c.Request().Method,
						"path":       c.Request().URL.Path,
					}).Error("panic recovered")

					_ = c.JSON(http.StatusInternalServerError, map[string]string{
						"error": "internal server error",
					})
				}
			}()
			return next(c)
		}
	}
}
