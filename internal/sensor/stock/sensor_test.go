package stock

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/log/logtest"
)

type fakeSettings struct {
	tickers    []string
	threshold  float64
	alwaysPoll bool
	ok         bool
}

func (f fakeSettings) StockConfig() ([]string, float64, bool, bool) {
	return f.tickers, f.threshold, f.alwaysPoll, f.ok
}

type fakeProvider struct {
	quotes map[string]*Quote
	errs   map[string]error
	calls  int
}

func (f *fakeProvider) Quote(_ context.Context, ticker string) (*Quote, error) {
	f.calls++
	if err, ok := f.errs[ticker]; ok {
		return nil, err
	}
	q, ok := f.quotes[ticker]
	if !ok {
		return nil, errors.New("unknown ticker")
	}
	return q, nil
}

// inWindowClock pins a Tuesday 14:00 UTC time so tests don't
// accidentally skip on wall-clock weekends or off-hours runs. Cases
// that specifically exercise the market-hours gate override via
// WithNow on the returned sensor.
var inWindowClock = time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC)

func newSensor(t *testing.T, settings fakeSettings, provider *fakeProvider, mem *logtest.MemReader) *Sensor {
	t.Helper()
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	return New(settings, provider, mem, mem, logger.WithField("c", "stock-test")).
		WithNow(func() time.Time { return inWindowClock })
}

func TestSync_NoTickers_NoOp(t *testing.T) {
	mem := logtest.NewMemReader()
	provider := &fakeProvider{}
	s := newSensor(t, fakeSettings{ok: false}, provider, mem)

	require.NoError(t, s.Sync(context.Background()))
	require.Empty(t, mem.Events(), "no events when no tickers")
	require.Equal(t, 0, provider.calls, "provider not called")
}

func TestSync_FirstPoll_Writes(t *testing.T) {
	asOf := time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC)
	provider := &fakeProvider{quotes: map[string]*Quote{
		"AAPL":  {Ticker: "AAPL", Price: 200, PrevClose: 195, Currency: "USD", AsOf: asOf},
		"GOOGL": {Ticker: "GOOGL", Price: 150, PrevClose: 152, Currency: "USD", AsOf: asOf},
	}}
	mem := logtest.NewMemReader()
	s := newSensor(t, fakeSettings{tickers: []string{"AAPL", "GOOGL"}, threshold: 0, ok: true}, provider, mem)

	require.NoError(t, s.Sync(context.Background()))

	events, err := mem.ByKind(context.Background(), log.KindStockSnapshot)
	require.NoError(t, err)
	require.Len(t, events, 2, "one snapshot per ticker on first poll")
}

func TestSync_SecondPoll_DedupesWhenUnchanged(t *testing.T) {
	asOf := time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC)
	provider := &fakeProvider{quotes: map[string]*Quote{
		"AAPL": {Ticker: "AAPL", Price: 200, PrevClose: 195, Currency: "USD", AsOf: asOf},
	}}
	mem := logtest.NewMemReader()
	s := newSensor(t, fakeSettings{tickers: []string{"AAPL"}, ok: true}, provider, mem)

	require.NoError(t, s.Sync(context.Background()))
	require.NoError(t, s.Sync(context.Background())) // second pass — same data

	events, err := mem.ByKind(context.Background(), log.KindStockSnapshot)
	require.NoError(t, err)
	require.Len(t, events, 1, "second poll with identical price+as_of must dedupe")
}

// Regression: Yahoo's regularMarketTime ticks every poll during market
// hours even when the price is flat. Dedupe on price + prev_close —
// not on as_of — so we don't accumulate a redundant snapshot per cron
// tick per ticker.
func TestSync_AsOfOnlyChange_StillDedupes(t *testing.T) {
	t0 := time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC)
	t1 := t0.Add(10 * time.Minute) // newer as_of, identical price/prev_close
	provider := &fakeProvider{quotes: map[string]*Quote{
		"AAPL": {Ticker: "AAPL", Price: 200, PrevClose: 195, Currency: "USD", AsOf: t0},
	}}
	mem := logtest.NewMemReader()
	s := newSensor(t, fakeSettings{tickers: []string{"AAPL"}, ok: true}, provider, mem)

	require.NoError(t, s.Sync(context.Background()))
	provider.quotes["AAPL"] = &Quote{Ticker: "AAPL", Price: 200, PrevClose: 195, Currency: "USD", AsOf: t1}
	require.NoError(t, s.Sync(context.Background()))

	events, err := mem.ByKind(context.Background(), log.KindStockSnapshot)
	require.NoError(t, err)
	require.Len(t, events, 1, "flat price with newer as_of must NOT write a redundant snapshot")
}

func TestSync_PriceChange_WritesNewSnapshot(t *testing.T) {
	t0 := time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC)
	t1 := t0.Add(10 * time.Minute)
	provider := &fakeProvider{quotes: map[string]*Quote{
		"AAPL": {Ticker: "AAPL", Price: 200, PrevClose: 195, Currency: "USD", AsOf: t0},
	}}
	mem := logtest.NewMemReader()
	s := newSensor(t, fakeSettings{tickers: []string{"AAPL"}, ok: true}, provider, mem)

	require.NoError(t, s.Sync(context.Background()))
	provider.quotes["AAPL"] = &Quote{Ticker: "AAPL", Price: 201, PrevClose: 195, Currency: "USD", AsOf: t1}
	require.NoError(t, s.Sync(context.Background()))

	events, err := mem.ByKind(context.Background(), log.KindStockSnapshot)
	require.NoError(t, err)
	require.Len(t, events, 2, "price change should write a second snapshot")
}

func TestSync_PerTickerError_SkipsButContinues(t *testing.T) {
	asOf := time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC)
	provider := &fakeProvider{
		quotes: map[string]*Quote{
			"GOOGL": {Ticker: "GOOGL", Price: 150, PrevClose: 152, Currency: "USD", AsOf: asOf},
		},
		errs: map[string]error{
			"BADTICK": errors.New("yahoo 404"),
		},
	}
	mem := logtest.NewMemReader()
	s := newSensor(t, fakeSettings{tickers: []string{"BADTICK", "GOOGL"}, ok: true}, provider, mem)

	require.NoError(t, s.Sync(context.Background()), "one bad ticker must not fail the whole sync when others succeed")

	events, err := mem.ByKind(context.Background(), log.KindStockSnapshot)
	require.NoError(t, err)
	require.Len(t, events, 1, "good ticker still recorded")
}

func TestSync_AlertOnLeadingEdge(t *testing.T) {
	t0 := time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC)
	provider := &fakeProvider{quotes: map[string]*Quote{
		// 5% move; threshold 3% → breach
		"AAPL": {Ticker: "AAPL", Price: 105, PrevClose: 100, Currency: "USD", AsOf: t0},
	}}
	mem := logtest.NewMemReader()
	s := newSensor(t, fakeSettings{tickers: []string{"AAPL"}, threshold: 3, ok: true}, provider, mem)

	require.NoError(t, s.Sync(context.Background()))

	alerts, err := mem.ByKind(context.Background(), log.KindStockAlert)
	require.NoError(t, err)
	require.Len(t, alerts, 1, "leading-edge breach should write a stock.alert event")
}

func TestSync_AlertSuppressedOnSustainedBreach(t *testing.T) {
	t0 := time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC)
	t1 := t0.Add(10 * time.Minute)
	provider := &fakeProvider{quotes: map[string]*Quote{
		"AAPL": {Ticker: "AAPL", Price: 105, PrevClose: 100, Currency: "USD", AsOf: t0},
	}}
	mem := logtest.NewMemReader()
	s := newSensor(t, fakeSettings{tickers: []string{"AAPL"}, threshold: 3, ok: true}, provider, mem)

	require.NoError(t, s.Sync(context.Background()))

	// Same direction, slightly higher — still breached, but no leading edge.
	provider.quotes["AAPL"] = &Quote{Ticker: "AAPL", Price: 106, PrevClose: 100, Currency: "USD", AsOf: t1}
	require.NoError(t, s.Sync(context.Background()))

	alerts, err := mem.ByKind(context.Background(), log.KindStockAlert)
	require.NoError(t, err)
	require.Len(t, alerts, 1, "sustained breach must not re-fire — only the leading edge writes an alert")
}

// Saturday 14:00 UTC is outside the US weekday window — sensor must
// return without polling the provider or writing anything.
func TestSync_OutsideMarketWindow_Skips(t *testing.T) {
	saturday := time.Date(2026, 5, 9, 14, 0, 0, 0, time.UTC)
	provider := &fakeProvider{quotes: map[string]*Quote{
		"AAPL": {Ticker: "AAPL", Price: 200, PrevClose: 195, AsOf: saturday},
	}}
	mem := logtest.NewMemReader()
	s := newSensor(t, fakeSettings{tickers: []string{"AAPL"}, ok: true}, provider, mem).
		WithNow(func() time.Time { return saturday })

	require.NoError(t, s.Sync(context.Background()))
	require.Empty(t, mem.Events(), "no events written outside market window")
	require.Equal(t, 0, provider.calls, "provider not called outside market window")
}

// alwaysPoll=true bypasses the market-hours gate.
func TestSync_AlwaysPoll_BypassesMarketWindow(t *testing.T) {
	saturday := time.Date(2026, 5, 9, 14, 0, 0, 0, time.UTC)
	provider := &fakeProvider{quotes: map[string]*Quote{
		"AAPL": {Ticker: "AAPL", Price: 200, PrevClose: 195, AsOf: saturday},
	}}
	mem := logtest.NewMemReader()
	s := newSensor(t, fakeSettings{tickers: []string{"AAPL"}, alwaysPoll: true, ok: true}, provider, mem).
		WithNow(func() time.Time { return saturday })

	require.NoError(t, s.Sync(context.Background()))
	events, _ := mem.ByKind(context.Background(), log.KindStockSnapshot)
	require.Len(t, events, 1, "alwaysPoll must bypass the weekend gate")
}

func TestSync_NoAlertWhenThresholdZero(t *testing.T) {
	t0 := time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC)
	provider := &fakeProvider{quotes: map[string]*Quote{
		"AAPL": {Ticker: "AAPL", Price: 110, PrevClose: 100, AsOf: t0},
	}}
	mem := logtest.NewMemReader()
	s := newSensor(t, fakeSettings{tickers: []string{"AAPL"}, threshold: 0, ok: true}, provider, mem)

	require.NoError(t, s.Sync(context.Background()))

	alerts, err := mem.ByKind(context.Background(), log.KindStockAlert)
	require.NoError(t, err)
	require.Empty(t, alerts, "threshold=0 disables alerting even on big moves")
}
