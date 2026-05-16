package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/eventbus"
	"github.com/zenocy/zeno-v2/internal/sensor/geocode"
	"github.com/zenocy/zeno-v2/internal/settings"
	"github.com/zenocy/zeno-v2/internal/store"
)

// SettingsHandler answers GET / PUT /api/settings. The handler is the
// single write path for system-level scalar settings (timezone,
// city/country/lat/lon). On every successful write it triggers
// Service.Reload so consumers (weather sensor, TZ-using handlers) see
// the new values without a process restart.
//
// AfterSave is fired in a detached goroutine after a successful PUT.
// Production wires it to scheduler.SyncAll so saving a new location
// kicks off an immediate weather sync — otherwise the user's widget
// keeps showing the old place name until the next cron tick.
type SettingsHandler struct {
	Repo      *store.SettingsRepo
	Service   *settings.Service
	Geocoder  geocode.Forward // optional; nil → city/country edits skip geocoding
	AfterSave func(context.Context)
	Bus       *eventbus.Bus
	Now       func() time.Time
	Log       *logrus.Entry
}

// Register attaches the two routes to the Echo instance.
func (h *SettingsHandler) Register(e *echo.Echo) {
	e.GET("/api/settings", h.get)
	e.PUT("/api/settings", h.put)
}

// settingsDTO is the JSON wire shape sent to and accepted from the UI.
// Set is response-only; the UI uses it to know whether to render the
// "first-time setup" copy. Lat/Long are derived (geocoded) on PUT and
// returned for transparency, but the UI doesn't write them directly.
type settingsDTO struct {
	Timezone          string  `json:"timezone"`
	City              string  `json:"city"`
	Country           string  `json:"country"`
	Latitude          float64 `json:"latitude"`
	Longitude         float64 `json:"longitude"`
	StockTickers      string  `json:"stock_tickers"`
	StockThresholdPct float64 `json:"stock_threshold_pct"`
	StockAlwaysPoll   bool    `json:"stock_always_poll"`
	WorldClocks       string  `json:"world_clocks"`
	UserName          string  `json:"user_name"`
	AssistantName     string  `json:"assistant_name"`
	AssistantTone     string  `json:"assistant_tone"`
	Set               bool    `json:"set,omitempty"`
	GeocodeError      string  `json:"geocode_error,omitempty"`
}

// V2.13.0 persona field length caps. Free-form strings — kept short so
// the system prompt budget isn't blown by adversarial input.
const (
	maxPersonaNameLen = 32
	maxPersonaToneLen = 80
)

// stockThresholdMaxPct is an upper bound on the threshold field — well
// above any sensible setting. Catches typos (3000 instead of 3.0)
// without enforcing a particular product opinion.
const stockThresholdMaxPct = 50.0

func (h *SettingsHandler) get(c echo.Context) error {
	snap := h.Service.Snapshot()
	return c.JSON(http.StatusOK, settingsDTO{
		Timezone:          snap.Timezone,
		City:              snap.City,
		Country:           snap.Country,
		Latitude:          snap.Latitude,
		Longitude:         snap.Longitude,
		StockTickers:      strings.Join(snap.StockTickers, ","),
		StockThresholdPct: snap.StockThresholdPct,
		StockAlwaysPoll:   snap.StockAlwaysPoll,
		WorldClocks:       strings.Join(snap.WorldClocks, ","),
		UserName:          snap.UserName,
		AssistantName:     snap.AssistantName,
		AssistantTone:     snap.AssistantTone,
		Set:               snap.Set,
	})
}

func (h *SettingsHandler) put(c echo.Context) error {
	var req settingsDTO
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return BadRequest(c, "invalid request body")
	}

	tz := strings.TrimSpace(req.Timezone)
	city := strings.TrimSpace(req.City)
	country := strings.TrimSpace(req.Country)

	if tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			return BadRequest(c, "invalid timezone")
		}
	}

	if req.StockThresholdPct < 0 || req.StockThresholdPct > stockThresholdMaxPct {
		return BadRequest(c, "stock_threshold_pct out of range")
	}

	userName := strings.TrimSpace(req.UserName)
	assistantName := strings.TrimSpace(req.AssistantName)
	assistantTone := strings.TrimSpace(req.AssistantTone)
	if len(userName) > maxPersonaNameLen {
		return BadRequest(c, "user_name too long")
	}
	if len(assistantName) > maxPersonaNameLen {
		return BadRequest(c, "assistant_name too long")
	}
	if len(assistantTone) > maxPersonaToneLen {
		return BadRequest(c, "assistant_tone too long")
	}
	// Normalize tickers (uppercase, dedupe, drop empties) on the way in
	// so what's persisted matches what Snapshot reads back.
	normalizedTickers := settings.ParseStockTickers(req.StockTickers)
	normalizedClocks := settings.ParseWorldClocks(req.WorldClocks)

	prior := h.Service.Snapshot()
	row := store.AppSettings{
		Timezone:          tz,
		City:              city,
		Country:           country,
		Latitude:          prior.Latitude,
		Longitude:         prior.Longitude,
		StockTickers:      strings.Join(normalizedTickers, ","),
		StockThresholdPct: req.StockThresholdPct,
		StockAlwaysPoll:   req.StockAlwaysPoll,
		WorldClocks:       strings.Join(normalizedClocks, ","),
		UserName:          userName,
		AssistantName:     assistantName,
		AssistantTone:     assistantTone,
	}

	// Geocode only when there's something to geocode AND the location
	// actually changed (avoids redundant network calls on TZ-only edits).
	geocodeErr := ""
	locationChanged := city != prior.City || country != prior.Country
	if city != "" && country != "" && locationChanged && h.Geocoder != nil {
		ctx, cancel := context.WithTimeout(c.Request().Context(), 8*time.Second)
		lat, lon, err := h.Geocoder.Geocode(ctx, city, country)
		cancel()
		if err != nil {
			if h.Log != nil {
				h.Log.WithError(err).WithFields(logrus.Fields{
					"city": city, "country": country,
				}).Warn("settings: geocode failed; saving without coords update")
			}
			if errors.Is(err, geocode.ErrNoMatch) {
				geocodeErr = "could not find that city/country"
			} else {
				geocodeErr = "geocoding service unavailable"
			}
		} else {
			row.Latitude = lat
			row.Longitude = lon
		}
	}
	// If both city and country are cleared, drop the stale lat/lon too.
	if city == "" && country == "" {
		row.Latitude = 0
		row.Longitude = 0
	}

	if err := h.Repo.Upsert(c.Request().Context(), row); err != nil {
		if h.Log != nil {
			h.Log.WithError(err).Error("settings upsert failed")
		}
		return Internal(c, err)
	}
	if err := h.Service.Reload(c.Request().Context()); err != nil {
		if h.Log != nil {
			h.Log.WithError(err).Error("settings reload failed")
		}
		return Internal(c, err)
	}

	// Fire-and-forget post-save hook (e.g. trigger SyncAll). The request
	// context is about to be canceled, so use a fresh background context
	// with a generous bound; the scheduler enforces its own per-sensor
	// timeouts internally.
	if h.AfterSave != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			h.AfterSave(ctx)
		}()
	}

	snap := h.Service.Snapshot()
	dto := settingsDTO{
		Timezone:          snap.Timezone,
		City:              snap.City,
		Country:           snap.Country,
		Latitude:          snap.Latitude,
		Longitude:         snap.Longitude,
		StockTickers:      strings.Join(snap.StockTickers, ","),
		StockThresholdPct: snap.StockThresholdPct,
		StockAlwaysPoll:   snap.StockAlwaysPoll,
		WorldClocks:       strings.Join(snap.WorldClocks, ","),
		UserName:          snap.UserName,
		AssistantName:     snap.AssistantName,
		AssistantTone:     snap.AssistantTone,
		Set:               snap.Set,
		GeocodeError:      geocodeErr,
	}
	if h.Bus != nil {
		// GeocodeError is omitted from the broadcast — it's a per-request
		// transient meant for the form that triggered the save, not for
		// passive listeners watching for steady-state changes.
		broadcast := dto
		broadcast.GeocodeError = ""
		if raw, err := json.Marshal(broadcast); err == nil {
			h.Bus.Publish(eventbus.SettingsChangedEvent{Settings: raw})
		}
	}
	return c.JSON(http.StatusOK, dto)
}
