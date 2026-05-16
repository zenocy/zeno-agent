// Package weather is the Open-Meteo sensor. It fetches the next ~24h of
// hourly forecast plus the current conditions and emits one
// weather.snapshot event per Sync.
//
// No auth, no cursor — Open-Meteo is stateless from the caller's perspective.
package weather

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/sensor"
)

const (
	defaultBase   = "https://api.open-meteo.com"
	hourlyVars    = "temperature_2m,precipitation,weather_code,wind_speed_10m,wind_direction_10m"
	currentVars   = "temperature_2m,precipitation,weather_code,wind_speed_10m,wind_direction_10m"
	dailyVars     = "temperature_2m_max,temperature_2m_min,weather_code,precipitation_sum"
	hourlyMaxKeep = 24
	// forecastDays covers today + 3 future days so the projection can surface
	// 2–3 days of multi-day predictions to the UI.
	forecastDays = 4

	// fetchAttempts is the total number of forecast-fetch attempts per
	// Sync (1 try + 1 retry on transient failure). Open-Meteo's free
	// endpoint occasionally hiccups; a single retry with a short
	// backoff covers the vast majority of those cases without burning
	// the sync_cron budget. Permanent errors (4xx, JSON malformed)
	// don't retry — only network/timeout/5xx do.
	fetchAttempts = 2
	// fetchRetryBackoff is the wait between attempts. Kept short
	// because the sync_cron interval is 10m — even a 2s pause on the
	// retry path adds negligible latency.
	fetchRetryBackoff = 2 * time.Second
	// fetchTimeout caps a single HTTP attempt. Open-Meteo's full
	// 4-day multi-parameter forecast is normally < 2s but has been
	// observed taking 15s+ under load; 30s gives the slow path
	// headroom without risking the sync_cron budget.
	fetchTimeout = 30 * time.Second
)

// Locator yields the live coordinates and timezone the sensor should use
// for the *current* Sync call. Implementing it as an interface (rather
// than freezing values at construction) lets the user change location
// from the Settings UI without restarting the process. ok=false means
// the user hasn't configured a location yet — Sync becomes a no-op.
type Locator interface {
	Coords() (lat, lon float64, tz string, ok bool)
}

// Reverser turns coordinates into a place name. The weather sensor uses it
// to stamp a human-friendly label onto each snapshot. nil is allowed — the
// snapshot just ships without a location.
type Reverser interface {
	Reverse(ctx context.Context, lat, lon float64) (string, error)
}

// Sensor implements sensor.Sensor for Open-Meteo.
type Sensor struct {
	locator      Locator
	writer       log.Writer
	http         *http.Client
	base         string
	log          *logrus.Entry
	now          func() time.Time
	geocoder     Reverser
	retryBackoff time.Duration // overridable for tests; defaults to fetchRetryBackoff
}

// New constructs a weather Sensor with sensible defaults. The Locator
// is consulted at every Sync, so location/timezone changes take effect
// without restart.
func New(locator Locator, w log.Writer, l *logrus.Entry) *Sensor {
	return &Sensor{
		locator: locator,
		writer:  w,
		http:    &http.Client{Timeout: fetchTimeout},
		base:    defaultBase,
		log:     l,
		now:     time.Now,
	}
}

// WithBaseURL overrides the Open-Meteo base URL (used by tests with httptest).
func (s *Sensor) WithBaseURL(u string) *Sensor { s.base = u; return s }

// WithHTTPClient swaps the HTTP client (used by tests).
func (s *Sensor) WithHTTPClient(c *http.Client) *Sensor { s.http = c; return s }

// WithNow overrides the clock (used by tests).
func (s *Sensor) WithNow(now func() time.Time) *Sensor { s.now = now; return s }

// WithGeocoder enables reverse-geocoding for the snapshot's Location field.
// Failures are logged and ignored — the snapshot still ships without a label.
func (s *Sensor) WithGeocoder(g Reverser) *Sensor { s.geocoder = g; return s }

// WithRetryBackoff overrides the inter-attempt sleep on transient
// fetch failures (used by tests; production uses fetchRetryBackoff).
func (s *Sensor) WithRetryBackoff(d time.Duration) *Sensor { s.retryBackoff = d; return s }

// Name returns the sensor identifier.
func (s *Sensor) Name() string { return "weather" }

// Sync fetches the forecast and emits a single weather.snapshot event.
// Returns nil with a debug log when no location is configured (the user
// hasn't opened the Settings UI yet).
func (s *Sensor) Sync(ctx context.Context) error {
	lat, lon, tz, ok := s.locator.Coords()
	if !ok {
		if s.log != nil {
			s.log.Debug("weather: skipping sync, no location set")
		}
		return nil
	}
	if tz == "" {
		tz = "auto"
	}
	url := fmt.Sprintf(
		"%s/v1/forecast?latitude=%.6f&longitude=%.6f&hourly=%s&current=%s&daily=%s&timezone=%s&forecast_days=%d",
		s.base, lat, lon, hourlyVars, currentVars, dailyVars, tz, forecastDays,
	)

	body, err := s.fetchWithRetry(ctx, url)
	if err != nil {
		return err
	}

	var raw rawForecast
	if err := json.Unmarshal(body, &raw); err != nil {
		if s.log != nil {
			s.log.WithError(err).Warn("weather: decode forecast failed")
		}
		return fmt.Errorf("decode forecast: %w", err)
	}

	loc, err := time.LoadLocation(raw.Timezone)
	if err != nil {
		// Open-Meteo always returns an IANA name when timezone=auto resolves;
		// fall back to UTC if that's somehow not the case.
		loc = time.UTC
	}

	payload, err := buildSnapshot(raw, loc, lat, lon, s.now())
	if err != nil {
		return fmt.Errorf("build snapshot: %w", err)
	}

	if s.geocoder != nil {
		if name, gerr := s.geocoder.Reverse(ctx, lat, lon); gerr != nil {
			if s.log != nil {
				s.log.WithError(gerr).Debug("reverse geocode failed; snapshot will ship without location")
			}
		} else {
			payload.Location = name
		}
	}

	ev, err := s.writer.Append(ctx, log.KindWeatherSnapshot, s.Name(), payload)
	if err != nil {
		return fmt.Errorf("append event: %w", err)
	}
	// Publish strictly AFTER the durable Append so the projection
	// publisher (which folds the log) sees the new snapshot when it
	// recomputes WeatherView.
	sensor.PublishObserved(ctx, log.KindWeatherSnapshot, ev.ID, nil)
	if s.log != nil {
		s.log.WithField("hourly_count", len(payload.Hourly)).Debug("weather snapshot emitted")
	}
	return nil
}

// rawForecast mirrors the slice-of-arrays JSON Open-Meteo returns.
type rawForecast struct {
	Latitude  float64    `json:"latitude"`
	Longitude float64    `json:"longitude"`
	Timezone  string     `json:"timezone"`
	Current   rawCurrent `json:"current"`
	Hourly    rawHourly  `json:"hourly"`
	Daily     rawDaily   `json:"daily"`
}

type rawCurrent struct {
	Time          string  `json:"time"`
	Temp          float64 `json:"temperature_2m"`
	Precip        float64 `json:"precipitation"`
	Code          int     `json:"weather_code"`
	WindSpeed     float64 `json:"wind_speed_10m"`
	WindDirection float64 `json:"wind_direction_10m"`
}

type rawHourly struct {
	Time          []string  `json:"time"`
	Temp          []float64 `json:"temperature_2m"`
	Precip        []float64 `json:"precipitation"`
	Code          []int     `json:"weather_code"`
	WindSpeed     []float64 `json:"wind_speed_10m"`
	WindDirection []float64 `json:"wind_direction_10m"`
}

type rawDaily struct {
	Time      []string  `json:"time"`
	TempMax   []float64 `json:"temperature_2m_max"`
	TempMin   []float64 `json:"temperature_2m_min"`
	Code      []int     `json:"weather_code"`
	PrecipSum []float64 `json:"precipitation_sum"`
}

// Snapshot is the payload schema written to the log.
type Snapshot struct {
	CapturedAt time.Time   `json:"captured_at"`
	Lat        float64     `json:"lat"`
	Lon        float64     `json:"lon"`
	Timezone   string      `json:"timezone"`
	Location   string      `json:"location,omitempty"`
	Current    HourPoint   `json:"current"`
	Hourly     []HourPoint `json:"hourly"`
	Daily      []DayPoint  `json:"daily,omitempty"`
}

// HourPoint is one point in the forecast.
type HourPoint struct {
	Time       time.Time `json:"time"`
	TempC      float64   `json:"temp_c"`
	PrecipMM   float64   `json:"precip_mm"`
	Code       int       `json:"code"`
	WindKmh    float64   `json:"wind_kmh"`
	WindDirDeg float64   `json:"wind_dir_deg"`
	Label      string    `json:"label,omitempty"`
}

// DayPoint is one day in the multi-day forecast.
type DayPoint struct {
	Date     time.Time `json:"date"`
	TempMaxC float64   `json:"temp_max_c"`
	TempMinC float64   `json:"temp_min_c"`
	Code     int       `json:"code"`
	PrecipMM float64   `json:"precip_mm"`
	Label    string    `json:"label,omitempty"`
}

func buildSnapshot(raw rawForecast, loc *time.Location, lat, lon float64, now time.Time) (Snapshot, error) {
	curT, err := parseLocal(raw.Current.Time, loc)
	if err != nil {
		return Snapshot{}, fmt.Errorf("current time: %w", err)
	}
	cur := HourPoint{
		Time:       curT,
		TempC:      raw.Current.Temp,
		PrecipMM:   raw.Current.Precip,
		Code:       raw.Current.Code,
		WindKmh:    raw.Current.WindSpeed,
		WindDirDeg: raw.Current.WindDirection,
		Label:      WMOLabel(raw.Current.Code),
	}

	n := len(raw.Hourly.Time)
	if n != len(raw.Hourly.Temp) || n != len(raw.Hourly.Precip) ||
		n != len(raw.Hourly.Code) || n != len(raw.Hourly.WindSpeed) ||
		n != len(raw.Hourly.WindDirection) {
		return Snapshot{}, fmt.Errorf("hourly array length mismatch")
	}

	// Drop hours strictly before "now" (in zone, truncated to the hour) so the
	// resulting slice always starts at the current or upcoming hour.
	startIdx := 0
	cutoff := now.In(loc).Truncate(time.Hour)
	for i, ts := range raw.Hourly.Time {
		t, err := parseLocal(ts, loc)
		if err != nil {
			return Snapshot{}, fmt.Errorf("hourly time %d: %w", i, err)
		}
		if !t.Before(cutoff) {
			startIdx = i
			break
		}
		startIdx = i + 1
	}

	end := min(startIdx+hourlyMaxKeep, n)

	hourly := make([]HourPoint, 0, end-startIdx)
	for i := startIdx; i < end; i++ {
		t, err := parseLocal(raw.Hourly.Time[i], loc)
		if err != nil {
			return Snapshot{}, fmt.Errorf("hourly time %d: %w", i, err)
		}
		hourly = append(hourly, HourPoint{
			Time:       t,
			TempC:      raw.Hourly.Temp[i],
			PrecipMM:   raw.Hourly.Precip[i],
			Code:       raw.Hourly.Code[i],
			WindKmh:    raw.Hourly.WindSpeed[i],
			WindDirDeg: raw.Hourly.WindDirection[i],
		})
	}

	daily, err := buildDaily(raw.Daily, loc)
	if err != nil {
		return Snapshot{}, fmt.Errorf("daily: %w", err)
	}

	return Snapshot{
		CapturedAt: now.UTC(),
		Lat:        lat,
		Lon:        lon,
		Timezone:   raw.Timezone,
		Current:    cur,
		Hourly:     hourly,
		Daily:      daily,
	}, nil
}

// buildDaily turns the daily slice-of-arrays payload into a flat []DayPoint.
// Returns an empty slice (not an error) when the API response omits the
// daily section — older fixtures and partial responses are still valid.
func buildDaily(raw rawDaily, loc *time.Location) ([]DayPoint, error) {
	n := len(raw.Time)
	if n == 0 {
		return nil, nil
	}
	if n != len(raw.TempMax) || n != len(raw.TempMin) || n != len(raw.Code) {
		return nil, fmt.Errorf("daily array length mismatch")
	}
	hasPrecip := len(raw.PrecipSum) == n

	const layout = "2006-01-02"
	out := make([]DayPoint, 0, n)
	for i, ts := range raw.Time {
		d, err := time.ParseInLocation(layout, ts, loc)
		if err != nil {
			return nil, fmt.Errorf("daily time %d: %w", i, err)
		}
		dp := DayPoint{
			Date:     d,
			TempMaxC: raw.TempMax[i],
			TempMinC: raw.TempMin[i],
			Code:     raw.Code[i],
			Label:    WMOLabel(raw.Code[i]),
		}
		if hasPrecip {
			dp.PrecipMM = raw.PrecipSum[i]
		}
		out = append(out, dp)
	}
	return out, nil
}

// Open-Meteo returns local times in the form "2026-04-25T10:00" without a
// trailing zone designator. Parse them in the location the API resolved.
func parseLocal(s string, loc *time.Location) (time.Time, error) {
	const layout = "2006-01-02T15:04"
	return time.ParseInLocation(layout, s, loc)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// fetchWithRetry GETs url with one retry on transient failure
// (network/timeout/5xx). Permanent failures (4xx, malformed body)
// short-circuit immediately. The final attempt's error is the only
// one logged at WARN — earlier attempts log at DEBUG so a successful
// retry doesn't leave a misleading warning trail in operator logs.
func (s *Sensor) fetchWithRetry(ctx context.Context, url string) ([]byte, error) {
	var lastErr error
	for attempt := 1; attempt <= fetchAttempts; attempt++ {
		body, err, retriable := s.fetchOnce(ctx, url)
		if err == nil {
			if attempt > 1 && s.log != nil {
				s.log.WithField("attempts", attempt).Info("weather: forecast fetch recovered after retry")
			}
			return body, nil
		}
		lastErr = err
		if !retriable || attempt == fetchAttempts {
			break
		}
		if s.log != nil {
			s.log.WithError(err).WithField("attempt", attempt).Debug("weather: forecast fetch transient failure; retrying")
		}
		backoff := s.retryBackoff
		if backoff <= 0 {
			backoff = fetchRetryBackoff
		}
		// Honor context cancellation during the backoff sleep.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
	}
	if s.log != nil {
		s.log.WithError(lastErr).Warn("weather: forecast fetch failed")
	}
	return nil, fmt.Errorf("fetch forecast: %w", lastErr)
}

// fetchOnce performs a single GET. Returns retriable=true for
// network errors and 5xx responses; false for everything else
// (4xx, body-read failures past the network boundary).
func (s *Sensor) fetchOnce(ctx context.Context, url string) ([]byte, error, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err), false
	}
	resp, err := s.http.Do(req)
	if err != nil {
		// Don't retry when the caller's context is dead — the
		// scheduler will hit us again at the next tick. Distinct
		// from http.Client.Timeout which surfaces a similar error
		// shape but leaves ctx.Err() == nil and IS retriable.
		if ctx.Err() != nil {
			return nil, err, false
		}
		return nil, err, true
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err), true
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("forecast returned %d: %s", resp.StatusCode, truncate(string(body), 200)), true
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("forecast returned %d: %s", resp.StatusCode, truncate(string(body), 200)), false
	}
	return body, nil, false
}
