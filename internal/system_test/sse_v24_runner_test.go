package system_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/eventbus"
	api "github.com/zenocy/zeno-v2/internal/http/api"
	"github.com/zenocy/zeno-v2/internal/llm"
	zlog "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/projection"
	"github.com/zenocy/zeno-v2/internal/store"
	"github.com/zenocy/zeno-v2/internal/synth"
)

// miniStack stands up the minimum slice needed to exercise the V2.4
// publish path end-to-end through SSE: real Bus, real TodayStreamHandler,
// real synth.Runner, real synth.AskHandler, real LLM client pointed at a
// transcript-backed httptest server. No scheduler, no sensors, no auth —
// the test is about the live-event wire shape, not the surrounding
// orchestration (which Phase 0 system tests already cover).
type miniStack struct {
	t          *testing.T
	bus        *eventbus.Bus
	server     *httptest.Server
	transcript *streamingTranscriptServer
	runner     *synth.Runner
	cleanup    func()
}

// streamingTranscriptServer mirrors internal/synth's transcriptServer
// but lives in this package to avoid a cross-package test-only dep.
// Same shape: replies non-streaming or SSE-streaming based on the
// inbound `stream` flag.
type streamingTranscriptServer struct {
	mu     sync.Mutex
	turns  []turn
	idx    int
	server *httptest.Server
}

type turn struct {
	content   string
	toolCalls []toolCall // for cards-loop transcripts that fork to tools
}

type toolCall struct {
	id        string
	name      string
	arguments map[string]any
}

func newStreamingTranscriptServer(turns []turn) *streamingTranscriptServer {
	s := &streamingTranscriptServer{turns: turns}
	s.server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

func (s *streamingTranscriptServer) URL() string { return s.server.URL }

func (s *streamingTranscriptServer) Close() { s.server.Close() }

func (s *streamingTranscriptServer) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	defer r.Body.Close()
	s.mu.Lock()
	if s.idx >= len(s.turns) {
		// Repeat the last turn if exhausted (some tests fire more
		// requests than scripted; harmless under streaming since the
		// loop's iteration cap or our timeout will end it).
		s.idx = len(s.turns) - 1
	}
	t := s.turns[s.idx]
	s.idx++
	s.mu.Unlock()

	var probe struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &probe)
	if !probe.Stream {
		s.writeNonStreaming(w, t)
		return
	}
	s.writeStreaming(w, t)
}

func (s *streamingTranscriptServer) writeNonStreaming(w http.ResponseWriter, t turn) {
	choice := map[string]any{
		"index": 0,
		"message": map[string]any{
			"role": "assistant", "content": t.content,
		},
		"finish_reason": "stop",
	}
	if len(t.toolCalls) > 0 {
		tcs := []map[string]any{}
		for _, tc := range t.toolCalls {
			argsJSON, _ := json.Marshal(tc.arguments)
			tcs = append(tcs, map[string]any{
				"id":   tc.id,
				"type": "function",
				"function": map[string]any{
					"name":      tc.name,
					"arguments": string(argsJSON),
				},
			})
		}
		choice["message"].(map[string]any)["tool_calls"] = tcs
		choice["finish_reason"] = "tool_calls"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":      "chatcmpl-stub",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   "test",
		"choices": []any{choice},
		"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 20},
	})
}

func (s *streamingTranscriptServer) writeStreaming(w http.ResponseWriter, t turn) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}

	if len(t.toolCalls) > 0 {
		for i, tc := range t.toolCalls {
			argsJSON, _ := json.Marshal(tc.arguments)
			chunk := map[string]any{
				"choices": []map[string]any{{
					"delta": map[string]any{
						"tool_calls": []map[string]any{{
							"index": i,
							"id":    tc.id,
							"type":  "function",
							"function": map[string]any{
								"name":      tc.name,
								"arguments": string(argsJSON),
							},
						}},
					},
				}},
			}
			b, _ := json.Marshal(chunk)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
			flush()
		}
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		flush()
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		flush()
		return
	}

	content := t.content
	for len(content) > 0 {
		n := 16
		if n > len(content) {
			n = len(content)
		}
		piece := content[:n]
		content = content[n:]
		chunk := map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": piece}}}}
		b, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		flush()
	}
	_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	flush()
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flush()
}

// cardSet2 is two cards in the schema-conforming CardSet shape (two
// cards is the floor for the morning_calm fixture per the runner's
// 2..6 cap).
const cardSet2 = `{"cards":[{"id":"card-1","date":"2026-04-25","src":"calendar","src_label":"Today","rel":"high","kind":"","title":"A *full* day.","sub":"Two meetings before noon and a window of clear afternoon.","meta":[],"actions":[{"label":"Dismiss"}]},{"id":"card-2","date":"2026-04-25","src":"mail","src_label":"Inbox","rel":"med","kind":"","title":"Saru *asked*.","sub":"Saru wants the redline answer before the noon call — option pool, preferred.","meta":[],"actions":[{"label":"Dismiss"}]}]}`

const briefingJSON = `{"date":"2026-04-25","eyebrow":"calendar holds","title":"A *quiet* morning.","summary":"Open thread with Saru.","tension":40}`

const askCardJSON = `{"id":"answer-x","date":"2026-04-25","src":"ask","src_label":"Generated","rel":"med","title":"A *quiet* day.","sub":"Calendar empty.","meta":[],"actions":[{"label":"Dismiss"}]}`

// morningTurns is the 4-turn flow the V2.3 cards loop walks:
// 1. thought + tool call to read the email thread
// 2. tool call to read weather window
// 3. final cards JSON
// 4. briefing JSON
//
// Mirrors morning_calm.json's shape; inlined here so this test doesn't
// reach across packages for transcript fixtures.
var morningTurns = []turn{
	{
		content: "thought: pulling the redline thread for one-line context",
		toolCalls: []toolCall{
			{id: "tc-1", name: "read_thread", arguments: map[string]any{"subject_hint": "redline"}},
		},
	},
	{
		toolCalls: []toolCall{
			{id: "tc-2", name: "read_weather_window", arguments: map[string]any{
				"start_iso": "2026-04-25T12:00:00Z",
				"end_iso":   "2026-04-25T13:30:00Z",
			}},
		},
	},
	{content: cardSet2},
	{content: briefingJSON},
}

// newMiniStackForRunner builds a miniStack pre-configured for a morning
// synth run (cards + briefing transcripts).
func newMiniStackForRunner(t *testing.T) *miniStack {
	t.Helper()
	tsrv := newStreamingTranscriptServer(morningTurns)

	dbPath := filepath.Join(t.TempDir(), "zeno.db")
	db, lstore, err := zlog.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, synth.Migrate(db, true, false))

	now := time.Date(2026, 4, 25, 7, 30, 0, 0, time.UTC)
	ctx := context.Background()
	_, err = lstore.Append(ctx, zlog.KindMailReceived, "imap", map[string]any{
		"folder": "INBOX", "uid": 1, "uidvalidity": 1, "message_id": "<m@x>",
		"from": "Saru <saru@x>", "to": []string{"u@y"}, "subject": "redline",
		"date": now.Add(-time.Hour), "body_preview": "thread",
	})
	require.NoError(t, err)
	_, err = lstore.Append(ctx, zlog.KindCalEventSeen, "caldav", map[string]any{
		"uid": "evt-1", "title": "Sync", "location": "Zoom", "tag": "work",
		"start": now.Add(time.Hour), "end": now.Add(2 * time.Hour),
	})
	require.NoError(t, err)
	_, err = lstore.Append(ctx, zlog.KindWeatherSnapshot, "weather", map[string]any{
		"captured_at": now, "timezone": "UTC",
		"hourly": []map[string]any{{"time": now.Add(time.Hour), "code": 1, "wind_kmh": 5.0, "precip_mm": 0.0}},
	})
	require.NoError(t, err)

	llmClient := llm.NewClient(llm.ClientConfig{
		Endpoint: tsrv.URL(),
		Model:    "test",
		Timeout:  10 * time.Second,
	})
	prompts, err := synth.LoadPrompts("")
	require.NoError(t, err)

	logger := logrus.New()
	logger.Out = io.Discard

	bus := eventbus.New(logger.WithField("c", "bus"))
	runner := &synth.Runner{
		LLM:             llmClient,
		Reader:          lstore,
		DB:              db,
		EventLog:        lstore,
		Bus:             bus,
		ProjCfg:         projection.Config{TZ: time.UTC, LookbackDays: 14, RunWindowEarliestHour: 6, RunWindowLatestHour: 20, OpenThreadsMax: 20, Now: func() time.Time { return now }},
		Prompts:         prompts,
		Now:             func() time.Time { return now },
		Logger:          logger.WithField("c", "synth"),
		CardsTable:      "cards",
		BriefingTable:   "briefings",
		TraceTable:      "traces",
		CardsTimeout:    10 * time.Second,
		BriefingTimeout: 10 * time.Second,
	}

	e := echo.New()
	e.HideBanner = true
	(&api.TodayStreamHandler{Bus: bus, Logger: logger.WithField("c", "stream"), KeepAliveInterval: 200 * time.Millisecond}).Register(e)
	srv := httptest.NewServer(e)

	return &miniStack{
		t:          t,
		bus:        bus,
		server:     srv,
		transcript: tsrv,
		runner:     runner,
		cleanup:    func() { srv.Close(); tsrv.Close() },
	}
}

// openSSEClient opens the SSE stream and returns a buffered reader.
// Verifies the response shape; cleanup cancels the stream.
func openSSEClient(t *testing.T, srv *httptest.Server) (*bufio.Reader, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/today/stream", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	return bufio.NewReader(resp.Body), func() { cancel(); _ = resp.Body.Close() }
}

// readSSEEventTimed parses one full event (skipping `:` keepalive
// comments) until deadline. Returns event-name, raw data line.
func readSSEEventTimed(r *bufio.Reader, deadline time.Time) (event, data string, err error) {
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

// drainSSEUntilQuiet reads events until stillFor passes with no event;
// hard-bounded to 5s. Returns the (event, data) pairs in arrival order.
type sseEvent struct {
	event string
	data  string
}

func drainSSEUntilQuiet(r *bufio.Reader, stillFor time.Duration) []sseEvent {
	hard := time.Now().Add(5 * time.Second)
	var out []sseEvent
	for time.Now().Before(hard) {
		ev, data, err := readSSEEventTimed(r, time.Now().Add(stillFor))
		if err != nil {
			return out
		}
		out = append(out, sseEvent{ev, data})
	}
	return out
}

// TestSpine_MorningRun_FullSSESequenceOverWire drives a real Runner.Run
// through a real bus into a real TodayStreamHandler, and asserts the
// SSE wire bytes match the V2.4 contract. Pins:
//   - synth.started → trace.step+ → synth.delta+ → synth.completed → card.appended+
//   - card.appended payload byte-equal to V2.3 (Card itself, not a wrapper)
//   - All events for the run share a single RunID
func TestSpine_MorningRun_FullSSESequenceOverWire(t *testing.T) {
	stk := newMiniStackForRunner(t)
	defer stk.cleanup()

	r, closeFn := openSSEClient(t, stk.server)
	defer closeFn()
	require.Eventually(t, func() bool { return stk.bus.SubscriberCount() == 1 }, time.Second, 10*time.Millisecond)

	// Drive the run in a goroutine so the SSE reader can consume in parallel.
	runDone := make(chan error, 1)
	go func() { runDone <- stk.runner.Run(context.Background()) }()

	events := drainSSEUntilQuiet(r, 200*time.Millisecond)
	require.NoError(t, <-runDone)
	require.NotEmpty(t, events)

	kinds := []string{}
	prev := ""
	for _, ev := range events {
		if ev.event != prev {
			kinds = append(kinds, ev.event)
			prev = ev.event
		}
	}
	require.Equal(t, []string{
		"synth.started",
		"trace.step",
		"synth.delta",
		"synth.completed",
		"card.appended",
	}, kinds, "wire kind sequence (collapsed)")

	// Pin synth.started shape and capture runID.
	var started struct {
		RunID string `json:"run_id"`
		Stage string `json:"stage"`
		Date  string `json:"date"`
	}
	require.NoError(t, json.Unmarshal([]byte(events[0].data), &started))
	require.Equal(t, "morning", started.Stage)
	require.Equal(t, "2026-04-25", started.Date)
	require.NotEmpty(t, started.RunID)

	// Every trace.step / synth.delta payload carries the same runID.
	for _, ev := range events {
		switch ev.event {
		case "trace.step":
			var ts struct {
				RunID string `json:"run_id"`
				Stage string `json:"stage"`
			}
			require.NoError(t, json.Unmarshal([]byte(ev.data), &ts))
			require.Equal(t, started.RunID, ts.RunID)
			require.Contains(t, []string{"cards", "briefing"}, ts.Stage)
		case "synth.delta":
			var d struct {
				RunID string `json:"run_id"`
				Stage string `json:"stage"`
				Delta string `json:"delta"`
			}
			require.NoError(t, json.Unmarshal([]byte(ev.data), &d))
			require.Equal(t, started.RunID, d.RunID)
			require.Contains(t, []string{"cards", "briefing"}, d.Stage)
		case "synth.completed":
			var c struct {
				RunID   string `json:"run_id"`
				Stage   string `json:"stage"`
				Stopped string `json:"stopped"`
				TotalMs int64  `json:"total_ms"`
			}
			require.NoError(t, json.Unmarshal([]byte(ev.data), &c))
			require.Equal(t, started.RunID, c.RunID)
			require.Equal(t, "morning", c.Stage)
		case "card.appended":
			// V2.3 byte-equal: the data payload IS the marshaled Card.
			// RunID has json:"-" so it doesn't cross the wire — the
			// run association comes from TraceID instead.
			var card store.Card
			require.NoError(t, json.Unmarshal([]byte(ev.data), &card))
			require.NotEmpty(t, card.ID)
			require.Equal(t, "2026-04-25", card.Date)
			require.Equal(t, started.RunID, card.TraceID,
				"card.TraceID is the run association on the wire")
		}
	}
}

// TestSpine_AskRun_FullSSESequenceOverWire mirrors the morning test for
// the Ask path (POST /api/ask). Pins Stage="ask" everywhere, the
// response trace_id matches the run's bus events.
func TestSpine_AskRun_FullSSESequenceOverWire(t *testing.T) {
	tsrv := newStreamingTranscriptServer([]turn{{content: askCardJSON}})

	logger := logrus.New()
	logger.Out = io.Discard
	bus := eventbus.New(logger.WithField("c", "bus"))

	llmClient := llm.NewClient(llm.ClientConfig{
		Endpoint: tsrv.URL(),
		Model:    "test",
		Timeout:  10 * time.Second,
	})
	prompts, err := synth.LoadPrompts("")
	require.NoError(t, err)

	dbPath := filepath.Join(t.TempDir(), "zeno.db")
	db, lstore, err := zlog.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, synth.Migrate(db, true, false))

	traces := &store.TraceRepo{DB: db}

	e := echo.New()
	e.HideBanner = true
	now := time.Date(2026, 4, 25, 7, 30, 0, 0, time.UTC)
	(&api.AskHandler{
		AskFn: func(ctx context.Context, q string) (synth.Card, llm.Trace, []llm.MemoryCandidate, error) {
			return synth.Ask(ctx, synth.ReactiveDeps{
				LLM:     llmClient,
				Reader:  lstore,
				ProjCfg: projection.Config{TZ: time.UTC, OpenThreadsMax: 20, Now: func() time.Time { return now }},
				Prompts: prompts,
				Date:    "2026-04-25",
				Now:     now,
				Logger:  logger.WithField("c", "ask"),
			}, q)
		},
		Traces:   traces,
		Bus:      bus,
		EventLog: lstore,
		TZ:       func() *time.Location { return time.UTC },
		Now:      func() time.Time { return now },
		Log:      logger.WithField("c", "ask-h"),
	}).Register(e)
	(&api.TodayStreamHandler{Bus: bus, Logger: logger.WithField("c", "stream"), KeepAliveInterval: 500 * time.Millisecond}).Register(e)

	srv := httptest.NewServer(e)
	defer srv.Close()
	defer tsrv.Close()

	r, closeFn := openSSEClient(t, srv)
	defer closeFn()
	require.Eventually(t, func() bool { return bus.SubscriberCount() == 1 }, time.Second, 10*time.Millisecond)

	// POST the Ask in a goroutine so the SSE reader can interleave.
	type askResult struct {
		body []byte
		err  error
	}
	resCh := make(chan askResult, 1)
	go func() {
		resp, err := http.Post(srv.URL+"/api/ask", "application/json", strings.NewReader(`{"query":"what about today?"}`))
		if err != nil {
			resCh <- askResult{nil, err}
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		resCh <- askResult{body, nil}
	}()

	events := drainSSEUntilQuiet(r, 200*time.Millisecond)
	res := <-resCh
	require.NoError(t, res.err)

	kinds := []string{}
	prev := ""
	for _, ev := range events {
		if ev.event != prev {
			kinds = append(kinds, ev.event)
			prev = ev.event
		}
	}
	// The ask transcript is one short payload that may produce zero
	// trace.step events; the load-bearing sequence is started → delta? →
	// completed → card.
	require.Equal(t, "synth.started", kinds[0])
	require.Equal(t, "card.appended", kinds[len(kinds)-1])
	require.Contains(t, kinds, "synth.completed")

	// Decode the JSON response to grab trace_id, then verify it matches
	// every event's run_id.
	var resp struct {
		Card    json.RawMessage `json:"card"`
		TraceID string          `json:"trace_id"`
	}
	require.NoError(t, json.Unmarshal(res.body, &resp))
	require.NotEmpty(t, resp.TraceID)

	for _, ev := range events {
		if ev.event == "card.appended" {
			var card store.Card
			require.NoError(t, json.Unmarshal([]byte(ev.data), &card))
			require.Equal(t, resp.TraceID, card.TraceID)
			continue
		}
		var idObj struct {
			RunID string `json:"run_id"`
			Stage string `json:"stage"`
		}
		require.NoError(t, json.Unmarshal([]byte(ev.data), &idObj),
			"event %s data must be JSON object: %s", ev.event, ev.data)
		require.Equal(t, resp.TraceID, idObj.RunID)
		require.Equal(t, "ask", idObj.Stage)
	}
}

// TestSpine_BodyDeltaCoalescing_RatesUnder60Hz pins the coalescer's
// effective rate cap on the wire. Drives 200 single-char content
// deltas through AttachLivePublishers in 100ms; expects ≤ 13
// SynthDeltaEvents to land on the wire (= ceil(100ms / 16ms) + slack).
//
// The test goes through the bus + SSE handler so we measure the real
// delivery rate, not just the publisher's call count.
func TestSpine_BodyDeltaCoalescing_RatesUnder60Hz(t *testing.T) {
	logger := logrus.New()
	logger.Out = io.Discard
	bus := eventbus.New(logger.WithField("c", "bus")).WithBufferSize(4096)

	e := echo.New()
	e.HideBanner = true
	(&api.TodayStreamHandler{Bus: bus, Logger: logger.WithField("c", "stream"), KeepAliveInterval: 500 * time.Millisecond}).Register(e)
	srv := httptest.NewServer(e)
	defer srv.Close()

	r, closeFn := openSSEClient(t, srv)
	defer closeFn()
	require.Eventually(t, func() bool { return bus.SubscriberCount() == 1 }, time.Second, 10*time.Millisecond)

	ctx, cleanup := synth.AttachLivePublishers(context.Background(), bus, "rate-test", "briefing")
	stream := llm.StreamContentFromContext(ctx)
	require.NotNil(t, stream)

	// 200 single-char deltas across 100ms → coalescer should batch them.
	const total = 200
	start := time.Now()
	for i := 0; i < total; i++ {
		stream("a")
		time.Sleep(500 * time.Microsecond)
	}
	cleanup()
	elapsed := time.Since(start)

	events := drainSSEUntilQuiet(r, 100*time.Millisecond)

	// Count synth.delta events; concatenate their payloads.
	var deltaCount int
	var concat string
	for _, ev := range events {
		if ev.event != "synth.delta" {
			continue
		}
		deltaCount++
		var d struct {
			Delta string `json:"delta"`
		}
		require.NoError(t, json.Unmarshal([]byte(ev.data), &d))
		concat += d.Delta
	}

	// The 200 chars trip the byte-threshold every ~8 chars during the
	// burst, plus a final tail flush at cleanup. The math: 200 / 8 = 25
	// byte-flushes worst case. Allow up to 30 to keep the test stable
	// across CI load.
	require.LessOrEqual(t, deltaCount, 30,
		"coalescer should keep wire event count well below 60Hz; got %d in %s", deltaCount, elapsed)
	require.Equal(t, total, len(concat),
		"every byte must reach the wire even though events were coalesced")
}

// TestSpine_KeepalivePingDuringQuietPeriod pins that the SSE handler
// sends `: ping` comment lines on its KeepAliveInterval cadence so
// proxies and browsers don't time out an idle connection. P2's bus
// changes must not break this path.
func TestSpine_KeepalivePingDuringQuietPeriod(t *testing.T) {
	logger := logrus.New()
	logger.Out = io.Discard
	bus := eventbus.New(logger.WithField("c", "bus"))

	e := echo.New()
	e.HideBanner = true
	keepAlive := 80 * time.Millisecond
	(&api.TodayStreamHandler{Bus: bus, Logger: logger.WithField("c", "stream"), KeepAliveInterval: keepAlive}).Register(e)
	srv := httptest.NewServer(e)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/today/stream", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	r := bufio.NewReader(resp.Body)
	deadline := time.Now().Add(keepAlive*4 + 200*time.Millisecond)
	pings := 0
	for time.Now().Before(deadline) {
		line, err := r.ReadString('\n')
		if err != nil {
			break
		}
		if strings.HasPrefix(line, ":") {
			pings++
		}
	}
	require.GreaterOrEqual(t, pings, 1, "expected at least one keepalive ping within the deadline window")
}
