package api

import (
	"context"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/schedule"
)

const defaultSyncTimeout = 90 * time.Second

// SyncHandler triggers an on-demand SyncAll across every sensor.
type SyncHandler struct {
	Scheduler *schedule.Scheduler
	Log       *logrus.Entry
	Timeout   time.Duration
}

// Register wires the route onto the Echo instance.
func (h *SyncHandler) Register(e *echo.Echo) {
	e.POST("/api/sync/now", h.handle)
}

type syncResultDTO struct {
	Name       string `json:"name"`
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

type syncResponseDTO struct {
	Sensors []syncResultDTO `json:"sensors"`
}

func (h *SyncHandler) handle(c echo.Context) error {
	to := h.Timeout
	if to <= 0 {
		to = defaultSyncTimeout
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), to)
	defer cancel()

	results := h.Scheduler.SyncAll(ctx)
	out := syncResponseDTO{Sensors: make([]syncResultDTO, 0, len(results))}
	for _, r := range results {
		out.Sensors = append(out.Sensors, syncResultDTO{
			Name:       r.Name,
			OK:         r.OK,
			Error:      r.Err,
			DurationMS: r.Duration.Milliseconds(),
		})
	}
	return c.JSON(http.StatusOK, out)
}
