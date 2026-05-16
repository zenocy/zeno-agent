package system_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/sensor/geocode"
)

// putJSON is a small helper for these settings tests so they don't need
// to live alongside the harness's PUT helpers (the harness only had
// GET/POST until now).
func putJSON(t *testing.T, url, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewBufferString(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// Boot with no settings row → /api/settings should report set=false and
// the weather sensor should sync without error but produce no events.
func TestSettings_EmptyBootSkipsWeather(t *testing.T) {
	h := NewHarness(t, HarnessConfig{SkipSettingsSeed: true})
	defer h.Close()

	status, body := h.Get("/api/settings")
	require.Equal(t, http.StatusOK, status)

	var resp struct {
		Set      bool    `json:"set"`
		City     string  `json:"city"`
		Country  string  `json:"country"`
		Latitude float64 `json:"latitude"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	require.False(t, resp.Set)
	require.Empty(t, resp.City)

	// SyncAll should still succeed (weather skips silently).
	results := h.Scheduler.SyncAll(context.Background())
	for _, r := range results {
		require.True(t, r.OK, "sensor %q failed: %s", r.Name, r.Err)
	}
	// No weather snapshot was emitted.
	require.Equal(t, 0, h.CountByKind(log.KindWeatherSnapshot))
}

// PUT /api/settings with new city/country invokes the geocoder, persists
// the lat/lon it returns, and the AfterSave hook kicks off SyncAll —
// so a fresh weather.snapshot event lands in the log shortly after the
// response without the test having to trigger a sync itself.
func TestSettings_PUTGeocodesAndDrivesWeather(t *testing.T) {
	h := NewHarness(t, HarnessConfig{
		SkipSettingsSeed: true,
		Geocoder:         &FakeForwardGeocoder{Lat: 37.9838, Lon: 23.7275},
	})
	defer h.Close()

	status, body := putJSON(t, h.Server.URL+"/api/settings",
		`{"timezone":"Europe/Athens","city":"Athens","country":"Greece"}`)
	require.Equal(t, http.StatusOK, status, "body: %s", body)

	var resp struct {
		Set       bool    `json:"set"`
		Timezone  string  `json:"timezone"`
		City      string  `json:"city"`
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	require.True(t, resp.Set)
	require.Equal(t, "Athens", resp.City)
	require.InDelta(t, 37.9838, resp.Latitude, 1e-6)
	require.InDelta(t, 23.7275, resp.Longitude, 1e-6)

	// Forward geocoder must have been called exactly once with the
	// right inputs (and only once — the PUT path is the only caller).
	require.Equal(t, 1, h.Geocoder.Calls)
	require.Equal(t, "Athens", h.Geocoder.LastCity)
	require.Equal(t, "Greece", h.Geocoder.LastCty)

	// Service snapshot reflects the new values immediately — the PUT
	// already triggered Reload before returning.
	snap := h.Settings.Snapshot()
	require.True(t, snap.Set)
	require.Equal(t, "Europe/Athens", snap.Timezone)

	// AfterSave fires SyncAll in a goroutine. Wait for the resulting
	// weather.snapshot event to show up — typically tens of ms with
	// the in-process httptest weather server.
	require.Eventually(t, func() bool {
		return h.CountByKind(log.KindWeatherSnapshot) >= 1
	}, 3*time.Second, 20*time.Millisecond, "AfterSave must trigger a fresh weather sync")
}

// A geocoder failure must not block the save. The settings row keeps
// the prior lat/lon (or zero if never set) and the response carries a
// geocode_error so the UI can surface it.
func TestSettings_PUTGeocodeFailureSurfacesErrorButSaves(t *testing.T) {
	geo := &FakeForwardGeocoder{Err: geocode.ErrNoMatch}
	h := NewHarness(t, HarnessConfig{
		SkipSettingsSeed: true,
		Geocoder:         geo,
	})
	defer h.Close()

	status, body := putJSON(t, h.Server.URL+"/api/settings",
		`{"timezone":"UTC","city":"Atlantis","country":"Mu"}`)
	require.Equal(t, http.StatusOK, status, "geocode failure must not block save: %s", body)

	var resp struct {
		Set          bool    `json:"set"`
		City         string  `json:"city"`
		Latitude     float64 `json:"latitude"`
		GeocodeError string  `json:"geocode_error"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	require.True(t, resp.Set)
	require.Equal(t, "Atlantis", resp.City)
	require.Equal(t, float64(0), resp.Latitude, "first save with geocode failure leaves lat at zero")
	require.NotEmpty(t, resp.GeocodeError)
	require.Contains(t, resp.GeocodeError, "could not find")
}

// TZ-only edits flow through immediately: the next request to a TZ-using
// endpoint sees the new zone without a process restart.
func TestSettings_TZChangeIsLive(t *testing.T) {
	tz, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 25, 8, 0, 0, 0, tz)
	h := NewHarness(t, HarnessConfig{TZ: tz, Now: func() time.Time { return now }})
	defer h.Close()

	// Seeded snapshot is in LA tz from the harness default seed.
	require.Equal(t, "America/Los_Angeles", h.Settings.Snapshot().Timezone)

	// Flip TZ to Europe/Athens via the API.
	status, _ := putJSON(t, h.Server.URL+"/api/settings",
		`{"timezone":"Europe/Athens","city":"San Francisco","country":"United States"}`)
	require.Equal(t, http.StatusOK, status)

	// New snapshot reflects the change without a restart.
	require.Equal(t, "Europe/Athens", h.Settings.Snapshot().Timezone)
	athens, _ := time.LoadLocation("Europe/Athens")
	require.Equal(t, athens.String(), h.Settings.TZ().String())
}

// Sanity: concurrent reads during a Reload don't yield torn snapshots.
// This guards the atomic.Pointer contract end-to-end (run with -race).
func TestSettings_ConcurrentReadDuringPUT(t *testing.T) {
	h := NewHarness(t, HarnessConfig{})
	defer h.Close()

	stop := make(chan struct{})
	errs := make(chan error, 4)
	for range 4 {
		go func() {
			for {
				select {
				case <-stop:
					errs <- nil
					return
				default:
					snap := h.Settings.Snapshot()
					if snap == nil || snap.Location == nil {
						errs <- errors.New("torn snapshot")
						return
					}
				}
			}
		}()
	}

	for range 30 {
		status, _ := putJSON(t, h.Server.URL+"/api/settings",
			`{"timezone":"UTC","city":"X","country":"Y"}`)
		require.Equal(t, http.StatusOK, status)
	}
	close(stop)
	for range 4 {
		require.NoError(t, <-errs)
	}
}
