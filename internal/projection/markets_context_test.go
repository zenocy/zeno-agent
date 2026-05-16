package projection

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/log/logtest"
)

func makeStockSnapshotFull(ticker string, price, prev, open float64, changePct float64, asOf time.Time) map[string]any {
	return map[string]any{
		"ticker":     ticker,
		"price":      price,
		"prev_close": prev,
		"open":       open,
		"change_pct": changePct,
		"currency":   "USD",
		"as_of":      asOf,
	}
}

func TestMarketsContext_NoTickers_ReturnsNil(t *testing.T) {
	mem := logtest.NewMemReader()
	now := time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC)

	out, err := MarketsContext{
		Cfg:     Config{Now: func() time.Time { return now }},
		Tickers: tickerStub{ok: false},
	}.Compute(context.Background(), mem)

	require.NoError(t, err)
	require.Nil(t, out)
}

func TestMarketsContext_AllCalm_ReturnsNil(t *testing.T) {
	now := time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC)
	mem := logtest.NewMemReader()
	mem.AppendEvent(logtest.MakeEvent(log.KindStockSnapshot, "stock", now,
		makeStockSnapshotFull("AAPL", 200.5, 200.0, 200.1, 0.25, now)))
	mem.AppendEvent(logtest.MakeEvent(log.KindStockSnapshot, "stock", now,
		makeStockSnapshotFull("GOOGL", 150.05, 150.0, 150.0, 0.03, now)))

	out, err := MarketsContext{
		Cfg:     Config{Now: func() time.Time { return now }},
		Tickers: tickerStub{tickers: []string{"AAPL", "GOOGL"}, ok: true},
	}.Compute(context.Background(), mem)

	require.NoError(t, err)
	require.Nil(t, out, "all tickers under the gap threshold → nil (quiet morning)")
}

func TestMarketsContext_BreachWakesIt(t *testing.T) {
	now := time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC)
	mem := logtest.NewMemReader()
	// Snapshot is calm.
	mem.AppendEvent(logtest.MakeEvent(log.KindStockSnapshot, "stock", now,
		makeStockSnapshotFull("AAPL", 200.5, 200.0, 200.0, 0.25, now)))
	// But there was a breach event in the lookback window.
	mem.AppendEvent(alertEvent("alert-aapl", "AAPL", 5.25, 3, now.Add(-1*time.Hour)))

	out, err := MarketsContext{
		Cfg:     Config{Now: func() time.Time { return now }},
		Tickers: tickerStub{tickers: []string{"AAPL"}, ok: true},
	}.Compute(context.Background(), mem)

	require.NoError(t, err)
	require.NotNil(t, out)
	require.Len(t, out.Movements, 1)
	require.True(t, out.HasBreach)
	require.True(t, out.Movements[0].Breached)
	require.False(t, out.Movements[0].BreachAt.IsZero())
}

func TestMarketsContext_GapAloneWakesIt(t *testing.T) {
	now := time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC)
	mem := logtest.NewMemReader()
	// 2.5% move, no breach event. Default gap is 1%.
	mem.AppendEvent(logtest.MakeEvent(log.KindStockSnapshot, "stock", now,
		makeStockSnapshotFull("AAPL", 205.0, 200.0, 200.0, 2.5, now)))

	out, err := MarketsContext{
		Cfg:     Config{Now: func() time.Time { return now }},
		Tickers: tickerStub{tickers: []string{"AAPL"}, ok: true},
	}.Compute(context.Background(), mem)

	require.NoError(t, err)
	require.NotNil(t, out)
	require.Len(t, out.Movements, 1)
	require.False(t, out.HasBreach, "gap-only mover doesn't set HasBreach")
	require.False(t, out.Movements[0].Breached)
}

func TestMarketsContext_SortsByAbsChangeDesc(t *testing.T) {
	now := time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC)
	mem := logtest.NewMemReader()
	mem.AppendEvent(logtest.MakeEvent(log.KindStockSnapshot, "stock", now,
		makeStockSnapshotFull("AAPL", 202.0, 200.0, 200.0, 1.0, now))) // edge
	mem.AppendEvent(logtest.MakeEvent(log.KindStockSnapshot, "stock", now,
		makeStockSnapshotFull("TSLA", 95.0, 100.0, 100.0, -5.0, now))) // biggest
	mem.AppendEvent(logtest.MakeEvent(log.KindStockSnapshot, "stock", now,
		makeStockSnapshotFull("MSFT", 408.0, 400.0, 400.0, 2.0, now))) // middle

	out, err := MarketsContext{
		Cfg:     Config{Now: func() time.Time { return now }},
		Tickers: tickerStub{tickers: []string{"AAPL", "TSLA", "MSFT"}, ok: true},
		GapPct:  1.0,
	}.Compute(context.Background(), mem)

	require.NoError(t, err)
	require.Len(t, out.Movements, 3)
	require.Equal(t, "TSLA", out.Movements[0].Ticker, "biggest absolute mover first")
	require.Equal(t, "MSFT", out.Movements[1].Ticker)
	require.Equal(t, "AAPL", out.Movements[2].Ticker)
}

func TestMarketsContext_OldBreachIgnored(t *testing.T) {
	now := time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC)
	mem := logtest.NewMemReader()
	// Calm snapshot.
	mem.AppendEvent(logtest.MakeEvent(log.KindStockSnapshot, "stock", now,
		makeStockSnapshotFull("AAPL", 200.5, 200.0, 200.0, 0.25, now)))
	// Old breach (48h ago) — outside the 24h default lookback.
	mem.AppendEvent(alertEvent("alert-aapl-old", "AAPL", 5.25, 3, now.Add(-48*time.Hour)))

	out, err := MarketsContext{
		Cfg:     Config{Now: func() time.Time { return now }},
		Tickers: tickerStub{tickers: []string{"AAPL"}, ok: true},
	}.Compute(context.Background(), mem)

	require.NoError(t, err)
	require.Nil(t, out, "breach outside lookback + calm snapshot → no card")
}

func TestMarketsContext_TickerWithoutSnapshotSkipped(t *testing.T) {
	now := time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC)
	mem := logtest.NewMemReader()
	mem.AppendEvent(logtest.MakeEvent(log.KindStockSnapshot, "stock", now,
		makeStockSnapshotFull("AAPL", 205.0, 200.0, 200.0, 2.5, now)))

	out, err := MarketsContext{
		Cfg:     Config{Now: func() time.Time { return now }},
		Tickers: tickerStub{tickers: []string{"AAPL", "BRANDNEW"}, ok: true},
	}.Compute(context.Background(), mem)

	require.NoError(t, err)
	require.NotNil(t, out)
	require.Len(t, out.Movements, 1, "ticker without snapshot just gets skipped, not zero-padded")
}
