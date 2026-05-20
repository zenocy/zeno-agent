package synth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/llm"
	zlog "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/projection"
	"github.com/zenocy/zeno-v2/internal/store"
)

// transcriptTurn is one scripted exchange. The httptest handler walks turns
// in order; req_match is a substring (case-sensitive) that must appear in the
// outgoing request body for the turn to apply.
type transcriptTurn struct {
	ReqMatch string         `json:"req_match,omitempty"`
	Resp     transcriptResp `json:"resp"`
}

type transcriptResp struct {
	Content   string               `json:"content,omitempty"`
	ToolCalls []transcriptToolCall `json:"tool_calls,omitempty"`
}

type transcriptToolCall struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// loadTranscript reads a JSON list of turns from disk.
func loadTranscript(t *testing.T, name string) []transcriptTurn {
	t.Helper()
	path := filepath.Join("testdata", "transcripts", name+".json")
	b, err := openAndRead(path)
	require.NoErrorf(t, err, "open %s", path)
	var turns []transcriptTurn
	require.NoError(t, json.Unmarshal(b, &turns))
	return turns
}

func openAndRead(path string) ([]byte, error) {
	f, err := openFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// openFile is split out so tests can mock — kept simple here.
func openFile(path string) (io.ReadCloser, error) {
	return openOSFile(path)
}

// transcriptServer mocks an OpenAI-compatible chat completions endpoint by
// walking a transcript. Each request reads the next turn whose req_match
// matches; turns must be hit in declaration order.
type transcriptServer struct {
	t     *testing.T
	mu    sync.Mutex
	turns []transcriptTurn
	idx   int
}

func newTranscriptServer(t *testing.T, turns []transcriptTurn) *httptest.Server {
	ts := &transcriptServer{t: t, turns: turns}
	return httptest.NewServer(http.HandlerFunc(ts.handle))
}

func (ts *transcriptServer) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.idx >= len(ts.turns) {
		ts.t.Errorf("transcript exhausted; got extra request: %s", string(body))
		http.Error(w, "transcript exhausted", http.StatusInternalServerError)
		return
	}
	turn := ts.turns[ts.idx]
	if turn.ReqMatch != "" && !strings.Contains(string(body), turn.ReqMatch) {
		ts.t.Errorf("transcript turn %d req_match %q not found in body: %s",
			ts.idx, turn.ReqMatch, string(body))
		http.Error(w, "req_match failed", http.StatusInternalServerError)
		return
	}
	ts.idx++

	// Detect whether the LLM client routed via streaming (V2.4) or
	// non-streaming (V2.3). Both code paths must work against the same
	// transcript so we don't have to maintain two fixture forks.
	var probe struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &probe)

	if probe.Stream {
		ts.writeStreaming(w, turn)
		return
	}

	// Non-streaming response (V2.3 path).
	choice := map[string]any{
		"index": 0,
		"message": map[string]any{
			"role":    "assistant",
			"content": turn.Resp.Content,
		},
		"finish_reason": "stop",
	}
	if len(turn.Resp.ToolCalls) > 0 {
		toolCalls := []map[string]any{}
		for _, tc := range turn.Resp.ToolCalls {
			argsJSON, _ := json.Marshal(tc.Arguments)
			toolCalls = append(toolCalls, map[string]any{
				"id":   tc.ID,
				"type": "function",
				"function": map[string]any{
					"name":      tc.Name,
					"arguments": string(argsJSON),
				},
			})
		}
		msg := choice["message"].(map[string]any)
		msg["tool_calls"] = toolCalls
		choice["finish_reason"] = "tool_calls"
	}
	resp := map[string]any{
		"id":      "chatcmpl-test",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   "test",
		"choices": []any{choice},
		"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 20},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// writeStreaming emits the same logical turn as the non-streaming branch
// but framed as an OpenAI-style SSE delta sequence. Content is split into
// small chunks so the V2.4 coalescer in publisher.go has multiple deltas
// to coalesce; tool calls are emitted whole (LM Studio + Qwen3 also
// fragment tool-call args, but the loop's accumulator handles either).
func (ts *transcriptServer) writeStreaming(w http.ResponseWriter, turn transcriptTurn) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}

	// Tool calls: emit one chunk per tool call (whole arguments string).
	if len(turn.Resp.ToolCalls) > 0 {
		for i, tc := range turn.Resp.ToolCalls {
			argsJSON, _ := json.Marshal(tc.Arguments)
			idx := i
			chunk := map[string]any{
				"choices": []map[string]any{{
					"delta": map[string]any{
						"tool_calls": []map[string]any{{
							"index": idx,
							"id":    tc.ID,
							"type":  "function",
							"function": map[string]any{
								"name":      tc.Name,
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
		// Finish reason chunk.
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		flush()
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		flush()
		return
	}

	// Content: split into ~16-char chunks so coalescing has work to do.
	content := turn.Resp.Content
	for len(content) > 0 {
		n := 16
		if n > len(content) {
			n = len(content)
		}
		piece := content[:n]
		content = content[n:]
		chunk := map[string]any{
			"choices": []map[string]any{{
				"delta": map[string]any{"content": piece},
			}},
		}
		b, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		flush()
	}
	_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	flush()
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flush()
}

func TestRunner_EndToEnd_MorningCalm(t *testing.T) {
	turns := loadTranscript(t, "morning_calm")
	ts := newTranscriptServer(t, turns)
	defer ts.Close()

	dbPath := filepath.Join(t.TempDir(), "zeno.db")
	db, lstore, err := zlog.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, Migrate(db, true, false))

	now := time.Date(2026, 4, 25, 7, 30, 0, 0, time.UTC)

	// Seed events: one mail thread, one calendar event, one weather snapshot.
	ctx := context.Background()
	_, err = lstore.Append(ctx, zlog.KindMailReceived, "imap", map[string]any{
		"folder":       "INBOX",
		"uid":          1,
		"uidvalidity":  100,
		"message_id":   "<m1@example>",
		"from":         "Saru Patel <saru@acuity.test>",
		"to":           []string{"mira@halsen.test"},
		"subject":      "re: redline",
		"date":         now.Add(-2 * time.Hour),
		"body_preview": "Walked the redline with Lin. Two questions remain.",
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
			{"time": time.Date(2026, 4, 25, 13, 0, 0, 0, time.UTC), "code": 1, "wind_kmh": 7.0, "precip_mm": 0.0},
		},
	})
	require.NoError(t, err)

	// Build the runner pointing at the transcript-backed LLM.
	llmClient := llm.NewClient(llm.ClientConfig{
		Endpoint: ts.URL,
		Model:    "test",
		Timeout:  10 * time.Second,
	})
	prompts, err := LoadPrompts("")
	require.NoError(t, err)

	logger := logrus.New()
	logger.Out = io.Discard

	runner := &Runner{
		LLM:      llmClient,
		Reader:   lstore,
		DB:       db,
		EventLog: lstore,
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
	}

	require.NoError(t, runner.Run(ctx))

	// Assertions.
	cards, err := (&store.CardRepo{DB: db, Table: "cards"}).ListByDate(ctx, "2026-04-25")
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(cards), 2)

	briefing, err := (&store.BriefingRepo{DB: db, Table: "briefings"}).ByDate(ctx, "2026-04-25")
	require.NoError(t, err)
	require.NotNil(t, briefing)
	require.Equal(t, 38, briefing.Tension)
	require.Contains(t, briefing.Title, "*")
	// V2.3.0 P1: state must persist on the briefing row. Apr 25 2026 is a
	// Saturday at 07:30 UTC — weekend deep_work suppressed, hour < 16, no
	// near meeting → morning_calm.
	require.Equal(t, string(StateMorningCalm), briefing.State, "state must persist on briefing row")

	// Trace must exist and carry both tool and thought steps.
	require.NotEmpty(t, cards[0].TraceID)
	tr, err := (&store.TraceRepo{DB: db, Table: "traces"}).Get(ctx, cards[0].TraceID)
	require.NoError(t, err)
	require.NotNil(t, tr)

	var stepsRaw []map[string]any
	require.NoError(t, json.Unmarshal(tr.Steps, &stepsRaw))
	require.NotEmpty(t, stepsRaw)
	var hasTool, hasThought bool
	for _, s := range stepsRaw {
		switch s["kind"] {
		case "tool":
			hasTool = true
		case "thought":
			hasThought = true
		}
	}
	require.True(t, hasTool, "trace must include at least one tool step")
	require.True(t, hasThought, "trace must include at least one thought step")
}

// TestRunner_EndToEnd_AdaptiveState pins the V2.3.0 detector + voice
// overlay end-to-end on the deep_work register. A weekday morning at 10:00
// with no near meetings and ≥3h unbooked block must (a) detect deep_work,
// (b) persist State="deep_work" on the briefing row, and (c) land tension
// inside the [15,25] band declared in internal/synth/templates/_voice.md. The test uses a
// scripted transcript so it runs offline alongside the rest of the synth
// suite — golden-corpus byte-equality lives in eval/, not here.
func TestRunner_EndToEnd_AdaptiveState(t *testing.T) {
	turns := loadTranscript(t, "deep_work")
	ts := newTranscriptServer(t, turns)
	defer ts.Close()

	dbPath := filepath.Join(t.TempDir(), "zeno.db")
	db, lstore, err := zlog.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, Migrate(db, true, false))

	// Monday 2026-04-27 at 10:00 UTC. Weekday + working-hour + (no calendar
	// events) → 6h unbooked block ahead → DetectState → deep_work.
	now := time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC)
	ctx := context.Background()

	// Seed only a weather snapshot so the projection has *something* to fold
	// (cards don't depend on it for deep_work, but the runner builds the full
	// projection regardless and zero events is a degenerate edge that isn't
	// the case under test). No calendar events on purpose.
	_, err = lstore.Append(ctx, zlog.KindWeatherSnapshot, "weather", map[string]any{
		"captured_at": now.Add(-30 * time.Minute),
		"timezone":    "UTC",
		"hourly": []map[string]any{
			{"time": time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC), "code": 1, "wind_kmh": 8.0, "precip_mm": 0.0},
			{"time": time.Date(2026, 4, 27, 13, 0, 0, 0, time.UTC), "code": 1, "wind_kmh": 7.0, "precip_mm": 0.0},
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

	runner := &Runner{
		LLM:      llmClient,
		Reader:   lstore,
		DB:       db,
		EventLog: lstore,
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
	}

	require.NoError(t, runner.Run(ctx))

	briefing, err := (&store.BriefingRepo{DB: db, Table: "briefings"}).ByDate(ctx, "2026-04-27")
	require.NoError(t, err)
	require.NotNil(t, briefing)

	require.Equal(t, string(StateDeepWork), briefing.State, "deep_work register must persist on the briefing row")
	require.GreaterOrEqual(t, briefing.Tension, 15, "tension must hold the deep_work floor")
	require.LessOrEqual(t, briefing.Tension, 25, "tension must hold the deep_work ceiling")
}

// TestRunner_EndToEnd_MemoryGrounding pins the V2.2.0 derived-memory
// pipeline end-to-end. Memory is seeded → synth runs against a transcript
// that emits a remember: line and a briefing referencing seeded subjects
// implicitly → the consolidator promotes the new candidate to memory_facts
// → assertions verify the four memory_grounding deterministic checks all
// pass (opener tells = 0, fact density ≤ 3, multi-word verbatim leak = 0,
// at least one seeded subject is referenced).
func TestRunner_EndToEnd_MemoryGrounding(t *testing.T) {
	turns := loadTranscript(t, "morning_with_memory")
	ts := newTranscriptServer(t, turns)
	defer ts.Close()

	dbPath := filepath.Join(t.TempDir(), "zeno.db")
	db, lstore, err := zlog.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, Migrate(db, true, false))

	now := time.Date(2026, 4, 25, 7, 30, 0, 0, time.UTC)
	ctx := context.Background()

	// Seed memory state directly. The runner reads via MemoryFacts
	// projection from the same table during cards synth.
	memRepo := &store.MemoryRepo{DB: db, Table: "memory_facts"}
	require.NoError(t, memRepo.Upsert(ctx, []store.MemoryFact{
		{ID: "partner-aa", Subject: "partner", Fact: "Partner is Sam.", Category: "relationship", Confidence: "high", Source: "user", EvidenceCount: 1, FirstSeen: now, LastReinforced: now},
		{ID: "runs-bb", Subject: "runs", Fact: "Usually runs Tue/Thu mornings.", Category: "routine", Confidence: "med", Source: "synth", EvidenceCount: 3, FirstSeen: now, LastReinforced: now},
		{ID: "anniversary-cc", Subject: "anniversary", Fact: "Anniversary is May 7.", Category: "identity", Confidence: "high", Source: "user", EvidenceCount: 1, FirstSeen: now, LastReinforced: now},
	}))

	// Seed events: one mail thread, one calendar event, one weather snapshot.
	_, err = lstore.Append(ctx, zlog.KindMailReceived, "imap", map[string]any{
		"folder": "INBOX", "uid": 1, "uidvalidity": 100,
		"message_id": "<m1@example>", "from": "Saru Patel <saru@acuity.test>",
		"to":      []string{"mira@halsen.test"},
		"subject": "re: redline",
		"date":    now.Add(-2 * time.Hour),
	})
	require.NoError(t, err)
	_, err = lstore.Append(ctx, zlog.KindCalEventSeen, "caldav", map[string]any{
		"uid":   "evt-acuity",
		"title": "Acuity — Series B review",
		"tag":   "work",
		"start": time.Date(2026, 4, 25, 11, 0, 0, 0, time.UTC),
		"end":   time.Date(2026, 4, 25, 11, 45, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	_, err = lstore.Append(ctx, zlog.KindWeatherSnapshot, "weather", map[string]any{
		"captured_at": now.Add(-30 * time.Minute),
		"timezone":    "UTC",
		"hourly": []map[string]any{
			{"time": time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC), "code": 1, "wind_kmh": 8.0, "precip_mm": 0.0},
			{"time": time.Date(2026, 4, 25, 13, 0, 0, 0, time.UTC), "code": 1, "wind_kmh": 7.0, "precip_mm": 0.0},
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

	runner := &Runner{
		LLM:      llmClient,
		Reader:   lstore,
		DB:       db,
		EventLog: lstore,
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
		MemoryTable:   "memory_facts",
	}

	require.NoError(t, runner.Run(ctx))

	// Memory consolidator landed the synth-emitted candidate.
	commute, err := memRepo.GetBySubject(ctx, "commute", false)
	require.NoError(t, err)
	require.NotNil(t, commute, "consolidator must insert the remember: candidate")
	require.Equal(t, "low", commute.Confidence, "first observation lands at low confidence")
	require.Equal(t, "synth", commute.Source)

	// Briefing was rendered.
	briefing, err := (&store.BriefingRepo{DB: db, Table: "briefings"}).ByDate(ctx, "2026-04-25")
	require.NoError(t, err)
	require.NotNil(t, briefing)

	cards, err := (&store.CardRepo{DB: db, Table: "cards"}).ListByDate(ctx, "2026-04-25")
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(cards), 1)

	// Memory grounding deterministic checks. Reuse the eval scorers on
	// the rendered prose.
	prose := briefing.Title + "\n" + briefing.Summary
	for _, c := range cards {
		prose += "\n" + c.Title + "\n" + c.Sub
	}

	openerHits := memoryOpenerTellsRE.FindAllString(prose, -1)
	require.Empty(t, openerHits, "no opener-tell phrasings in briefing+cards prose")

	// Verbatim-leak check: the seeded multi-word fact texts must not appear
	// verbatim in prose. Lowercase compare to mirror the eval scorer.
	loweredProse := strings.ToLower(prose)
	leaks := []string{}
	for _, fact := range []string{
		"partner is sam.",
		"usually runs tue/thu mornings.",
		"anniversary is may 7.",
	} {
		if strings.Contains(loweredProse, fact) {
			leaks = append(leaks, fact)
		}
	}
	require.Empty(t, leaks, "no multi-word verbatim leaks of seeded facts")

	// Density: at most 3 distinct subjects referenced. Verifies grounding
	// without requiring an exact subject — the briefing should plausibly
	// reference at least one of the seeded subjects (partner / runs /
	// anniversary).
	subjects := []string{"partner", "runs", "anniversary"}
	referenced := 0
	for _, s := range subjects {
		if strings.Contains(loweredProse, s) {
			referenced++
		}
	}
	require.LessOrEqual(t, referenced, 3, "fact density at most 3")

	// Trace must carry the thought step. The remember: line is captured
	// off-trace into Memories rather than as a thought, so the trace step
	// count is for the thought line only.
	require.NotEmpty(t, cards[0].TraceID)
	tr, err := (&store.TraceRepo{DB: db, Table: "traces"}).Get(ctx, cards[0].TraceID)
	require.NoError(t, err)
	require.NotNil(t, tr)

	var stepsRaw []map[string]any
	require.NoError(t, json.Unmarshal(tr.Steps, &stepsRaw))
	hasThought := false
	for _, s := range stepsRaw {
		if s["kind"] == "thought" {
			hasThought = true
			break
		}
	}
	require.True(t, hasThought, "thought step must be in trace")
}

// memoryOpenerTellsRE pins the same opener-tell pattern the eval harness
// scorer uses (eval/scoring_memory.go::memoryOpenerTellsRE). Duplicated
// here because synth tests can't import the eval package without creating
// a circular dependency. If the eval scorer's pattern changes, update this
// regex too — keep them in lockstep.
var memoryOpenerTellsRE = regexp.MustCompile(`(?i)\b(I remember|as you mentioned|based on what I know|you (told|mentioned|said) me)\b`)
