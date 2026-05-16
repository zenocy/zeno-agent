package weather

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/log/logtest"
)

// fakeLocator returns a fixed (lat, lon, tz, ok) tuple. When ok is false
// the sensor should skip the sync entirely.
type fakeLocator struct {
	lat, lon float64
	tz       string
	ok       bool
}

func (f *fakeLocator) Coords() (float64, float64, string, bool) {
	return f.lat, f.lon, f.tz, f.ok
}

func sfLocator() *fakeLocator {
	return &fakeLocator{lat: 37.7749, lon: -122.4194, tz: "America/Los_Angeles", ok: true}
}

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return b
}

func newServer(t *testing.T, body []byte, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/forecast", r.URL.Path)
		q := r.URL.Query()
		require.NotEmpty(t, q.Get("latitude"))
		require.NotEmpty(t, q.Get("longitude"))
		require.NotEmpty(t, q.Get("hourly"))
		require.NotEmpty(t, q.Get("current"))
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
}

func newSensor(t *testing.T, base string, mem *logtest.MemReader, fakeNow time.Time) *Sensor {
	t.Helper()
	logger := logrus.New()
	logger.Out = io.Discard
	s := New(sfLocator(), mem, logger.WithField("c", "weather"))
	s.WithBaseURL(base)
	s.WithNow(func() time.Time { return fakeNow })
	// Zero backoff so retry-aware tests don't sleep 2s between
	// attempts. Production uses fetchRetryBackoff via the default.
	s.WithRetryBackoff(time.Microsecond)
	return s
}

func TestSync_HappyPath(t *testing.T) {
	srv := newServer(t, loadFixture(t, "forecast.json"), 200)
	defer srv.Close()

	mem := logtest.NewMemReader()
	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 25, 10, 0, 0, 0, loc)
	s := newSensor(t, srv.URL, mem, now)

	require.NoError(t, s.Sync(context.Background()))

	events, err := mem.ByKind(context.Background(), log.KindWeatherSnapshot)
	require.NoError(t, err)
	require.Len(t, events, 1)

	var snap Snapshot
	require.NoError(t, json.Unmarshal(events[0].Payload, &snap))
	require.Equal(t, "America/Los_Angeles", snap.Timezone)
	require.Equal(t, 14.5, snap.Current.TempC)
	require.Equal(t, "Mainly clear", snap.Current.Label)
	require.LessOrEqual(t, len(snap.Hourly), 24)
	// First hourly entry must be at-or-after the now-truncated-to-hour mark.
	require.False(t, snap.Hourly[0].Time.Before(now.Truncate(time.Hour)))
}

func TestSync_HourlyTruncation(t *testing.T) {
	srv := newServer(t, loadFixture(t, "forecast.json"), 200)
	defer srv.Close()

	mem := logtest.NewMemReader()
	loc, _ := time.LoadLocation("America/Los_Angeles")
	// Force startIdx=0 by claiming "now" is before all hourly entries.
	now := time.Date(2026, 4, 25, 0, 0, 0, 0, loc)
	s := newSensor(t, srv.URL, mem, now)

	require.NoError(t, s.Sync(context.Background()))

	events, _ := mem.ByKind(context.Background(), log.KindWeatherSnapshot)
	require.Len(t, events, 1)
	var snap Snapshot
	require.NoError(t, json.Unmarshal(events[0].Payload, &snap))
	require.Equal(t, 24, len(snap.Hourly), "fixture has 48 hours, must truncate to 24")
}

func TestSync_HTTPError(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(500)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	mem := logtest.NewMemReader()
	s := newSensor(t, srv.URL, mem, time.Now())

	err := s.Sync(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "500")
	require.Equal(t, 2, calls, "5xx is retriable; expect 1 try + 1 retry")

	require.Empty(t, mem.Events(), "no event written on HTTP error")
}

func TestSync_RetryRecoversFromTransient5xx(t *testing.T) {
	calls := 0
	body := loadFixture(t, "forecast.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(503)
			_, _ = w.Write([]byte("temporarily unavailable"))
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	mem := logtest.NewMemReader()
	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 25, 10, 0, 0, 0, loc)
	s := newSensor(t, srv.URL, mem, now)

	require.NoError(t, s.Sync(context.Background()))
	require.Equal(t, 2, calls, "second attempt should succeed")

	events, err := mem.ByKind(context.Background(), log.KindWeatherSnapshot)
	require.NoError(t, err)
	require.Len(t, events, 1, "snapshot emitted on retry success")
}

func TestSync_DoesNotRetryOn4xx(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(400)
		_, _ = w.Write([]byte("bad request"))
	}))
	defer srv.Close()

	mem := logtest.NewMemReader()
	s := newSensor(t, srv.URL, mem, time.Now())

	err := s.Sync(context.Background())
	require.Error(t, err)
	require.Equal(t, 1, calls, "4xx is permanent; must not retry")
}

func TestSync_MalformedJSON(t *testing.T) {
	srv := newServer(t, []byte("not json"), 200)
	defer srv.Close()

	mem := logtest.NewMemReader()
	s := newSensor(t, srv.URL, mem, time.Now())

	err := s.Sync(context.Background())
	require.Error(t, err)
	require.Empty(t, mem.Events())
}

func TestSync_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	mem := logtest.NewMemReader()
	s := newSensor(t, srv.URL, mem, time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := s.Sync(ctx)
	require.Error(t, err)
	require.Empty(t, mem.Events())
}

type fakeReverser struct {
	name string
	err  error
	hits int
}

func (f *fakeReverser) Reverse(_ context.Context, _, _ float64) (string, error) {
	f.hits++
	return f.name, f.err
}

func TestSync_StampsLocationFromGeocoder(t *testing.T) {
	srv := newServer(t, loadFixture(t, "forecast.json"), 200)
	defer srv.Close()

	mem := logtest.NewMemReader()
	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 25, 10, 0, 0, 0, loc)
	rev := &fakeReverser{name: "San Francisco"}
	s := newSensor(t, srv.URL, mem, now).WithGeocoder(rev)

	require.NoError(t, s.Sync(context.Background()))
	require.Equal(t, 1, rev.hits)

	events, _ := mem.ByKind(context.Background(), log.KindWeatherSnapshot)
	require.Len(t, events, 1)
	var snap Snapshot
	require.NoError(t, json.Unmarshal(events[0].Payload, &snap))
	require.Equal(t, "San Francisco", snap.Location)
}

func TestSync_GeocoderFailureNonFatal(t *testing.T) {
	srv := newServer(t, loadFixture(t, "forecast.json"), 200)
	defer srv.Close()

	mem := logtest.NewMemReader()
	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 25, 10, 0, 0, 0, loc)
	rev := &fakeReverser{err: errAny()}
	s := newSensor(t, srv.URL, mem, now).WithGeocoder(rev)

	require.NoError(t, s.Sync(context.Background()), "geocoder error must not fail Sync")
	events, _ := mem.ByKind(context.Background(), log.KindWeatherSnapshot)
	require.Len(t, events, 1)
	var snap Snapshot
	require.NoError(t, json.Unmarshal(events[0].Payload, &snap))
	require.Empty(t, snap.Location)
}

func errAny() error { return &netErr{} }

type netErr struct{}

func (*netErr) Error() string { return "boom" }

// When the locator says no location is set, Sync must be a silent
// no-op — the user just hasn't visited the Settings page yet.
func TestSync_SkipsWhenLocatorUnset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("HTTP must not be hit when locator is unset")
	}))
	defer srv.Close()

	mem := logtest.NewMemReader()
	logger := logrus.New()
	logger.Out = io.Discard
	s := New(&fakeLocator{ok: false}, mem, logger.WithField("c", "weather")).WithBaseURL(srv.URL)

	require.NoError(t, s.Sync(context.Background()))
	require.Empty(t, mem.Events(), "no event written when locator is unset")
}

func TestWMOLabel(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{0, "Clear sky"},
		{1, "Mainly clear"},
		{2, "Partly cloudy"},
		{45, "Foggy"},
		{51, "Light drizzle"},
		{61, "Slight rain"},
		{71, "Slight snowfall"},
		{95, "Thunderstorm"},
	}
	for _, c := range cases {
		require.Equal(t, c.want, WMOLabel(c.code), "code=%d", c.code)
	}
	require.Contains(t, WMOLabel(9999), "Unknown")
}
