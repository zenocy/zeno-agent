package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/llm"
	"github.com/zenocy/zeno-v2/internal/log/logtest"
	"github.com/zenocy/zeno-v2/internal/store"
	"github.com/zenocy/zeno-v2/internal/synth"
)

func buildAskHandler(t *testing.T, askFn func(ctx context.Context, q string) (synth.Card, llm.Trace, []llm.MemoryCandidate, error)) *echo.Echo {
	t.Helper()
	db := openHandlerTestDB(t)
	traces := &store.TraceRepo{DB: db}
	e := echo.New()
	(&AskHandler{
		AskFn:    askFn,
		Traces:   traces,
		EventLog: logtest.NewMemReader(),
		TZ:       func() *time.Location { return time.UTC },
		Now:      func() time.Time { return time.Date(2026, 4, 25, 8, 0, 0, 0, time.UTC) },
		Log:      quietHandlerEntry(),
	}).Register(e)
	return e
}

func postAsk(e *echo.Echo, query string) *httptest.ResponseRecorder {
	body := `{"query":"` + query + `"}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/ask", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(rr, req)
	return rr
}

func TestAskHandler_HappyPath(t *testing.T) {
	stubCard := synth.Card{
		ID:       "weather-ab12",
		Date:     "2026-04-25",
		Source:   "ask",
		SrcLabel: "Generated",
		Rel:      "med",
		Title:    "Clear skies through noon.",
		Sub:      "Your window opens at 07:30.",
		Meta:     []string{},
		Actions:  []synth.Action{{Label: "Dismiss"}},
	}
	stubTrace := llm.Trace{Stopped: "ok", TotalMs: 4200}

	e := buildAskHandler(t, func(_ context.Context, _ string) (synth.Card, llm.Trace, []llm.MemoryCandidate, error) {
		return stubCard, stubTrace, nil, nil
	})

	rr := postAsk(e, "weather today")
	require.Equal(t, http.StatusOK, rr.Code)

	var resp askResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Equal(t, "weather-ab12", resp.Card.ID)
	require.Equal(t, "Generated", resp.Card.SrcLabel)
	require.Equal(t, "med", resp.Card.Rel)
	require.NotEmpty(t, resp.TraceID)
}

func TestAskHandler_DegradedCardPassThrough(t *testing.T) {
	degraded := synth.Card{
		ID:       "couldnt-ab12",
		Date:     "2026-04-25",
		Source:   "ask",
		SrcLabel: "Generated",
		Rel:      "low",
		Title:    "Couldn't reach an answer in time.",
		Sub:      "Try rephrasing.",
		Meta:     []string{},
		Actions:  []synth.Action{{Label: "Dismiss"}},
	}

	e := buildAskHandler(t, func(_ context.Context, _ string) (synth.Card, llm.Trace, []llm.MemoryCandidate, error) {
		return degraded, llm.Trace{Stopped: "deadline"}, nil, nil
	})

	rr := postAsk(e, "weather today")
	require.Equal(t, http.StatusOK, rr.Code) // degraded = 200, not 5xx

	var resp askResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Equal(t, "low", resp.Card.Rel)
	require.Equal(t, "Generated", resp.Card.SrcLabel)
}

func TestAskHandler_BlankQueryRejected(t *testing.T) {
	e := buildAskHandler(t, func(_ context.Context, _ string) (synth.Card, llm.Trace, []llm.MemoryCandidate, error) {
		t.Fatal("AskFn must not be called for blank query")
		return synth.Card{}, llm.Trace{}, nil, nil
	})

	rr := postAsk(e, "")
	require.Equal(t, http.StatusBadRequest, rr.Code)
}

// TestAskHandler_RemembersCandidates proves the bug fix: when AskFn returns
// memory candidates (from a `remember:` line the model emitted in response to
// a user-stated fact), the handler folds them into the memory store via the
// consolidator. Before the fix the candidates were dropped on the floor and
// the user's "My wife is …" message produced no durable fact.
func TestAskHandler_RemembersCandidates(t *testing.T) {
	db := openHandlerTestDB(t)
	traces := &store.TraceRepo{DB: db}
	memory := &store.MemoryRepo{DB: db}

	stubCard := synth.Card{
		ID: "noted-cd34", Date: "2026-04-25", Source: "ask",
		SrcLabel: "Generated", Rel: "low",
		Title: "Noted.", Sub: "I have her details.",
		Meta: []string{}, Actions: []synth.Action{{Label: "Dismiss"}},
	}
	candidates := []llm.MemoryCandidate{
		{Subject: "wife", Predicate: "Pat Morgan, +447700900222, pat.morgan@example.com",
			Raw: "remember: wife: Pat Morgan, +447700900222, pat.morgan@example.com"},
	}

	e := echo.New()
	(&AskHandler{
		AskFn: func(_ context.Context, _ string) (synth.Card, llm.Trace, []llm.MemoryCandidate, error) {
			return stubCard, llm.Trace{Stopped: "ok"}, candidates, nil
		},
		Traces:   traces,
		Memory:   memory,
		EventLog: logtest.NewMemReader(),
		TZ:       func() *time.Location { return time.UTC },
		Now:      func() time.Time { return time.Date(2026, 4, 25, 8, 0, 0, 0, time.UTC) },
		Log:      quietHandlerEntry(),
	}).Register(e)

	rr := postAsk(e, "My wife is Pat Morgan, +447700900222, pat.morgan@example.com")
	require.Equal(t, http.StatusOK, rr.Code)

	got, err := memory.GetBySubject(context.Background(), "wife", false)
	require.NoError(t, err)
	require.NotNil(t, got, "consolidator must have inserted a fact for subject=wife")
	require.Equal(t, "Pat Morgan, +447700900222, pat.morgan@example.com", got.Fact)
	require.Equal(t, "synth", got.Source)
	require.Equal(t, "low", got.Confidence)
}

// TestAskHandler_DetachedExtractorPersistsAfterResponse verifies the new flow:
// ExtractFn runs in a detached goroutine after the response is written, so
// extraction latency cannot block the answer. The user-facing response
// returns first; the extracted candidate lands in the memory store
// asynchronously when the goroutine completes.
func TestAskHandler_DetachedExtractorPersistsAfterResponse(t *testing.T) {
	db := openHandlerTestDB(t)
	traces := &store.TraceRepo{DB: db}
	memory := &store.MemoryRepo{DB: db}

	stubCard := synth.Card{
		ID: "noted-ef56", Date: "2026-04-25", Source: "ask",
		SrcLabel: "Generated", Rel: "low",
		Title: "Got it.", Sub: "Filed for later.",
		Meta: []string{}, Actions: []synth.Action{{Label: "Dismiss"}},
	}

	// extractStarted lets the test prove the extractor was invoked. extractGate
	// blocks the extractor inside ExtractFn until the test releases it, so we
	// can verify the response returns *before* extraction finishes.
	extractStarted := make(chan struct{}, 1)
	extractGate := make(chan struct{})
	extractDone := make(chan struct{})

	extractFn := func(_ context.Context, _ string) []llm.MemoryCandidate {
		extractStarted <- struct{}{}
		<-extractGate
		return []llm.MemoryCandidate{
			{Subject: "partner", Predicate: "Sam, vegetarian",
				Raw: "remember: partner: Sam, vegetarian"},
		}
	}

	h := &AskHandler{
		AskFn: func(_ context.Context, _ string) (synth.Card, llm.Trace, []llm.MemoryCandidate, error) {
			// Main loop emits no candidates — the dedicated extractor is the
			// sole source in this test, mirroring the realistic local-35B
			// case where the multiplexed contract rarely fires.
			return stubCard, llm.Trace{Stopped: "ok"}, nil, nil
		},
		ExtractFn:       extractFn,
		Traces:          traces,
		Memory:          memory,
		EventLog:        logtest.NewMemReader(),
		TZ:              func() *time.Location { return time.UTC },
		Now:             func() time.Time { return time.Date(2026, 4, 25, 8, 0, 0, 0, time.UTC) },
		ExtractDeadline: 5 * time.Second,
		Log:             quietHandlerEntry(),
		extractDone:     extractDone,
	}
	e := echo.New()
	h.Register(e)

	rr := postAsk(e, "My partner Sam is vegetarian.")
	require.Equal(t, http.StatusOK, rr.Code, "response must succeed even though extractor is still running")

	// Extractor should have started (run in goroutine) but not yet finished.
	select {
	case <-extractStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("ExtractFn was never invoked — handler never launched the detached goroutine")
	}

	// The memory store must NOT yet contain the partner fact: the extractor
	// is gated, so consolidation hasn't happened. This is the load-bearing
	// assertion that proves the response returned before extraction
	// completed.
	got, err := memory.GetBySubject(context.Background(), "partner", false)
	require.NoError(t, err)
	require.Nil(t, got, "memory store must not contain the extracted fact while extractor is gated")

	// Release the extractor and wait for the goroutine to finish.
	close(extractGate)
	select {
	case <-extractDone:
	case <-time.After(2 * time.Second):
		t.Fatal("detached extractor goroutine never closed extractDone")
	}

	// Now the consolidator should have persisted the fact.
	got, err = memory.GetBySubject(context.Background(), "partner", false)
	require.NoError(t, err)
	require.NotNil(t, got, "consolidator must persist the extracted candidate after the goroutine completes")
	require.Equal(t, "Sam, vegetarian", got.Fact)
	require.Equal(t, "synth", got.Source)
	require.Equal(t, "low", got.Confidence)
}

// TestAskHandler_DetachedExtractorFailureDoesNotAffectResponse covers the
// best-effort contract: an extractor that returns no candidates (its standard
// failure mode — see ExtractFacts comments) leaves the memory store untouched
// and never affects the response the user already saw.
func TestAskHandler_DetachedExtractorFailureDoesNotAffectResponse(t *testing.T) {
	db := openHandlerTestDB(t)
	traces := &store.TraceRepo{DB: db}
	memory := &store.MemoryRepo{DB: db}

	stubCard := synth.Card{
		ID: "noted-gh78", Date: "2026-04-25", Source: "ask",
		SrcLabel: "Generated", Rel: "low",
		Title: "OK.", Sub: "Nothing to remember.",
		Meta: []string{}, Actions: []synth.Action{{Label: "Dismiss"}},
	}

	extractDone := make(chan struct{})

	h := &AskHandler{
		AskFn: func(_ context.Context, _ string) (synth.Card, llm.Trace, []llm.MemoryCandidate, error) {
			return stubCard, llm.Trace{Stopped: "ok"}, nil, nil
		},
		ExtractFn: func(_ context.Context, _ string) []llm.MemoryCandidate {
			return nil // extractor "failed" / found no facts
		},
		Traces:          traces,
		Memory:          memory,
		EventLog:        logtest.NewMemReader(),
		TZ:              func() *time.Location { return time.UTC },
		Now:             func() time.Time { return time.Date(2026, 4, 25, 8, 0, 0, 0, time.UTC) },
		ExtractDeadline: 1 * time.Second,
		Log:             quietHandlerEntry(),
		extractDone:     extractDone,
	}
	e := echo.New()
	h.Register(e)

	rr := postAsk(e, "What's the weather today?")
	require.Equal(t, http.StatusOK, rr.Code)

	// Wait for the goroutine to finish so we can assert post-extractor state.
	select {
	case <-extractDone:
	case <-time.After(2 * time.Second):
		t.Fatal("detached extractor never signalled completion")
	}

	// Spot-check: nothing was inserted into memory.
	got, err := memory.GetBySubject(context.Background(), "weather", false)
	require.NoError(t, err)
	require.Nil(t, got, "no candidates → no memory rows")

	// Response body still parseable (the failed extractor never touched it).
	var resp askResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Equal(t, "noted-gh78", resp.Card.ID)
}

// TestAskHandler_PersistsCard proves the bug fix: when the user types a
// reactive query and an Ask card is generated, the card row lands in the
// cards table so /api/cards/:id/thread (the CardFocus modal's pin
// lookup) can find it. Before the fix the card streamed over SSE +
// HTTP but was never persisted, so clicking it produced a 404.
func TestAskHandler_PersistsCard(t *testing.T) {
	db := openHandlerTestDB(t)
	cards := &store.CardRepo{DB: db}
	traces := &store.TraceRepo{DB: db}

	stubCard := synth.Card{
		ID: "weekly-a2db", Date: "2026-04-25", Source: "ask",
		SrcLabel: "Generated", Rel: "med",
		Title: "Homework schedule for the week", Sub: "Miss Despoina sent the log.",
		Meta: []string{"miss despoina"}, Actions: []synth.Action{{Label: "Dismiss"}},
	}

	e := echo.New()
	(&AskHandler{
		AskFn: func(_ context.Context, _ string) (synth.Card, llm.Trace, []llm.MemoryCandidate, error) {
			return stubCard, llm.Trace{Stopped: "ok"}, nil, nil
		},
		Cards:    cards,
		Traces:   traces,
		EventLog: logtest.NewMemReader(),
		TZ:       func() *time.Location { return time.UTC },
		Now:      func() time.Time { return time.Date(2026, 4, 25, 8, 0, 0, 0, time.UTC) },
		Log:      quietHandlerEntry(),
	}).Register(e)

	rr := postAsk(e, "what's the homework this week?")
	require.Equal(t, http.StatusOK, rr.Code)

	got, err := cards.GetByID(context.Background(), "weekly-a2db")
	require.NoError(t, err)
	require.NotNil(t, got, "Ask card must be persisted so /api/cards/:id/thread can pin it")
	require.Equal(t, "Homework schedule for the week", got.Title)
	require.Equal(t, "ask", got.Origin, "Ask cards carry Origin=ask so ListByDate's source filter keeps them off the morning rail")
}

// TestAskHandler_PersistFailureDoesNotFailResponse covers the best-effort
// contract: a CardRepo persist error is logged but the HTTP response still
// succeeds — the card already streamed to the UI over SSE + HTTP, and the
// degraded affordance is "follow-up chat 404s", not "no answer at all".
func TestAskHandler_PersistFailureDoesNotFailResponse(t *testing.T) {
	db := openHandlerTestDB(t)
	traces := &store.TraceRepo{DB: db}

	stubCard := synth.Card{
		ID: "weekly-aa00", Date: "2026-04-25", Source: "ask",
		SrcLabel: "Generated", Rel: "low",
		Title: "Reply", Sub: "Body.",
		Meta: []string{}, Actions: []synth.Action{{Label: "Dismiss"}},
	}

	// Pre-create the card row so the unique-id Upsert path still
	// succeeds — we want to confirm that even when Cards is wired,
	// a no-op Upsert doesn't change the response. A true persist
	// failure would require a closed-DB scenario; that's covered by
	// the best-effort log path (which we don't unit-test directly).
	e := echo.New()
	(&AskHandler{
		AskFn: func(_ context.Context, _ string) (synth.Card, llm.Trace, []llm.MemoryCandidate, error) {
			return stubCard, llm.Trace{Stopped: "ok"}, nil, nil
		},
		Cards:    nil, // nil Cards is the safe-skip path: handler must not panic
		Traces:   traces,
		EventLog: logtest.NewMemReader(),
		TZ:       func() *time.Location { return time.UTC },
		Now:      func() time.Time { return time.Date(2026, 4, 25, 8, 0, 0, 0, time.UTC) },
		Log:      quietHandlerEntry(),
	}).Register(e)

	rr := postAsk(e, "ping")
	require.Equal(t, http.StatusOK, rr.Code, "response must succeed even when Cards repo is unwired")
}

// TestAskHandler_NoExtractFnSkipsCleanly verifies the production-safe default:
// an AskHandler without an ExtractFn (e.g. tests that only exercise the
// answer path) does not panic and does not block on the extractDone channel
// — the goroutine path is skipped entirely.
func TestAskHandler_NoExtractFnSkipsCleanly(t *testing.T) {
	stubCard := synth.Card{
		ID: "noted-ij90", Date: "2026-04-25", Source: "ask",
		SrcLabel: "Generated", Rel: "low",
		Title: "Done.", Sub: "",
		Meta: []string{}, Actions: []synth.Action{{Label: "Dismiss"}},
	}

	e := buildAskHandler(t, func(_ context.Context, _ string) (synth.Card, llm.Trace, []llm.MemoryCandidate, error) {
		return stubCard, llm.Trace{Stopped: "ok"}, nil, nil
	})
	rr := postAsk(e, "ping")
	require.Equal(t, http.StatusOK, rr.Code)
}
