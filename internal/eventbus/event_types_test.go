package eventbus

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/llm"
	"github.com/zenocy/zeno-v2/internal/store"
)

// TestEvent_Kinds pins each event's Kind() to its exact wire string. These
// strings are public contracts: the SSE handler uses them as `event:` names,
// the React consumer registers EventSource listeners against them, and
// observability systems filter on them. A typo in one of these constants
// would silently break the live trace UI without a compile error.
func TestEvent_Kinds(t *testing.T) {
	cases := []struct {
		name string
		ev   Event
		want string
	}{
		{"CardAppendedEvent", CardAppendedEvent{}, "card.appended"},
		{"TraceStepEvent", TraceStepEvent{}, "trace.step"},
		{"SynthDeltaEvent", SynthDeltaEvent{}, "synth.delta"},
		{"SynthStartedEvent", SynthStartedEvent{}, "synth.started"},
		{"SynthCompletedEvent", SynthCompletedEvent{}, "synth.completed"},
		{"SensorEventObservedEvent", SensorEventObservedEvent{}, "sensor.event_observed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.ev.Kind())
		})
	}
}

// TestCardAppendedEvent_MarshalsAsCard pins the V2.3 backward-compat shape:
// the SSE `card.appended` data payload is the marshaled Card itself, NOT a
// wrapper object like {"card": {...}}. V2.3 React clients depend on this.
//
// Note: the SSE handler marshals e.Card (not e), so this test exercises the
// nested Card field, which is exactly what the SSE wire carries.
func TestCardAppendedEvent_MarshalsAsCard(t *testing.T) {
	ev := CardAppendedEvent{Card: store.Card{
		ID:     "c1",
		Date:   "2026-04-30",
		Title:  "Test card",
		Origin: "inject",
	}}
	body, err := json.Marshal(ev.Card)
	require.NoError(t, err)
	got := string(body)
	require.Contains(t, got, `"id":"c1"`)
	require.Contains(t, got, `"date":"2026-04-30"`)
	require.Contains(t, got, `"title":"Test card"`)
	require.Contains(t, got, `"origin":"inject"`)
	// The wrapper-shape regression: if someone wraps the Card in a {"card": ...}
	// envelope, V2.3 clients break. Pin the absence.
	require.NotContains(t, got, `"card":{"id"`)
}

// TestTraceStepEvent_JSONShape pins the shape the React LiveSynthPanel
// parses: {run_id, stage, step:{kind, op, target, note, t, ms_elapsed}}.
func TestTraceStepEvent_JSONShape(t *testing.T) {
	ev := TraceStepEvent{
		RunID: "r1",
		Stage: "cards",
		Step: llm.TraceStep{
			Kind:   llm.KindTool,
			Op:     "READ",
			Target: "calendar",
			Note:   "ok",
			MsAt:   1234,
		},
	}
	body, err := json.Marshal(ev)
	require.NoError(t, err)
	got := string(body)
	require.Contains(t, got, `"run_id":"r1"`)
	require.Contains(t, got, `"stage":"cards"`)
	require.Contains(t, got, `"step":`)
	require.Contains(t, got, `"kind":"tool"`)
	require.Contains(t, got, `"op":"READ"`)
	require.Contains(t, got, `"target":"calendar"`)
	require.Contains(t, got, `"note":"ok"`)
	require.Contains(t, got, `"ms_elapsed":1234`)
}

// TestTraceStepEvent_ThoughtOmitsToolFields pins that a thought-kind step
// omits the tool-only fields (op, target, note) when they're empty — the
// JSON tags use omitempty so the wire stays compact.
func TestTraceStepEvent_ThoughtOmitsToolFields(t *testing.T) {
	ev := TraceStepEvent{
		RunID: "r1",
		Stage: "cards",
		Step: llm.TraceStep{
			Kind: llm.KindThought,
			T:    "looking at the calendar",
			MsAt: 800,
		},
	}
	body, err := json.Marshal(ev)
	require.NoError(t, err)
	got := string(body)
	require.Contains(t, got, `"kind":"thought"`)
	require.Contains(t, got, `"t":"looking at the calendar"`)
	require.NotContains(t, got, `"op":`)
	require.NotContains(t, got, `"target":`)
	require.NotContains(t, got, `"note":`)
}

// TestSynthDeltaEvent_JSONShape pins the body-token chunk shape consumed
// by the React LiveSynthPanel's body paragraph.
func TestSynthDeltaEvent_JSONShape(t *testing.T) {
	ev := SynthDeltaEvent{RunID: "r1", Stage: "briefing", Delta: "Ten "}
	body, err := json.Marshal(ev)
	require.NoError(t, err)
	require.Equal(t, `{"run_id":"r1","stage":"briefing","delta":"Ten "}`, string(body))
}

// TestSynthStartedEvent_JSONShape pins the run-lifecycle start shape: the
// React UI mounts the LiveSynthPanel on this event and binds RunID for
// subsequent step / delta filtering.
func TestSynthStartedEvent_JSONShape(t *testing.T) {
	ev := SynthStartedEvent{RunID: "r1", Stage: "morning", Date: "2026-04-30"}
	body, err := json.Marshal(ev)
	require.NoError(t, err)
	require.Equal(t, `{"run_id":"r1","stage":"morning","date":"2026-04-30"}`, string(body))
}

// TestSynthCompletedEvent_JSONShape pins the run-lifecycle end shape. The
// React UI dissolves the LiveSynthPanel on this event after a 600ms settle.
func TestSynthCompletedEvent_JSONShape(t *testing.T) {
	ev := SynthCompletedEvent{RunID: "r1", Stage: "morning", Stopped: "ok", TotalMs: 28412}
	body, err := json.Marshal(ev)
	require.NoError(t, err)
	require.Equal(t, `{"run_id":"r1","stage":"morning","stopped":"ok","total_ms":28412}`, string(body))
}

// TestSensorEventObservedEvent_JSONShape — the bus-internal event still has
// to be JSON-serializable in case a future consumer (e.g. a debug endpoint)
// wants to expose it. The trailing-underscore Kind_ field carries the
// observation kind under a clean `kind` JSON tag.
func TestSensorEventObservedEvent_JSONShape(t *testing.T) {
	ev := SensorEventObservedEvent{
		Kind_:      "mail.received",
		EvidenceID: "msg-42",
		Payload:    map[string]any{"from": "saru@example.com"},
	}
	body, err := json.Marshal(ev)
	require.NoError(t, err)
	got := string(body)
	require.Contains(t, got, `"kind":"mail.received"`)
	require.Contains(t, got, `"evidence_id":"msg-42"`)
	require.Contains(t, got, `"payload":{"from":"saru@example.com"}`)
}

// TestSensorEventObservedEvent_OmitsEmptyPayload pins that an observation
// without a payload doesn't emit `"payload":null` — the omitempty tag
// keeps the wire tidy for sensors that only carry an evidence ID.
func TestSensorEventObservedEvent_OmitsEmptyPayload(t *testing.T) {
	ev := SensorEventObservedEvent{Kind_: "cal.event_seen", EvidenceID: "uid-1"}
	body, err := json.Marshal(ev)
	require.NoError(t, err)
	got := string(body)
	require.NotContains(t, got, `"payload"`)
}

// TestEvent_InterfaceSatisfaction is a compile-time pin: each concrete
// event must satisfy the Event interface. The slice initialization fails
// at compile time if any of the types loses Kind() or grows a different
// receiver type.
func TestEvent_InterfaceSatisfaction(t *testing.T) {
	var _ []Event = []Event{
		CardAppendedEvent{},
		TraceStepEvent{},
		SynthDeltaEvent{},
		SynthStartedEvent{},
		SynthCompletedEvent{},
		SensorEventObservedEvent{},
	}
}
