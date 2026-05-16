package system_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/eventbus"
	"github.com/zenocy/zeno-v2/internal/store"
)

// TestSpine_ReactiveInject_StockBreachTriggersCardViaSSE pins the
// stock-breach reactive flow at the system-test layer. It mirrors the
// VIP-email test in inject_reactive_test.go but flips the Kind_ to
// "stock.threshold_breach" and inspects the resulting card's stock-
// shaped src/src_label.
//
// The detector itself is unit-tested in
// internal/sensor/inject_detector_stock_test.go — this test exists to
// catch regressions where the bus subscriber accidentally adds a
// kind-name filter or upstream wiring drops the event before it
// reaches Scheduler.RunInjectNow.
//
// The injectFn is a stub (same pattern as the VIP test) — it
// unconditionally publishes a stock-shaped card so the SSE assertion
// stays deterministic without standing up the LLM loop.
func TestSpine_ReactiveInject_StockBreachTriggersCardViaSSE(t *testing.T) {
	tz, _ := time.LoadLocation("America/New_York")
	now := time.Date(2026, 5, 5, 14, 0, 0, 0, tz) // Tuesday 14:00 NY → 18:00 UTC, inside market window

	bus := eventbus.New(logrus.NewEntry(logrus.New()))
	card := store.Card{
		ID:       "stock-aapl-1",
		Date:     "2026-05-05",
		Title:    "AAPL +5.20%",
		Sub:      "Watched ticker breached the 3% threshold during the morning session.",
		Origin:   "inject",
		Source:   "tasks",
		SrcLabel: "Markets · AAPL",
	}
	var injectCalls atomic.Int32
	stubInject := func(ctx context.Context, signal any) error {
		injectCalls.Add(1)
		bus.PublishCard(card)
		return nil
	}

	h := NewHarness(t, HarnessConfig{
		TZ:         tz,
		Now:        func() time.Time { return now },
		Bus:        bus,
		WithInject: stubInject,
	})
	defer h.Close()

	stop := startSubscriber(t, h)
	defer stop()

	r, closeSSE := openSSE(t, h)
	defer closeSSE()
	require.Eventually(t, func() bool { return bus.SubscriberCount() >= 2 }, time.Second, 10*time.Millisecond,
		"reactive subscriber + SSE client both subscribed")

	// Sensor publishes a stock.threshold_breach observation onto the
	// bus (simulating a successful stock sensor poll on the leading
	// edge of a breach). The subscriber consumes any
	// SensorEventObservedEvent regardless of kind, so the stub
	// injectFn fires and publishes the card.
	bus.Publish(eventbus.SensorEventObservedEvent{
		Kind_:      "stock.threshold_breach",
		EvidenceID: "AAPL:1746543210",
		Payload: map[string]any{
			"ticker":        "AAPL",
			"price":         210.5,
			"prev_close":    200.0,
			"change_pct":    5.25,
			"threshold_pct": 3.0,
			"as_of":         now.UTC(),
		},
	})

	event, data, err := readSSEEvent(t, r, time.Now().Add(2*time.Second))
	require.NoError(t, err, "SSE client must receive card.appended within 2s of observation")
	require.Equal(t, "card.appended", event)
	require.Contains(t, data, `"id":"stock-aapl-1"`)
	require.Contains(t, data, `"origin":"inject"`)
	// Stock breach cards route to src=tasks with a Markets · TICKER label.
	require.Contains(t, data, `"src":"tasks"`)
	require.True(t, strings.Contains(data, "Markets") || strings.Contains(data, "AAPL"),
		"card payload must reference the ticker so the user knows what fired")
	require.Equal(t, int32(1), injectCalls.Load(), "exactly one inject pass")
}
