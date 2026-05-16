package system_test

import (
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/eventbus"
	"github.com/zenocy/zeno-v2/internal/llm"
	"github.com/zenocy/zeno-v2/internal/store"
)

// TestSpine_LiveTrace_FullRunOverSSE is the load-bearing V2.4 system
// test for the typed eventbus → SSE wiring. It exercises the full
// HTTP → bus → SSE → client path with the *complete* live-trace event
// sequence a real V2.4 synth run produces:
//
//	synth.started → trace.step (×N) → synth.delta (×M)
//	             → synth.completed → card.appended
//
// The test stubs the synth runner (Phase 0 hasn't built that yet —
// publishers wire in P2) by publishing the events directly to the bus.
// What this validates is the wire layer: Bus[Event] correctly fans out
// every concrete event type, the SSE handler emits each under its
// expected event name, and the client receives them in publish order.
//
// Once Phase 2 lands the runner publishers, the only change to this
// test is dropping the stub and triggering a real synth run; the
// expectation list stays identical.
func TestSpine_LiveTrace_FullRunOverSSE(t *testing.T) {
	tz, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 30, 8, 0, 0, 0, tz)

	bus := eventbus.New(logrus.NewEntry(logrus.New()))

	h := NewHarness(t, HarnessConfig{
		TZ:  tz,
		Now: func() time.Time { return now },
		Bus: bus,
	})
	defer h.Close()
	require.Same(t, bus, h.Bus, "harness must wire the supplied Bus through to the SSE handler")

	r, closeSSE := openSSE(t, h)
	defer closeSSE()
	require.Eventually(t, func() bool { return bus.SubscriberCount() == 1 }, time.Second, 10*time.Millisecond,
		"SSE handler must subscribe to the bus before the publish")

	const runID = "morning-r1"
	expected := []eventbus.Event{
		eventbus.SynthStartedEvent{RunID: runID, Stage: "morning", Date: "2026-04-30"},
		eventbus.TraceStepEvent{RunID: runID, Stage: "cards", Step: llm.TraceStep{
			Kind: llm.KindThought, T: "looking at calendar", MsAt: 100,
		}},
		eventbus.TraceStepEvent{RunID: runID, Stage: "cards", Step: llm.TraceStep{
			Kind: llm.KindTool, Op: "READ", Target: "calendar", MsAt: 250,
		}},
		eventbus.TraceStepEvent{RunID: runID, Stage: "briefing", Step: llm.TraceStep{
			Kind: llm.KindThought, T: "drafting", MsAt: 1100,
		}},
		eventbus.SynthDeltaEvent{RunID: runID, Stage: "briefing", Delta: "Ten "},
		eventbus.SynthDeltaEvent{RunID: runID, Stage: "briefing", Delta: "minutes between meetings."},
		eventbus.SynthCompletedEvent{RunID: runID, Stage: "morning", Stopped: "ok", TotalMs: 28412},
		eventbus.CardAppendedEvent{Card: store.Card{
			ID: "c-board", Date: "2026-04-30", Title: "Board pre-read", Origin: "morning",
		}},
	}
	for _, ev := range expected {
		bus.Publish(ev)
	}

	// Read N events from the SSE stream. Match each one against the
	// expected slice by event name + a small payload-shape assertion.
	deadline := time.Now().Add(3 * time.Second)
	for i, want := range expected {
		ev, data, err := readSSEEvent(t, r, deadline)
		require.NoErrorf(t, err, "event %d (%s)", i, want.Kind())
		require.Equalf(t, want.Kind(), ev, "event %d name", i)
		assertEventPayload(t, want, data, i)
	}
}

// TestSpine_LiveTrace_TwoConcurrentRuns pins that two simultaneous
// runs (e.g., morning still finishing while a reactive Ask kicks off)
// both deliver their events to the same SSE subscriber, interleaved by
// publish order. UI-side filtering by RunID is the React layer's job;
// the wire delivers everything.
func TestSpine_LiveTrace_TwoConcurrentRuns(t *testing.T) {
	tz, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 30, 9, 0, 0, 0, tz)

	bus := eventbus.New(logrus.NewEntry(logrus.New()))

	h := NewHarness(t, HarnessConfig{
		TZ:  tz,
		Now: func() time.Time { return now },
		Bus: bus,
	})
	defer h.Close()

	r, closeSSE := openSSE(t, h)
	defer closeSSE()
	require.Eventually(t, func() bool { return bus.SubscriberCount() == 1 }, time.Second, 10*time.Millisecond)

	// Interleaved publishes: morning's first step, then ask starts,
	// ask's first step, morning's body delta, ask completes,
	// morning completes.
	bus.Publish(eventbus.SynthStartedEvent{RunID: "morning", Stage: "morning", Date: "2026-04-30"})
	bus.Publish(eventbus.TraceStepEvent{RunID: "morning", Stage: "cards", Step: llm.TraceStep{Kind: llm.KindThought, T: "calendar"}})
	bus.Publish(eventbus.SynthStartedEvent{RunID: "ask", Stage: "ask", Date: "2026-04-30"})
	bus.Publish(eventbus.TraceStepEvent{RunID: "ask", Stage: "ask", Step: llm.TraceStep{Kind: llm.KindThought, T: "interpreting question"}})
	bus.Publish(eventbus.SynthDeltaEvent{RunID: "morning", Stage: "briefing", Delta: "Quiet "})
	bus.Publish(eventbus.SynthCompletedEvent{RunID: "ask", Stage: "ask", Stopped: "ok", TotalMs: 1234})
	bus.Publish(eventbus.SynthCompletedEvent{RunID: "morning", Stage: "morning", Stopped: "ok", TotalMs: 12345})

	expectedSequence := []struct {
		event string
		runID string
	}{
		{"synth.started", "morning"},
		{"trace.step", "morning"},
		{"synth.started", "ask"},
		{"trace.step", "ask"},
		{"synth.delta", "morning"},
		{"synth.completed", "ask"},
		{"synth.completed", "morning"},
	}
	deadline := time.Now().Add(3 * time.Second)
	for i, want := range expectedSequence {
		ev, data, err := readSSEEvent(t, r, deadline)
		require.NoErrorf(t, err, "event %d", i)
		require.Equalf(t, want.event, ev, "event %d name", i)
		require.Containsf(t, data, `"run_id":"`+want.runID+`"`, "event %d run_id", i)
	}
}

// TestSpine_LiveTrace_LateSubscriberMissesPriorEvents pins the bus's
// no-replay contract: events published before subscribe land on no one.
// V2.4 accepts this because durable persistence (cards table, traces
// table) is the source of truth for catching up on reload — the SSE
// stream is "from this moment forward" only.
func TestSpine_LiveTrace_LateSubscriberMissesPriorEvents(t *testing.T) {
	tz, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 30, 9, 0, 0, 0, tz)

	bus := eventbus.New(logrus.NewEntry(logrus.New()))

	h := NewHarness(t, HarnessConfig{
		TZ:  tz,
		Now: func() time.Time { return now },
		Bus: bus,
	})
	defer h.Close()

	// Publish a complete run BEFORE any subscriber connects. With no
	// subscribers registered, every Publish call is a fan-out over an
	// empty list — the events are dropped silently.
	bus.Publish(eventbus.SynthStartedEvent{RunID: "early", Stage: "morning", Date: "2026-04-30"})
	bus.Publish(eventbus.SynthCompletedEvent{RunID: "early", Stage: "morning", Stopped: "ok", TotalMs: 1})

	r, closeSSE := openSSE(t, h)
	defer closeSSE()
	require.Eventually(t, func() bool { return bus.SubscriberCount() == 1 }, time.Second, 10*time.Millisecond)

	// Now publish a fresh card. The subscriber receives ONLY this — no
	// replay of the early run.
	bus.PublishCard(store.Card{ID: "post-subscribe", Date: "2026-04-30", Origin: "morning"})

	deadline := time.Now().Add(2 * time.Second)
	for {
		ev, data, err := readSSEEvent(t, r, deadline)
		require.NoError(t, err)
		require.NotEqualf(t, "synth.started", ev, "must not see replayed pre-subscribe event")
		require.NotEqualf(t, "synth.completed", ev, "must not see replayed pre-subscribe event")
		if ev == "card.appended" {
			require.Contains(t, data, `"id":"post-subscribe"`)
			return
		}
	}
}

// TestSpine_LiveTrace_SensorObservationStaysBusInternal pins the V2.4
// boundary: SensorEventObservedEvent flows on the bus (the inject
// subscriber needs it in P4) but does NOT cross the SSE wire. A
// browser tab opening /api/today/stream sees only events meant for
// the user-facing UI.
func TestSpine_LiveTrace_SensorObservationStaysBusInternal(t *testing.T) {
	tz, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 30, 9, 0, 0, 0, tz)

	bus := eventbus.New(logrus.NewEntry(logrus.New()))

	h := NewHarness(t, HarnessConfig{
		TZ:  tz,
		Now: func() time.Time { return now },
		Bus: bus,
	})
	defer h.Close()

	r, closeSSE := openSSE(t, h)
	defer closeSSE()
	require.Eventually(t, func() bool { return bus.SubscriberCount() == 1 }, time.Second, 10*time.Millisecond)

	// Publish three sensor observations interleaved with two card
	// publications. The SSE consumer must see exactly the two cards
	// in order, never any sensor.event_observed.
	bus.Publish(eventbus.SensorEventObservedEvent{Kind_: "mail.received", EvidenceID: "msg-1"})
	bus.PublishCard(store.Card{ID: "card-1", Date: "2026-04-30", Origin: "morning"})
	bus.Publish(eventbus.SensorEventObservedEvent{Kind_: "cal.event_changed", EvidenceID: "uid-2"})
	bus.Publish(eventbus.SensorEventObservedEvent{Kind_: "mail.received", EvidenceID: "msg-3"})
	bus.PublishCard(store.Card{ID: "card-2", Date: "2026-04-30", Origin: "morning"})

	wantIDs := []string{"card-1", "card-2"}
	deadline := time.Now().Add(2 * time.Second)
	for _, wantID := range wantIDs {
		ev, data, err := readSSEEvent(t, r, deadline)
		require.NoError(t, err)
		require.NotEqualf(t, "sensor.event_observed", ev,
			"sensor observation must NOT cross SSE wire; got data=%s", data)
		require.Equal(t, "card.appended", ev)
		require.Contains(t, data, `"id":"`+wantID+`"`)
	}
}

// TestSpine_LiveTrace_MultipleSubscribersBothReceiveSequence pins the
// fan-out contract: two browser tabs both receive the full event
// sequence. Each subscribers's buffer is independent (256 slots in V2.4),
// and a slow tab does not back-pressure a fast one.
func TestSpine_LiveTrace_MultipleSubscribersBothReceiveSequence(t *testing.T) {
	tz, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 30, 9, 0, 0, 0, tz)

	bus := eventbus.New(logrus.NewEntry(logrus.New()))

	h := NewHarness(t, HarnessConfig{
		TZ:  tz,
		Now: func() time.Time { return now },
		Bus: bus,
	})
	defer h.Close()

	r1, close1 := openSSE(t, h)
	defer close1()
	r2, close2 := openSSE(t, h)
	defer close2()
	require.Eventually(t, func() bool { return bus.SubscriberCount() == 2 }, time.Second, 10*time.Millisecond,
		"both SSE subscribers must register before the publish")

	bus.Publish(eventbus.SynthStartedEvent{RunID: "fan", Stage: "morning", Date: "2026-04-30"})
	bus.Publish(eventbus.SynthCompletedEvent{RunID: "fan", Stage: "morning", Stopped: "ok", TotalMs: 100})

	deadline := time.Now().Add(2 * time.Second)

	ev1a, _, err := readSSEEvent(t, r1, deadline)
	require.NoError(t, err)
	ev1b, _, err := readSSEEvent(t, r1, deadline)
	require.NoError(t, err)
	require.Equal(t, "synth.started", ev1a)
	require.Equal(t, "synth.completed", ev1b)

	ev2a, _, err := readSSEEvent(t, r2, deadline)
	require.NoError(t, err)
	ev2b, _, err := readSSEEvent(t, r2, deadline)
	require.NoError(t, err)
	require.Equal(t, "synth.started", ev2a)
	require.Equal(t, "synth.completed", ev2b)
}

// TestSpine_PollingReplacement_NewEventsCrossSSEWire pins that every
// event type added to replace UI polling lands on the SSE wire under
// the expected event name. Prevents the today_stream emit() switch
// silently dropping a new kind back into the default-skip branch.
func TestSpine_PollingReplacement_NewEventsCrossSSEWire(t *testing.T) {
	tz, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 5, 9, 9, 0, 0, 0, tz)

	bus := eventbus.New(logrus.NewEntry(logrus.New()))

	h := NewHarness(t, HarnessConfig{
		TZ:  tz,
		Now: func() time.Time { return now },
		Bus: bus,
	})
	defer h.Close()

	r, closeSSE := openSSE(t, h)
	defer closeSSE()
	require.Eventually(t, func() bool { return bus.SubscriberCount() == 1 }, time.Second, 10*time.Millisecond)

	type spec struct {
		event   eventbus.Event
		ssename string
	}
	cases := []spec{
		{
			event:   eventbus.WhatsAppStatusEvent{Enabled: true, Connected: true, OwnJID: "1@s.whatsapp.net"},
			ssename: "whatsapp.status_changed",
		},
		{
			event:   eventbus.TaskDeletedEvent{UID: "task-x"},
			ssename: "task.deleted",
		},
		{
			event:   eventbus.SettingsChangedEvent{Settings: []byte(`{"timezone":"America/Los_Angeles"}`)},
			ssename: "settings.changed",
		},
		{
			event:   eventbus.WeatherUpdatedEvent{Weather: nil},
			ssename: "weather.updated",
		},
		{
			event:   eventbus.StockUpdatedEvent{Stock: nil},
			ssename: "stock.updated",
		},
		{
			event:   eventbus.CalendarTodayChangedEvent{Events: nil},
			ssename: "calendar.today_changed",
		},
		{
			event:   eventbus.CalendarTomorrowChangedEvent{Events: nil},
			ssename: "calendar.tomorrow_changed",
		},
		{
			event:   eventbus.CalendarWeekChangedEvent{Events: nil},
			ssename: "calendar.week_changed",
		},
		{
			event:   eventbus.MemoryChangedEvent{Memory: []byte(`{"facts":[]}`)},
			ssename: "memory.changed",
		},
		{
			event:   eventbus.HealthChangedEvent{OK: true, Version: "test", Uptime: "5s", DBOK: true, LLMReachable: true},
			ssename: "health.changed",
		},
	}
	for _, c := range cases {
		bus.Publish(c.event)
	}

	deadline := time.Now().Add(3 * time.Second)
	for i, c := range cases {
		ev, data, err := readSSEEvent(t, r, deadline)
		require.NoErrorf(t, err, "event %d (%s)", i, c.ssename)
		require.Equalf(t, c.ssename, ev, "event %d wire name", i)
		require.NotEmpty(t, data, "event %d data", i)
	}
}

// assertEventPayload performs a small payload-shape spot check based
// on the concrete type of the expected event. Full byte-equality is
// fragile across JSON field-ordering changes; the spot checks pin the
// load-bearing fields the React consumer reads.
func assertEventPayload(t *testing.T, want eventbus.Event, data string, idx int) {
	t.Helper()
	switch e := want.(type) {
	case eventbus.CardAppendedEvent:
		require.Containsf(t, data, `"id":"`+e.Card.ID+`"`, "event %d id", idx)
	case eventbus.SynthStartedEvent:
		require.Containsf(t, data, `"run_id":"`+e.RunID+`"`, "event %d run_id", idx)
		require.Containsf(t, data, `"stage":"`+e.Stage+`"`, "event %d stage", idx)
	case eventbus.SynthCompletedEvent:
		require.Containsf(t, data, `"run_id":"`+e.RunID+`"`, "event %d run_id", idx)
		require.Containsf(t, data, `"stopped":"`+e.Stopped+`"`, "event %d stopped", idx)
	case eventbus.TraceStepEvent:
		require.Containsf(t, data, `"run_id":"`+e.RunID+`"`, "event %d run_id", idx)
		require.Containsf(t, data, `"stage":"`+e.Stage+`"`, "event %d stage", idx)
	case eventbus.SynthDeltaEvent:
		require.Containsf(t, data, `"run_id":"`+e.RunID+`"`, "event %d run_id", idx)
		require.Containsf(t, data, `"delta":`, "event %d delta field", idx)
		// The delta string itself escapes through the JSON encoder; spot
		// check that the raw text is reachable (covers the SSE single-line
		// constraint indirectly).
		require.Containsf(t, data, strings.TrimSpace(e.Delta), "event %d delta content", idx)
	}
}
