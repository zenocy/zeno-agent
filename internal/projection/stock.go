package projection

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/zenocy/zeno-v2/internal/log"
)

// staleStockAfter is how old a stock.snapshot can be before the
// projection flags it as stale. 24h is generous: weekend / holiday
// closes will keep the most recent close visible until the next
// trading day, but a sensor that's been silent longer than a day is a
// real outage — surface it to the UI.
const staleStockAfter = 24 * time.Hour

// DefaultStockBreachHorizon is the look-back window for
// RecentStockBreaches. The inject detector reads breaches inside this
// window; older alerts are stale awareness and should not wake the
// reactive synth.
const DefaultStockBreachHorizon = 15 * time.Minute

// DefaultMarketsContextGapPct is the threshold a ticker's day move
// must clear to qualify as "interesting" for the morning markets
// card. Anything below this is noise; the card stays quiet.
const DefaultMarketsContextGapPct = 1.0

// DefaultMarketsContextLookback is the look-back window for breach
// events feeding the morning markets context — last 24h captures
// "anything happened since I last looked".
const DefaultMarketsContextLookback = 24 * time.Hour

// TickerSource is the narrow surface the Stock projection reads to
// know which tickers the user is watching. Implemented by
// settings.Service.
type TickerSource interface {
	StockConfig() (tickers []string, thresholdPct float64, alwaysPoll bool, ok bool)
}

// Stock projects the latest stock.snapshot per configured ticker into
// the UI-shaped view the right-rail widget consumes. Returns nil when
// no tickers are configured (the widget renders an empty state).
type Stock struct {
	Cfg     Config
	Tickers TickerSource
}

// Name returns the projection identifier.
func (p Stock) Name() string { return "stock" }

// rawSnapshot mirrors the payload written by the stock sensor.
type rawSnapshot struct {
	Ticker      string    `json:"ticker"`
	Price       float64   `json:"price"`
	PrevClose   float64   `json:"prev_close"`
	Currency    string    `json:"currency"`
	ChangePct   float64   `json:"change_pct"`
	AsOf        time.Time `json:"as_of"`
	Open        float64   `json:"open"`
	DayHigh     float64   `json:"day_high"`
	DayLow      float64   `json:"day_low"`
	Volume      int64     `json:"volume"`
	PostPrice   float64   `json:"post_price"`
	PostChange  float64   `json:"post_change_pct"`
	MarketState string    `json:"market_state"`
}

// minIntradayPoints is the threshold below which we fall back to "the
// last N points regardless of date" for the sparkline. After a fresh
// daemon restart on a Tuesday morning, today's data is too sparse to
// draw — borrowing from yesterday's close keeps the widget useful.
const minIntradayPoints = 6
const maxSeriesPoints = 60

// Compute reads the latest stock.snapshot per configured ticker and
// returns a StockView. Tickers without any snapshot yet are omitted
// (the sensor hasn't run for them yet).
func (p Stock) Compute(ctx context.Context, r log.Reader) (*StockView, error) {
	if p.Tickers == nil {
		return nil, nil
	}
	tickers, _, _, ok := p.Tickers.StockConfig()
	if !ok {
		return nil, nil
	}

	events, err := r.ByKind(ctx, log.KindStockSnapshot)
	if err != nil {
		return nil, fmt.Errorf("read stock snapshots: %w", err)
	}

	// ByKind returns oldest-first. Build a per-ticker timeline so the
	// last write wins for "latest" while we still have access to the
	// full history for the sparkline series.
	timeline := make(map[string][]struct {
		ev  log.Event
		raw rawSnapshot
	}, len(tickers))
	for _, e := range events {
		var raw rawSnapshot
		if err := json.Unmarshal(e.Payload, &raw); err != nil {
			continue
		}
		timeline[raw.Ticker] = append(timeline[raw.Ticker], struct {
			ev  log.Event
			raw rawSnapshot
		}{ev: e, raw: raw})
	}

	now := p.Cfg.now()
	tz := p.Cfg.tz()
	todayStart := startOfDayLocal(now, tz)
	var newest time.Time
	out := make([]StockQuote, 0, len(tickers))
	for _, ticker := range tickers {
		hist := timeline[ticker]
		if len(hist) == 0 {
			continue
		}
		hit := hist[len(hist)-1]
		stale := now.Sub(hit.ev.TS) > staleStockAfter
		out = append(out, StockQuote{
			Ticker:      hit.raw.Ticker,
			Price:       hit.raw.Price,
			PrevClose:   hit.raw.PrevClose,
			Currency:    hit.raw.Currency,
			ChangePct:   hit.raw.ChangePct,
			AsOf:        hit.raw.AsOf,
			Stale:       stale,
			Open:        hit.raw.Open,
			DayHigh:     hit.raw.DayHigh,
			DayLow:      hit.raw.DayLow,
			Volume:      hit.raw.Volume,
			PostPrice:   hit.raw.PostPrice,
			PostChange:  hit.raw.PostChange,
			MarketState: hit.raw.MarketState,
			Series:      buildSeries(hist, todayStart),
		})
		if hit.ev.TS.After(newest) {
			newest = hit.ev.TS
		}
	}

	return &StockView{
		AsOf:   newest,
		Quotes: out,
	}, nil
}

// buildSeries returns up to maxSeriesPoints intraday ticks for the
// sparkline. Prefers today's points; falls back to the last N
// regardless of date when today is too sparse (post-restart, weekend,
// market just opened).
func buildSeries(hist []struct {
	ev  log.Event
	raw rawSnapshot
}, todayStart time.Time) []StockTick {
	if len(hist) == 0 {
		return nil
	}
	todays := make([]StockTick, 0, len(hist))
	for _, h := range hist {
		if !h.ev.TS.Before(todayStart) {
			todays = append(todays, StockTick{AsOf: h.raw.AsOf, Price: h.raw.Price})
		}
	}
	if len(todays) >= minIntradayPoints {
		return capSeries(todays)
	}
	// Fall back to the trailing N regardless of date.
	all := make([]StockTick, len(hist))
	for i, h := range hist {
		all[i] = StockTick{AsOf: h.raw.AsOf, Price: h.raw.Price}
	}
	return capSeries(all)
}

func capSeries(s []StockTick) []StockTick {
	if len(s) <= maxSeriesPoints {
		return s
	}
	return s[len(s)-maxSeriesPoints:]
}

func startOfDayLocal(now time.Time, tz *time.Location) time.Time {
	if tz == nil {
		tz = time.UTC
	}
	local := now.In(tz)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, tz)
}

// rawAlert mirrors the payload written by the stock sensor on the
// leading edge of a threshold breach.
type rawAlert struct {
	Ticker       string    `json:"ticker"`
	Price        float64   `json:"price"`
	PrevClose    float64   `json:"prev_close"`
	ChangePct    float64   `json:"change_pct"`
	ThresholdPct float64   `json:"threshold_pct"`
	Currency     string    `json:"currency"`
	AsOf         time.Time `json:"as_of"`
}

// RecentStockBreaches projects the recent stock.alert events into the
// shape the inject detector reads. Only events with TS >= now-Horizon
// are returned, sorted newest-first. Per-ticker dedupe: a sustained
// breach already collapses to one alert per leading edge in the
// sensor; this projection just trims by horizon.
type RecentStockBreaches struct {
	Cfg     Config
	Horizon time.Duration
}

// Name returns the projection identifier.
func (p RecentStockBreaches) Name() string { return "recent_stock_breaches" }

// Compute reads stock.alert events within the horizon and returns
// them newest-first.
func (p RecentStockBreaches) Compute(ctx context.Context, r log.Reader) ([]StockBreach, error) {
	horizon := p.Horizon
	if horizon <= 0 {
		horizon = DefaultStockBreachHorizon
	}
	now := p.Cfg.now()
	cutoff := now.Add(-horizon)

	events, err := r.ByKind(ctx, log.KindStockAlert)
	if err != nil {
		return nil, fmt.Errorf("read stock alerts: %w", err)
	}
	out := make([]StockBreach, 0, len(events))
	for _, e := range events {
		if e.TS.Before(cutoff) {
			continue
		}
		var a rawAlert
		if err := json.Unmarshal(e.Payload, &a); err != nil {
			continue
		}
		out = append(out, StockBreach{
			Ticker:       a.Ticker,
			Price:        a.Price,
			PrevClose:    a.PrevClose,
			ChangePct:    a.ChangePct,
			ThresholdPct: a.ThresholdPct,
			Currency:     a.Currency,
			AsOf:         a.AsOf,
			EvidenceID:   e.ID,
		})
	}
	// Newest first — ByKind returns oldest-first; reverse.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// MarketsContext is the morning cards loop's view of the watchlist.
// Compute returns nil when no ticker is "interesting" today — the
// cards prompt then omits the Markets section and the model won't
// emit a markets card.
//
// A ticker is interesting when:
//   - |ChangePct| >= GapPct (default 1%), OR
//   - it had a stock.alert in the lookback window (default 24h)
//
// Tickers without any snapshot at all are silently skipped (sensor
// hasn't polled yet).
type MarketsContext struct {
	Cfg      Config
	Tickers  TickerSource
	GapPct   float64
	Lookback time.Duration
}

// MarketsSummary is the result of MarketsContext.Compute. Movements is
// the ordered list of interesting tickers, sorted by absolute change
// descending. HasBreach is true when at least one movement was
// triggered by a stock.alert (vs. just a quiet gap).
type MarketsSummary struct {
	AsOf      time.Time        `json:"as_of"`
	Movements []TickerMovement `json:"movements"`
	HasBreach bool             `json:"has_breach"`
}

// TickerMovement is one row in MarketsSummary.Movements.
type TickerMovement struct {
	Ticker     string    `json:"ticker"`
	Price      float64   `json:"price"`
	PrevClose  float64   `json:"prev_close"`
	Open       float64   `json:"open"`
	ChangePct  float64   `json:"change_pct"`
	GapPct     float64   `json:"gap_pct"` // (open - prev_close) / prev_close * 100, 0 when open is unknown
	Breached   bool      `json:"breached"`
	BreachAt   time.Time `json:"breach_at,omitempty"`
	AsOf       time.Time `json:"as_of"`
	Currency   string    `json:"currency,omitempty"`
}

// Name returns the projection identifier.
func (p MarketsContext) Name() string { return "markets_context" }

// Compute produces the morning markets summary. Returns nil when the
// watchlist is empty OR no ticker qualifies as interesting.
func (p MarketsContext) Compute(ctx context.Context, r log.Reader) (*MarketsSummary, error) {
	if p.Tickers == nil {
		return nil, nil
	}
	tickers, _, _, ok := p.Tickers.StockConfig()
	if !ok {
		return nil, nil
	}

	gap := p.GapPct
	if gap <= 0 {
		gap = DefaultMarketsContextGapPct
	}
	lookback := p.Lookback
	if lookback <= 0 {
		lookback = DefaultMarketsContextLookback
	}
	now := p.Cfg.now()
	cutoff := now.Add(-lookback)

	// Latest snapshot per ticker.
	snapEvents, err := r.ByKind(ctx, log.KindStockSnapshot)
	if err != nil {
		return nil, fmt.Errorf("read stock snapshots: %w", err)
	}
	latestSnap := make(map[string]rawSnapshot, len(tickers))
	for _, e := range snapEvents {
		var raw rawSnapshot
		if err := json.Unmarshal(e.Payload, &raw); err != nil {
			continue
		}
		latestSnap[raw.Ticker] = raw
	}

	// Most recent breach per ticker within lookback.
	alertEvents, err := r.ByKind(ctx, log.KindStockAlert)
	if err != nil {
		return nil, fmt.Errorf("read stock alerts: %w", err)
	}
	latestBreach := make(map[string]time.Time, len(tickers))
	for _, e := range alertEvents {
		if e.TS.Before(cutoff) {
			continue
		}
		var a rawAlert
		if err := json.Unmarshal(e.Payload, &a); err != nil {
			continue
		}
		// ByKind is oldest-first; the last assignment wins.
		latestBreach[a.Ticker] = e.TS
	}

	movements := make([]TickerMovement, 0, len(tickers))
	hasBreach := false
	var newest time.Time
	for _, ticker := range tickers {
		snap, hasSnap := latestSnap[ticker]
		if !hasSnap {
			continue
		}
		breachAt, breached := latestBreach[ticker]
		gapPct := 0.0
		if snap.Open != 0 && snap.PrevClose != 0 {
			gapPct = (snap.Open - snap.PrevClose) / snap.PrevClose * 100
		}
		interesting := breached || (snap.ChangePct >= gap || snap.ChangePct <= -gap)
		if !interesting {
			continue
		}
		movements = append(movements, TickerMovement{
			Ticker:    snap.Ticker,
			Price:     snap.Price,
			PrevClose: snap.PrevClose,
			Open:      snap.Open,
			ChangePct: snap.ChangePct,
			GapPct:    gapPct,
			Breached:  breached,
			BreachAt:  breachAt,
			AsOf:      snap.AsOf,
			Currency:  snap.Currency,
		})
		if breached {
			hasBreach = true
		}
		if snap.AsOf.After(newest) {
			newest = snap.AsOf
		}
	}
	if len(movements) == 0 {
		return nil, nil
	}
	// Sort by absolute change pct descending.
	sort.Slice(movements, func(i, j int) bool {
		return absFloat(movements[i].ChangePct) > absFloat(movements[j].ChangePct)
	})
	return &MarketsSummary{
		AsOf:      newest,
		Movements: movements,
		HasBreach: hasBreach,
	}, nil
}

func absFloat(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
