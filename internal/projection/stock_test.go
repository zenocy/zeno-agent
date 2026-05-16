package projection

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/log/logtest"
)

type tickerStub struct {
	tickers []string
	ok      bool
}

func (t tickerStub) StockConfig() ([]string, float64, bool, bool) {
	return t.tickers, 0, false, t.ok
}

func makeStockPayload(ticker string, price, prev float64, asOf time.Time) map[string]any {
	return map[string]any{
		"ticker":     ticker,
		"price":      price,
		"prev_close": prev,
		"currency":   "USD",
		"change_pct": (price/prev - 1) * 100,
		"as_of":      asOf,
	}
}

func TestStockProjection_NoTickers_ReturnsNil(t *testing.T) {
	mem := logtest.NewMemReader()
	now := time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC)

	view, err := Stock{
		Cfg:     Config{Now: func() time.Time { return now }},
		Tickers: tickerStub{ok: false},
	}.Compute(context.Background(), mem)

	require.NoError(t, err)
	require.Nil(t, view, "no tickers configured -> nil view")
}

func TestStockProjection_LatestPerTicker(t *testing.T) {
	now := time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC)
	older := now.Add(-1 * time.Hour)
	newer := now.Add(-5 * time.Minute)

	mem := logtest.NewMemReader()
	mem.AppendEvent(logtest.MakeEvent(log.KindStockSnapshot, "stock", older, makeStockPayload("AAPL", 199, 195, older)))
	mem.AppendEvent(logtest.MakeEvent(log.KindStockSnapshot, "stock", newer, makeStockPayload("AAPL", 201, 195, newer)))
	mem.AppendEvent(logtest.MakeEvent(log.KindStockSnapshot, "stock", newer, makeStockPayload("GOOGL", 150, 152, newer)))

	view, err := Stock{
		Cfg:     Config{Now: func() time.Time { return now }},
		Tickers: tickerStub{tickers: []string{"AAPL", "GOOGL"}, ok: true},
	}.Compute(context.Background(), mem)

	require.NoError(t, err)
	require.NotNil(t, view)
	require.Len(t, view.Quotes, 2)

	byTicker := map[string]StockQuote{}
	for _, q := range view.Quotes {
		byTicker[q.Ticker] = q
	}
	require.InDelta(t, 201.0, byTicker["AAPL"].Price, 0.001, "latest snapshot wins")
	require.InDelta(t, 150.0, byTicker["GOOGL"].Price, 0.001)
}

func TestStockProjection_PreservesUserOrder(t *testing.T) {
	now := time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC)
	mem := logtest.NewMemReader()
	mem.AppendEvent(logtest.MakeEvent(log.KindStockSnapshot, "stock", now, makeStockPayload("MSFT", 400, 395, now)))
	mem.AppendEvent(logtest.MakeEvent(log.KindStockSnapshot, "stock", now, makeStockPayload("AAPL", 200, 195, now)))

	view, err := Stock{
		Cfg:     Config{Now: func() time.Time { return now }},
		Tickers: tickerStub{tickers: []string{"AAPL", "MSFT"}, ok: true},
	}.Compute(context.Background(), mem)

	require.NoError(t, err)
	require.Len(t, view.Quotes, 2)
	require.Equal(t, "AAPL", view.Quotes[0].Ticker, "user-listed order preserved")
	require.Equal(t, "MSFT", view.Quotes[1].Ticker)
}

func TestStockProjection_StaleSnapshotFlagged(t *testing.T) {
	now := time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC)
	stale := now.Add(-48 * time.Hour)

	mem := logtest.NewMemReader()
	mem.AppendEvent(logtest.MakeEvent(log.KindStockSnapshot, "stock", stale, makeStockPayload("AAPL", 199, 195, stale)))

	view, err := Stock{
		Cfg:     Config{Now: func() time.Time { return now }},
		Tickers: tickerStub{tickers: []string{"AAPL"}, ok: true},
	}.Compute(context.Background(), mem)

	require.NoError(t, err)
	require.NotNil(t, view)
	require.Len(t, view.Quotes, 1)
	require.True(t, view.Quotes[0].Stale, "snapshot older than staleStockAfter must be flagged stale")
}

func makeAlertPayload(ticker string, changePct, threshold float64, asOf time.Time) map[string]any {
	return map[string]any{
		"ticker":        ticker,
		"price":         100 + changePct,
		"prev_close":    100.0,
		"change_pct":    changePct,
		"threshold_pct": threshold,
		"currency":      "USD",
		"as_of":         asOf,
	}
}

// alertEvent builds a stock.alert log event with an explicit ID so
// the EvidenceID assertion in RecentStockBreaches has something to
// match. Production gormStore generates a UUID on Append; the
// in-memory test helper leaves it blank by default.
func alertEvent(id, ticker string, changePct, threshold float64, ts time.Time) log.Event {
	ev := logtest.MakeEvent(log.KindStockAlert, "stock", ts, makeAlertPayload(ticker, changePct, threshold, ts))
	ev.ID = id
	return ev
}

func TestRecentStockBreaches_WithinHorizon(t *testing.T) {
	now := time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC)
	mem := logtest.NewMemReader()
	mem.AppendEvent(alertEvent("alert-1", "AAPL", 5.2, 3, now.Add(-5*time.Minute)))
	mem.AppendEvent(alertEvent("alert-2", "TSLA", -4.0, 3, now.Add(-2*time.Minute)))

	out, err := RecentStockBreaches{
		Cfg:     Config{Now: func() time.Time { return now }},
		Horizon: 15 * time.Minute,
	}.Compute(context.Background(), mem)

	require.NoError(t, err)
	require.Len(t, out, 2)
	require.Equal(t, "TSLA", out[0].Ticker, "newest-first ordering")
	require.Equal(t, "AAPL", out[1].Ticker)
	require.Equal(t, "alert-2", out[0].EvidenceID, "evidence_id is the event-log row ID")
}

func TestRecentStockBreaches_DropsAged(t *testing.T) {
	now := time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC)
	mem := logtest.NewMemReader()
	mem.AppendEvent(logtest.MakeEvent(log.KindStockAlert, "stock", now.Add(-30*time.Minute),
		makeAlertPayload("AAPL", 5.2, 3, now.Add(-30*time.Minute))))
	mem.AppendEvent(logtest.MakeEvent(log.KindStockAlert, "stock", now.Add(-2*time.Minute),
		makeAlertPayload("TSLA", -4.0, 3, now.Add(-2*time.Minute))))

	out, err := RecentStockBreaches{
		Cfg:     Config{Now: func() time.Time { return now }},
		Horizon: 15 * time.Minute,
	}.Compute(context.Background(), mem)

	require.NoError(t, err)
	require.Len(t, out, 1, "only the in-horizon breach is returned")
	require.Equal(t, "TSLA", out[0].Ticker)
}

func TestRecentStockBreaches_DefaultHorizon(t *testing.T) {
	now := time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC)
	mem := logtest.NewMemReader()
	mem.AppendEvent(logtest.MakeEvent(log.KindStockAlert, "stock", now.Add(-1*time.Minute),
		makeAlertPayload("AAPL", 5.2, 3, now.Add(-1*time.Minute))))

	out, err := RecentStockBreaches{
		Cfg: Config{Now: func() time.Time { return now }},
		// Horizon zero → defaults to DefaultStockBreachHorizon (15min)
	}.Compute(context.Background(), mem)

	require.NoError(t, err)
	require.Len(t, out, 1)
}

func TestStockProjection_TickerWithNoSnapshotOmitted(t *testing.T) {
	now := time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC)
	mem := logtest.NewMemReader()
	mem.AppendEvent(logtest.MakeEvent(log.KindStockSnapshot, "stock", now, makeStockPayload("AAPL", 200, 195, now)))

	view, err := Stock{
		Cfg:     Config{Now: func() time.Time { return now }},
		Tickers: tickerStub{tickers: []string{"AAPL", "BRANDNEW"}, ok: true},
	}.Compute(context.Background(), mem)

	require.NoError(t, err)
	require.NotNil(t, view)
	require.Len(t, view.Quotes, 1, "ticker with no snapshot yet is omitted, not zero-padded")
	require.Equal(t, "AAPL", view.Quotes[0].Ticker)
}
