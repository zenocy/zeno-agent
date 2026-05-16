// Package system_test exercises the spine end-to-end with a real SQLite log,
// real scheduler, real Echo server, and real handlers. Only the outermost
// network adapters (IMAP dial, CalDAV provider, weather HTTP) are stubbed.
//
// These tests catch wiring bugs that per-package unit tests can't see.
package system_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gimap "github.com/emersion/go-imap/v2"
	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/clock"
	"github.com/zenocy/zeno-v2/internal/config"
	"github.com/zenocy/zeno-v2/internal/eventbus"
	"github.com/zenocy/zeno-v2/internal/http/api"
	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/projection"
	"github.com/zenocy/zeno-v2/internal/schedule"
	"github.com/zenocy/zeno-v2/internal/sensor"
	caldavsensor "github.com/zenocy/zeno-v2/internal/sensor/caldav"
	imapsensor "github.com/zenocy/zeno-v2/internal/sensor/imap"
	weathersensor "github.com/zenocy/zeno-v2/internal/sensor/weather"
	"github.com/zenocy/zeno-v2/internal/settings"
	storepkg "github.com/zenocy/zeno-v2/internal/store"
)

// FakeForwardGeocoder is the harness-side stand-in for the Open-Meteo
// forward geocoder. Tests configure Lat/Lon/Err and assert on Calls.
type FakeForwardGeocoder struct {
	Lat, Lon float64
	Err      error
	Calls    int
	LastCity string
	LastCty  string
}

// Geocode satisfies geocode.Forward.
func (f *FakeForwardGeocoder) Geocode(_ context.Context, city, country string) (float64, float64, error) {
	f.Calls++
	f.LastCity = city
	f.LastCty = country
	return f.Lat, f.Lon, f.Err
}

// HarnessConfig describes how to build a Harness.
type HarnessConfig struct {
	DBPath          string           // empty → tmpdir
	TZ              *time.Location   // defaults to America/Los_Angeles
	Now             func() time.Time // defaults to time.Now
	SyncCron        string           // empty → no cron
	LANToken        string           // empty → no bearer auth
	IMAPFolders     []string         // defaults to ["INBOX"]
	WeatherResponse string           // raw JSON; defaults to a 24h clear forecast
	WithBootPrime   bool
	WithProjCfg     *projection.Config // override projection.Config

	// SkipSettingsSeed leaves the app_settings table empty so tests can
	// exercise the unset-location path. Default behavior seeds a SF row
	// so existing tests keep their weather sensor enabled.
	SkipSettingsSeed bool
	// Geocoder, if non-nil, is wired into the SettingsHandler. Default
	// is a FakeForwardGeocoder returning Athens coordinates.
	Geocoder *FakeForwardGeocoder

	// V2.3.0 P3: opt-in inject + SSE wiring. WithInject installs an
	// InjectFunc on the scheduler so /api/synth/now?kind=inject becomes
	// callable; the harness also mounts SynthHandler and TodayStreamHandler.
	// WithInject is typically a stub that publishes a card to Bus instead
	// of running the real synth.SynthesizeInject pipeline (no LLM in the
	// system test). Set Bus to a constructed *eventbus.Bus the test will
	// publish through; if Bus is nil but WithInject is non-nil, a fresh bus
	// is created. Both nil → no inject/SSE wiring (default).
	WithInject schedule.InjectFunc
	Bus        *eventbus.Bus
}

// Harness ties together every Phase 1 component for a single test.
type Harness struct {
	t            *testing.T
	DB           *gorm.DB
	Store        log.Store
	Scheduler    *schedule.Scheduler
	Server       *httptest.Server
	IMAP         *FakeIMAP
	CalDAV       *FakeCalDAV
	Weather      *httptest.Server
	TZ           *time.Location
	Settings     *settings.Service
	SettingsRepo *storepkg.SettingsRepo
	Geocoder     *FakeForwardGeocoder
	now          func() time.Time
	dbPath       string

	// Bus is non-nil when HarnessConfig.WithInject was supplied (or when
	// the caller passed an explicit Bus). Tests publish here directly to
	// simulate the real inject orchestrator; SSE subscribers receive
	// what gets published.
	Bus *eventbus.Bus
}

// NewHarness builds the Harness. The caller calls Close() when done.
func NewHarness(t *testing.T, cfg HarnessConfig) *Harness {
	t.Helper()

	tz := cfg.TZ
	if tz == nil {
		tz, _ = time.LoadLocation("America/Los_Angeles")
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Date(2026, 4, 25, 8, 0, 0, 0, tz) }
	}

	dbPath := cfg.DBPath
	if dbPath == "" {
		dbPath = filepath.Join(t.TempDir(), "zeno.db")
	}
	db, store, err := log.Open(dbPath)
	require.NoError(t, err)

	logger := logrus.New()
	logger.Out = io.Discard

	// Settings: build the live in-memory snapshot service. By default,
	// pre-seed a San Francisco row so the weather sensor and TZ-using
	// handlers behave exactly as they did before this feature landed.
	settingsRepo := &storepkg.SettingsRepo{DB: db}
	require.NoError(t, settingsRepo.Migrate())
	if !cfg.SkipSettingsSeed {
		require.NoError(t, settingsRepo.Upsert(context.Background(), storepkg.AppSettings{
			Timezone: tz.String(), City: "San Francisco", Country: "United States",
			Latitude: 37.7749, Longitude: -122.4194,
		}))
	}
	settingsSvc := settings.New(settingsRepo)
	require.NoError(t, settingsSvc.Load(context.Background()))

	geocoder := cfg.Geocoder
	if geocoder == nil {
		geocoder = &FakeForwardGeocoder{Lat: 37.9838, Lon: 23.7275}
	}

	// Weather sensor → httptest.Server returning the supplied forecast.
	weatherBody := cfg.WeatherResponse
	if weatherBody == "" {
		weatherBody = defaultWeatherJSON(now(), tz)
	}
	weatherSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(weatherBody))
	}))

	weather := weathersensor.New(settingsSvc, store, logger.WithField("c", "weather"))
	weather.WithBaseURL(weatherSrv.URL)
	weather.WithNow(now)

	// IMAP sensor → fake Dialer.
	folders := cfg.IMAPFolders
	if folders == nil {
		folders = []string{"INBOX"}
	}
	fimap := NewFakeIMAP()
	for _, f := range folders {
		fimap.AddFolder(f, 1)
	}
	imapSensor := imapsensor.NewWithDialer(config.IMAPConfig{
		Host: "imap.test", Port: 993, Username: "u", Password: "p", TLS: "implicit",
		Folders: folders,
	}, store, store, fimap.Dialer(), logger.WithField("c", "imap"))

	// CalDAV sensor → fake Provider.
	fcaldav := &FakeCalDAV{}
	calSensor := caldavsensor.New(config.CalDAVConfig{URL: "https://caldav.test/", Username: "u", Password: "p"},
		caldavsensor.FixedLocation(tz), fcaldav, store, store, logger.WithField("c", "caldav")).WithNow(now)

	sensors := []sensor.Sensor{weather, imapSensor, calSensor}

	sched, err := schedule.New(config.ScheduleConfig{SyncCron: cfg.SyncCron}, sensors, nil, logger.WithField("c", "sched"))
	require.NoError(t, err)
	sched.WithPerSensorTimeout(10 * time.Second)
	// Mirror production wiring: scheduler retargets when the user changes
	// timezone via PUT /api/settings.
	sched.WithLocation(tz)
	settingsSvc.Subscribe(func(s *settings.Snapshot) {
		if s == nil || s.Location == nil {
			return
		}
		sched.Retarget(s.Location)
	})

	// Live TZ + pinned "now": projections compute against the harness's
	// fixed wall clock but read the user's TZ from the settings service
	// so PUT /api/settings → projection.Compute reflects the new zone
	// without rebuilding the harness.
	projCfg := projection.Config{
		Clock:                 clock.NewLiveTZ(settingsSvc, now),
		LookbackDays:          14,
		RunWindowMinMinutes:   45,
		RunWindowMaxWindKmh:   25,
		RunWindowEarliestHour: 6,
		RunWindowLatestHour:   20,
		OpenThreadsMax:        20,
	}
	if cfg.WithProjCfg != nil {
		projCfg = *cfg.WithProjCfg
	}

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	if cfg.LANToken != "" {
		e.Use(bearerOnAPI(cfg.LANToken))
	}
	(&api.ProjectionsHandler{Reader: store, Cfg: projCfg, Log: logger.WithField("c", "proj")}).Register(e)
	(&api.SyncHandler{Scheduler: sched, Log: logger.WithField("c", "sync")}).Register(e)
	(&api.SettingsHandler{
		Repo: settingsRepo, Service: settingsSvc, Geocoder: geocoder,
		AfterSave: func(ctx context.Context) {
			sched.SyncAll(ctx)
		},
		Now: now, Log: logger.WithField("c", "settings"),
	}).Register(e)

	// V2.3.0 P3: opt-in inject + SSE wiring. The harness itself doesn't
	// run the real synth.SynthesizeInject pipeline (no LLM); the test
	// supplies a stub InjectFunc that typically just publishes a card
	// onto the bus, simulating a successful inject pass. This exercises
	// the full HTTP → scheduler → injectFn → bus → SSE → client path
	// without depending on a live model endpoint.
	var bus *eventbus.Bus
	if cfg.WithInject != nil || cfg.Bus != nil {
		bus = cfg.Bus
		if bus == nil {
			bus = eventbus.New(logger.WithField("c", "eventbus"))
		}
		(&api.SynthHandler{Scheduler: sched, Log: logger.WithField("c", "synth-api")}).Register(e)
		(&api.TodayStreamHandler{Bus: bus, Logger: logger.WithField("c", "today-stream")}).Register(e)
		if cfg.WithInject != nil {
			sched.WithInject(cfg.WithInject)
		}
	}

	srv := httptest.NewServer(e)

	h := &Harness{
		t: t, DB: db, Store: store, Scheduler: sched, Server: srv,
		IMAP: fimap, CalDAV: fcaldav, Weather: weatherSrv,
		TZ: tz, now: now, dbPath: dbPath, Bus: bus,
		Settings: settingsSvc, SettingsRepo: settingsRepo, Geocoder: geocoder,
	}
	if cfg.WithBootPrime {
		go func() { _ = sched.SyncAll(context.Background()) }()
	}
	return h
}

// Close drains the scheduler and closes the test servers.
func (h *Harness) Close() {
	stopped := h.Scheduler.Stop()
	select {
	case <-stopped.Done():
	case <-time.After(5 * time.Second):
		h.t.Log("scheduler did not drain in time")
	}
	h.Server.Close()
	h.Weather.Close()
}

// Get is a small helper for hitting projection endpoints.
func (h *Harness) Get(path string) (int, []byte) {
	resp, err := http.Get(h.Server.URL + path)
	require.NoError(h.t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// GetWithToken adds an Authorization header.
func (h *Harness) GetWithToken(path, token string) (int, []byte) {
	req, err := http.NewRequest(http.MethodGet, h.Server.URL+path, nil)
	require.NoError(h.t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(h.t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// Post hits an endpoint with no body.
func (h *Harness) Post(path string) (int, []byte) {
	resp, err := http.Post(h.Server.URL+path, "application/json", nil)
	require.NoError(h.t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// PostWithToken hits a POST endpoint with an optional bearer token.
func (h *Harness) PostWithToken(path, token string) (int, []byte) {
	req, err := http.NewRequest(http.MethodPost, h.Server.URL+path, nil)
	require.NoError(h.t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(h.t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// CountByKind queries the SQLite DB directly.
func (h *Harness) CountByKind(kind string) int {
	var n int64
	require.NoError(h.t, h.DB.Model(&log.Event{}).Where("kind = ?", kind).Count(&n).Error)
	return int(n)
}

func bearerOnAPI(token string) echo.MiddlewareFunc {
	want := "Bearer " + token
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			path := c.Request().URL.Path
			if len(path) >= 5 && path[:5] == "/api/" {
				if c.Request().Header.Get("Authorization") != want {
					return c.JSON(http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				}
			}
			return next(c)
		}
	}
}

// FakeIMAP is the in-memory IMAP "server" used by system tests. It exposes
// helper methods so tests can arrange folder state and UID validity bumps.
type FakeIMAP struct {
	mu       sync.Mutex
	folders  map[string]*fakeFolder
	failNext atomic.Bool
}

type fakeFolder struct {
	UIDValidity uint32
	NextUID     uint32
	Messages    []imapsensor.RawMessage
}

func NewFakeIMAP() *FakeIMAP { return &FakeIMAP{folders: map[string]*fakeFolder{}} }

func (f *FakeIMAP) AddFolder(name string, uidValidity uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.folders[name] = &fakeFolder{UIDValidity: uidValidity, NextUID: 1}
}

func (f *FakeIMAP) BumpValidity(name string, newValidity uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.folders[name].UIDValidity = newValidity
}

// PutMessage appends a message to the named folder, auto-assigning a UID.
func (f *FakeIMAP) PutMessage(folder, subject, from, to string, body []byte) uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	fld := f.folders[folder]
	uid := fld.NextUID
	fld.NextUID++
	at := -1
	for i, c := range from {
		if c == '@' {
			at = i
			break
		}
	}
	addrFrom := gimap.Address{Mailbox: from, Host: ""}
	if at >= 0 {
		addrFrom = gimap.Address{Mailbox: from[:at], Host: from[at+1:]}
	}
	atTo := -1
	for i, c := range to {
		if c == '@' {
			atTo = i
			break
		}
	}
	addrTo := gimap.Address{Mailbox: to, Host: ""}
	if atTo >= 0 {
		addrTo = gimap.Address{Mailbox: to[:atTo], Host: to[atTo+1:]}
	}
	fld.Messages = append(fld.Messages, imapsensor.RawMessage{
		UID:    uid,
		Folder: folder,
		Env: &gimap.Envelope{
			Subject:   subject,
			From:      []gimap.Address{addrFrom},
			To:        []gimap.Address{addrTo},
			MessageID: subject + "@example.test",
		},
		Body: body,
	})
	return uid
}

// FailNext arranges the next IMAP login to fail. (Used to verify partial
// failure handling.)
func (f *FakeIMAP) FailNext() { f.failNext.Store(true) }

// Dialer returns an imapsensor.Dialer backed by this fake.
func (f *FakeIMAP) Dialer() imapsensor.Dialer { return &fakeDialer{f: f} }

type fakeDialer struct{ f *FakeIMAP }

func (d *fakeDialer) Dial(_ context.Context) (imapsensor.Client, error) {
	return &fakeClient{f: d.f}, nil
}

type fakeClient struct{ f *FakeIMAP }

func (c *fakeClient) Login(_, _ string) error {
	if c.f.failNext.Swap(false) {
		return errors.New("simulated login failure")
	}
	return nil
}

func (c *fakeClient) Select(folder string) (*imapsensor.SelectData, error) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	fld, ok := c.f.folders[folder]
	if !ok {
		return nil, errors.New("folder not found: " + folder)
	}
	return &imapsensor.SelectData{UIDValidity: fld.UIDValidity, UIDNext: fld.NextUID}, nil
}

func (c *fakeClient) UIDSearchAfter(folder string, lastUID uint32) ([]uint32, error) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	fld := c.f.folders[folder]
	out := make([]uint32, 0, len(fld.Messages))
	for _, m := range fld.Messages {
		if m.UID > lastUID {
			out = append(out, m.UID)
		}
	}
	return out, nil
}

func (c *fakeClient) UIDSearchAll(folder string) ([]uint32, error) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	fld := c.f.folders[folder]
	out := make([]uint32, 0, len(fld.Messages))
	for _, m := range fld.Messages {
		out = append(out, m.UID)
	}
	return out, nil
}

func (c *fakeClient) FetchEnvelopeAndBody(folder string, uids []uint32) ([]imapsensor.RawMessage, error) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	fld := c.f.folders[folder]
	want := make(map[uint32]struct{}, len(uids))
	for _, u := range uids {
		want[u] = struct{}{}
	}
	var out []imapsensor.RawMessage
	for _, m := range fld.Messages {
		if _, ok := want[m.UID]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}

func (c *fakeClient) Logout() error { return nil }
func (c *fakeClient) Close() error  { return nil }

// V2.8 write methods — system tests don't exercise them; no-op stubs
// keep the Client interface satisfied.
func (c *fakeClient) Append(_ string, _ []string, _ time.Time, _ []byte) (uint32, error) {
	return 0, nil
}
func (c *fakeClient) Store(_ string, _ uint32, _, _ []string) error { return nil }
func (c *fakeClient) Move(_ string, _ []uint32, _ string) error     { return nil }

// FakeCalDAV is the stub Provider used by system tests.
type FakeCalDAV struct {
	mu     sync.Mutex
	events []caldavsensor.RawEvent
	err    error
	calls  int
}

// SetEvents replaces the event list returned by ListEvents.
func (f *FakeCalDAV) SetEvents(events ...caldavsensor.RawEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = events
}

// SetError makes the next ListEvents call return err.
func (f *FakeCalDAV) SetError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// ListEvents satisfies caldav.Provider.
func (f *FakeCalDAV) ListEvents(_ context.Context, _, _ time.Time) ([]caldavsensor.RawEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		err := f.err
		f.err = nil
		return nil, err
	}
	out := make([]caldavsensor.RawEvent, len(f.events))
	copy(out, f.events)
	return out, nil
}

// V2.8 write methods — system tests don't exercise CalDAV write today;
// no-op stubs satisfy the interface.

func (f *FakeCalDAV) GetEvent(_ context.Context, _ string) (*caldavsensor.RawEvent, error) {
	return nil, nil
}
func (f *FakeCalDAV) PutEvent(_ context.Context, _, _ string) (string, string, error) {
	return "", "", nil
}
func (f *FakeCalDAV) UpdateEvent(_ context.Context, _, _, _ string) (string, error) {
	return "", nil
}
func (f *FakeCalDAV) DeleteEvent(_ context.Context, _, _ string) error { return nil }

// MakeICS builds a minimal VCALENDAR payload for system-test arrangement.
func MakeICS(uid, summary, location, tag string, start, end time.Time, tz *time.Location) string {
	tagLine := ""
	if tag != "" {
		tagLine = "CATEGORIES:" + tag + "\r\n"
	}
	const layout = "20060102T150405"
	tzID := tz.String()
	dtStart := "DTSTART;TZID=" + tzID + ":" + start.In(tz).Format(layout) + "\r\n"
	dtEnd := "DTEND;TZID=" + tzID + ":" + end.In(tz).Format(layout) + "\r\n"
	body := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//system_test//EN\r\nBEGIN:VEVENT\r\nUID:" + uid + "\r\n" + dtStart + dtEnd + "SUMMARY:" + summary +
		"\r\nLOCATION:" + location + "\r\n" + tagLine + "END:VEVENT\r\nEND:VCALENDAR\r\n"
	return body
}

// defaultWeatherJSON returns a 24h clear forecast in the given zone, anchored
// at the given "now" (truncated to the hour).
func defaultWeatherJSON(now time.Time, tz *time.Location) string {
	type hourly struct {
		Time          []string  `json:"time"`
		Temp          []float64 `json:"temperature_2m"`
		Precip        []float64 `json:"precipitation"`
		Code          []int     `json:"weather_code"`
		WindSpeed     []float64 `json:"wind_speed_10m"`
		WindDirection []float64 `json:"wind_direction_10m"`
	}
	type current struct {
		Time          string  `json:"time"`
		Temp          float64 `json:"temperature_2m"`
		Precip        float64 `json:"precipitation"`
		Code          int     `json:"weather_code"`
		WindSpeed     float64 `json:"wind_speed_10m"`
		WindDirection float64 `json:"wind_direction_10m"`
	}
	type forecast struct {
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
		Timezone  string  `json:"timezone"`
		Current   current `json:"current"`
		Hourly    hourly  `json:"hourly"`
	}

	const layout = "2006-01-02T15:04"
	start := now.In(tz).Truncate(time.Hour)
	h := hourly{}
	for i := 0; i < 48; i++ {
		t := start.Add(time.Duration(i) * time.Hour)
		h.Time = append(h.Time, t.Format(layout))
		h.Temp = append(h.Temp, 15)
		h.Precip = append(h.Precip, 0)
		h.Code = append(h.Code, 1)
		h.WindSpeed = append(h.WindSpeed, 8)
		h.WindDirection = append(h.WindDirection, 270)
	}
	cur := current{
		Time: start.Format(layout), Temp: 15, Precip: 0, Code: 1, WindSpeed: 8, WindDirection: 270,
	}
	f := forecast{Latitude: 37.7749, Longitude: -122.4194, Timezone: tz.String(), Current: cur, Hourly: h}
	b, _ := json.Marshal(f)
	return string(b)
}
