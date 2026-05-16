package synth

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDegradedInjectCard_StockBreachUsesMarketsSrc(t *testing.T) {
	now := time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC)
	signal := InjectSignal{
		Kind:       "stock_breach",
		Subject:    "AAPL +5.20%",
		EvidenceID: "stock-alert-AAPL",
		At:         now,
	}
	card := degradedInjectCard("2026-05-05", signal, now, "test-run")

	require.Equal(t, "markets", card.Source, "Phase 4: stock_breach routes to the dedicated markets src")
	require.Equal(t, "Markets · breach", card.SrcLabel)
	require.Contains(t, card.Title, "AAPL")
	// store.Card.Meta is JSON-serialized; check the encoded form for the kind.
	require.Contains(t, string(card.Meta), "stock_breach")
}

func TestDegradedInjectCard_CalendarMoveStillUsesCalendar(t *testing.T) {
	// Regression: extending the switch must not regress the existing kinds.
	now := time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC)
	signal := InjectSignal{Kind: "calendar_move", Subject: "Board call moved", EvidenceID: "cal-1", At: now}
	card := degradedInjectCard("2026-05-05", signal, now, "test-run")
	require.Equal(t, "calendar", card.Source)
	require.Equal(t, "Inject · calendar_move", card.SrcLabel)
}

func TestDegradedInjectCard_EmailDefaultsToMail(t *testing.T) {
	now := time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC)
	signal := InjectSignal{Kind: "email", Subject: "URGENT redline", EvidenceID: "URGENT redline", At: now}
	card := degradedInjectCard("2026-05-05", signal, now, "test-run")
	require.Equal(t, "mail", card.Source)
}
