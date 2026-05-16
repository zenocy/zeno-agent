package stock

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const aaplChartFixture = `{
  "chart": {
    "result": [{
      "meta": {
        "currency": "USD",
        "symbol": "AAPL",
        "regularMarketPrice": 200.5,
        "chartPreviousClose": 198.0,
        "previousClose": 198.0,
        "regularMarketTime": 1746543210,
        "regularMarketOpen": 198.5,
        "regularMarketDayHigh": 201.2,
        "regularMarketDayLow": 197.9,
        "regularMarketVolume": 53210000,
        "postMarketPrice": 201.0,
        "postMarketChangePercent": 0.25,
        "marketState": "POST"
      },
      "timestamp": [1746543210],
      "indicators": {"quote": [{"close": [200.5]}]}
    }],
    "error": null
  }
}`

func TestYahooProvider_Quote_ParsesChartResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.True(t, strings.HasPrefix(r.URL.Path, "/v8/finance/chart/"), "must hit chart endpoint, got %q", r.URL.Path)
		require.Equal(t, "AAPL", strings.TrimPrefix(r.URL.Path, "/v8/finance/chart/"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(aaplChartFixture))
	}))
	defer srv.Close()

	p := NewYahoo().WithBaseURL(srv.URL)
	q, err := p.Quote(context.Background(), "AAPL")
	require.NoError(t, err)
	require.NotNil(t, q)
	require.Equal(t, "AAPL", q.Ticker)
	require.InDelta(t, 200.5, q.Price, 1e-6)
	require.InDelta(t, 198.0, q.PrevClose, 1e-6)
	require.Equal(t, "USD", q.Currency)
	require.Equal(t, int64(1746543210), q.AsOf.Unix())
	// Phase 4: OHLC + after-hours fields plumb through.
	require.InDelta(t, 198.5, q.Open, 1e-6)
	require.InDelta(t, 201.2, q.DayHigh, 1e-6)
	require.InDelta(t, 197.9, q.DayLow, 1e-6)
	require.Equal(t, int64(53210000), q.Volume)
	require.InDelta(t, 201.0, q.PostPrice, 1e-6)
	require.InDelta(t, 0.25, q.PostChange, 1e-6)
	require.Equal(t, "POST", q.MarketState)
}

func TestYahooProvider_Quote_NonOK_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthenticated"}`))
	}))
	defer srv.Close()

	p := NewYahoo().WithBaseURL(srv.URL)
	_, err := p.Quote(context.Background(), "AAPL")
	require.Error(t, err)
	require.Contains(t, err.Error(), "401")
}

func TestYahooProvider_Quote_FallsBackToPreviousClose(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"chart":{"result":[{"meta":{"symbol":"X","regularMarketPrice":10,"previousClose":9,"regularMarketTime":1}}],"error":null}}`))
	}))
	defer srv.Close()

	p := NewYahoo().WithBaseURL(srv.URL)
	q, err := p.Quote(context.Background(), "X")
	require.NoError(t, err)
	require.InDelta(t, 9.0, q.PrevClose, 1e-6, "should fall back to previousClose when chartPreviousClose is missing")
}
