package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/sensor/geocode"
	"github.com/zenocy/zeno-v2/internal/settings"
	"github.com/zenocy/zeno-v2/internal/store"
)

// fakeForwardGeocoder records calls and returns a canned (lat, lon, err)
// triple. Unit tests use this to assert the handler calls the geocoder
// exactly when expected and surfaces failures correctly.
type fakeForwardGeocoder struct {
	lat, lon  float64
	err       error
	callCount int
	lastCity  string
	lastCty   string
}

func (f *fakeForwardGeocoder) Geocode(_ context.Context, city, country string) (float64, float64, error) {
	f.callCount++
	f.lastCity = city
	f.lastCty = country
	return f.lat, f.lon, f.err
}

type settingsTestHarness struct {
	e        *echo.Echo
	repo     *store.SettingsRepo
	service  *settings.Service
	geocoder *fakeForwardGeocoder
}

func buildSettingsHandler(t *testing.T) *settingsTestHarness {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_journal_mode=WAL&_foreign_keys=on"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	repo := &store.SettingsRepo{DB: db}
	require.NoError(t, repo.Migrate())

	svc := settings.New(repo)
	require.NoError(t, svc.Load(context.Background()))

	geo := &fakeForwardGeocoder{lat: 37.9838, lon: 23.7275}

	e := echo.New()
	(&SettingsHandler{
		Repo: repo, Service: svc, Geocoder: geo,
		Now: func() time.Time { return time.Date(2026, 4, 28, 8, 0, 0, 0, time.UTC) },
		Log: quietHandlerEntry(),
	}).Register(e)

	return &settingsTestHarness{e: e, repo: repo, service: svc, geocoder: geo}
}

func doSettingsRequest(t *testing.T, e *echo.Echo, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, "/api/settings", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, "/api/settings", nil)
	}
	e.ServeHTTP(rr, req)
	return rr
}

func TestSettings_GET_EmptyDB(t *testing.T) {
	h := buildSettingsHandler(t)
	rr := doSettingsRequest(t, h.e, http.MethodGet, "")
	require.Equal(t, http.StatusOK, rr.Code)

	var resp settingsDTO
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.False(t, resp.Set)
	require.Equal(t, "", resp.Timezone)
	require.Equal(t, "", resp.City)
}

func TestSettings_GET_PopulatedDB(t *testing.T) {
	h := buildSettingsHandler(t)
	require.NoError(t, h.repo.Upsert(context.Background(), store.AppSettings{
		Timezone: "Europe/Athens", City: "Athens", Country: "Greece",
		Latitude: 37.9838, Longitude: 23.7275,
	}))
	require.NoError(t, h.service.Reload(context.Background()))

	rr := doSettingsRequest(t, h.e, http.MethodGet, "")
	require.Equal(t, http.StatusOK, rr.Code)

	var resp settingsDTO
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.True(t, resp.Set)
	require.Equal(t, "Europe/Athens", resp.Timezone)
	require.Equal(t, "Athens", resp.City)
	require.Equal(t, "Greece", resp.Country)
	require.InDelta(t, 37.9838, resp.Latitude, 1e-6)
}

func TestSettings_PUT_HappyPath(t *testing.T) {
	h := buildSettingsHandler(t)

	body := `{"timezone":"Europe/Athens","city":"Athens","country":"Greece"}`
	rr := doSettingsRequest(t, h.e, http.MethodPut, body)
	require.Equal(t, http.StatusOK, rr.Code)

	var resp settingsDTO
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.True(t, resp.Set)
	require.Equal(t, "Europe/Athens", resp.Timezone)
	require.Equal(t, "Athens", resp.City)
	require.InDelta(t, 37.9838, resp.Latitude, 1e-6)
	require.Equal(t, "", resp.GeocodeError)

	// Geocoder must have been called exactly once with our inputs.
	require.Equal(t, 1, h.geocoder.callCount)
	require.Equal(t, "Athens", h.geocoder.lastCity)
	require.Equal(t, "Greece", h.geocoder.lastCty)

	// The service snapshot reflects the new values immediately —
	// proving Reload happened before we returned.
	snap := h.service.Snapshot()
	require.True(t, snap.Set)
	require.Equal(t, "Europe/Athens", snap.Timezone)
}

func TestSettings_PUT_InvalidTimezoneRejected(t *testing.T) {
	h := buildSettingsHandler(t)

	body := `{"timezone":"Not/A/Real/Zone","city":"Athens","country":"Greece"}`
	rr := doSettingsRequest(t, h.e, http.MethodPut, body)
	require.Equal(t, http.StatusBadRequest, rr.Code)

	// Nothing written — service still empty.
	require.False(t, h.service.Snapshot().Set)
	// Geocoder NOT called when validation fails up-front.
	require.Equal(t, 0, h.geocoder.callCount)
}

func TestSettings_PUT_GeocodeErrorPreservesPriorCoords(t *testing.T) {
	h := buildSettingsHandler(t)
	// Seed prior coords.
	require.NoError(t, h.repo.Upsert(context.Background(), store.AppSettings{
		Timezone: "Europe/Athens", City: "Athens", Country: "Greece",
		Latitude: 37.9838, Longitude: 23.7275,
	}))
	require.NoError(t, h.service.Reload(context.Background()))

	// Now make the geocoder fail and PUT a city/country change.
	h.geocoder.err = geocode.ErrNoMatch
	body := `{"timezone":"Europe/Athens","city":"Atlantis","country":"Mu"}`
	rr := doSettingsRequest(t, h.e, http.MethodPut, body)
	require.Equal(t, http.StatusOK, rr.Code, "geocode failure must not block save")

	var resp settingsDTO
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.GeocodeError, "UI needs to surface the geocode error")
	require.Equal(t, "Atlantis", resp.City)
	// Lat/lon stay at the prior values (not zeroed, not random).
	require.InDelta(t, 37.9838, resp.Latitude, 1e-6)
	require.InDelta(t, 23.7275, resp.Longitude, 1e-6)
}

func TestSettings_PUT_GeocodeNetworkError(t *testing.T) {
	h := buildSettingsHandler(t)
	h.geocoder.err = errors.New("upstream went away")

	body := `{"timezone":"UTC","city":"Athens","country":"Greece"}`
	rr := doSettingsRequest(t, h.e, http.MethodPut, body)
	require.Equal(t, http.StatusOK, rr.Code)

	var resp settingsDTO
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.GeocodeError)
	require.Contains(t, resp.GeocodeError, "geocoding service unavailable")
}

// TZ-only edits don't change city/country, so the geocoder shouldn't be
// pinged for them. Reduces upstream load + speeds up the response.
func TestSettings_PUT_TZOnlyEditSkipsGeocoder(t *testing.T) {
	h := buildSettingsHandler(t)
	require.NoError(t, h.repo.Upsert(context.Background(), store.AppSettings{
		Timezone: "Europe/Athens", City: "Athens", Country: "Greece",
		Latitude: 37.9838, Longitude: 23.7275,
	}))
	require.NoError(t, h.service.Reload(context.Background()))
	require.Equal(t, 0, h.geocoder.callCount)

	body := `{"timezone":"America/New_York","city":"Athens","country":"Greece"}`
	rr := doSettingsRequest(t, h.e, http.MethodPut, body)
	require.Equal(t, http.StatusOK, rr.Code)

	require.Equal(t, 0, h.geocoder.callCount, "TZ-only edit must not call geocoder")
	require.Equal(t, "America/New_York", h.service.Snapshot().Timezone)
}

// AfterSave must fire after a successful PUT so callers can wire it
// to scheduler.SyncAll and have the weather widget refresh immediately.
// It must NOT fire when validation rejects the request — there's
// nothing new to sync.
func TestSettings_PUT_AfterSaveFiresOnSuccess(t *testing.T) {
	h := buildSettingsHandler(t)
	called := make(chan struct{}, 1)
	// Re-register a handler with AfterSave wired. The harness builds
	// one without it, so swap by re-registering on a fresh Echo.
	e := echo.New()
	(&SettingsHandler{
		Repo: h.repo, Service: h.service, Geocoder: h.geocoder,
		AfterSave: func(_ context.Context) { called <- struct{}{} },
		Now:       func() time.Time { return time.Date(2026, 4, 28, 8, 0, 0, 0, time.UTC) },
		Log:       quietHandlerEntry(),
	}).Register(e)

	rr := doSettingsRequest(t, e, http.MethodPut,
		`{"timezone":"UTC","city":"Athens","country":"Greece"}`)
	require.Equal(t, http.StatusOK, rr.Code)

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("AfterSave was not invoked within 2s of a successful PUT")
	}
}

func TestSettings_PUT_StockFieldsRoundTrip(t *testing.T) {
	h := buildSettingsHandler(t)

	body := `{"timezone":"UTC","city":"","country":"","stock_tickers":"aapl, GOOGL, ,aapl","stock_threshold_pct":3.5,"stock_always_poll":true}`
	rr := doSettingsRequest(t, h.e, http.MethodPut, body)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var resp settingsDTO
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Equal(t, "AAPL,GOOGL", resp.StockTickers, "tickers must be normalized + dedup'd")
	require.InDelta(t, 3.5, resp.StockThresholdPct, 1e-6)
	require.True(t, resp.StockAlwaysPoll, "stock_always_poll round-trips")

	// Service snapshot reflects parsed slice form.
	tickers, threshold, alwaysPoll, ok := h.service.StockConfig()
	require.True(t, ok)
	require.Equal(t, []string{"AAPL", "GOOGL"}, tickers)
	require.InDelta(t, 3.5, threshold, 1e-6)
	require.True(t, alwaysPoll)
}

func TestSettings_PUT_WorldClocksRoundTrip(t *testing.T) {
	h := buildSettingsHandler(t)

	body := `{"timezone":"UTC","city":"","country":"","world_clocks":" America/Los_Angeles , Foo/Bar , Europe/London , America/Los_Angeles"}`
	rr := doSettingsRequest(t, h.e, http.MethodPut, body)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var resp settingsDTO
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Equal(t, "America/Los_Angeles,Europe/London", resp.WorldClocks,
		"world_clocks must be normalized: invalid entries dropped, dupes removed, order preserved")

	snap := h.service.Snapshot()
	require.Equal(t, []string{"America/Los_Angeles", "Europe/London"}, snap.WorldClocks)
}

func TestSettings_PUT_AssistantPersonaRoundTrip(t *testing.T) {
	h := buildSettingsHandler(t)

	body := `{"timezone":"UTC","city":"","country":"","user_name":"Jamie","assistant_name":"Aria","assistant_tone":"warm but brisk"}`
	rr := doSettingsRequest(t, h.e, http.MethodPut, body)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var resp settingsDTO
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Equal(t, "Jamie", resp.UserName)
	require.Equal(t, "Aria", resp.AssistantName)
	require.Equal(t, "warm but brisk", resp.AssistantTone)

	// Snapshot reflects the persona on the live service.
	snap := h.service.Snapshot()
	require.Equal(t, "Jamie", snap.UserName)
	require.Equal(t, "Aria", snap.AssistantName)
	require.Equal(t, "warm but brisk", snap.AssistantTone)
	require.True(t, snap.AssistantEnabled())
}

func TestSettings_PUT_AssistantPersonaTooLongRejected(t *testing.T) {
	h := buildSettingsHandler(t)
	body := `{"timezone":"UTC","assistant_name":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`
	rr := doSettingsRequest(t, h.e, http.MethodPut, body)
	require.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestSettings_PUT_StockThresholdOutOfRange(t *testing.T) {
	h := buildSettingsHandler(t)

	body := `{"timezone":"UTC","stock_tickers":"AAPL","stock_threshold_pct":3000}`
	rr := doSettingsRequest(t, h.e, http.MethodPut, body)
	require.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestSettings_PUT_AfterSaveSkippedOnValidationFailure(t *testing.T) {
	h := buildSettingsHandler(t)
	called := make(chan struct{}, 1)
	e := echo.New()
	(&SettingsHandler{
		Repo: h.repo, Service: h.service, Geocoder: h.geocoder,
		AfterSave: func(_ context.Context) { called <- struct{}{} },
		Now:       func() time.Time { return time.Date(2026, 4, 28, 8, 0, 0, 0, time.UTC) },
		Log:       quietHandlerEntry(),
	}).Register(e)

	rr := doSettingsRequest(t, e, http.MethodPut,
		`{"timezone":"Not/A/Real/Zone","city":"X","country":"Y"}`)
	require.Equal(t, http.StatusBadRequest, rr.Code)

	select {
	case <-called:
		t.Fatal("AfterSave fired even though validation rejected the PUT")
	case <-time.After(200 * time.Millisecond):
		// expected — no callback fired
	}
}

func TestSettings_PUT_EmptyCityCountrySkipsGeocoder(t *testing.T) {
	h := buildSettingsHandler(t)

	body := `{"timezone":"UTC","city":"","country":""}`
	rr := doSettingsRequest(t, h.e, http.MethodPut, body)
	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, 0, h.geocoder.callCount)

	snap := h.service.Snapshot()
	require.True(t, snap.Set)
	require.Equal(t, float64(0), snap.Latitude)
}
