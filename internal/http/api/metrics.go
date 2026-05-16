package api

import (
	"context"

	"github.com/labstack/echo/v4"

	zlog "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/metrics"
)

// MetricsHandler exposes the Prometheus exposition endpoint and a sibling
// JSON snapshot endpoint backed by the latest stats.snapshot row in the
// observation log. Both routes live under /api/* so the lan_token bearer
// already protects them.
type MetricsHandler struct {
	Metrics *metrics.Metrics
	Reader  zlog.Reader // for /api/metrics/snapshot fallback to live state when no row exists
}

// Register attaches /api/metrics and /api/metrics/snapshot to the Echo
// instance. If h.Metrics is nil this is a no-op.
func (h *MetricsHandler) Register(e *echo.Echo) {
	if h == nil || h.Metrics == nil {
		return
	}
	e.GET("/api/metrics", h.Metrics.PrometheusHandler())
	e.GET("/api/metrics/snapshot", h.Metrics.SnapshotHandler(h.latest()))
}

func (h *MetricsHandler) latest() metrics.LatestStatsLookup {
	if h.Reader == nil {
		return nil
	}
	return func(ctx context.Context) ([]byte, error) {
		ev, err := h.Reader.Latest(ctx, zlog.KindStatsSnapshot)
		if err != nil || ev == nil {
			return nil, err
		}
		return []byte(ev.Payload), nil
	}
}
