package synth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/eventbus"
	"github.com/zenocy/zeno-v2/internal/llm"
)

// TestSynthesizeInject_PublishesLiveEventsHappyPath pins the V2.4 event
// sequence on the inject path. SynthesizeInject is responsible for
// synth.started / trace.step* / synth.completed; the orchestrator
// (cmd/zeno/inject.go) publishes card.appended after persist, so this
// test must NOT see a card.appended.
func TestSynthesizeInject_PublishesLiveEventsHappyPath(t *testing.T) {
	deps, sig, _, cleanup := injectTestSetup(t, "inject_happy")
	defer cleanup()

	bus := eventbus.New(logrus.NewEntry(logrus.New()))
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	deps.Bus = bus

	res, err := SynthesizeInject(context.Background(), deps, sig)
	require.NoError(t, err)
	require.NotEmpty(t, res.Card.RunID)

	events := drainBus(t, sub, 100*time.Millisecond)
	require.NotEmpty(t, events)

	// First event is synth.started{stage=inject}; bus emissions stop
	// before card.appended because the orchestrator handles that.
	started := events[0].(eventbus.SynthStartedEvent)
	require.Equal(t, "inject", started.Stage)
	require.Equal(t, "2026-04-30", started.Date)
	require.NotEmpty(t, started.RunID)
	runID := started.RunID

	// Card RunID must match the bus events' RunID (regression on the
	// runID-hoist refactor).
	require.Equal(t, runID, res.Card.RunID)

	// trace.step events (when any are emitted — the inject_happy
	// transcript goes straight to a content response with no tools or
	// thinking, so the count can be 0) are tagged Stage=inject + runID.
	for _, ev := range eventsByKind(events, "trace.step") {
		ts := ev.(eventbus.TraceStepEvent)
		require.Equal(t, "inject", ts.Stage)
		require.Equal(t, runID, ts.RunID)
	}

	// synth.delta events are emitted when content streams; the inject
	// path streams the body of both turns.
	deltas := eventsByKind(events, "synth.delta")
	require.NotEmpty(t, deltas, "content body should produce coalesced synth.delta events")
	for _, ev := range deltas {
		d := ev.(eventbus.SynthDeltaEvent)
		require.Equal(t, "inject", d.Stage)
		require.Equal(t, runID, d.RunID)
	}

	// Exactly one synth.completed.
	completedEvents := eventsByKind(events, "synth.completed")
	require.Len(t, completedEvents, 1)
	completed := completedEvents[0].(eventbus.SynthCompletedEvent)
	require.Equal(t, "inject", completed.Stage)
	require.Equal(t, runID, completed.RunID)
	require.NotEmpty(t, completed.Stopped)

	// No card.appended events from SynthesizeInject — that's the
	// orchestrator's job.
	cards := eventsByKind(events, "card.appended")
	require.Empty(t, cards, "SynthesizeInject must not publish cards; the orchestrator does")
}

// TestSynthesizeInject_DegradedFallback_StillPublishesCompleted — when
// the LLM call fails, SynthesizeInject still emits the boundary events so
// the UI dissolves the live panel even on the unhappy path.
func TestSynthesizeInject_DegradedFallback_StillPublishesCompleted(t *testing.T) {
	// Fail-only LLM endpoint guarantees the loop returns runErr and we
	// fall through to the degradedInjectCard path.
	failServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "synthetic", http.StatusInternalServerError)
	}))
	defer failServer.Close()

	deps, sig, _, cleanup := injectTestSetup(t, "inject_happy")
	defer cleanup()

	// Swap the LLM endpoint to the failing server.
	deps.LLM = llm.NewClient(llm.ClientConfig{
		Endpoint: failServer.URL,
		Model:    "test",
		Timeout:  300 * time.Millisecond,
	})
	deps.LoopTimeout = 300 * time.Millisecond

	bus := eventbus.New(logrus.NewEntry(logrus.New()))
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)
	deps.Bus = bus

	res, err := SynthesizeInject(context.Background(), deps, sig)
	require.NoError(t, err) // soft failure → no error
	require.NotEmpty(t, res.Card.ID)

	events := drainBus(t, sub, 50*time.Millisecond)
	completedEvents := eventsByKind(events, "synth.completed")
	require.Len(t, completedEvents, 1, "synth.completed must fire even on degraded path")
}

// TestSynthesizeInject_NilBus_StillProducesResult — nil bus is the
// eval/replay configuration; the function must still return a valid
// result without panicking.
func TestSynthesizeInject_NilBus_StillProducesResult(t *testing.T) {
	deps, sig, _, cleanup := injectTestSetup(t, "inject_happy")
	defer cleanup()
	deps.Bus = nil

	require.NotPanics(t, func() {
		res, err := SynthesizeInject(context.Background(), deps, sig)
		require.NoError(t, err)
		require.NotEmpty(t, res.Card.ID)
	})
}
