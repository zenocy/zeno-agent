package middleware

import (
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
)

// LoggingOptions tunes the logging middleware. SlowMs is the threshold above
// which a 2xx response is logged at INFO with `slow=true`; faster 2xx
// responses log at DEBUG to keep INFO output actionable. 4xx logs at INFO
// (client error, not an operator alarm); 5xx and handler errors at ERROR.
type LoggingOptions struct {
	SlowMs time.Duration
}

// Logging returns Echo middleware that logs each request via logrus with
// method, path, status, duration, remote IP, user agent, and response size.
func Logging(logger *logrus.Logger) echo.MiddlewareFunc {
	return LoggingWithOptions(logger, LoggingOptions{})
}

// LoggingWithOptions is the configurable form of Logging. Use this from the
// HTTP server to thread the slow-request threshold from config.
func LoggingWithOptions(logger *logrus.Logger, opts LoggingOptions) echo.MiddlewareFunc {
	slowMs := opts.SlowMs
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			start := time.Now()
			req := c.Request()

			err := next(c)

			duration := time.Since(start)
			status := c.Response().Status

			fields := logrus.Fields{
				"method":      req.Method,
				"path":        req.URL.Path,
				"status":      status,
				"duration_ms": duration.Milliseconds(),
				"remote_ip":   c.RealIP(),
				"user_agent":  req.UserAgent(),
				"bytes_out":   c.Response().Size,
			}
			if reqID := GetRequestID(c); reqID != "" {
				fields["request_id"] = reqID
			}

			slow := slowMs > 0 && duration >= slowMs
			if slow {
				fields["slow"] = true
			}
			entry := logger.WithFields(fields)
			switch {
			case err != nil:
				entry.WithError(err).Error("request failed")
			case status >= 500:
				entry.Error("server error")
			case status >= 400:
				entry.Info("client error")
			case slow:
				entry.Info("request completed (slow)")
			default:
				entry.Debug("request completed")
			}
			return err
		}
	}
}
