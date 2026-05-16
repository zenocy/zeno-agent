package api

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/eventbus"
	"github.com/zenocy/zeno-v2/internal/llm"
	"github.com/zenocy/zeno-v2/internal/store"
)

// TestWriteSSE_EmitsCanonicalFraming pins the wire format the SSE handler
// produces: `event: <name>\ndata: <json>\n\n`. Browsers' EventSource
// parser accepts variations (CRLF, multi-line data:), but locking the
// bytes makes regressions in the framing visible to the test suite.
func TestWriteSSE_EmitsCanonicalFraming(t *testing.T) {
	var buf bytes.Buffer
	err := writeSSE(&buf, "card.appended", store.Card{ID: "c1", Title: "Hello"})
	require.NoError(t, err)
	got := buf.String()
	require.True(t, strings.HasPrefix(got, "event: card.appended\n"))
	require.Contains(t, got, "data: ")
	require.True(t, strings.HasSuffix(got, "\n\n"), "SSE event must terminate with a blank line; got %q", got)
	// JSON must follow the data: prefix on a single line — no embedded
	// newlines (otherwise EventSource would treat each line after the
	// first as a separate data line, which we explicitly avoid).
	dataLine := strings.SplitN(strings.TrimSuffix(got, "\n\n"), "\n", 2)[1]
	require.True(t, strings.HasPrefix(dataLine, "data: "))
	body := strings.TrimPrefix(dataLine, "data: ")
	require.NotContains(t, body, "\n")
	require.Contains(t, body, `"id":"c1"`)
}

// TestWriteSSE_PropagatesMarshalError pins that writeSSE returns the
// json.Marshal error rather than swallowing it. The handler's emit() loop
// inspects this error to decide whether to log + continue.
func TestWriteSSE_PropagatesMarshalError(t *testing.T) {
	var buf bytes.Buffer
	err := writeSSE(&buf, "boom", unmarshalable{})
	require.Error(t, err)
	require.Equal(t, 0, buf.Len(), "no bytes should be written when marshal fails")
}

// TestWriteSSE_FlushesWhenWriterIsFlusher pins that an http.Flusher writer
// is flushed after the framing bytes are written. SSE delivery depends on
// flushing per event; without it, OS / proxy buffering would batch events
// for seconds.
func TestWriteSSE_FlushesWhenWriterIsFlusher(t *testing.T) {
	w := &flusherSpy{}
	err := writeSSE(w, "tick", map[string]int{"n": 1})
	require.NoError(t, err)
	require.True(t, w.flushed, "writeSSE must call Flush on http.Flusher writers")
}

// TestEmit_RoutesEachEventToItsName pins the type-switch in emit(): each
// concrete event maps to the matching SSE event name. Direct unit test
// against the handler bypasses the full HTTP plumbing — useful when one
// case is broken and the round-trip tests are too noisy to localize.
func TestEmit_RoutesEachEventToItsName(t *testing.T) {
	cases := []struct {
		name string
		ev   eventbus.Event
		want string // expected event name; "" means no SSE bytes (suppressed)
	}{
		{"card", eventbus.CardAppendedEvent{Card: store.Card{ID: "c1"}}, "card.appended"},
		{"started", eventbus.SynthStartedEvent{RunID: "r1"}, "synth.started"},
		{"step", eventbus.TraceStepEvent{RunID: "r1", Step: llm.TraceStep{Kind: llm.KindTool, Op: "READ"}}, "trace.step"},
		{"delta", eventbus.SynthDeltaEvent{RunID: "r1", Delta: "x"}, "synth.delta"},
		{"completed", eventbus.SynthCompletedEvent{RunID: "r1", Stopped: "ok"}, "synth.completed"},
		{"observed", eventbus.SensorEventObservedEvent{Kind_: "mail.received"}, ""}, // suppressed
		{"concern_proposed", eventbus.ConcernProposedEvent{ConcernID: "x", Name: "Construction"}, "concern.proposed"},
		{"concern_state", eventbus.ConcernStateChangedEvent{ConcernID: "x", PriorState: "proposed", NewState: "active"}, "concern.state_changed"},
		{"concern_tagged", eventbus.ConcernTaggedEvent{ConcernID: "x", EventIDs: []string{"e1"}, Source: "user"}, "concern.tagged"},
		{"retrospective_progress", eventbus.RetrospectiveProgressEvent{ConcernID: "x", Processed: 12, Total: 100, Status: "running"}, "concern.retrospective_progress"},
		{"unknown", customEvent{kind: "not.in.switch"}, ""}, // unknown type, skipped
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			h := &TodayStreamHandler{}
			err := h.emit(&buf, tc.ev)
			require.NoError(t, err)
			got := buf.String()
			if tc.want == "" {
				require.Empty(t, got, "%s must not emit SSE bytes", tc.name)
				return
			}
			require.Containsf(t, got, "event: "+tc.want+"\n", "%s framing", tc.name)
		})
	}
}

// unmarshalable is a value json.Marshal cannot encode (channels are
// not JSON-representable).
type unmarshalable struct {
	Ch chan int
}

// MarshalJSON forces the encoder into the failure branch deterministically
// without relying on stdlib internals — channels in struct fields would
// also fail, but this is more obvious to the reader.
func (unmarshalable) MarshalJSON() ([]byte, error) {
	return nil, errors.New("intentional marshal error")
}

// flusherSpy is an io.Writer that also implements http.Flusher and
// records whether Flush was called. Used to assert writeSSE's flush
// behavior without a real HTTP response.
type flusherSpy struct {
	bytes.Buffer
	flushed bool
}

func (f *flusherSpy) Flush() { f.flushed = true }

// customEvent is an Event implementation the SSE handler doesn't know
// about. emit() must route it through the default branch and skip the
// emission rather than panicking or sending malformed bytes.
type customEvent struct{ kind string }

func (c customEvent) Kind() string { return c.kind }
