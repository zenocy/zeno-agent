package sensor

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/projection"
	"github.com/zenocy/zeno-v2/internal/store"
)

func recentStockBreach(ticker string, changePct float64, ageFromNow time.Duration) projection.StockBreach {
	return projection.StockBreach{
		Ticker:       ticker,
		Price:        100,
		PrevClose:    100 - changePct, // approximate; not load-bearing for the detector
		ChangePct:    changePct,
		ThresholdPct: 3,
		EvidenceID:   "stock-alert-" + ticker,
		AsOf:         detectorTime.Add(-ageFromNow),
	}
}

func TestDetect_StockBreachWithinHorizonFires(t *testing.T) {
	deps := InjectDetectorDeps{
		StockBreaches: []projection.StockBreach{
			recentStockBreach("AAPL", 5.2, 3*time.Minute),
		},
		Now: fixedNow(detectorTime),
	}
	sig := Detect(deps, DefaultInjectConfig())
	require.NotNil(t, sig, "fresh stock breach must fire")
	require.Equal(t, "stock_breach", sig.Kind)
	require.True(t, strings.HasPrefix(sig.Subject, "AAPL "), "subject should lead with ticker, got %q", sig.Subject)
	require.Contains(t, sig.Subject, "5.20%")
	require.Equal(t, "stock-alert-AAPL", sig.EvidenceID)
}

func TestDetect_StockBreachOutsideHorizonDoesNotFire(t *testing.T) {
	deps := InjectDetectorDeps{
		StockBreaches: []projection.StockBreach{
			// Default horizon is 15min; 30min ago is outside.
			recentStockBreach("AAPL", 5.2, 30*time.Minute),
		},
		Now: fixedNow(detectorTime),
	}
	require.Nil(t, Detect(deps, DefaultInjectConfig()), "old breach must not fire")
}

func TestDetect_StockBreachMostRecentWins(t *testing.T) {
	deps := InjectDetectorDeps{
		StockBreaches: []projection.StockBreach{
			recentStockBreach("AAPL", 5.2, 10*time.Minute),
			recentStockBreach("TSLA", -4.0, 1*time.Minute), // freshest
			recentStockBreach("MSFT", 3.5, 5*time.Minute),
		},
		Now: fixedNow(detectorTime),
	}
	sig := Detect(deps, DefaultInjectConfig())
	require.NotNil(t, sig)
	require.True(t, strings.HasPrefix(sig.Subject, "TSLA "), "most-recent wins, got %q", sig.Subject)
	require.Contains(t, sig.Subject, "−") // unicode minus for negative move
}

// Calendar-move outranks a stock breach — meeting moves are more
// time-sensitive than a price tick.
func TestDetect_CalendarMoveBeatsStockBreach(t *testing.T) {
	calStart := detectorTime.Add(2 * time.Hour)
	deps := InjectDetectorDeps{
		Calendar: []projection.CalendarEvent{{
			UID: "evt-1", Title: "Series B review (board)",
			Start: calStart, End: calStart.Add(time.Hour),
			Attendees:    []string{"Saru Patel", "Lin Vega"},
			LastModified: detectorTime.Add(-5 * time.Minute),
		}},
		StockBreaches: []projection.StockBreach{
			recentStockBreach("AAPL", 5.2, 1*time.Minute),
		},
		Now: fixedNow(detectorTime),
	}
	sig := Detect(deps, DefaultInjectConfig())
	require.NotNil(t, sig)
	require.Equal(t, "calendar_move", sig.Kind, "calendar trumps stock when both qualify")
}

// Stock breach outranks a VIP email — the breach is unambiguous and
// time-sensitive (market hours), while VIP-name matching is heuristic.
func TestDetect_StockBreachBeatsVIPEmail(t *testing.T) {
	deps := InjectDetectorDeps{
		Cards: []store.Card{vipCard("Acuity — Series B review with Saru Patel", "Lin Vega")},
		Threads: []projection.Thread{{
			Subject:      "URGENT — board call moved to 10:30",
			LastSender:   "Saru Patel",
			LastReceived: detectorTime.Add(-2 * time.Minute),
			UnreadCount:  1,
		}},
		StockBreaches: []projection.StockBreach{
			recentStockBreach("AAPL", -3.5, 1*time.Minute),
		},
		Now: fixedNow(detectorTime),
	}
	sig := Detect(deps, DefaultInjectConfig())
	require.NotNil(t, sig)
	require.Equal(t, "stock_breach", sig.Kind, "stock_breach trumps VIP email when both qualify")
}

func TestDetect_StockBreachDebouncedByLastFire(t *testing.T) {
	deps := InjectDetectorDeps{
		StockBreaches: []projection.StockBreach{
			recentStockBreach("AAPL", 5.2, 1*time.Minute),
		},
		LastFire: detectorTime.Add(-5 * time.Minute), // inside DefaultDebounceWindow
		Now:      fixedNow(detectorTime),
	}
	require.Nil(t, Detect(deps, DefaultInjectConfig()), "debounce gate runs ahead of all paths")
}

func TestDetect_StockBreachSubjectFormatsBothDirections(t *testing.T) {
	cases := []struct {
		name    string
		change  float64
		wantSub string // substring expected in the formatted subject
	}{
		{"positive", 5.25, "AAPL +5.25%"},
		{"negative", -3.7, "AAPL −3.70%"}, // unicode minus, two decimals
		{"tiny positive", 0.05, "AAPL +0.05%"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := InjectDetectorDeps{
				StockBreaches: []projection.StockBreach{
					recentStockBreach("AAPL", tc.change, 1*time.Minute),
				},
				Now: fixedNow(detectorTime),
			}
			sig := Detect(deps, DefaultInjectConfig())
			require.NotNil(t, sig)
			require.Equal(t, tc.wantSub, sig.Subject)
		})
	}
}
