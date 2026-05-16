package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/config"
	"github.com/zenocy/zeno-v2/internal/schedule"
	"github.com/zenocy/zeno-v2/internal/sensor"
)

func mountSynth(t *testing.T, fn schedule.MorningSynthFunc) *echo.Echo {
	t.Helper()
	sched, err := schedule.New(config.ScheduleConfig{}, []sensor.Sensor{}, fn, quietEntry())
	require.NoError(t, err)
	e := echo.New()
	(&SynthHandler{Scheduler: sched, Log: quietEntry()}).Register(e)
	return e
}

func TestSynthHandler_OK(t *testing.T) {
	e := mountSynth(t, func(ctx context.Context) error { return nil })

	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/synth/now", nil))
	require.Equal(t, http.StatusOK, rr.Code)

	var resp synthResponseDTO
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.True(t, resp.OK)
	require.GreaterOrEqual(t, resp.DurationMS, int64(0))
}

func TestSynthHandler_ConflictWhenInFlight(t *testing.T) {
	// First request blocks until released; second must observe 409.
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	e := mountSynth(t, func(ctx context.Context) error {
		once.Do(func() { close(started) })
		<-release
		return nil
	})

	go func() {
		rr := httptest.NewRecorder()
		e.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/synth/now", nil))
	}()
	<-started

	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/synth/now", nil))
	require.Equal(t, http.StatusConflict, rr.Code)

	var resp synthResponseDTO
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.False(t, resp.OK)
	require.Contains(t, resp.Error, "in flight")

	close(release)
}

func TestSynthHandler_NoSynthFunc(t *testing.T) {
	e := mountSynth(t, nil)

	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/synth/now", nil))
	require.Equal(t, http.StatusServiceUnavailable, rr.Code)
}

func TestSynthHandler_RunnerError(t *testing.T) {
	e := mountSynth(t, func(ctx context.Context) error { return errors.New("kaboom") })

	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/synth/now", nil))
	require.Equal(t, http.StatusInternalServerError, rr.Code)

	var resp synthResponseDTO
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.False(t, resp.OK)
	require.Contains(t, resp.Error, "kaboom")
}

func TestSynthHandler_TimesOut(t *testing.T) {
	sched, err := schedule.New(config.ScheduleConfig{}, []sensor.Sensor{}, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}, quietEntry())
	require.NoError(t, err)
	e := echo.New()
	(&SynthHandler{Scheduler: sched, Timeout: 100 * time.Millisecond, Log: quietEntry()}).Register(e)

	rr := httptest.NewRecorder()
	start := time.Now()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/synth/now", nil))
	elapsed := time.Since(start)
	require.Less(t, elapsed, time.Second)
	require.Equal(t, http.StatusGatewayTimeout, rr.Code)
}

// V2.3.0 P3: ?kind=inject routes through the injectFn registered via
// WithInject and supplies a synthetic InjectSignal built from query params.
func TestSynthHandler_InjectKind_RoutesToInjectFn(t *testing.T) {
	var captured any
	sched, err := schedule.New(config.ScheduleConfig{}, []sensor.Sensor{}, nil, quietEntry())
	require.NoError(t, err)
	sched.WithInject(func(ctx context.Context, signal any) error {
		captured = signal
		return nil
	})
	e := echo.New()
	(&SynthHandler{Scheduler: sched, Log: quietEntry()}).Register(e)

	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodPost,
		"/api/synth/now?kind=inject&inject_kind=email&inject_subject=test%20subject", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	require.NotNil(t, captured, "injectFn must receive a non-nil signal on the manual debug path")
}

// V2.3.0 P3: ?kind=inject without an injectFn registered must surface
// 503 ServiceUnavailable, mirroring the morning case.
func TestSynthHandler_InjectKind_NoInjectFunc(t *testing.T) {
	sched, err := schedule.New(config.ScheduleConfig{}, []sensor.Sensor{}, nil, quietEntry())
	require.NoError(t, err)
	e := echo.New()
	(&SynthHandler{Scheduler: sched, Log: quietEntry()}).Register(e)

	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/synth/now?kind=inject", nil))
	require.Equal(t, http.StatusServiceUnavailable, rr.Code)
}

// V2.3.0 P3: orchestrator can return ErrInjectDebounced; handler maps to 429.
func TestSynthHandler_InjectKind_DebouncedReturns429(t *testing.T) {
	sched, err := schedule.New(config.ScheduleConfig{}, []sensor.Sensor{}, nil, quietEntry())
	require.NoError(t, err)
	sched.WithInject(func(ctx context.Context, signal any) error {
		return ErrInjectDebounced
	})
	e := echo.New()
	(&SynthHandler{Scheduler: sched, Log: quietEntry()}).Register(e)

	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/synth/now?kind=inject", nil))
	require.Equal(t, http.StatusTooManyRequests, rr.Code)
}

// V2.3.0 P3: ?force=1 sets a context value the orchestrator can read to
// bypass debounce. We verify the value is propagated; the orchestrator's
// own debounce logic is tested elsewhere.
func TestSynthHandler_InjectKind_ForcePropagatesContextValue(t *testing.T) {
	var sawForce bool
	sched, err := schedule.New(config.ScheduleConfig{}, []sensor.Sensor{}, nil, quietEntry())
	require.NoError(t, err)
	sched.WithInject(func(ctx context.Context, signal any) error {
		v, _ := ctx.Value(InjectForceKey{}).(bool)
		sawForce = v
		return nil
	})
	e := echo.New()
	(&SynthHandler{Scheduler: sched, Log: quietEntry()}).Register(e)

	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/synth/now?kind=inject&force=1", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	require.True(t, sawForce, "InjectForceKey must be set in ctx when force=1")
}

func TestSynthHandler_UnknownKindReturns400(t *testing.T) {
	e := mountSynth(t, func(ctx context.Context) error { return nil })

	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/synth/now?kind=bogus", nil))
	require.Equal(t, http.StatusBadRequest, rr.Code)
}
