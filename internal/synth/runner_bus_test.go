package synth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/eventbus"
	"github.com/zenocy/zeno-v2/internal/llm"
	zlog "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/projection"
)

// runnerBusFixture builds a Runner backed by the morning_calm transcript and
// a real eventbus.Bus so the V2.4 publish path is exercised end-to-end.
type runnerBusFixture struct {
	runner  *Runner
	bus     *eventbus.Bus
	sub     chan eventbus.Event
	now     time.Time
	cleanup func()
}

func newRunnerBusFixture(t *testing.T, transcript string) *runnerBusFixture {
	t.Helper()
	turns := loadTranscript(t, transcript)
	ts := newTranscriptServer(t, turns)

	dbPath := filepath.Join(t.TempDir(), "zeno.db")
	db, lstore, err := zlog.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, Migrate(db, true, false))

	now := time.Date(2026, 4, 25, 7, 30, 0, 0, time.UTC)
	ctx := context.Background()

	// Seed minimal projection inputs (mirrors TestRunner_EndToEnd_MorningCalm).
	_, err = lstore.Append(ctx, zlog.KindMailReceived, "imap", map[string]any{
		"folder":       "INBOX",
		"uid":          1,
		"uidvalidity":  100,
		"message_id":   "<m1@example>",
		"from":         "Saru Patel <saru@acuity.test>",
		"to":           []string{"mira@halsen.test"},
		"subject":      "re: redline",
		"date":         now.Add(-2 * time.Hour),
		"body_preview": "Walked the redline with Lin.",
	})
	require.NoError(t, err)
	_, err = lstore.Append(ctx, zlog.KindCalEventSeen, "caldav", map[string]any{
		"uid":      "evt-acuity",
		"title":    "Acuity — Series B review",
		"location": "Zoom",
		"tag":      "work",
		"start":    time.Date(2026, 4, 25, 11, 0, 0, 0, time.UTC),
		"end":      time.Date(2026, 4, 25, 11, 45, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	_, err = lstore.Append(ctx, zlog.KindWeatherSnapshot, "weather", map[string]any{
		"captured_at": now.Add(-30 * time.Minute),
		"timezone":    "UTC",
		"hourly": []map[string]any{
			{"time": time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC), "code": 1, "wind_kmh": 8.0, "precip_mm": 0.0},
		},
	})
	require.NoError(t, err)

	llmClient := llm.NewClient(llm.ClientConfig{
		Endpoint: ts.URL,
		Model:    "test",
		Timeout:  10 * time.Second,
	})
	prompts, err := LoadPrompts("")
	require.NoError(t, err)

	logger := logrus.New()
	logger.Out = io.Discard

	bus := eventbus.New(logger.WithField("c", "bus-test"))
	sub := bus.Subscribe()

	runner := &Runner{
		LLM:      llmClient,
		Reader:   lstore,
		DB:       db,
		EventLog: lstore,
		Bus:      bus,
		ProjCfg: projection.Config{
			TZ:                    time.UTC,
			LookbackDays:          14,
			RunWindowMinMinutes:   45,
			RunWindowMaxWindKmh:   25,
			RunWindowEarliestHour: 6,
			RunWindowLatestHour:   20,
			OpenThreadsMax:        20,
			Now:                   func() time.Time { return now },
		},
		Prompts:            prompts,
		Now:                func() time.Time { return now },
		Logger:             logger.WithField("c", "synth-test"),
		CardsTable:         "cards",
		BriefingTable:      "briefings",
		TraceTable:         "traces",
		BriefingRetryDelay: 5 * time.Millisecond, // collapse retry to keep tests fast
	}

	return &runnerBusFixture{
		runner:  runner,
		bus:     bus,
		sub:     sub,
		now:     now,
		cleanup: func() { ts.Close(); bus.Unsubscribe(sub) },
	}
}

// TestRunner_PublishesLiveEventsInOrder pins the full V2.4 event sequence
// for a successful morning run: started → trace.step → synth.delta →
// completed → card.appended.
func TestRunner_PublishesLiveEventsInOrder(t *testing.T) {
	f := newRunnerBusFixture(t, "morning_calm")
	defer f.cleanup()

	ctx := context.Background()
	require.NoError(t, f.runner.Run(ctx))

	events := drainBus(t, f.sub, 100*time.Millisecond)
	require.NotEmpty(t, events)

	kinds := collapsedKinds(events)
	require.Equal(t, []string{
		"synth.started",
		"trace.step",
		"synth.delta",
		"synth.completed",
		"card.appended",
	}, kinds, "collapsed kind sequence must match the V2.4 contract; got %v", kinds)

	// Started event content.
	started := events[0].(eventbus.SynthStartedEvent)
	require.NotEmpty(t, started.RunID)
	require.Equal(t, "morning", started.Stage)
	require.Equal(t, "2026-04-25", started.Date)
	runID := started.RunID

	// All trace.step + synth.delta events carry runID and a sub-stage.
	subStages := map[string]bool{}
	traceSteps := eventsByKind(events, "trace.step")
	require.NotEmpty(t, traceSteps)
	for _, ev := range traceSteps {
		ts := ev.(eventbus.TraceStepEvent)
		require.Equal(t, runID, ts.RunID)
		require.Contains(t, []string{"cards", "briefing"}, ts.Stage)
		subStages[ts.Stage] = true
	}
	deltas := eventsByKind(events, "synth.delta")
	for _, ev := range deltas {
		d := ev.(eventbus.SynthDeltaEvent)
		require.Equal(t, runID, d.RunID)
		require.Contains(t, []string{"cards", "briefing"}, d.Stage)
	}

	// Completed before any card.
	var completedIdx, firstCardIdx int = -1, -1
	for i, ev := range events {
		switch ev.(type) {
		case eventbus.SynthCompletedEvent:
			if completedIdx == -1 {
				completedIdx = i
			}
		case eventbus.CardAppendedEvent:
			if firstCardIdx == -1 {
				firstCardIdx = i
			}
		}
	}
	require.GreaterOrEqual(t, completedIdx, 0, "expected synth.completed event")
	require.GreaterOrEqual(t, firstCardIdx, 0, "expected card.appended event")
	require.Less(t, completedIdx, firstCardIdx, "synth.completed must precede card.appended")

	completed := events[completedIdx].(eventbus.SynthCompletedEvent)
	require.Equal(t, runID, completed.RunID)
	require.Equal(t, "morning", completed.Stage)
	require.NotEmpty(t, completed.Stopped)
	require.Greater(t, completed.TotalMs, int64(-1))

	cards := eventsByKind(events, "card.appended")
	require.NotEmpty(t, cards)
	for _, ev := range cards {
		ca := ev.(eventbus.CardAppendedEvent)
		require.Equal(t, runID, ca.Card.RunID, "card.RunID must match the run")
		require.Equal(t, "2026-04-25", ca.Card.Date)
		require.NotEmpty(t, ca.Card.ID)
	}
}

// TestRunner_NilBus_NoEventsAndNoPanic confirms backward-compat: with
// Bus==nil the runner persists cards normally and never touches an event
// bus. Mirrors eval and replay paths that don't construct a bus.
func TestRunner_NilBus_NoEventsAndNoPanic(t *testing.T) {
	f := newRunnerBusFixture(t, "morning_calm")
	defer f.cleanup()
	f.runner.Bus = nil // disable

	ctx := context.Background()
	require.NotPanics(t, func() {
		require.NoError(t, f.runner.Run(ctx))
	})

	// The fixture's subscriber should see nothing because we cleared Bus
	// after subscribing; the publish path never fires.
	events := drainBus(t, f.sub, 30*time.Millisecond)
	require.Empty(t, events, "no events should fire when Bus is nil")
}

// TestRunner_BriefingFailure_StillPublishesCompletedAndCards asserts the
// degraded-briefing path still emits the boundary events (so the UI's
// LiveSynthPanel always dissolves and the cards prepend).
func TestRunner_BriefingFailure_StillPublishesCompletedAndCards(t *testing.T) {
	// We use the morning_calm transcript but force the briefing to fail
	// by giving it a deadline so short the second LLM call can't possibly
	// complete on the test runner.
	f := newRunnerBusFixture(t, "morning_calm")
	defer f.cleanup()

	f.runner.BriefingTimeout = 1 * time.Nanosecond
	// Don't trigger the retry tail in tests.
	f.runner.BriefingRetryDelay = 24 * time.Hour

	ctx := context.Background()
	require.NoError(t, f.runner.Run(ctx))

	events := drainBus(t, f.sub, 100*time.Millisecond)
	require.NotEmpty(t, events)

	// synth.completed must still fire even though briefing degraded.
	completedEvents := eventsByKind(events, "synth.completed")
	require.Len(t, completedEvents, 1)
	cardsAppended := eventsByKind(events, "card.appended")
	require.NotEmpty(t, cardsAppended,
		"cards must still be announced on the bus when briefing degrades")
}

// TestRunner_CardsFailure_PublishesNoCompleted — when the cards loop
// itself fails (and Run returns an error), no synth.completed nor
// card.appended is emitted, so the UI never thinks a failed run "finished
// successfully."
func TestRunner_CardsFailure_PublishesNoCompleted(t *testing.T) {
	// HTTP 500 endpoint guarantees every chat completion call fails;
	// after iteration retries the cards loop returns an error.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "synthetic failure", http.StatusInternalServerError)
	}))
	defer ts.Close()

	dbPath := filepath.Join(t.TempDir(), "zeno.db")
	db, lstore, err := zlog.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, Migrate(db, true, false))

	now := time.Date(2026, 4, 25, 7, 30, 0, 0, time.UTC)

	llmClient := llm.NewClient(llm.ClientConfig{
		Endpoint: ts.URL,
		Model:    "test",
		Timeout:  500 * time.Millisecond,
	})
	prompts, err := LoadPrompts("")
	require.NoError(t, err)

	logger := logrus.New()
	logger.Out = io.Discard

	bus := eventbus.New(logger.WithField("c", "bus-test"))
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	runner := &Runner{
		LLM:      llmClient,
		Reader:   lstore,
		DB:       db,
		EventLog: lstore,
		Bus:      bus,
		ProjCfg: projection.Config{
			TZ:                    time.UTC,
			LookbackDays:          14,
			RunWindowMinMinutes:   45,
			RunWindowMaxWindKmh:   25,
			RunWindowEarliestHour: 6,
			RunWindowLatestHour:   20,
			OpenThreadsMax:        20,
			Now:                   func() time.Time { return now },
		},
		Prompts:       prompts,
		Now:           func() time.Time { return now },
		Logger:        logger.WithField("c", "synth-test"),
		CardsTable:    "cards",
		BriefingTable: "briefings",
		TraceTable:    "traces",
		CardsTimeout:  500 * time.Millisecond,
	}

	err = runner.Run(context.Background())
	require.Error(t, err, "cards failure should bubble")

	events := drainBus(t, sub, 50*time.Millisecond)
	// We expect synth.started and nothing else.
	require.NotEmpty(t, events)
	require.Equal(t, "synth.started", events[0].Kind())
	for _, ev := range events {
		require.NotEqual(t, "synth.completed", ev.Kind(),
			"failed runs must not emit synth.completed")
		require.NotEqual(t, "card.appended", ev.Kind(),
			"failed runs must not emit card.appended")
	}
}
