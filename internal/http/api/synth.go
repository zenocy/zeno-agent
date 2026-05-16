package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/schedule"
	"github.com/zenocy/zeno-v2/internal/synth"
)

const defaultSynthTimeout = 180 * time.Second

// SynthHandler triggers an on-demand morning synth via the scheduler. Mirrors
// SyncHandler in shape; uses the scheduler's single-flight guard so a manual
// trigger can't collide with a cron tick mid-flight.
//
// V2.3.0 P3: also accepts ?kind=inject for the manual debug path. The
// inject path bypasses sensor.Detect — the handler builds a synthetic
// synth.InjectSignal from query params and the scheduler invokes the
// registered injectFn with the signal supplied (skipping its own Detect).
type SynthHandler struct {
	Scheduler *schedule.Scheduler
	Log       *logrus.Entry
	Timeout   time.Duration
}

// Register wires the route onto the Echo instance.
func (h *SynthHandler) Register(e *echo.Echo) {
	e.POST("/api/synth/now", h.handle)
}

type synthResponseDTO struct {
	OK         bool   `json:"ok"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

func (h *SynthHandler) handle(c echo.Context) error {
	to := h.Timeout
	if to <= 0 {
		to = defaultSynthTimeout
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), to)
	defer cancel()

	switch c.QueryParam("kind") {
	case "", "morning":
		return h.handleMorning(c, ctx)
	case "inject":
		return h.handleInject(c, ctx)
	default:
		return c.JSON(http.StatusBadRequest, synthResponseDTO{
			Error: "unknown kind: must be 'morning' or 'inject'",
		})
	}
}

func (h *SynthHandler) handleMorning(c echo.Context, ctx context.Context) error {
	start := time.Now()
	err := h.Scheduler.RunMorningNow(ctx)
	dur := time.Since(start).Milliseconds()

	if err == nil {
		if h.Log != nil {
			h.Log.WithField("ms", dur).Info("synth: manual morning trigger completed")
		}
		return c.JSON(http.StatusOK, synthResponseDTO{OK: true, DurationMS: dur})
	}
	if errors.Is(err, schedule.ErrMorningInFlight) {
		return c.JSON(http.StatusConflict, synthResponseDTO{
			OK: false, DurationMS: dur, Error: err.Error(),
		})
	}
	if errors.Is(err, schedule.ErrNoMorningSynth) {
		return c.JSON(http.StatusServiceUnavailable, synthResponseDTO{
			OK: false, DurationMS: dur, Error: err.Error(),
		})
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return c.JSON(http.StatusGatewayTimeout, synthResponseDTO{
			OK: false, DurationMS: dur, Error: "synth exceeded handler timeout",
		})
	}
	if h.Log != nil {
		h.Log.WithError(err).WithField("ms", dur).Warn("synth: manual morning trigger failed")
	}
	return c.JSON(http.StatusInternalServerError, synthResponseDTO{
		OK: false, DurationMS: dur, Error: err.Error(),
	})
}

// handleInject is the manual debug path — POST /api/synth/now?kind=inject.
//
// Builds a synthetic synth.InjectSignal from `inject_kind` (default
// "email") and `inject_subject` (default "test inject") query params,
// then routes through Scheduler.RunInjectNowWithSignal which bypasses
// sensor.Detect. The orchestrator (registered via WithInject in main.go)
// is responsible for honoring `force=1` to bypass debounce — the
// scheduler itself does not enforce debounce. The orchestrator returns a
// sentinel error string "inject debounced" to signal "rejected by
// debounce gate"; the handler maps it to HTTP 429.
func (h *SynthHandler) handleInject(c echo.Context, ctx context.Context) error {
	kind := c.QueryParam("inject_kind")
	if kind == "" {
		kind = "email"
	}
	subject := c.QueryParam("inject_subject")
	if subject == "" {
		subject = "test inject"
	}

	signal := &synth.InjectSignal{
		Kind:    kind,
		Subject: subject,
		// EvidenceID intentionally empty — the manual debug path has no
		// underlying observation log row. SynthesizeInject doesn't care.
		At: time.Now(),
	}
	// Tag the signal context with `force` so the orchestrator can choose
	// to bypass debounce. We pass via a context value (no public scheduler
	// API for this — keeps the schedule package untouched).
	if c.QueryParam("force") == "1" {
		ctx = context.WithValue(ctx, InjectForceKey{}, true)
	}

	start := time.Now()
	err := h.Scheduler.RunInjectNowWithSignal(ctx, signal)
	dur := time.Since(start).Milliseconds()

	if err == nil {
		if h.Log != nil {
			h.Log.WithFields(logrus.Fields{"ms": dur, "kind": kind, "subject": subject}).
				Info("synth: manual inject trigger completed")
		}
		return c.JSON(http.StatusOK, synthResponseDTO{OK: true, DurationMS: dur})
	}
	if errors.Is(err, schedule.ErrInjectInFlight) {
		return c.JSON(http.StatusConflict, synthResponseDTO{
			OK: false, DurationMS: dur, Error: err.Error(),
		})
	}
	if errors.Is(err, schedule.ErrNoInjectFunc) {
		return c.JSON(http.StatusServiceUnavailable, synthResponseDTO{
			OK: false, DurationMS: dur, Error: err.Error(),
		})
	}
	if errors.Is(err, ErrInjectDebounced) {
		return c.JSON(http.StatusTooManyRequests, synthResponseDTO{
			OK: false, DurationMS: dur, Error: err.Error(),
		})
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return c.JSON(http.StatusGatewayTimeout, synthResponseDTO{
			OK: false, DurationMS: dur, Error: "inject exceeded handler timeout",
		})
	}
	if h.Log != nil {
		h.Log.WithError(err).WithField("ms", dur).Warn("synth: manual inject trigger failed")
	}
	return c.JSON(http.StatusInternalServerError, synthResponseDTO{
		OK: false, DurationMS: dur, Error: err.Error(),
	})
}

// InjectForceKey is the context key the manual debug path uses to
// signal "bypass debounce". The orchestrator (in cmd/zeno/main.go)
// reads it via ctx.Value(InjectForceKey{}). Defining it as an
// exported type rather than a string avoids the lint warning about
// untyped context keys and lets other packages reach it.
type InjectForceKey struct{}

// ErrInjectDebounced is the sentinel error the orchestrator returns
// when the debounce gate rejects a manual trigger. The handler maps it
// to HTTP 429 Too Many Requests so the client knows to retry later
// (or use ?force=1 to override).
var ErrInjectDebounced = errors.New("inject debounced")
