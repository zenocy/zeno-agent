package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
	"github.com/zenocy/zeno-v2/internal/idgen"
	"gorm.io/datatypes"

	"github.com/zenocy/zeno-v2/internal/embeddings"
	"github.com/zenocy/zeno-v2/internal/eventbus"
	"github.com/zenocy/zeno-v2/internal/llm"
	logp "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/store"
	"github.com/zenocy/zeno-v2/internal/synth"
)

// AskHandler answers POST /api/ask.
type AskHandler struct {
	AskFn func(ctx context.Context, query string) (synth.Card, llm.Trace, []llm.MemoryCandidate, error)
	// ExtractFn runs the dedicated `remember:` extractor against the user's
	// query. Optional; nil → no extraction (e.g. tests). Invoked from a
	// detached goroutine after the HTTP response is written so extraction
	// latency never affects the answer's user-perceived latency.
	ExtractFn func(ctx context.Context, query string) []llm.MemoryCandidate
	// Cards persists the produced Ask card so the CardFocus modal's
	// /api/cards/:id/thread lookup resolves. Optional; nil → no
	// persistence (the card still streams to the UI but follow-up
	// chat will 404). Cards persisted here carry Origin="ask" and are
	// kept out of the morning rail by ListByDate's `source != 'ask'`
	// filter.
	Cards           *store.CardRepo
	Traces          *store.TraceRepo
	Memory          *store.MemoryRepo       // optional; nil → no consolidation (e.g. tests)
	EmbeddingStore  *embeddings.Store       // optional; updates vector cache after `remember:` consolidation
	EmbeddingIndex  *embeddings.MemoryIndex // optional; same
	EventLog        logp.Writer
	TZ              func() *time.Location
	Now             func() time.Time
	Deadline        time.Duration // HTTP outer deadline; 0 → 46s (1s over the 45s loop default)
	ExtractDeadline time.Duration // detached extractor budget; 0 → reuse Deadline (or 45s default)
	Log             *logrus.Entry
	// Bus is the V2.4 typed eventbus. When non-nil, ask publishes
	// synth.started / trace.step* / synth.delta* / synth.completed /
	// card.appended events for SSE consumers. nil → no live events.
	Bus *eventbus.Bus
	// extractDone is closed by the detached extractor goroutine after it
	// finishes (regardless of outcome). Tests use it to wait for the
	// asynchronous consolidation deterministically. Production paths leave it
	// nil — the goroutine simply runs to completion in the background.
	extractDone chan<- struct{}
}

// Register attaches the ask route to the Echo instance.
func (h *AskHandler) Register(e *echo.Echo) {
	e.POST("/api/ask", h.ask)
}

type askRequest struct {
	Query string `json:"query"`
}

type askResponse struct {
	Card    cardDTO `json:"card"`
	TraceID string  `json:"trace_id"`
}

func (h *AskHandler) ask(c echo.Context) error {
	var req askRequest
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return BadRequest(c, "invalid request body")
	}
	if req.Query == "" {
		return BadRequest(c, "query is required")
	}

	tz := tzFrom(h.TZ)
	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}
	date := now.In(tz).Format("2006-01-02")

	httpDeadline := h.Deadline + time.Second
	if h.Deadline <= 0 {
		httpDeadline = 46 * time.Second
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), httpDeadline)
	defer cancel()

	// Lift runID before AskFn so the run-boundary events on the bus
	// share an id with the persisted trace and the card.
	runID := idgen.New()
	if h.Bus != nil {
		h.Bus.Publish(eventbus.SynthStartedEvent{
			RunID: runID, Stage: "ask", Date: date,
		})
	}
	// Ask returns a json_schema-constrained Card; streaming body tokens
	// would dump raw `{"id":"...","title":"..."}` JSON into the live
	// panel. Trace steps still flow.
	askCtx := synth.AttachLiveTrace(ctx, h.Bus, runID, "ask")

	card, trace, memories, err := h.AskFn(askCtx, req.Query)
	if err != nil && h.Log != nil {
		h.Log.WithError(err).WithField("query", req.Query).Warn("ask: synthesis error")
	}

	if card.ID == "" {
		card.ID = "ask-" + runID[:8]
	}
	card.Date = date

	if persistErr := h.persist(c.Request().Context(), runID, date, trace); persistErr != nil && h.Log != nil {
		h.Log.WithError(persistErr).Warn("ask: persist trace failed")
	}

	// Persist the Ask card so /api/cards/:id/thread (the CardFocus
	// modal) can pin it for follow-up turns. Best-effort: a failure
	// here doesn't fail the response — the card still reaches the UI
	// over SSE + HTTP, only the chat affordance degrades.
	if h.Cards != nil && err == nil {
		if upErr := h.Cards.Upsert(c.Request().Context(), []store.Card{storeCard(card, runID, date)}); upErr != nil && h.Log != nil {
			h.Log.WithError(upErr).WithField("card_id", card.ID).Warn("ask: persist card failed")
		}
	}

	if h.EventLog != nil {
		_, _ = h.EventLog.Append(c.Request().Context(), logp.KindUserAsk, "ui", map[string]any{
			"query":   req.Query,
			"card_id": card.ID,
		})
	}

	// Best-effort: fold any `remember:` candidates the model emitted into the
	// durable memory store. Mirrors synth.Runner.consolidateMemoryAudit —
	// failures are logged but never fail the response. The main answer loop
	// rarely emits `remember:` lines on local 35B models (the multiplexed
	// contract is unreliable); the dedicated extractor below is the primary
	// source of memory candidates.
	h.consolidateMemoryAudit(c.Request().Context(), runID, date, memories)

	// Kick off the dedicated extractor in a detached goroutine. The goroutine
	// runs with a fresh context derived from context.Background() (not the
	// request context, which dies when c.JSON returns), capped at
	// ExtractDeadline. This decouples extraction latency from the user-
	// perceived answer latency: a fast answer no longer kills a near-finished
	// extraction, and a slow extraction never delays the response.
	h.runDetachedExtractor(runID, date, req.Query)

	// Publish synth.completed + card.appended on the bus only when the
	// synthesis itself succeeded. A degraded card from a failed AskFn
	// is still returned to the HTTP client (V2.3 contract) but the
	// bus stays silent so the LiveSynthPanel doesn't claim a normal run.
	if h.Bus != nil && err == nil {
		h.Bus.Publish(eventbus.SynthCompletedEvent{
			RunID: runID, Stage: "ask",
			Stopped: trace.Stopped, TotalMs: trace.TotalMs,
		})
		h.Bus.PublishCard(storeCard(card, runID, date))
	}

	return c.JSON(http.StatusOK, askResponse{
		Card:    toCardDTO(storeCard(card, runID, date)),
		TraceID: runID,
	})
}

// runDetachedExtractor launches the extractor in a background goroutine. The
// extractor's context is rooted at context.Background() so it survives the
// request goroutine returning. Errors and outcomes are logged but never bubble
// anywhere — extraction is best-effort decoration on top of the answer card.
//
// The same runID is reused for the consolidator call so the audit log links
// the extracted memory back to the originating ask. Consolidate's idempotency
// (LastSeenRunID) safely handles the rare case where the main answer loop
// also emitted a candidate for the same subject — the second consolidate
// call simply skips that subject.
func (h *AskHandler) runDetachedExtractor(runID, date, query string) {
	if h.ExtractFn == nil {
		// Signal completion for tests that wired up extractDone but no
		// ExtractFn (defensive — shouldn't happen in production).
		if h.extractDone != nil {
			close(h.extractDone)
		}
		return
	}

	deadline := h.ExtractDeadline
	if deadline <= 0 {
		deadline = h.Deadline
	}
	if deadline <= 0 {
		deadline = 45 * time.Second
	}

	log := h.Log
	if log != nil {
		log = log.WithField("step", "extract").WithField("run_id", runID)
	}

	go func() {
		if h.extractDone != nil {
			defer close(h.extractDone)
		}
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		defer cancel()

		started := time.Now()
		if log != nil {
			log.WithField("deadline_ms", deadline.Milliseconds()).
				Info("extract: detached run started")
		}

		candidates := h.ExtractFn(ctx, query)
		if log != nil {
			log.WithField("count", len(candidates)).
				WithField("ms", time.Since(started).Milliseconds()).
				Info("extract: detached run complete")
		}
		if len(candidates) == 0 {
			return
		}
		h.consolidateMemoryAudit(ctx, runID, date, candidates)
	}()
}

// consolidateMemoryAudit emits memory.candidates, runs the deterministic
// consolidator, and emits memory.consolidated. Best-effort throughout — a
// nil Memory repo (test wiring) or any consolidator error is logged and
// swallowed so the user always gets their card back.
func (h *AskHandler) consolidateMemoryAudit(ctx context.Context, runID, date string, candidates []llm.MemoryCandidate) {
	if len(candidates) == 0 || h.Memory == nil {
		return
	}
	if h.EventLog != nil {
		_, _ = h.EventLog.Append(ctx, logp.KindMemoryCandidates, "ask", map[string]any{
			"run_id":     runID,
			"date":       date,
			"candidates": candidates,
		})
	}
	delta, err := synth.Consolidate(ctx, synth.ConsolidateDeps{
		Repo:           h.Memory,
		EmbeddingStore: h.EmbeddingStore,
		EmbeddingIndex: h.EmbeddingIndex,
		Now:            h.Now,
		Logger:         h.Log,
	}, synth.ConsolidateConfig{}, runID, candidates)
	if err != nil {
		if h.Log != nil {
			h.Log.WithError(err).Warn("ask: memory consolidator failed (best-effort, response continues)")
		}
		if h.EventLog != nil {
			_, _ = h.EventLog.Append(ctx, logp.KindMemoryConsolidateFailed, "ask", map[string]any{
				"run_id": runID,
				"date":   date,
				"error":  err.Error(),
			})
		}
		return
	}
	if h.EventLog != nil {
		_, _ = h.EventLog.Append(ctx, logp.KindMemoryConsolidated, "ask", map[string]any{
			"run_id": runID,
			"date":   date,
			"delta":  delta,
		})
	}
}

// persist writes the trace row only. Reactive ask cards are ephemeral —
// they live in UI state for the session and are not stored alongside
// morning-synth cards (which would conflict on the (date, kind, source)
// uniqueness invariant). The trace_id returned to the UI is the lookup key
// for /api/traces/:id.
func (h *AskHandler) persist(ctx context.Context, runID, date string, trace llm.Trace) error {
	stepsJSON, _ := json.Marshal(trace.Steps)
	traceRow := store.Trace{
		ID:        runID,
		RunID:     runID,
		Date:      date,
		Stopped:   trace.Stopped,
		TotalMs:   trace.TotalMs,
		Steps:     datatypes.JSON(stepsJSON),
		CreatedAt: time.Now(),
	}
	return h.Traces.Create(ctx, traceRow)
}

// storeCard converts a synth.Card into a store.Card stub for DTO conversion.
// Origin is stamped "ask" so the V2.4 React UI can route the SSE-delivered
// card into its Generated section (filtering by `card.origin === "ask"`).
func storeCard(c synth.Card, runID, date string) store.Card {
	metaJSON, _ := json.Marshal(c.Meta)
	actionsJSON, _ := json.Marshal(c.Actions)
	var expandJSON datatypes.JSON
	if len(c.Expand) > 0 {
		expandJSON, _ = json.Marshal(c.Expand)
	}
	return store.Card{
		ID:       c.ID,
		Date:     date,
		Kind:     c.Kind,
		Source:   c.Source,
		SrcLabel: c.SrcLabel,
		Rel:      c.Rel,
		Origin:   "ask",
		Title:    c.Title,
		Sub:      c.Sub,
		Meta:     datatypes.JSON(metaJSON),
		Actions:  datatypes.JSON(actionsJSON),
		Expand:   expandJSON,
		TraceID:  runID,
	}
}
