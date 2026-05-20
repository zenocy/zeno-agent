// Package api holds the HTTP handlers for /api/*.
package api

import (
	"context"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/llm"
	zlog "github.com/zenocy/zeno-v2/internal/log"
)

// Version is set at build time via -ldflags="-X .../api.Version=...".
var Version = "dev"

// HealthHandler answers GET /api/health.
type HealthHandler struct {
	DB        *gorm.DB
	LLM       llm.Provider
	Reader    zlog.Reader // optional; used to surface last_synth_at / last_sync_at
	StartedAt time.Time
}

// HealthResponse is the JSON returned by /api/health.
type HealthResponse struct {
	OK           bool       `json:"ok"`
	Version      string     `json:"version"`
	Uptime       string     `json:"uptime"`
	DBOK         bool       `json:"db_ok"`
	LLMReachable bool       `json:"llm_reachable"`
	LLMError     string     `json:"llm_error,omitempty"`
	LastSynthAt  *time.Time `json:"last_synth_at,omitempty"`
	LastSyncAt   *time.Time `json:"last_sync_at,omitempty"`
}

// Register attaches the health handler to the Echo instance.
func (h *HealthHandler) Register(e *echo.Echo) {
	e.GET("/api/health", h.handle)
}

func (h *HealthHandler) handle(c echo.Context) error {
	resp := HealthResponse{
		Version: Version,
		Uptime:  time.Since(h.StartedAt).Truncate(time.Second).String(),
	}

	// DB ping
	if h.DB != nil {
		if sqlDB, err := h.DB.DB(); err == nil && sqlDB.Ping() == nil {
			resp.DBOK = true
		}
	}

	// LLM reachability — capped to 3s so /api/health stays snappy.
	if h.LLM != nil {
		ctx, cancel := context.WithTimeout(c.Request().Context(), 3*time.Second)
		defer cancel()
		if err := h.LLM.Reachable(ctx); err != nil {
			resp.LLMError = err.Error()
		} else {
			resp.LLMReachable = true
		}
	}

	// last_synth_at / last_sync_at — best-effort, capped at 500ms so a slow
	// log read never holds up the healthcheck.
	if h.Reader != nil {
		readCtx, cancel := context.WithTimeout(c.Request().Context(), 500*time.Millisecond)
		defer cancel()
		if e, err := h.Reader.Latest(readCtx, zlog.KindSynthRunCompleted); err == nil && e != nil {
			ts := e.TS.UTC()
			resp.LastSynthAt = &ts
		}
		if e, err := h.Reader.Latest(readCtx, zlog.KindSyncCompleted); err == nil && e != nil {
			ts := e.TS.UTC()
			resp.LastSyncAt = &ts
		}
	}

	resp.OK = resp.DBOK // LLM may be down without zeno being unhealthy.

	return c.JSON(http.StatusOK, resp)
}
