package stock

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const yahooDefaultBase = "https://query1.finance.yahoo.com"

// YahooProvider implements Provider against Yahoo Finance.
//
// We use /v8/finance/chart/{symbol} rather than the older
// /v7/finance/quote because the v7 endpoint started requiring a crumb
// /cookie pair in early 2024 (returns 401 unauthenticated). The v8
// chart endpoint still serves anonymously and exposes both the
// current price and the previous close in its `meta` block, which is
// all we need for the widget + threshold trigger.
type YahooProvider struct {
	base string
	http *http.Client
}

// NewYahoo constructs a YahooProvider with sensible defaults.
func NewYahoo() *YahooProvider {
	return &YahooProvider{
		base: yahooDefaultBase,
		http: &http.Client{Timeout: 10 * time.Second},
	}
}

// WithBaseURL overrides the Yahoo base URL (used by tests with httptest).
func (p *YahooProvider) WithBaseURL(u string) *YahooProvider { p.base = u; return p }

// WithHTTPClient swaps the HTTP client (used by tests).
func (p *YahooProvider) WithHTTPClient(c *http.Client) *YahooProvider { p.http = c; return p }

// Quote fetches one ticker via the v8 chart endpoint. We ask for the
// shortest interval/range that still gives us a populated meta block;
// `interval=1d&range=1d` is sufficient.
func (p *YahooProvider) Quote(ctx context.Context, ticker string) (*Quote, error) {
	u := fmt.Sprintf("%s/v8/finance/chart/%s?interval=1d&range=1d", p.base, url.PathEscape(ticker))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	// Yahoo rejects empty / default Go UAs on some edges.
	req.Header.Set("User-Agent", "Mozilla/5.0 (zeno-stock-sensor/1.0)")
	req.Header.Set("Accept", "application/json")

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch chart: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("yahoo returned %d for %s", resp.StatusCode, ticker)
	}

	var raw yahooChartResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode chart: %w", err)
	}
	if raw.Chart.Error != nil {
		return nil, fmt.Errorf("yahoo error: %v", raw.Chart.Error)
	}
	if len(raw.Chart.Result) == 0 {
		return nil, fmt.Errorf("no result returned for %s", ticker)
	}
	meta := raw.Chart.Result[0].Meta
	if meta.RegularMarketPrice == 0 && meta.RegularMarketTime == 0 {
		return nil, fmt.Errorf("empty quote for %s (delisted or unknown)", ticker)
	}
	// chartPreviousClose is the prior session's close, which is what we want
	// for "today's move" — it doesn't shift intra-day. PreviousClose can lag
	// during pre-market on some symbols, so prefer chartPreviousClose.
	prev := meta.ChartPreviousClose
	if prev == 0 {
		prev = meta.PreviousClose
	}
	return &Quote{
		Ticker:      meta.Symbol,
		Price:       meta.RegularMarketPrice,
		PrevClose:   prev,
		Open:        meta.RegularMarketOpen,
		DayHigh:     meta.RegularMarketDayHigh,
		DayLow:      meta.RegularMarketDayLow,
		Volume:      meta.RegularMarketVolume,
		PostPrice:   meta.PostMarketPrice,
		PostChange:  meta.PostMarketChangePercent,
		MarketState: meta.MarketState,
		Currency:    meta.Currency,
		AsOf:        time.Unix(meta.RegularMarketTime, 0).UTC(),
	}, nil
}

type yahooChartResponse struct {
	Chart struct {
		Result []yahooChartResult `json:"result"`
		Error  any                `json:"error"`
	} `json:"chart"`
}

type yahooChartResult struct {
	Meta yahooChartMeta `json:"meta"`
}

type yahooChartMeta struct {
	Currency                string  `json:"currency"`
	Symbol                  string  `json:"symbol"`
	RegularMarketPrice      float64 `json:"regularMarketPrice"`
	ChartPreviousClose      float64 `json:"chartPreviousClose"`
	PreviousClose           float64 `json:"previousClose"`
	RegularMarketTime       int64   `json:"regularMarketTime"`
	RegularMarketOpen       float64 `json:"regularMarketOpen"`
	RegularMarketDayHigh    float64 `json:"regularMarketDayHigh"`
	RegularMarketDayLow     float64 `json:"regularMarketDayLow"`
	RegularMarketVolume     int64   `json:"regularMarketVolume"`
	PostMarketPrice         float64 `json:"postMarketPrice"`
	PostMarketChangePercent float64 `json:"postMarketChangePercent"`
	MarketState             string  `json:"marketState"`
}
