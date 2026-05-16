// Package stock is the market-data sensor. It polls a configured list of
// tickers from a Provider and emits stock.snapshot events for changes,
// plus stock.alert events on the leading edge of a percent-move
// threshold breach (the inject pipeline subscribes to those).
//
// Provider abstracts the data source so the default (Yahoo Finance,
// no auth) can be swapped for a key-based provider without touching
// the sensor or projection layers.
package stock

import (
	"context"
	"time"
)

// Quote is a single point-in-time market quote for one ticker.
//
// Phase 4 added the OHLC + volume + after-hours fields; older callers
// that only read Price / PrevClose still work since the new fields
// stay at their zero values when absent from the upstream response.
type Quote struct {
	Ticker    string
	Price     float64
	PrevClose float64
	// Today's session OHLC. Volume is in shares.
	Open    float64
	DayHigh float64
	DayLow  float64
	Volume  int64
	// After-hours quote when the regular session is closed.
	// PostPrice is 0 when not available (e.g. mid-session, weekends).
	PostPrice  float64
	PostChange float64 // PostMarketChangePercent — already a percent
	// MarketState mirrors Yahoo's marketState field: REGULAR, POST,
	// PRE, CLOSED, PREPRE, POSTPOST. Empty when the upstream response
	// omits it.
	MarketState string
	Currency    string
	AsOf        time.Time
}

// Provider returns the latest Quote for one ticker. Implementations
// should treat transient errors as recoverable and return them; the
// sensor logs and skips per-ticker failures so one bad symbol doesn't
// abort the whole sync.
type Provider interface {
	Quote(ctx context.Context, ticker string) (*Quote, error)
}
