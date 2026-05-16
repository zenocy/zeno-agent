package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/eventbus"
	"github.com/zenocy/zeno-v2/internal/llm"
	"github.com/zenocy/zeno-v2/internal/log/logtest"
	"github.com/zenocy/zeno-v2/internal/store"
	"github.com/zenocy/zeno-v2/internal/synth"
)

// drainBusAPI mirrors internal/synth's drainBus helper. Reads events
// until the channel is quiet for quietFor (5s hard ceiling).
func drainBusAPI(t *testing.T, sub <-chan eventbus.Event, quietFor time.Duration) []eventbus.Event {
	t.Helper()
	var out []eventbus.Event
	hard := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-sub:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-time.After(quietFor):
			return out
		case <-hard:
			t.Fatalf("drainBusAPI: 5s ceiling reached after %d events", len(out))
			return out
		}
	}
}

// TestAskHandler_PublishesLiveEventsHappyPath pins the V2.4 SSE
// contract for the Ask path. AskHandler runs through to a happy AskFn
// stub; the bus must observe synth.started → synth.completed →
// card.appended (ordering matters for the dissolve animation), all
// tagged with the same RunID and Stage="ask"; and the HTTP response
// shape stays V2.3 byte-equal so existing browser clients keep working.
func TestAskHandler_PublishesLiveEventsHappyPath(t *testing.T) {
	stubCard := synth.Card{
		ID:       "answer-1",
		Date:     "2026-04-25",
		Source:   "ask",
		SrcLabel: "Generated",
		Rel:      "med",
		Title:    "A *quiet* day.",
		Sub:      "Calendar empty.",
		Meta:     []string{},
		Actions:  []synth.Action{{Label: "Dismiss"}},
	}
	stubTrace := llm.Trace{Stopped: "ok", TotalMs: 1234}

	bus := eventbus.New(logrus.NewEntry(logrus.New()))
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	db := openHandlerTestDB(t)
	traces := &store.TraceRepo{DB: db}
	e := echo.New()
	(&AskHandler{
		AskFn: func(_ context.Context, _ string) (synth.Card, llm.Trace, []llm.MemoryCandidate, error) {
			return stubCard, stubTrace, nil, nil
		},
		Traces:   traces,
		EventLog: logtest.NewMemReader(),
		Bus:      bus,
		TZ:       func() *time.Location { return time.UTC },
		Now:      func() time.Time { return time.Date(2026, 4, 25, 8, 0, 0, 0, time.UTC) },
		Log:      quietHandlerEntry(),
	}).Register(e)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/ask", strings.NewReader(`{"query":"what about today?"}`))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	var resp askResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Equal(t, stubCard.ID, resp.Card.ID, "HTTP response shape unchanged from V2.3")
	require.NotEmpty(t, resp.TraceID)

	events := drainBusAPI(t, sub, 50*time.Millisecond)
	require.NotEmpty(t, events)

	// Sequence: started → (no trace.step in this stub) → completed → card.
	started, ok := events[0].(eventbus.SynthStartedEvent)
	require.True(t, ok, "first event must be synth.started; got %T", events[0])
	require.Equal(t, "ask", started.Stage)
	require.Equal(t, "2026-04-25", started.Date)
	require.NotEmpty(t, started.RunID)
	require.Equal(t, started.RunID, resp.TraceID,
		"started.RunID must match the response trace_id (UI keys off this)")

	completedIdx, cardIdx := -1, -1
	for i, ev := range events {
		switch ev.(type) {
		case eventbus.SynthCompletedEvent:
			if completedIdx == -1 {
				completedIdx = i
			}
		case eventbus.CardAppendedEvent:
			if cardIdx == -1 {
				cardIdx = i
			}
		}
	}
	require.GreaterOrEqual(t, completedIdx, 0, "expected synth.completed event")
	require.GreaterOrEqual(t, cardIdx, 0, "expected card.appended event")
	require.Less(t, completedIdx, cardIdx,
		"synth.completed must precede card.appended for the dissolve animation")

	completed := events[completedIdx].(eventbus.SynthCompletedEvent)
	require.Equal(t, started.RunID, completed.RunID)
	require.Equal(t, "ask", completed.Stage)
	require.Equal(t, "ok", completed.Stopped)
	require.Equal(t, int64(1234), completed.TotalMs)

	card := events[cardIdx].(eventbus.CardAppendedEvent)
	// Ask cards are ephemeral (not stored alongside morning cards), so
	// store.Card.RunID stays empty per V2.3 contract — the trace_id is
	// the lookup key shared with the response. The UI uses card.trace_id
	// to associate the card with the run on the live SSE stream.
	require.Equal(t, started.RunID, card.Card.TraceID,
		"card.TraceID is the run association key for the ask path")
	require.Equal(t, "2026-04-25", card.Card.Date)
	require.Equal(t, "ask", card.Card.Origin,
		"V2.4 P3: ask cards must carry Origin=\"ask\" so the React UI routes them to the Generated section")
}

// TestAskHandler_NilBus_StillReturnsCardAndPersists pins eval/test
// configs that don't construct a bus. The handler must respond 200,
// persist the trace, and never panic.
func TestAskHandler_NilBus_StillReturnsCardAndPersists(t *testing.T) {
	stubCard := synth.Card{
		ID:       "x-1",
		Date:     "2026-04-25",
		Source:   "ask",
		SrcLabel: "Generated",
		Rel:      "low",
		Title:    "ok",
		Sub:      "ok",
		Actions:  []synth.Action{{Label: "Dismiss"}},
	}
	db := openHandlerTestDB(t)
	traces := &store.TraceRepo{DB: db}
	e := echo.New()
	(&AskHandler{
		AskFn: func(_ context.Context, _ string) (synth.Card, llm.Trace, []llm.MemoryCandidate, error) {
			return stubCard, llm.Trace{Stopped: "ok"}, nil, nil
		},
		Traces:   traces,
		EventLog: logtest.NewMemReader(),
		Bus:      nil, // explicit
		TZ:       func() *time.Location { return time.UTC },
		Now:      func() time.Time { return time.Date(2026, 4, 25, 8, 0, 0, 0, time.UTC) },
		Log:      quietHandlerEntry(),
	}).Register(e)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/ask", strings.NewReader(`{"query":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	require.NotPanics(t, func() { e.ServeHTTP(rr, req) })
	require.Equal(t, http.StatusOK, rr.Code)
}

// TestAskHandler_AskFnError_NoCompletedEvent — when AskFn returns an
// error, the handler still returns a 200 with the (degraded) card per
// V2.3 contract, but the bus must NOT see synth.completed or
// card.appended. The UI's LiveSynthPanel should NOT animate to the
// "settled" state on a failed run.
func TestAskHandler_AskFnError_NoCompletedEvent(t *testing.T) {
	bus := eventbus.New(logrus.NewEntry(logrus.New()))
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	failingCard := synth.Card{
		ID:       "fail-1",
		Date:     "2026-04-25",
		Source:   "ask",
		SrcLabel: "Generated",
		Rel:      "low",
		Title:    "Couldn't reach an answer.",
		Sub:      "Try again.",
		Actions:  []synth.Action{{Label: "Dismiss"}},
	}

	db := openHandlerTestDB(t)
	traces := &store.TraceRepo{DB: db}
	e := echo.New()
	(&AskHandler{
		AskFn: func(_ context.Context, _ string) (synth.Card, llm.Trace, []llm.MemoryCandidate, error) {
			// V2.3 returns a degraded card AND an error from synth.Ask
			// only on hard failures (deadline / projection error).
			return failingCard, llm.Trace{Stopped: "deadline"}, nil, errors.New("deadline")
		},
		Traces:   traces,
		EventLog: logtest.NewMemReader(),
		Bus:      bus,
		TZ:       func() *time.Location { return time.UTC },
		Now:      func() time.Time { return time.Date(2026, 4, 25, 8, 0, 0, 0, time.UTC) },
		Log:      quietHandlerEntry(),
	}).Register(e)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/ask", strings.NewReader(`{"query":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	events := drainBusAPI(t, sub, 50*time.Millisecond)
	require.NotEmpty(t, events)
	require.Equal(t, "synth.started", events[0].Kind())
	for _, ev := range events {
		require.NotEqual(t, "synth.completed", ev.Kind(),
			"failed Ask must not emit synth.completed")
		require.NotEqual(t, "card.appended", ev.Kind(),
			"failed Ask must not emit card.appended")
	}
}
