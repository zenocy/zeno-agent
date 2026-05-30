package synth

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/zenocy/zeno-v2/internal/idgen"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/embeddings"
	"github.com/zenocy/zeno-v2/internal/eventbus"
	"github.com/zenocy/zeno-v2/internal/jina"
	"github.com/zenocy/zeno-v2/internal/llm"
	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/projection"
	"github.com/zenocy/zeno-v2/internal/store"
)

// Runner orchestrates one synth pass: cards via tool loop, briefing via
// single literary call, and a transactional persist of cards + briefing +
// trace. The configurable Tables fields route writes to either the prod
// tables ("cards"/"briefings"/"traces") or the replay siblings.
//
// Timeouts subdivide the parent context into explicit per-stage budgets so a
// slow briefing call can't starve the cards persistence; on briefing failure
// the cards are persisted anyway with a degraded-briefing fallback row.
type Runner struct {
	LLM      llm.Provider
	Reader   log.Reader
	Tasks    *store.TaskRepo // V2.11: backs read_tasks; nil → tool returns empty
	DB       *gorm.DB
	EventLog log.Writer
	ProjCfg  projection.Config
	Prompts  *PromptSet
	Now      func() time.Time
	Logger   *logrus.Entry

	// Persistence targets. Empty falls back to the prod table names.
	CardsTable     string
	BriefingTable  string
	TraceTable     string
	MemoryTable    string // memory_facts (prod) or memory_facts_replay (replay)
	EmbeddingTable string // memory_embeddings (prod) or memory_embeddings_replay (replay)

	// MemoryRanker re-ranks the candidate pool by query relevance for the
	// reactive Ask path and by today's signals for cards. nil → V2.2.0
	// ListTop ordering for both paths.
	MemoryRanker *MemoryRanker

	// EmbeddingStore + EmbeddingIndex keep the relevance index in step with
	// consolidator writes. Both nil → consolidator runs as before with no
	// embedding-side work. Both must be non-nil to take effect.
	EmbeddingStore *embeddings.Store
	EmbeddingIndex *embeddings.MemoryIndex

	// Per-stage timeouts. Zero values fall back to defaults
	// (cards 30s, briefing 45s, tool 5s).
	CardsTimeout    time.Duration
	BriefingTimeout time.Duration
	ToolTimeout     time.Duration
	// FinalCallBudget caps the loop's final wrap-up LLM call (the call
	// that produces the answer after MaxIterations is reached). Zero
	// falls back to llm.LoopConfig default (15s).
	FinalCallBudget time.Duration

	// Cards-loop iteration cap. Zero falls back to 6.
	CardsMaxIterations int

	// BriefingRetryDelay is the wait before a single best-effort retry of a
	// degraded briefing. Zero falls back to 5 minutes. Tests override to a
	// short duration.
	BriefingRetryDelay time.Duration

	// InjectSignal is set only by Phase 3's inject pipeline (or by the eval
	// harness via fixture inject_signal block). nil for production morning
	// runs. When non-nil, the V2.3 state detector picks StateMessageInject.
	InjectSignal *InjectSignal

	// Bus is the V2.4 typed eventbus. When non-nil, Run publishes
	// synth.started / trace.step* / synth.delta* / synth.completed /
	// card.appended events for SSE consumers. Eval and replay paths leave
	// this nil; the runner behaves byte-equal to V2.3 in that case.
	Bus *eventbus.Bus

	// V2.5.0 Phase 3: optional concern wiring. When both repos are
	// non-nil, the runner computes ActiveConcerns once at the top of
	// Run, threads it into both CardsDeps and BriefingDeps for prompt
	// rendering, and lets the cards inheritance pass attach
	// `Card.ConcernID` from the join. Nil → byte-equal V2.4 behavior.
	Concerns            *store.ConcernRepo
	ConcernObservations *store.ConcernObservationRepo

	// LoopObserver receives per-LLM-call / tool-dispatch / repair / loop
	// iteration counts so the synth runner can record Prometheus metrics
	// without leaking the metrics package into internal/llm. Nil disables.
	LoopObserver llm.LoopObserver
	// OnSynthRun is invoked once per stage with (stage, outcome, dur).
	// stage ∈ {"cards","briefing","morning"}. outcome ∈ {"ok","degraded","failed"}.
	OnSynthRun func(stage, outcome string, dur time.Duration)

	// V2.6: optional Jina web tools threaded into the cards loop.
	JinaClient JinaClient
	JinaCache  *jina.Store
	SearchTTL  time.Duration
	ReadTTL    time.Duration

	// Phase 4: optional watchlist source. When non-nil the cards loop
	// computes MarketsContext and the model can emit a markets card
	// on interesting days. Nil → byte-equal pre-Phase-4 behavior.
	Tickers projection.TickerSource

	// V2.8.1: action vocabulary the runner advertises to the LLM.
	// See cards.go for the WiredIntent type. Empty → the prompt's
	// "Action verbs" section renders empty and post-process drop is
	// a no-op (matches replay/eval where no action registry exists).
	WiredIntents []WiredIntent

	// V2.13.0: assistant-mode wiring. ExpectedReplies is consulted at
	// the top of Run to populate ProposalSuppress (event UIDs the cards
	// loop must NOT re-propose a "text X to confirm" card for). Nil
	// disables the suppress block — matches eval/replay paths.
	ExpectedReplies *store.ExpectedReplyRepo
}

// Run executes one synth pass. Boundary events synth.run_started /
// synth.run_completed / synth.failed are appended to the observation log.
//
// Per-stage budgets: cards and briefing each get their own context with
// timeouts derived from CardsTimeout / BriefingTimeout (defaulting to 30s and
// 45s). On briefing failure the cards are still persisted with a degraded-
// briefing fallback row so the user wakes up to something usable.
func (r *Runner) Run(ctx context.Context) (retErr error) {
	now := r.now()
	tz := r.ProjCfg.TZ
	if tz == nil {
		tz = time.UTC
	}
	date := now.In(tz).Format("2006-01-02")
	runID := idgen.New()

	logger := r.Logger
	if logger == nil {
		logger = logrus.NewEntry(logrus.New())
	}
	logger = logger.WithField("run_id", runID).WithField("date", date)
	logger.Info("synth: run starting")

	runStart := time.Now()
	briefingDegraded := false
	defer func() {
		if r.OnSynthRun == nil {
			return
		}
		outcome := "ok"
		if retErr != nil {
			outcome = "failed"
		} else if briefingDegraded {
			outcome = "degraded"
		}
		r.OnSynthRun("morning", outcome, time.Since(runStart))
	}()

	cardsTimeout := r.CardsTimeout
	if cardsTimeout <= 0 {
		cardsTimeout = 30 * time.Second
	}
	briefingTimeout := r.BriefingTimeout
	if briefingTimeout <= 0 {
		briefingTimeout = 45 * time.Second
	}
	toolTimeout := r.ToolTimeout
	if toolTimeout <= 0 {
		toolTimeout = 5 * time.Second
	}

	r.appendEvent(ctx, log.KindSynthRunStarted, map[string]any{"run_id": runID, "date": date})

	if r.Bus != nil {
		r.Bus.Publish(eventbus.SynthStartedEvent{RunID: runID, Stage: "morning", Date: date})
	}

	// --- Stage 0: state detection (before cards so cards_system.tmpl can
	// render the per-state bias overlay). Pure-Go inference from today's
	// projections plus the optional inject signal; persists on the briefing
	// row so the UI (Phase 4) and eval harness can read it back. State is
	// also passed into BriefingDeps so the briefing template's StateVoice
	// overlay renders. ---
	state := r.detectState(ctx, now, date, logger)

	// V2.5.0 Phase 3: compute ActiveConcerns once and share across cards
	// + briefing. Skipped cleanly when concerns wiring is absent — V2.4
	// behavior holds byte-equal.
	var activeConcerns []projection.Concern
	if r.Concerns != nil {
		concerns, cerr := projection.ActiveConcerns{
			Repo:    r.Concerns,
			TagRepo: r.ConcernObservations,
			Config:  projection.ActiveConcernsConfig{Limit: 5},
		}.Compute(ctx, r.Reader)
		if cerr != nil {
			logger.WithError(cerr).Warn("synth: active concerns projection failed; surfacing without")
		} else {
			activeConcerns = concerns
		}
	}

	// --- Stage 1: cards (with per-stage budget). ---
	// Cards run in json_schema mode — streaming body tokens would be raw
	// JSON noise in the live panel. Trace steps still flow.
	cardsCtx, cardsCancel := context.WithTimeout(ctx, cardsTimeout)
	cardsCtx = AttachLiveTrace(cardsCtx, r.Bus, runID, "cards")
	releaseCardsLive := func() {}
	cardsStart := time.Now()
	memoryRepo := &store.MemoryRepo{DB: r.DB, Table: defaultIfEmpty(r.MemoryTable, "memory_facts")}

	// V2.13.0: gather event UIDs that already have an assistant-mode
	// proposal in flight so the cards prompt can be told not to re-emit.
	// Best-effort: a lookup error is logged and we proceed without
	// suppression rather than blocking the morning.
	var proposalSuppress []string
	if r.ExpectedReplies != nil {
		ids, err := r.ExpectedReplies.OpenContextIDs(cardsCtx, now)
		if err != nil {
			logger.WithError(err).Warn("synth: open_context_ids lookup failed; proceeding without suppress block")
		} else {
			proposalSuppress = ids
		}
	}
	// V2.x: entities surfaced in the last week feed the continuity prompt
	// block so the model updates/suppresses rather than duplicating.
	// Best-effort: a lookup error proceeds without the block.
	var recentCards []store.RecentEntity
	{
		cardsRepo := &store.CardRepo{DB: r.DB, Table: defaultIfEmpty(r.CardsTable, "cards")}
		sinceDate := now.AddDate(0, 0, -7).Format("2006-01-02")
		if rc, rcErr := cardsRepo.ListRecentEntities(cardsCtx, sinceDate); rcErr != nil {
			logger.WithError(rcErr).Warn("synth: recent entities lookup failed; proceeding without continuity block")
		} else {
			recentCards = rc
		}
	}

	cardSet, trace, memCandidates, err := SynthesizeCards(cardsCtx, CardsDeps{
		LLM:                    r.LLM,
		Reader:                 r.Reader,
		Tasks:                  r.Tasks,
		ProjCfg:                r.ProjCfg,
		Memory:                 memoryRepo,
		MemoryRanker:           r.MemoryRanker,
		Prompts:                r.Prompts,
		Date:                   date,
		Now:                    now,
		State:                  state,
		Logger:                 logger.WithField("step", "cards"),
		LoopTimeout:            cardsTimeout,
		ToolTimeout:            toolTimeout,
		FinalCallBudget:        r.FinalCallBudget,
		MaxIterations:          r.CardsMaxIterations,
		Concerns:               activeConcerns,
		ConcernRepo:            r.Concerns,
		ConcernObservationRepo: r.ConcernObservations,
		LoopObserver:           r.LoopObserver,
		JinaClient:             r.JinaClient,
		JinaCache:              r.JinaCache,
		SearchTTL:              r.SearchTTL,
		ReadTTL:                r.ReadTTL,
		MarketsCtx:             projection.MarketsContext{Cfg: r.ProjCfg, Tickers: r.Tickers},
		WiredIntents:           r.WiredIntents,
		ProposalSuppress:       proposalSuppress,
		RecentCards:            recentCards,
	})
	releaseCardsLive()
	cardsCancel()
	cardsDur := time.Since(cardsStart)
	if err != nil {
		if r.OnSynthRun != nil {
			r.OnSynthRun("cards", "failed", cardsDur)
		}
		r.appendDetachedEvent(log.KindSynthFailed, map[string]any{
			"run_id": runID, "date": date, "stage": "cards", "error": err.Error(),
		})
		return fmt.Errorf("cards: %w", err)
	}
	if r.OnSynthRun != nil {
		r.OnSynthRun("cards", "ok", cardsDur)
	}
	logger.WithField("ms", time.Since(cardsStart).Milliseconds()).
		WithField("count", len(cardSet.Cards)).
		WithField("trace_stopped", trace.Stopped).
		Info("synth: cards done")

	// --- Stage 2: briefing (with per-stage budget). On failure we DO NOT
	// abort the run — we persist cards plus a degraded-briefing fallback so
	// the user has something to wake up to. ---
	briefingCtx, briefingCancel := context.WithTimeout(ctx, briefingTimeout)
	briefingCtx, releaseBriefingLive := AttachLivePublishers(briefingCtx, r.Bus, runID, "briefing")
	briefingStart := time.Now()
	briefing, briefingErr := SynthesizeBriefing(briefingCtx, BriefingDeps{
		LLM:          r.LLM,
		Prompts:      r.Prompts,
		Date:         date,
		State:        state,
		Logger:       logger.WithField("step", "briefing"),
		Concerns:     activeConcerns,
		LoopObserver: r.LoopObserver,
	}, cardSet)
	releaseBriefingLive()
	briefingCancel()
	briefingDur := time.Since(briefingStart)
	if briefingErr != nil {
		r.appendDetachedEvent(log.KindSynthFailed, map[string]any{
			"run_id": runID, "date": date, "stage": "briefing", "error": briefingErr.Error(),
		})
		logger.WithError(briefingErr).Warn("synth: briefing failed — persisting cards with degraded briefing")
		briefing = degradedBriefing(date, len(cardSet.Cards))
		briefingDegraded = true
		if r.OnSynthRun != nil {
			r.OnSynthRun("briefing", "degraded", briefingDur)
		}
	} else {
		logger.WithField("ms", briefingDur.Milliseconds()).
			WithField("tension", briefing.Tension).
			Info("synth: briefing done")
		if r.OnSynthRun != nil {
			r.OnSynthRun("briefing", "ok", briefingDur)
		}
	}

	// --- Stage 3: persist (uses parent ctx, no extra timeout — DB writes are local). ---
	if err := r.persist(ctx, runID, date, state, cardSet, briefing, trace); err != nil {
		r.appendDetachedEvent(log.KindSynthFailed, map[string]any{
			"run_id": runID, "date": date, "stage": "persist", "error": err.Error(),
		})
		return fmt.Errorf("persist: %w", err)
	}

	// --- Stage 3b: derived-memory consolidation (best-effort — never fails
	// the run). The cards loop's `remember:` extractor produced
	// memCandidates above; we audit them in memory.candidates, fold them
	// into the durable store via the deterministic consolidator, and audit
	// the deltas in memory.consolidated. On failure, log warn + audit row
	// and continue. ---
	r.consolidateMemoryAudit(ctx, runID, date, memoryRepo, memCandidates, logger)

	if r.Bus != nil {
		// synth.completed publishes BEFORE the card.appended fan-out so the
		// UI's LiveSynthPanel can begin its 600ms dissolve before cards
		// prepend into the grid (Phase 3's settle behavior). card.appended
		// remains byte-equal to V2.3 (PublishCard wraps in CardAppendedEvent).
		r.Bus.Publish(eventbus.SynthCompletedEvent{
			RunID: runID, Stage: "morning",
			Stopped: trace.Stopped, TotalMs: trace.TotalMs,
		})
		for _, c := range cardSet.Cards {
			r.Bus.PublishCard(toStoreCard(c, runID, date))
		}
	}

	r.appendEvent(ctx, log.KindSynthRunCompleted, map[string]any{
		"run_id":            runID,
		"date":              date,
		"card_count":        len(cardSet.Cards),
		"tension":           briefing.Tension,
		"briefing_degraded": briefingDegraded,
	})
	if briefingDegraded {
		retryDelay := r.BriefingRetryDelay
		if retryDelay <= 0 {
			retryDelay = 5 * time.Minute
		}
		r.appendEvent(ctx, log.KindSynthBriefingRetryScheduled, map[string]any{
			"run_id":   runID,
			"date":     date,
			"retry_at": r.now().Add(retryDelay).UTC().Format(time.RFC3339),
		})
		go r.scheduleBriefingRetry(runID, date, cardSet, retryDelay, briefingTimeout, logger)
		logger.Info("synth: run completed (briefing degraded; retry scheduled)")
	} else {
		logger.Info("synth: run completed")
	}
	return nil
}

// scheduleBriefingRetry sleeps for delay then attempts the briefing one more
// time. On success, the briefing row is replaced in place and a
// synth.briefing_retry_completed{success: true} event is appended. On failure,
// the same event is appended with success: false and the original degraded
// row stays in place. Single-shot — no second retry.
//
// Uses a fresh context.Background() because the parent cron tick has long
// since returned by the time the retry fires.
func (r *Runner) scheduleBriefingRetry(runID, date string, cardSet CardSet, delay, briefingTimeout time.Duration, logger *logrus.Entry) {
	time.Sleep(delay)

	ctx, cancel := context.WithTimeout(context.Background(), briefingTimeout)
	defer cancel()

	briefing, err := SynthesizeBriefing(ctx, BriefingDeps{
		LLM:     r.LLM,
		Prompts: r.Prompts,
		Date:    date,
		Logger:  logger.WithField("step", "briefing_retry"),
	}, cardSet)
	payload := map[string]any{"run_id": runID, "date": date, "success": err == nil}
	if err != nil {
		payload["error"] = err.Error()
		logger.WithError(err).Warn("synth: briefing retry failed")
		r.appendEvent(context.Background(), log.KindSynthBriefingRetryCompleted, payload)
		return
	}
	if persistErr := r.persistBriefingOnly(ctx, runID, date, briefing); persistErr != nil {
		payload["success"] = false
		payload["error"] = persistErr.Error()
		logger.WithError(persistErr).Warn("synth: briefing retry persist failed")
		r.appendEvent(context.Background(), log.KindSynthBriefingRetryCompleted, payload)
		return
	}
	logger.WithField("tension", briefing.Tension).Info("synth: briefing retry succeeded")
	r.appendEvent(context.Background(), log.KindSynthBriefingRetryCompleted, payload)
}

// persistBriefingOnly replaces just the briefing row for date. Cards and
// trace from the original run are untouched. Used by the retry path.
func (r *Runner) persistBriefingOnly(ctx context.Context, runID, date string, b Briefing) error {
	briefTbl := defaultIfEmpty(r.BriefingTable, "briefings")
	row := store.Briefing{
		Date:              date,
		Eyebrow:           b.Eyebrow,
		Title:             b.Title,
		Summary:           b.Summary,
		Tension:           b.Tension,
		SuggestedFollowup: b.SuggestedFollowup,
		RunID:             runID,
		CreatedAt:         time.Now(),
	}
	return (&store.BriefingRepo{DB: r.DB, Table: briefTbl}).UpsertMorning(ctx, row)
}

// degradedBriefing returns a placeholder briefing used when the briefing call
// fails. The voice intentionally signals "something failed" to the user
// without surfacing a raw error — they get a usable surface (the cards) plus
// a one-line acknowledgement.
func degradedBriefing(date string, cardCount int) Briefing {
	return Briefing{
		Date:    date,
		Eyebrow: "draft pending",
		Title:   "Cards ready. *Briefing* in retry.",
		Summary: fmt.Sprintf("Zeno couldn't compose a briefing on the first pass — the %d cards below are valid.", cardCount),
		Tension: 50,
	}
}

// toStoreCard converts a synth.Card into a store.Card row tagged with the
// run's id + date. Used by both the persist transaction and the V2.4 SSE
// publish path so the wire shape and the durable shape stay in lockstep.
func toStoreCard(c Card, runID, date string) store.Card {
	metaJSON, _ := json.Marshal(c.Meta)
	actionsJSON, _ := json.Marshal(c.Actions)
	var expandJSON datatypes.JSON
	if len(c.Expand) > 0 {
		expandJSON, _ = json.Marshal(c.Expand)
	}
	var itemsJSON datatypes.JSON
	if len(c.Items) > 0 {
		itemsJSON, _ = json.Marshal(c.Items)
	}
	var liveJSON datatypes.JSON
	if len(c.Live) > 0 {
		liveJSON, _ = json.Marshal(c.Live)
	}
	return store.Card{
		ID:        c.ID,
		Date:      date,
		Kind:      c.Kind,
		Source:    c.Source,
		SrcLabel:  c.SrcLabel,
		Rel:       c.Rel,
		Title:     c.Title,
		Sub:       c.Sub,
		Meta:      datatypes.JSON(metaJSON),
		Actions:   datatypes.JSON(actionsJSON),
		Expand:    expandJSON,
		TraceID:   runID,
		RunID:     runID,
		CreatedAt: time.Now(),
		ConcernID: c.ConcernID,
		EntityKey: c.EntityKey,
		Items:     itemsJSON,
		Live:      liveJSON,
	}
}

// persist writes cards + briefing + trace in one transaction. All cards in
// this run share runID, and stale rows from prior runs on the same date are
// swept out so the read API never returns a mix. The state argument is
// the V2.3 register the briefing was synthesized under; persisted on the
// briefing row.
func (r *Runner) persist(ctx context.Context, runID, date string, state State, cs CardSet, b Briefing, tr llm.Trace) error {
	cardsTbl := defaultIfEmpty(r.CardsTable, "cards")
	briefTbl := defaultIfEmpty(r.BriefingTable, "briefings")
	traceTbl := defaultIfEmpty(r.TraceTable, "traces")

	cardRows := make([]store.Card, 0, len(cs.Cards))
	for _, c := range cs.Cards {
		cardRows = append(cardRows, toStoreCard(c, runID, date))
	}

	stepsJSON, _ := json.Marshal(tr.Steps)
	traceRow := store.Trace{
		ID:        runID,
		RunID:     runID,
		Date:      date,
		Stopped:   tr.Stopped,
		TotalMs:   tr.TotalMs,
		Steps:     datatypes.JSON(stepsJSON),
		CreatedAt: time.Now(),
	}

	briefRow := store.Briefing{
		Date:              date,
		Eyebrow:           b.Eyebrow,
		Title:             b.Title,
		Summary:           b.Summary,
		Tension:           b.Tension,
		State:             string(state),
		SuggestedFollowup: b.SuggestedFollowup,
		RunID:             runID,
		CreatedAt:         time.Now(),
	}

	return r.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		cards := store.CardRepo{DB: tx, Table: cardsTbl}
		briefings := store.BriefingRepo{DB: tx, Table: briefTbl}
		traces := store.TraceRepo{DB: tx, Table: traceTbl}
		if err := cards.UpsertWithContinuity(ctx, cardRows, time.Now()); err != nil {
			return fmt.Errorf("upsert cards: %w", err)
		}
		if err := cards.DeleteStale(ctx, date, runID); err != nil {
			return fmt.Errorf("delete stale cards: %w", err)
		}
		if err := briefings.UpsertMorning(ctx, briefRow); err != nil {
			return fmt.Errorf("upsert briefing: %w", err)
		}
		if err := traces.Create(ctx, traceRow); err != nil {
			return fmt.Errorf("create trace: %w", err)
		}
		return nil
	})
}

// consolidateMemoryAudit emits the memory.candidates audit event, folds the
// candidates through the deterministic consolidator, and emits the resulting
// memory.consolidated event. Best-effort throughout: a consolidator failure
// is logged at WARN and recorded as memory.consolidate.failed but never
// fails the synth run. Memory derivation is decoration, not load-bearing.
func (r *Runner) consolidateMemoryAudit(ctx context.Context, runID, date string, repo *store.MemoryRepo, candidates []llm.MemoryCandidate, logger *logrus.Entry) {
	if len(candidates) == 0 {
		return
	}
	r.appendEvent(ctx, log.KindMemoryCandidates, map[string]any{
		"run_id":     runID,
		"date":       date,
		"candidates": candidates,
	})
	delta, err := Consolidate(ctx, ConsolidateDeps{
		Repo:           repo,
		EmbeddingStore: r.EmbeddingStore,
		EmbeddingIndex: r.EmbeddingIndex,
		Now:            r.Now,
		Logger:         logger.WithField("step", "consolidate"),
	}, ConsolidateConfig{}, runID, candidates)
	if err != nil {
		logger.WithError(err).Warn("synth: memory consolidator failed (best-effort, run continues)")
		r.appendEvent(ctx, log.KindMemoryConsolidateFailed, map[string]any{
			"run_id": runID,
			"date":   date,
			"error":  err.Error(),
		})
		return
	}
	r.appendEvent(ctx, log.KindMemoryConsolidated, map[string]any{
		"run_id": runID,
		"date":   date,
		"delta":  delta,
	})
	logger.WithFields(map[string]any{
		"added":        len(delta.Added),
		"incremented":  len(delta.Incremented),
		"promoted":     len(delta.Promoted),
		"evicted":      len(delta.Evicted),
		"skipped":      len(delta.Skipped),
		"skipped_seen": len(delta.SkippedSeen),
	}).Info("synth: memory consolidated")
}

// detectState computes the V2.3 adaptive-state register for this synth
// pass, applies the pre_meeting hysteresis, and emits a
// synth.state_changed event when the effective state differs from the
// prior briefing on the same date.
//
// Best-effort: projection failures degrade to morning_calm rather than
// failing the run. The detector is decoration that grounds the briefing's
// register; if it can't read calendar/threads, the day defaults to calm
// and the briefing still ships.
func (r *Runner) detectState(ctx context.Context, now time.Time, date string, logger *logrus.Entry) State {
	cal, calErr := projection.TodaysCalendar{Cfg: r.ProjCfg}.Compute(ctx, r.Reader)
	threads, threadsErr := projection.OpenEmailThreads{Cfg: r.ProjCfg}.Compute(ctx, r.Reader)
	if calErr != nil || threadsErr != nil {
		logger.WithError(firstNonNil(calErr, threadsErr)).
			Warn("synth: state-detector projections failed; defaulting to morning_calm")
		return StateMorningCalm
	}

	// BuildStateInputs reads Weekday/LocalHour from the now it's handed.
	// Convert to the user's tz here so the detector's local-hour rules
	// (DeepWorkEarliestHour / EndOfDayHour / FridayEndOfDayHour) fire on
	// the user's clock, not the server's UTC clock.
	tz := r.ProjCfg.TZ
	if tz == nil {
		tz = time.UTC
	}
	inputs := BuildStateInputs(now.In(tz), cal, threads, r.InjectSignal)
	detected := DetectState(inputs)

	// Lookup prior briefing for hysteresis.
	prevState := State("")
	prevAt := time.Time{}
	briefingRepo := &store.BriefingRepo{DB: r.DB, Table: defaultIfEmpty(r.BriefingTable, "briefings")}
	if prior, err := briefingRepo.ByDate(ctx, date); err == nil && prior != nil {
		prevState = State(prior.State)
		prevAt = prior.CreatedAt
	}

	state := ApplyHysteresis(prevState, detected, prevAt, now)

	if state != prevState {
		r.appendEvent(ctx, log.KindSynthStateChanged, map[string]any{
			"date":     date,
			"prev":     string(prevState),
			"next":     string(state),
			"detected": string(detected), // pre-hysteresis, for debugging
			"inputs":   inputs,
		})
	}

	logger.WithField("state", string(state)).
		WithField("detected", string(detected)).
		WithField("prev", string(prevState)).
		Info("synth: state detected")

	return state
}

// firstNonNil returns the first non-nil error from the args, or nil.
func firstNonNil(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// appendEvent writes a boundary event to the observation log. Failures are
// logged but never propagate — the synth result is more important than the
// audit row.
func (r *Runner) appendEvent(ctx context.Context, kind string, payload any) {
	if r.EventLog == nil {
		return
	}
	if _, err := r.EventLog.Append(ctx, kind, "synth", payload); err != nil && r.Logger != nil {
		r.Logger.WithError(err).WithField("kind", kind).Warn("synth: append event failed")
	}
}

// appendDetachedEvent is appendEvent for the failure paths — uses a fresh
// short-deadline context so the audit row lands even when the parent ctx is
// already dead (which is precisely why we're recording a failure).
func (r *Runner) appendDetachedEvent(kind string, payload any) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r.appendEvent(ctx, kind, payload)
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func defaultIfEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// Migrate runs AutoMigrate against all the configured tables. Call once at
// boot so prod and replay tables are both up to date.
func Migrate(db *gorm.DB, prod, replay bool) error {
	type tableSet struct {
		cards      string
		briefing   string
		trace      string
		memory     string
		embedding  string
		concerns   string
		concernObs string
	}
	var tables []tableSet
	if prod {
		tables = append(tables, tableSet{
			"cards", "briefings", "traces", "memory_facts", "memory_embeddings",
			"concerns", "concern_observations",
		})
	}
	if replay {
		tables = append(tables, tableSet{
			"cards_replay", "briefings_replay", "traces_replay", "memory_facts_replay", "memory_embeddings_replay",
			"concerns_replay", "concern_observations_replay",
		})
	}
	for _, t := range tables {
		if err := (&store.CardRepo{DB: db, Table: t.cards}).Migrate(); err != nil {
			return fmt.Errorf("migrate %s: %w", t.cards, err)
		}
		if err := (&store.BriefingRepo{DB: db, Table: t.briefing}).Migrate(); err != nil {
			return fmt.Errorf("migrate %s: %w", t.briefing, err)
		}
		if err := (&store.TraceRepo{DB: db, Table: t.trace}).Migrate(); err != nil {
			return fmt.Errorf("migrate %s: %w", t.trace, err)
		}
		if err := (&store.MemoryRepo{DB: db, Table: t.memory}).Migrate(); err != nil {
			return fmt.Errorf("migrate %s: %w", t.memory, err)
		}
		if err := (&embeddings.Store{DB: db, Table: t.embedding}).Migrate(); err != nil {
			return fmt.Errorf("migrate %s: %w", t.embedding, err)
		}
		if err := (&store.ConcernRepo{DB: db, Table: t.concerns}).Migrate(); err != nil {
			return fmt.Errorf("migrate %s: %w", t.concerns, err)
		}
		if err := (&store.ConcernObservationRepo{DB: db, Table: t.concernObs}).Migrate(); err != nil {
			return fmt.Errorf("migrate %s: %w", t.concernObs, err)
		}
	}
	// app_settings is a singleton — prod-only, no replay variant.
	if prod {
		if err := (&store.SettingsRepo{DB: db}).Migrate(); err != nil {
			return fmt.Errorf("migrate app_settings: %w", err)
		}
		// jina_cache holds search/read responses with TTLs. Runtime
		// cache, not durable observation data — prod-only, no replay
		// variant (parallel to app_settings above).
		if err := (&jina.Store{DB: db}).Migrate(); err != nil {
			return fmt.Errorf("migrate jina_cache: %w", err)
		}
	}
	return nil
}
