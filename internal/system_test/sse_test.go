package system_test

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/eventbus"
	"github.com/zenocy/zeno-v2/internal/http/api"
	"github.com/zenocy/zeno-v2/internal/store"
	"github.com/zenocy/zeno-v2/internal/synth"
)

// publishOnInject returns a schedule.InjectFunc that publishes `card`
// to bus on every invocation, recording the signal it received in
// signalSink. Used to drive the SSE pipeline end-to-end without a real
// LLM call. before is an optional hook the stub runs before publishing
// — return a non-nil error to short-circuit (for the debounce / failure
// system tests).
func publishOnInject(bus *eventbus.Bus, card store.Card, signalSink *atomic.Value, before func(ctx context.Context, signal any) error) func(ctx context.Context, signal any) error {
	return func(ctx context.Context, signal any) error {
		if signalSink != nil && signal != nil {
			signalSink.Store(signal)
		}
		if before != nil {
			if err := before(ctx, signal); err != nil {
				return err
			}
		}
		bus.PublishCard(card)
		return nil
	}
}

// openSSE starts a GET on /api/today/stream against the harness server
// and returns a buffered reader on the response body plus a teardown
// func. Asserts the response shape (200 + text/event-stream) before
// returning.
func openSSE(t *testing.T, h *Harness) (*bufio.Reader, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", h.Server.URL+"/api/today/stream", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	cleanup := func() { cancel(); _ = resp.Body.Close() }
	return bufio.NewReader(resp.Body), cleanup
}

// readSSEEvent parses one full SSE event (event + data + blank-line
// terminator) from r. Comment lines (`: ping`) are skipped silently —
// the keep-alive heartbeat is part of the contract but isn't an
// event the consumer reacts to.
func readSSEEvent(t *testing.T, r *bufio.Reader, deadline time.Time) (event, data string, err error) {
	t.Helper()
	for time.Now().Before(deadline) {
		line, lineErr := r.ReadString('\n')
		if lineErr != nil {
			return "", "", lineErr
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, ":"):
			continue
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

// TestSpine_Inject_SSEDeliversCardEndToEnd is the load-bearing system
// test for V2.3.0 P3's inject + SSE wiring. It exercises the full
// HTTP → scheduler → injectFn → bus → SSE → client path with no real
// component stubbed except the LLM (the stub injectFn publishes a
// pre-built card directly).
//
// Flow under test:
//  1. The harness mounts SynthHandler + TodayStreamHandler against a
//     real Echo server with a real eventbus.Bus.
//  2. A fake browser tab subscribes via GET /api/today/stream.
//  3. POST /api/synth/now?kind=inject builds a synth.InjectSignal from
//     query params, threads it through Scheduler.RunInjectNowWithSignal,
//     and the stub injectFn publishes the canned card.
//  4. The SSE reader receives `event: card.appended` carrying the
//     JSON-encoded card.
//  5. Client disconnect releases the bus subscription.
func TestSpine_Inject_SSEDeliversCardEndToEnd(t *testing.T) {
	tz, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 30, 10, 0, 0, 0, tz)

	bus := eventbus.New(logrus.NewEntry(logrus.New()))
	card := store.Card{
		ID:     "saru-board",
		Date:   "2026-04-30",
		Title:  "Saru Patel — board call moved to 10:30",
		Sub:    "Need redline answer by 10:30 — option pool, non-participating preferred.",
		Origin: "inject",
	}
	var sigSink atomic.Value

	h := NewHarness(t, HarnessConfig{
		TZ:         tz,
		Now:        func() time.Time { return now },
		Bus:        bus,
		WithInject: publishOnInject(bus, card, &sigSink, nil),
	})
	defer h.Close()
	require.Same(t, bus, h.Bus, "harness must wire the supplied Bus through to the SSE handler")

	r, closeSSE := openSSE(t, h)
	defer closeSSE()
	require.Eventually(t, func() bool { return bus.SubscriberCount() == 1 }, time.Second, 10*time.Millisecond,
		"SSE handler must subscribe to the bus before the publish")

	status, body := h.Post("/api/synth/now?kind=inject&inject_kind=email&inject_subject=Saru%20Patel%20%E2%80%94%20board%20call%20moved%20to%2010%3A30")
	require.Equalf(t, http.StatusOK, status, "manual inject trigger must return 200; body=%s", body)

	event, data, err := readSSEEvent(t, r, time.Now().Add(2*time.Second))
	require.NoError(t, err, "SSE client must receive a card.appended event within 2s")
	require.Equal(t, "card.appended", event)
	require.Contains(t, data, `"id":"saru-board"`)
	require.Contains(t, data, `"origin":"inject"`)
	require.Contains(t, data, `"title":"Saru Patel — board call moved to 10:30"`)

	sig, ok := sigSink.Load().(*synth.InjectSignal)
	require.True(t, ok, "injectFn must receive *synth.InjectSignal on the manual debug path")
	require.Equal(t, "email", sig.Kind)
	require.Contains(t, sig.Subject, "Saru Patel")
	require.Contains(t, sig.Subject, "10:30")

	closeSSE()
	require.Eventually(t, func() bool { return bus.SubscriberCount() == 0 }, 2*time.Second, 20*time.Millisecond,
		"SSE handler must unsubscribe from the bus when the client disconnects")
}

// TestSpine_Inject_DebounceReturns429 pins the manual-path debounce
// gate end-to-end: when the orchestrator returns api.ErrInjectDebounced,
// the HTTP handler maps it to 429. Catches regressions in error mapping
// or middleware ordering that the per-handler unit test wouldn't see.
func TestSpine_Inject_DebounceReturns429(t *testing.T) {
	tz, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 30, 10, 0, 0, 0, tz)

	bus := eventbus.New(logrus.NewEntry(logrus.New()))
	debouncedFn := func(ctx context.Context, signal any) error {
		return api.ErrInjectDebounced
	}

	h := NewHarness(t, HarnessConfig{
		TZ:         tz,
		Now:        func() time.Time { return now },
		Bus:        bus,
		WithInject: debouncedFn,
	})
	defer h.Close()

	status, body := h.Post("/api/synth/now?kind=inject&inject_subject=test")
	require.Equalf(t, http.StatusTooManyRequests, status,
		"orchestrator returning ErrInjectDebounced must surface as 429; body=%s", body)
	require.Contains(t, string(body), "debounced")
}

// TestSpine_Inject_ForceBypassesDebounce exercises the ?force=1 escape
// hatch end-to-end: the API handler stamps api.InjectForceKey{} into
// the request context; the orchestrator reads it via ctx.Value and
// chooses to publish despite a recent fire. The fact that this contract
// crosses the schedule package via an `any`-typed context (no public
// scheduler API for "force") makes a system-level pin worth keeping.
func TestSpine_Inject_ForceBypassesDebounce(t *testing.T) {
	tz, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 30, 10, 0, 0, 0, tz)

	bus := eventbus.New(logrus.NewEntry(logrus.New()))
	card := store.Card{ID: "forced", Date: "2026-04-30", Title: "Forced inject", Origin: "inject"}
	var sawForce atomic.Bool

	branchOnForce := func(ctx context.Context, signal any) error {
		force, _ := ctx.Value(api.InjectForceKey{}).(bool)
		if !force {
			return api.ErrInjectDebounced
		}
		sawForce.Store(true)
		bus.PublishCard(card)
		return nil
	}

	h := NewHarness(t, HarnessConfig{
		TZ:         tz,
		Now:        func() time.Time { return now },
		Bus:        bus,
		WithInject: branchOnForce,
	})
	defer h.Close()

	r, closeSSE := openSSE(t, h)
	defer closeSSE()
	require.Eventually(t, func() bool { return bus.SubscriberCount() == 1 }, time.Second, 10*time.Millisecond)

	// Without force=1 → debounced.
	status, _ := h.Post("/api/synth/now?kind=inject&inject_subject=again")
	require.Equal(t, http.StatusTooManyRequests, status)

	// With force=1 → succeeds, publishes, SSE delivers.
	status, _ = h.Post("/api/synth/now?kind=inject&inject_subject=forced&force=1")
	require.Equal(t, http.StatusOK, status)
	require.True(t, sawForce.Load(), "orchestrator must observe InjectForceKey=true in ctx")

	event, data, err := readSSEEvent(t, r, time.Now().Add(2*time.Second))
	require.NoError(t, err)
	require.Equal(t, "card.appended", event)
	require.Contains(t, data, `"id":"forced"`)
}
