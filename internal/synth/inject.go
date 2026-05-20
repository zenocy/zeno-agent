package synth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/eventbus"
	"github.com/zenocy/zeno-v2/internal/jina"
	"github.com/zenocy/zeno-v2/internal/llm"
	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/projection"
	"github.com/zenocy/zeno-v2/internal/store"
)

// InjectMaxIterations caps the inject cards loop. Lower than the morning
// cap (6) because the output is one card and the model has a single
// signal to expand — most well-behaved runs land in 1–2 iterations.
const InjectMaxIterations = 4

// InjectDeps bundles everything SynthesizeInject needs to render one card
// + a 1-paragraph briefing fragment for a mid-day inject signal.
type InjectDeps struct {
	LLM             llm.Provider
	Reader          log.Reader
	ProjCfg         projection.Config
	Memory          *store.MemoryRepo
	MemoryRanker    *MemoryRanker
	Prompts         *PromptSet
	PriorCards      []store.Card // today's existing cards (not output, just context)
	Date            string       // YYYY-MM-DD in local TZ
	Now             time.Time
	Logger          *logrus.Entry
	LoopTimeout     time.Duration // cards-loop deadline; 0 → 30s
	BriefingTimeout time.Duration // fragment deadline; 0 → 30s
	ToolTimeout     time.Duration // per-tool timeout; 0 → 5s
	FinalCallBudget time.Duration // per-call deadline for the loop's final wrap-up LLM call; 0 → 15s (llm.LoopConfig default)

	// Bus is the V2.4 typed eventbus. When non-nil, SynthesizeInject
	// publishes synth.started / trace.step* / synth.delta* /
	// synth.completed for SSE consumers (the orchestrator still
	// publishes card.appended after persisting). nil → no live events.
	Bus *eventbus.Bus

	// LoopObserver receives per-LLM-call / tool / repair / loop-iteration
	// events from the underlying tool loop. Nil disables.
	LoopObserver llm.LoopObserver
	// OnSynthRun is invoked once with stage="inject" and outcome ok|degraded.
	OnSynthRun func(stage, outcome string, dur time.Duration)

	// V2.6: optional Jina web tools. When JinaClient is non-nil the
	// search_web + read_url tools are registered into the inject loop
	// so an inject signal carrying a URL can be expanded with web
	// content. JinaCache is normally wired; SearchTTL/ReadTTL fall
	// back to 6h/24h when zero.
	JinaClient JinaClient
	JinaCache  *jina.Store
	SearchTTL  time.Duration
	ReadTTL    time.Duration

	// V2.8.1: action vocabulary the runner advertises to the LLM.
	// See cards.go for the WiredIntent type. Inject prompt renders
	// the same "Action verbs" section as the morning prompt when
	// non-empty.
	WiredIntents []WiredIntent
}

// InjectResult is what SynthesizeInject returns. The caller persists Card
// (with Origin="inject") and Fragment (via BriefingRepo.UpsertInject) and
// publishes Card on the event bus for SSE delivery.
type InjectResult struct {
	Card     store.Card
	Fragment store.Briefing
	Trace    llm.Trace
}

// injectRunID derives the inject run identifier from the signal's
// observation timestamp. Deterministic so replays produce identical IDs
// and so the durable card row's RunID matches the events emitted on the
// bus during synthesis. Format byte-equal to V2.3.
func injectRunID(signal InjectSignal) string {
	return "inject-" + signal.At.UTC().Format("20060102T150405")
}

// SynthesizeInject runs the V2.3.0 P3 message_inject synth pipeline:
// renders the cards system prompt with State=StateMessageInject (so the
// per-state cards-bias overlay collapses output to 1 card), runs the
// llm.RunLoop with the same tool registry as the morning cards loop,
// validates the result, sets Origin="inject" on the chosen card, then
// renders a 1-paragraph briefing fragment in the message_inject voice.
//
// Failure modes:
//   - LLM error or zero cards parsed → returns a degraded card + fragment
//     constructed from the signal alone, with Trace.Stopped="degraded" and
//     no error. The caller still publishes; the user sees something.
//   - More than one card → first is taken, rest discarded.
//   - Briefing-fragment failure → fragment falls back to a minimal
//     placeholder built from the signal subject. Whole call still succeeds.
//
// The pipeline never returns an error to the caller for soft failures —
// the inject pipeline is "best effort, deny by default upstream" and the
// downstream cron logs everything via synth.message_inject events. A real
// hard error (context canceled, projection read failure) does propagate.
func SynthesizeInject(ctx context.Context, d InjectDeps, signal InjectSignal) (res InjectResult, retErr error) {
	runStart := time.Now()
	defer func() {
		if d.OnSynthRun == nil {
			return
		}
		outcome := "ok"
		if retErr != nil {
			outcome = "failed"
		} else if res.Trace.Stopped == "degraded" {
			outcome = "degraded"
		}
		d.OnSynthRun("inject", outcome, time.Since(runStart))
	}()

	// Hoist runID so the same id labels the boundary events, the
	// trace_step events, the synth.delta events, and the persisted card
	// row. The deterministic timestamp form is byte-equal to V2.3.
	runID := injectRunID(signal)

	if d.Bus != nil {
		d.Bus.Publish(eventbus.SynthStartedEvent{
			RunID: runID, Stage: "inject", Date: d.Date,
		})
	}

	loopTimeout := d.LoopTimeout
	if loopTimeout <= 0 {
		loopTimeout = 30 * time.Second
	}
	briefTimeout := d.BriefingTimeout
	if briefTimeout <= 0 {
		briefTimeout = 30 * time.Second
	}
	toolTimeout := d.ToolTimeout
	if toolTimeout <= 0 {
		toolTimeout = 5 * time.Second
	}
	if d.JinaClient != nil && toolTimeout < 20*time.Second {
		toolTimeout = 20 * time.Second
	}

	cal, calErr := projection.TodaysCalendar{Cfg: d.ProjCfg}.Compute(ctx, d.Reader)
	if calErr != nil {
		return InjectResult{}, fmt.Errorf("inject projection calendar: %w", calErr)
	}
	threads, threadsErr := projection.OpenEmailThreads{Cfg: d.ProjCfg}.Compute(ctx, d.Reader)
	if threadsErr != nil {
		return InjectResult{}, fmt.Errorf("inject projection threads: %w", threadsErr)
	}
	window, windowErr := projection.RunWindow{Cfg: d.ProjCfg}.Compute(ctx, d.Reader)
	if windowErr != nil {
		return InjectResult{}, fmt.Errorf("inject projection run window: %w", windowErr)
	}

	// Memory enrichment mirrors SynthesizeCards. Inject is rarer but no
	// less voice-sensitive — a memory fact about the inject's subject can
	// substantially sharpen the resulting card.
	poolLimit := CardsMemoryLimit
	if d.MemoryRanker != nil {
		poolLimit = CardsPoolLimit
	}
	memory, err := projection.MemoryFacts{Repo: d.Memory, Config: projection.MemoryFactsConfig{Limit: poolLimit}}.Compute(ctx, d.Reader)
	if err != nil {
		return InjectResult{}, fmt.Errorf("inject projection memory: %w", err)
	}
	if d.MemoryRanker != nil && len(memory) > 0 {
		// Use the inject signal's subject as the ranking query — its noun
		// phrases are the fastest signal of what to surface.
		if signal.Subject != "" {
			memory = d.MemoryRanker.Rank(ctx, signal.Subject, memory, CardsMemoryLimit)
		} else if len(memory) > CardsMemoryLimit {
			memory = memory[:CardsMemoryLimit]
		}
	}

	tz := d.ProjCfg.TZ
	if tz == nil {
		tz = time.UTC
	}

	systemBuf := &bytes.Buffer{}
	if err := d.Prompts.CardsSystem.Execute(systemBuf, map[string]any{
		"VoiceShort":   d.Prompts.VoiceShort,
		"State":        string(StateMessageInject),
		"StateBias":    d.Prompts.StateBias[StateMessageInject],
		"Date":         d.Date,
		"TZ":           tz.String(),
		"Calendar":     cal,
		"Threads":      threads,
		"RunWindow":    window,
		"Memory":       memory,
		"WiredIntents": d.WiredIntents,
	}); err != nil {
		return InjectResult{}, fmt.Errorf("render inject prompt: %w", err)
	}

	toolHint := "Use read_thread / read_event tools to expand the underlying evidence before composing."
	if signal.Kind == "stock_breach" {
		toolHint = fmt.Sprintf(
			"Use read_stock_alert with evidence_id=%q to expand the underlying breach (price, prior close, threshold) before composing.",
			signal.EvidenceID,
		)
	}
	user := fmt.Sprintf(
		"A high-signal mid-day event arrived: kind=%q subject=%q. Produce exactly ONE card describing what the user should do, in the message_inject voice. %s",
		signal.Kind, signal.Subject, toolHint,
	)

	reg := llm.NewRegistry(
		&ReadThreadTool{Reader: d.Reader, Now: func() time.Time { return d.Now }},
		&ReadEventTool{Reader: d.Reader, TZ: tz},
		&ReadWeatherWindowTool{Reader: d.Reader, TZ: tz},
		&ReadStockAlertTool{Reader: d.Reader},
	)
	nativeSearch := d.LLM != nil && d.LLM.NativeSearchEnabled()
	if d.JinaClient != nil {
		ttls := jinaTTLs{Search: d.SearchTTL, Read: d.ReadTTL}
		if !nativeSearch {
			reg.Register(&SearchWebTool{Client: d.JinaClient, Cache: d.JinaCache, TTLs: ttls})
		}
		reg.Register(&ReadURLTool{Client: d.JinaClient, Cache: d.JinaCache, TTLs: ttls})
	}

	// Use the InjectCardSet schema (1–3 cards) for constrained decoding so
	// the model doesn't drift into the morning's 2–6 shape. Post-parse we
	// still take only the first card.
	chatOpts := []llm.ChatOption{}
	if d.LLM.JSONSchemaEnabled() {
		chatOpts = append(chatOpts, llm.WithJSONSchema("inject_cards", InjectCardSetSchemaMap()))
	}
	if nativeSearch {
		chatOpts = append(chatOpts, llm.WithGoogleSearch())
	}

	// Inject cards loop runs json_schema-constrained — only stream trace
	// steps; the JSON body would be unreadable noise in the live panel.
	loopCtx, loopCancel := context.WithTimeout(ctx, loopTimeout)
	loopCtx = AttachLiveTrace(loopCtx, d.Bus, runID, "inject")
	result, runErr := llm.RunLoop(loopCtx, d.LLM, systemBuf.String(), user, reg, llm.LoopConfig{
		MaxIterations:    InjectMaxIterations,
		Deadline:         loopTimeout,
		ToolTimeout:      toolTimeout,
		FinalCallBudget:  d.FinalCallBudget,
		ChatOptions:      chatOpts,
		Logger:           d.Logger,
		Stage:            "inject",
		Observer:         d.LoopObserver,
	})
	loopCancel()

	var card store.Card
	switch {
	case runErr != nil:
		if d.Logger != nil {
			d.Logger.WithError(runErr).WithField("stopped", result.Stopped).
				Warn("inject: cards loop failed — using degraded fallback card")
		}
		card = degradedInjectCard(d.Date, signal, d.Now, runID)
	default:
		var ok bool
		card, ok = parseFirstInjectCard(result.Content, d.Date, signal, d.Now, runID, d.Logger)
		if !ok {
			card = degradedInjectCard(d.Date, signal, d.Now, runID)
		}
	}

	// Briefing fragment — short paragraph in the message_inject voice.
	// Re-attach live publishers so the fragment's body tokens also stream.
	fragCtx, fragCancel := context.WithTimeout(ctx, briefTimeout)
	fragCtx, releaseFragLive := AttachLivePublishers(fragCtx, d.Bus, runID, "inject")
	fragment, fragErr := SynthesizeBriefing(fragCtx, BriefingDeps{
		LLM:     d.LLM,
		Prompts: d.Prompts,
		Date:    d.Date,
		State:   StateMessageInject,
		Logger:  loggerWithStep(d.Logger, "inject_fragment"),
	}, CardSet{Cards: []Card{cardToSynth(card)}})
	releaseFragLive()
	fragCancel()
	if fragErr != nil {
		if d.Logger != nil {
			d.Logger.WithError(fragErr).
				Warn("inject: fragment briefing failed — using degraded fragment")
		}
		fragment = degradedInjectFragment(d.Date, signal)
	}

	fragRow := store.Briefing{
		Date:              d.Date,
		Eyebrow:           fragment.Eyebrow,
		Title:             fragment.Title,
		Summary:           fragment.Summary,
		Tension:           fragment.Tension,
		State:             string(StateMessageInject),
		SuggestedFollowup: fragment.SuggestedFollowup,
		CreatedAt:         d.Now,
	}

	if d.Bus != nil {
		// Publish synth.completed BEFORE the orchestrator publishes the
		// card so the UI's LiveSynthPanel dissolves before the inject
		// card pops into the grid.
		d.Bus.Publish(eventbus.SynthCompletedEvent{
			RunID: runID, Stage: "inject",
			Stopped: result.Trace.Stopped, TotalMs: result.Trace.TotalMs,
		})
	}

	return InjectResult{
		Card:     card,
		Fragment: fragRow,
		Trace:    result.Trace,
	}, nil
}

// parseFirstInjectCard parses the model's content as an InjectCardSet,
// validates against the inject schema, and returns the first card with
// post-processing applied + Origin="inject" set. If validation fails or
// the array is empty, returns ok=false; the caller substitutes a degraded
// card so the user always sees something.
func parseFirstInjectCard(raw, date string, signal InjectSignal, now time.Time, runID string, logger *logrus.Entry) (store.Card, bool) {
	cleaned := stripCodeFences(raw)
	if cleaned == "" {
		return store.Card{}, false
	}
	var set InjectCardSet
	if err := json.Unmarshal([]byte(cleaned), &set); err != nil {
		if logger != nil {
			logger.WithError(err).WithField("raw_preview", briefingPreview(cleaned, 400)).
				Warn("inject: parse cards JSON failed — falling back to degraded card")
		}
		return store.Card{}, false
	}
	applyInjectCardDefaults(&set)
	rebuilt, err := json.Marshal(set)
	if err != nil {
		return store.Card{}, false
	}
	if err := ValidateJSON(InjectCardSetSchema(), rebuilt); err != nil {
		if logger != nil {
			logger.WithError(err).Warn("inject: schema validation failed — falling back")
		}
		return store.Card{}, false
	}
	if len(set.Cards) == 0 {
		return store.Card{}, false
	}
	first := set.Cards[0]
	first.Title = canonicalizeMarkdown(first.Title)
	first.Sub = canonicalizeMarkdown(first.Sub)
	first.Date = date
	first.ID = slugFromTitle(first.Title)
	if len(set.Cards) > 1 && logger != nil {
		logger.WithField("dropped", len(set.Cards)-1).
			Info("inject: more than one card emitted — kept first, dropped rest")
	}
	return synthCardToStore(first, signal, now, runID), true
}

// applyInjectCardDefaults mirrors applyCardDefaults for the inject path.
// Local 7B/8B models drop optional fields under constrained decoding;
// supplying defaults stops the schema check from failing on shape rather
// than substance.
func applyInjectCardDefaults(set *InjectCardSet) {
	for i := range set.Cards {
		if len(set.Cards[i].Actions) == 0 {
			set.Cards[i].Actions = []Action{{Label: "Dismiss", Intent: "dismiss"}}
		}
		if set.Cards[i].Meta == nil {
			set.Cards[i].Meta = []string{}
		}
	}
}

// synthCardToStore converts a synth.Card to a store.Card and stamps the
// inject metadata: Origin="inject", the explicit runID provided by the
// caller (so the bus events and the persisted card share an id), and the
// signal's evidence ID on the trace field for cross-referencing.
func synthCardToStore(c Card, signal InjectSignal, now time.Time, runID string) store.Card {
	metaJSON, _ := json.Marshal(c.Meta)
	actionsJSON, _ := json.Marshal(c.Actions)
	var expandJSON []byte
	if len(c.Expand) > 0 {
		expandJSON, _ = json.Marshal(c.Expand)
	}
	return store.Card{
		ID:        c.ID,
		Date:      c.Date,
		Kind:      c.Kind,
		Source:    c.Source,
		SrcLabel:  c.SrcLabel,
		Rel:       firstNonEmpty(c.Rel, "high"),
		Origin:    "inject",
		Title:     c.Title,
		Sub:       c.Sub,
		Meta:      metaJSON,
		Actions:   actionsJSON,
		Expand:    expandJSON,
		TraceID:   signal.EvidenceID,
		RunID:     runID,
		CreatedAt: now,
	}
}

// cardToSynth flips a store.Card back to a synth.Card so the briefing
// renderer (which expects synth.CardSet) can stitch it into the fragment
// prompt. Only the visible-to-the-model fields are populated.
func cardToSynth(c store.Card) Card {
	var meta []string
	if len(c.Meta) > 0 {
		_ = json.Unmarshal(c.Meta, &meta)
	}
	var actions []Action
	if len(c.Actions) > 0 {
		_ = json.Unmarshal(c.Actions, &actions)
	}
	var expand map[string]string
	if len(c.Expand) > 0 {
		_ = json.Unmarshal(c.Expand, &expand)
	}
	return Card{
		ID:       c.ID,
		Date:     c.Date,
		Kind:     c.Kind,
		Source:   c.Source,
		SrcLabel: c.SrcLabel,
		Rel:      c.Rel,
		Title:    c.Title,
		Sub:      c.Sub,
		Meta:     meta,
		Actions:  actions,
		Expand:   expand,
	}
}

// degradedInjectCard constructs a minimum-viable card from the signal
// alone — used when the LLM call fails or returns nothing parseable.
// The card gets Origin="inject" so the UI badges it correctly even
// though the model never weighed in. runID matches the bus events.
func degradedInjectCard(date string, signal InjectSignal, now time.Time, runID string) store.Card {
	subject := strings.TrimSpace(signal.Subject)
	if subject == "" {
		subject = "Inject signal"
	}
	src := "mail"
	srcLabel := "Inject · " + signal.Kind
	switch signal.Kind {
	case "calendar_move":
		src = "calendar"
	case "stock_breach":
		// Card schema gained a dedicated "markets" src in Phase 4 so
		// stock cards are first-class (and so portfolio cards have a
		// home). The src_label carries the user-facing context.
		src = "markets"
		srcLabel = "Markets · breach"
	}
	c := Card{
		Date:     date,
		Kind:     "",
		Source:   src,
		SrcLabel: srcLabel,
		Rel:      "high",
		Title:    subject,
		Sub:      "Zeno couldn't compose a full card on first pass — the underlying signal is preserved as-is.",
		Meta:     []string{signal.Kind},
		Actions:  []Action{{Label: "Open", Primary: true, Intent: "open_url"}, {Label: "Dismiss", Intent: "dismiss"}},
	}
	c.ID = slugFromTitle(c.Title)
	return synthCardToStore(c, signal, now, runID)
}

// degradedInjectFragment constructs a minimum-viable fragment when the
// briefing renderer fails. Tension stays in the message_inject band so
// the rubric doesn't flag it as voice drift.
func degradedInjectFragment(date string, signal InjectSignal) Briefing {
	subject := strings.TrimSpace(signal.Subject)
	if subject == "" {
		subject = "One priority signal"
	}
	return Briefing{
		Date:    date,
		Eyebrow: "one priority signal",
		Title:   subject,
		Summary: "A signal arrived that warrants attention. The card alongside this fragment names the action.",
		Tension: 80,
	}
}

// firstNonEmpty returns s if non-empty, otherwise fallback. Used for
// post-parse defaults where the model dropped a required field that the
// schema-check step would otherwise reject.
func firstNonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// loggerWithStep returns a child logger annotated with step=name when
// the parent is non-nil; otherwise nil (matches the rest of synth's
// optional-logger conventions).
func loggerWithStep(l *logrus.Entry, name string) *logrus.Entry {
	if l == nil {
		return nil
	}
	return l.WithField("step", name)
}
