package stock

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/sensor"
)

// SettingsSource is the narrow surface the sensor reads from
// settings.Service. Defined as an interface so tests can pass a fake
// without depending on the full Service.
type SettingsSource interface {
	StockConfig() (tickers []string, thresholdPct float64, alwaysPoll bool, ok bool)
}

// Sensor implements sensor.Sensor for stock quotes.
type Sensor struct {
	settings SettingsSource
	provider Provider
	reader   log.Reader
	writer   log.Writer
	log      *logrus.Entry
	now      func() time.Time
}

// New constructs a stock Sensor. The SettingsSource is consulted at
// every Sync, so ticker / threshold edits take effect without a
// restart (mirrors the weather pattern).
func New(s SettingsSource, p Provider, r log.Reader, w log.Writer, l *logrus.Entry) *Sensor {
	return &Sensor{
		settings: s,
		provider: p,
		reader:   r,
		writer:   w,
		log:      l,
		now:      time.Now,
	}
}

// WithNow overrides the clock (tests).
func (s *Sensor) WithNow(now func() time.Time) *Sensor { s.now = now; return s }

// Name implements sensor.Sensor.
func (s *Sensor) Name() string { return "stock" }

// snapshotPayload is the JSON shape of stock.snapshot events.
//
// Open / DayHigh / DayLow / Volume / PostPrice / PostChange /
// MarketState are Phase 4 additions; older snapshots from before the
// upgrade just leave them at zero values, so projections must guard
// with non-zero checks before rendering.
type snapshotPayload struct {
	Ticker      string    `json:"ticker"`
	Price       float64   `json:"price"`
	PrevClose   float64   `json:"prev_close"`
	Currency    string    `json:"currency,omitempty"`
	ChangePct   float64   `json:"change_pct"`
	Open        float64   `json:"open,omitempty"`
	DayHigh     float64   `json:"day_high,omitempty"`
	DayLow      float64   `json:"day_low,omitempty"`
	Volume      int64     `json:"volume,omitempty"`
	PostPrice   float64   `json:"post_price,omitempty"`
	PostChange  float64   `json:"post_change_pct,omitempty"`
	MarketState string    `json:"market_state,omitempty"`
	AsOf        time.Time `json:"as_of"`
}

// alertPayload is the JSON shape of stock.alert events (Phase 2).
type alertPayload struct {
	Ticker       string    `json:"ticker"`
	Price        float64   `json:"price"`
	PrevClose    float64   `json:"prev_close"`
	ChangePct    float64   `json:"change_pct"`
	ThresholdPct float64   `json:"threshold_pct"`
	Currency     string    `json:"currency,omitempty"`
	AsOf         time.Time `json:"as_of"`
}

// Sync polls each configured ticker and appends a stock.snapshot event
// when the price or as_of changed vs the latest prior snapshot. On the
// leading edge of a threshold breach (current move > threshold AND
// prior snapshot did NOT breach) it also appends a stock.alert event
// and publishes a SensorEventObservedEvent so the inject subscriber
// wakes up.
//
// One bad ticker (network error, delisted symbol) is logged and
// skipped — the rest of the watchlist still polls.
func (s *Sensor) Sync(ctx context.Context) error {
	tickers, thresholdPct, alwaysPoll, ok := s.settings.StockConfig()
	if !ok {
		if s.log != nil {
			s.log.Debug("stock: skipping sync, no tickers configured")
		}
		return nil
	}
	if !alwaysPoll && !inMarketWindow(s.now()) {
		if s.log != nil {
			s.log.Debug("stock: skipping sync, outside US market window")
		}
		return nil
	}

	prior, err := s.lastSnapshots(ctx)
	if err != nil {
		return fmt.Errorf("read history: %w", err)
	}

	written, alerted, skipped := 0, 0, 0
	var firstErr error
	for _, ticker := range tickers {
		if err := ctx.Err(); err != nil {
			return err
		}
		quote, qerr := s.provider.Quote(ctx, ticker)
		if qerr != nil {
			skipped++
			if firstErr == nil {
				firstErr = qerr
			}
			if s.log != nil {
				s.log.WithError(qerr).WithField("ticker", ticker).
					Warn("stock: quote fetch failed; skipping ticker")
			}
			continue
		}

		payload := snapshotPayload{
			Ticker:      quote.Ticker,
			Price:       quote.Price,
			PrevClose:   quote.PrevClose,
			Currency:    quote.Currency,
			ChangePct:   changePct(quote.Price, quote.PrevClose),
			Open:        quote.Open,
			DayHigh:     quote.DayHigh,
			DayLow:      quote.DayLow,
			Volume:      quote.Volume,
			PostPrice:   quote.PostPrice,
			PostChange:  quote.PostChange,
			MarketState: quote.MarketState,
			AsOf:        quote.AsOf,
		}

		prev, hadPrev := prior[quote.Ticker]
		// Dedupe on price + prev_close: those two fields determine the
		// derived change_pct and represent the only "did anything
		// actually move?" state that matters. Yahoo's regularMarketTime
		// (carried as AsOf) ticks every poll during market hours even
		// when the price is flat, so including it in the comparison
		// would write a redundant snapshot per cron tick per ticker.
		if hadPrev && prev.Price == payload.Price && prev.PrevClose == payload.PrevClose {
			continue
		}

		snapEv, err := s.writer.Append(ctx, log.KindStockSnapshot, s.Name(), payload)
		if err != nil {
			return fmt.Errorf("append stock.snapshot: %w", err)
		}
		written++
		// Wakes the projection publisher so the StockUpdatedEvent (full
		// StockView) lands on the SSE wire and the UI's stock widget
		// updates without a poll. Bus-internal SensorEventObservedEvent
		// itself is filtered out of the wire.
		sensor.PublishObserved(ctx, log.KindStockSnapshot, snapEv.ID, nil)

		if thresholdPct > 0 && breached(payload.ChangePct, thresholdPct) {
			priorBreached := hadPrev && breached(prev.ChangePct, thresholdPct)
			if !priorBreached {
				alert := alertPayload{
					Ticker:       payload.Ticker,
					Price:        payload.Price,
					PrevClose:    payload.PrevClose,
					ChangePct:    payload.ChangePct,
					ThresholdPct: thresholdPct,
					Currency:     payload.Currency,
					AsOf:         payload.AsOf,
				}
				if _, err := s.writer.Append(ctx, log.KindStockAlert, s.Name(), alert); err != nil {
					return fmt.Errorf("append stock.alert: %w", err)
				}
				alerted++
				// V2.4 reactive trigger: publish strictly AFTER successful append.
				evidenceID := fmt.Sprintf("%s:%d", payload.Ticker, payload.AsOf.Unix())
				sensor.PublishObserved(ctx, "stock.threshold_breach", evidenceID, map[string]any{
					"ticker":        alert.Ticker,
					"price":         alert.Price,
					"prev_close":    alert.PrevClose,
					"change_pct":    alert.ChangePct,
					"threshold_pct": alert.ThresholdPct,
					"as_of":         alert.AsOf,
				})
			}
		}
	}

	if s.log != nil {
		s.log.WithFields(logrus.Fields{
			"tickers": len(tickers),
			"written": written,
			"alerted": alerted,
			"skipped": skipped,
		}).Info("stock: sync complete")
	}

	if firstErr != nil && written == 0 {
		return firstErr
	}
	return nil
}

// lastSnapshots folds the recent stock.snapshot history into the latest
// payload per ticker. Same pattern as caldav.Sensor.lastSeen.
func (s *Sensor) lastSnapshots(ctx context.Context) (map[string]snapshotPayload, error) {
	events, err := s.reader.ByKind(ctx, log.KindStockSnapshot)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].TS.Before(events[j].TS) })

	out := make(map[string]snapshotPayload, len(events))
	for _, e := range events {
		var p snapshotPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		out[p.Ticker] = p
	}
	return out, nil
}

func changePct(price, prevClose float64) float64 {
	if prevClose == 0 {
		return 0
	}
	return (price/prevClose - 1) * 100
}

func breached(changePct, thresholdPct float64) bool {
	return math.Abs(changePct) > thresholdPct
}
