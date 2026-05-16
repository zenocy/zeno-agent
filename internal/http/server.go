// Package http wires the Echo HTTP server, middleware, and API handlers.
package http

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"

	mw "github.com/zenocy/zeno-v2/internal/http/middleware"
)

// ServerConfig holds runtime knobs.
type ServerConfig struct {
	Bind            string
	Port            int
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	ShutdownTimeout time.Duration
	LANToken        string        // empty → loopback only, no token check
	HTTPSlowMs      time.Duration // 2xx ≥ this duration logs at INFO with slow=true; faster 2xx logs at DEBUG
	MetricsObserver mw.HTTPObserver
	MetricsSlow     mw.SlowMarker
}

// Server wraps an Echo instance with lifecycle management.
type Server struct {
	Echo   *echo.Echo
	cfg    ServerConfig
	logger *logrus.Logger
}

// New constructs a Server with the standard middleware stack already wired.
func New(cfg ServerConfig, logger *logrus.Logger) *Server {
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	e.Server.ReadTimeout = cfg.ReadTimeout
	e.Server.WriteTimeout = cfg.WriteTimeout

	e.Use(mw.RequestID())
	e.Use(mw.LoggingWithOptions(logger, mw.LoggingOptions{SlowMs: cfg.HTTPSlowMs}))
	e.Use(mw.Recovery(logger))
	e.Use(mw.Metrics(mw.MetricsOptions{
		Observe: cfg.MetricsObserver,
		SlowMs:  cfg.HTTPSlowMs,
		Slow:    cfg.MetricsSlow,
	}))

	if cfg.LANToken != "" {
		e.Use(bearerMiddleware(cfg.LANToken))
	}

	return &Server{Echo: e, cfg: cfg, logger: logger}
}

// Address returns the bind:port string.
func (s *Server) Address() string {
	return fmt.Sprintf("%s:%d", s.cfg.Bind, s.cfg.Port)
}

// Start begins listening; blocks until the server stops.
func (s *Server) Start() error {
	addr := s.Address()
	s.logger.WithField("address", addr).Info("starting HTTP server")
	if err := s.Echo.Start(addr); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Shutdown stops the server with the configured timeout.
func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("shutting down HTTP server")
	shutdownCtx, cancel := context.WithTimeout(ctx, s.cfg.ShutdownTimeout)
	defer cancel()
	return s.Echo.Shutdown(shutdownCtx)
}

// bearerMiddleware enforces an `Authorization: Bearer <token>` check on
// /api/* paths. Used when the user explicitly sets server.lan_token.
func bearerMiddleware(token string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			path := c.Request().URL.Path
			if len(path) < 5 || path[:5] != "/api/" {
				return next(c)
			}
			got := c.Request().Header.Get("Authorization")
			if got != "Bearer "+token {
				return echo.NewHTTPError(http.StatusUnauthorized, "unauthorized")
			}
			return next(c)
		}
	}
}
