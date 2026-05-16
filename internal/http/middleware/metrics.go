package middleware

import (
	"time"

	"github.com/labstack/echo/v4"
)

// HTTPObserver is the narrow callback the metrics middleware uses. Mirrors
// metrics.Metrics.ObserveHTTP so this package stays free of the metrics
// import. Pass nil to disable the middleware (registration becomes a no-op).
type HTTPObserver func(route string, status int, dur time.Duration)

// SlowMarker is invoked when the request exceeded slowMs so the metrics
// state can flag it for the snapshot view. May be nil.
type SlowMarker func()

// MetricsOptions tunes the metrics middleware. Route normalisation reads
// c.Path() (the matched template, e.g. /api/cards/:id), never the raw URL.
type MetricsOptions struct {
	Observe HTTPObserver
	SlowMs  time.Duration
	Slow    SlowMarker
}

// Metrics returns middleware that records one ObserveHTTP call per request.
// Place it in the chain so it runs after routing — Echo populates c.Path()
// only once a route has matched. The middleware records duration including
// the handler's downstream latency by deferring the observation.
func Metrics(opts MetricsOptions) echo.MiddlewareFunc {
	if opts.Observe == nil {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	slowMs := opts.SlowMs
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			start := time.Now()
			err := next(c)
			dur := time.Since(start)

			route := c.Path()
			if route == "" {
				// No route matched (404 from the router). Bucket as a single
				// label to keep cardinality bounded — never use the raw URL.
				route = "unmatched"
			}
			opts.Observe(route, c.Response().Status, dur)
			if slowMs > 0 && dur >= slowMs && opts.Slow != nil {
				opts.Slow()
			}
			return err
		}
	}
}
