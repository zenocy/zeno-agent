package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/config"
	"github.com/zenocy/zeno-v2/internal/schedule"
	"github.com/zenocy/zeno-v2/internal/sensor"
)

type stubSensor struct {
	name  string
	err   error
	delay time.Duration
}

func (s stubSensor) Name() string { return s.name }
func (s stubSensor) Sync(ctx context.Context) error {
	if s.delay > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.delay):
		}
	}
	return s.err
}

func mountSync(t *testing.T, sensors []sensor.Sensor) *echo.Echo {
	t.Helper()
	sched, err := schedule.New(config.ScheduleConfig{}, sensors, nil, quietEntry())
	require.NoError(t, err)
	e := echo.New()
	(&SyncHandler{Scheduler: sched, Log: quietEntry()}).Register(e)
	return e
}

func TestSyncHandler_AllOK(t *testing.T) {
	e := mountSync(t, []sensor.Sensor{stubSensor{name: "a"}, stubSensor{name: "b"}})

	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/sync/now", nil))
	require.Equal(t, http.StatusOK, rr.Code)

	var resp syncResponseDTO
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Len(t, resp.Sensors, 2)
	for _, r := range resp.Sensors {
		require.True(t, r.OK)
		require.GreaterOrEqual(t, r.DurationMS, int64(0))
	}
}

func TestSyncHandler_OnePassesOneFails(t *testing.T) {
	e := mountSync(t, []sensor.Sensor{
		stubSensor{name: "ok"},
		stubSensor{name: "broken", err: errors.New("kaboom")},
	})

	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/sync/now", nil))
	require.Equal(t, http.StatusOK, rr.Code)

	var resp syncResponseDTO
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	byName := map[string]syncResultDTO{}
	for _, r := range resp.Sensors {
		byName[r.Name] = r
	}
	require.True(t, byName["ok"].OK)
	require.False(t, byName["broken"].OK)
	require.Contains(t, byName["broken"].Error, "kaboom")
}

func TestSyncHandler_TimesOut(t *testing.T) {
	sched, err := schedule.New(config.ScheduleConfig{}, []sensor.Sensor{stubSensor{name: "slow", delay: 2 * time.Second}}, nil, quietEntry())
	require.NoError(t, err)
	e := echo.New()
	(&SyncHandler{Scheduler: sched, Timeout: 200 * time.Millisecond}).Register(e)

	rr := httptest.NewRecorder()
	start := time.Now()
	e.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/sync/now", nil))
	elapsed := time.Since(start)

	require.Equal(t, http.StatusOK, rr.Code, "handler still returns 200 even when sensor times out")
	require.Less(t, elapsed, time.Second)

	var resp syncResponseDTO
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Len(t, resp.Sensors, 1)
	require.False(t, resp.Sensors[0].OK)
}
