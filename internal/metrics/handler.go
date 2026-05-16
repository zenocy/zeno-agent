package metrics

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// PrometheusHandler returns an Echo handler exposing the registry in
// Prometheus exposition format at the path it's mounted on.
func (m *Metrics) PrometheusHandler() echo.HandlerFunc {
	h := promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
	return echo.WrapHandler(h)
}

// LatestStatsLookup returns the JSON payload of the most recent
// stats.snapshot event, or nil if none exists. Caller-supplied so this
// package stays free of an internal/log dependency.
type LatestStatsLookup func(ctx context.Context) ([]byte, error)

// SnapshotHandler returns the most recent stats.snapshot row from the event
// log as JSON. Falls back to a fresh in-memory snapshot if no row exists or
// the lookup is nil.
func (m *Metrics) SnapshotHandler(latest LatestStatsLookup) echo.HandlerFunc {
	return func(c echo.Context) error {
		if latest != nil {
			payload, err := latest(c.Request().Context())
			if err != nil {
				return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
			}
			if len(payload) > 0 {
				c.Response().Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
				c.Response().WriteHeader(http.StatusOK)
				_, err = c.Response().Write(payload)
				return err
			}
		}
		return c.JSON(http.StatusOK, m.Snapshot())
	}
}
