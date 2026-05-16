package api

import (
	"bufio"
	"context"
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
	"github.com/zenocy/zeno-v2/internal/store"
)

func newQuietLogger() *logrus.Entry {
	l := logrus.New()
	l.Out = &nullWriter{}
	return logrus.NewEntry(l)
}

type nullWriter struct{}

func (nullWriter) Write(p []byte) (int, error) { return len(p), nil }

// startStreamServer spins up an Echo server with TodayStreamHandler
// wired to a fresh bus and returns the test server + bus + cancel.
func startStreamServer(t *testing.T, keepAlive time.Duration) (*httptest.Server, *eventbus.Bus) {
	t.Helper()
	bus := eventbus.New(newQuietLogger())
	e := echo.New()
	(&TodayStreamHandler{Bus: bus, Logger: newQuietLogger(), KeepAliveInterval: keepAlive}).Register(e)
	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)
	return srv, bus
}

// readSSEEvent reads from r until it has accumulated one complete SSE
// event (an `event:` line followed by a `data:` line followed by a blank
// line). Returns the event name and the data payload, or an error.
func readSSEEvent(t *testing.T, r *bufio.Reader, deadline time.Time) (event, data string, err error) {
	t.Helper()
	for time.Now().Before(deadline) {
		line, lineErr := r.ReadString('\n')
		if lineErr != nil {
			return "", "", lineErr
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case line == "" && event != "" && data != "":
			return event, data, nil
		}
	}
	return "", "", context.DeadlineExceeded
}

// TestTodayStream_HeadersAndCardAppended pins the canonical case:
// connect, publish a card, receive it as `event: card.appended` with the
// JSON-encoded card as the data payload.
func TestTodayStream_HeadersAndCardAppended(t *testing.T) {
	srv, bus := startStreamServer(t, 5*time.Second) // long ping so it doesn't interfere

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/today/stream", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	require.Equal(t, "no-cache", resp.Header.Get("Cache-Control"))
	require.Equal(t, "no", resp.Header.Get("X-Accel-Buffering"))

	// Wait until the handler has subscribed before publishing — otherwise
	// the publish races the subscribe and the card lands on no one.
	require.Eventually(t, func() bool { return bus.SubscriberCount() == 1 }, time.Second, 10*time.Millisecond)

	bus.PublishCard(store.Card{ID: "c1", Title: "Test card", Date: "2026-04-30", Origin: "inject"})

	r := bufio.NewReader(resp.Body)
	ev, data, err := readSSEEvent(t, r, time.Now().Add(2*time.Second))
	require.NoError(t, err)
	require.Equal(t, "card.appended", ev)
	require.Contains(t, data, `"id":"c1"`)
	require.Contains(t, data, `"origin":"inject"`)
}

// TestTodayStream_KeepAlivePing pins that a tab with no card publishes
// still receives `: ping` heartbeats so dead-client detection lands in
// O(seconds), not O(hours of TCP timeout).
func TestTodayStream_KeepAlivePing(t *testing.T) {
	srv, _ := startStreamServer(t, 100*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/today/stream", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	r := bufio.NewReader(resp.Body)
	deadline := time.Now().Add(time.Second)
	gotPing := false
	for time.Now().Before(deadline) {
		line, lineErr := r.ReadString('\n')
		if lineErr != nil {
			break
		}
		if strings.HasPrefix(line, ": ping") {
			gotPing = true
			break
		}
	}
	require.True(t, gotPing, "keep-alive ping must arrive within 1s when interval is 100ms")
}

// TestTodayStream_DisconnectUnsubscribesCleanly pins that closing the
// client's request context drops the subscription so the bus subscriber
// list doesn't leak across reconnects.
func TestTodayStream_DisconnectUnsubscribesCleanly(t *testing.T) {
	srv, bus := startStreamServer(t, 5*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/today/stream", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	require.Eventually(t, func() bool { return bus.SubscriberCount() == 1 }, time.Second, 10*time.Millisecond)

	cancel() // simulate client disconnect
	resp.Body.Close()

	require.Eventually(t, func() bool { return bus.SubscriberCount() == 0 }, 2*time.Second, 20*time.Millisecond,
		"subscriber must be removed when client disconnects")
}

// TestTodayStream_ServiceUnavailableWithoutBus pins that omitting the bus
// fails the request rather than panicking.
func TestTodayStream_ServiceUnavailableWithoutBus(t *testing.T) {
	e := echo.New()
	(&TodayStreamHandler{}).Register(e)
	srv := httptest.NewServer(e)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/today/stream")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

// openTestStream is a small helper for the V2.4 event-type tests below:
// connect, wait for the handler to subscribe, return a buffered reader
// the test reads SSE events from.
func openTestStream(t *testing.T, srv *httptest.Server, bus *eventbus.Bus) *bufio.Reader {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/today/stream", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Eventually(t, func() bool { return bus.SubscriberCount() == 1 }, time.Second, 10*time.Millisecond)
	return bufio.NewReader(resp.Body)
}

// TestTodayStream_TraceStepRoundTrip pins the V2.4 trace.step shape:
// a TraceStepEvent published on the bus shows up as `event: trace.step`
// with the JSON-encoded event (run_id + stage + step) as the payload.
func TestTodayStream_TraceStepRoundTrip(t *testing.T) {
	srv, bus := startStreamServer(t, 5*time.Second)
	r := openTestStream(t, srv, bus)

	bus.Publish(eventbus.TraceStepEvent{
		RunID: "r1",
		Stage: "cards",
		Step: llm.TraceStep{
			Kind:   llm.KindTool,
			Op:     "READ",
			Target: "calendar",
			MsAt:   1234,
		},
	})

	ev, data, err := readSSEEvent(t, r, time.Now().Add(2*time.Second))
	require.NoError(t, err)
	require.Equal(t, "trace.step", ev)
	require.Contains(t, data, `"run_id":"r1"`)
	require.Contains(t, data, `"stage":"cards"`)
	require.Contains(t, data, `"op":"READ"`)
	require.Contains(t, data, `"target":"calendar"`)
}

// TestTodayStream_SynthDeltaRoundTrip pins the body-token delta path
// — the chunks the React LiveSynthPanel concatenates into a paragraph.
func TestTodayStream_SynthDeltaRoundTrip(t *testing.T) {
	srv, bus := startStreamServer(t, 5*time.Second)
	r := openTestStream(t, srv, bus)

	bus.Publish(eventbus.SynthDeltaEvent{
		RunID: "r1",
		Stage: "briefing",
		Delta: "Ten minutes between meetings.",
	})

	ev, data, err := readSSEEvent(t, r, time.Now().Add(2*time.Second))
	require.NoError(t, err)
	require.Equal(t, "synth.delta", ev)
	require.Contains(t, data, `"run_id":"r1"`)
	require.Contains(t, data, `"stage":"briefing"`)
	require.Contains(t, data, `"delta":"Ten minutes between meetings."`)
}

// TestTodayStream_StartedCompletedPair pins the run-lifecycle
// boundary events: synth.started arrives, then synth.completed arrives,
// in publish order.
func TestTodayStream_StartedCompletedPair(t *testing.T) {
	srv, bus := startStreamServer(t, 5*time.Second)
	r := openTestStream(t, srv, bus)

	bus.Publish(eventbus.SynthStartedEvent{RunID: "r1", Stage: "morning", Date: "2026-04-30"})
	bus.Publish(eventbus.SynthCompletedEvent{RunID: "r1", Stage: "morning", Stopped: "ok", TotalMs: 28412})

	ev1, data1, err := readSSEEvent(t, r, time.Now().Add(2*time.Second))
	require.NoError(t, err)
	require.Equal(t, "synth.started", ev1)
	require.Contains(t, data1, `"date":"2026-04-30"`)

	ev2, data2, err := readSSEEvent(t, r, time.Now().Add(2*time.Second))
	require.NoError(t, err)
	require.Equal(t, "synth.completed", ev2)
	require.Contains(t, data2, `"stopped":"ok"`)
	require.Contains(t, data2, `"total_ms":28412`)
}

// TestTodayStream_MarshalFailureDoesNotKillStream pins the resilience
// contract: when emit() encounters an event type the handler can't
// serialize (custom Event implementations from a future code path that
// hasn't taught the type-switch about), the connection stays open and
// subsequent valid events still flow. The failing event is dropped
// silently; downstream UI can catch up via the database on reconnect.
func TestTodayStream_MarshalFailureDoesNotKillStream(t *testing.T) {
	srv, bus := startStreamServer(t, 5*time.Second)
	r := openTestStream(t, srv, bus)

	// Publish an unknown-type event (emit's default branch) — silently
	// skipped, no SSE bytes. The connection MUST stay open.
	bus.Publish(stubUnknownEvent{})

	// Then publish a normal card. If the connection survived the
	// unknown-event publish, this card lands as `event: card.appended`.
	bus.PublishCard(store.Card{ID: "after-unknown", Date: "2026-04-30"})

	ev, data, err := readSSEEvent(t, r, time.Now().Add(2*time.Second))
	require.NoError(t, err, "stream must survive an unknown event publish")
	require.Equal(t, "card.appended", ev)
	require.Contains(t, data, `"id":"after-unknown"`)
}

// stubUnknownEvent is an Event the SSE handler doesn't know about.
// Used by TestTodayStream_MarshalFailureDoesNotKillStream to exercise
// the default branch of emit's type-switch through the live HTTP
// handler (not just a unit-level call).
type stubUnknownEvent struct{}

func (stubUnknownEvent) Kind() string { return "test.unknown" }

// TestTodayStream_SensorEventObservedNotEmitted pins that sensor
// observations stay bus-internal: a SensorEventObservedEvent does NOT
// flow over SSE. The browser receives the keepalive ping (or nothing)
// instead.
func TestTodayStream_SensorEventObservedNotEmitted(t *testing.T) {
	srv, bus := startStreamServer(t, 100*time.Millisecond) // short keepalive so we get a ping fast
	r := openTestStream(t, srv, bus)

	bus.Publish(eventbus.SensorEventObservedEvent{
		Kind_:      "mail.received",
		EvidenceID: "msg-42",
	})
	// Follow with a card.appended so we have a known event to wait on.
	bus.PublishCard(store.Card{ID: "after-sensor", Date: "2026-04-30"})

	// The SSE consumer must see card.appended first — never a
	// sensor.event_observed. Read events until we see card.appended; if
	// we observe sensor.event_observed before that, fail.
	deadline := time.Now().Add(2 * time.Second)
	for {
		ev, data, err := readSSEEvent(t, r, deadline)
		require.NoError(t, err)
		require.NotEqualf(t, "sensor.event_observed", ev,
			"sensor.event_observed must NOT flow over SSE; got data=%s", data)
		if ev == "card.appended" {
			require.Contains(t, data, `"id":"after-sensor"`)
			return
		}
	}
}
