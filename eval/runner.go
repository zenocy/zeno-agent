package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/llm"
	"github.com/zenocy/zeno-v2/internal/projection"
	"github.com/zenocy/zeno-v2/internal/store"
	"github.com/zenocy/zeno-v2/internal/synth"
)

// RunResult is one fixture's evaluated output.
type RunResult struct {
	FixtureName  string         `json:"fixture_name"`
	FixturePath  string         `json:"fixture_path"`
	Briefing     synth.Briefing `json:"briefing"`
	Cards        synth.CardSet  `json:"cards"`
	Trace        llm.Trace      `json:"trace"`
	BriefingErr  string         `json:"briefing_err,omitempty"`
	CardsErr     string         `json:"cards_err,omitempty"`
	TotalLatency time.Duration  `json:"total_latency_ms"`
	Scoreboard   Scoreboard     `json:"scoreboard"`
	BriefingDeg  bool           `json:"briefing_degraded"`
}

// RunOpts bundles the per-run dependencies the harness needs.
type RunOpts struct {
	LLM             *llm.Client
	Prompts         *synth.PromptSet
	CardsTimeout    time.Duration // 0 → 30s
	BriefingTimeout time.Duration // 0 → 45s
	ToolTimeout     time.Duration // 0 → 5s
	ReactiveTimeout time.Duration // 0 → 180s; per-fixture deadline for synth.Ask in RunReactiveFixture
	Logger          *logrus.Entry
}

// RunFixture loads a fixture, seeds an ephemeral store, executes the synth
// pipeline against it, reads the persisted cards + briefing back out, and
// scores. The ephemeral store is removed before return.
func RunFixture(ctx context.Context, fixturePath string, opts RunOpts) (*RunResult, error) {
	f, err := LoadFixture(fixturePath)
	if err != nil {
		return nil, err
	}

	estore, err := NewEphemeralStore("")
	if err != nil {
		return nil, err
	}
	defer estore.Close()

	if err := estore.Seed(ctx, f); err != nil {
		return nil, fmt.Errorf("seed: %w", err)
	}

	now, err := f.TodayTime()
	if err != nil {
		return nil, err
	}

	logger := opts.Logger
	if logger == nil {
		logger = logrus.NewEntry(logrus.New())
	}

	loc, err := time.LoadLocation(f.User.TZ)
	if err != nil {
		loc = time.UTC
	}
	projCfg := projection.Config{
		TZ:                    loc,
		LookbackDays:          14,
		RunWindowMinMinutes:   45,
		RunWindowMaxWindKmh:   25,
		RunWindowEarliestHour: 6,
		RunWindowLatestHour:   20,
		OpenThreadsMax:        20,
		Now:                   func() time.Time { return now },
	}

	runner := &synth.Runner{
		LLM:             opts.LLM,
		Reader:          estore.Store,
		DB:              estore.DB,
		EventLog:        estore.Store,
		ProjCfg:         projCfg,
		Prompts:         opts.Prompts,
		Now:             func() time.Time { return now },
		Logger:          logger.WithField("c", "eval-synth"),
		CardsTable:      "cards",
		BriefingTable:   "briefings",
		TraceTable:      "traces",
		CardsTimeout:    opts.CardsTimeout,
		BriefingTimeout: opts.BriefingTimeout,
		ToolTimeout:     opts.ToolTimeout,
		// Wire a synthetic TickerSource derived from the fixture's
		// stock seed so the MarketsContext projection actually fires.
		// Without this, eval fixtures that seed stock_alerts (e.g.
		// morning_markets) silently get an empty markets block in the
		// cards prompt and the model never emits a markets card —
		// despite the fixture's must_card explicitly demanding one.
		Tickers: fixtureTickerSource(f),
	}

	// V2.3.0 P1: if the fixture declares an inject_signal block, synthesize
	// the marker so the detector picks StateMessageInject. P3 owns the real
	// signal pipeline; this is the harness-only path until then.
	if f.InjectSignal != nil {
		runner.InjectSignal = &synth.InjectSignal{
			Kind:       f.InjectSignal.Kind,
			Subject:    f.InjectSignal.Subject,
			EvidenceID: f.InjectSignal.EvidenceID,
			At:         now,
		}
	}

	start := time.Now()
	runErr := runner.Run(ctx)
	totalLat := time.Since(start)

	cardRepo := &store.CardRepo{DB: estore.DB, Table: "cards"}
	briefingRepo := &store.BriefingRepo{DB: estore.DB, Table: "briefings"}

	storeCards, _ := cardRepo.ListByDate(ctx, f.Today)
	cs := storeCardsToSynth(storeCards)

	briefing := synth.Briefing{}
	storeBriefing, briefingErr := briefingRepo.ByDate(ctx, f.Today)
	if storeBriefing != nil {
		briefing = synth.Briefing{
			Date:              storeBriefing.Date,
			Eyebrow:           storeBriefing.Eyebrow,
			Title:             storeBriefing.Title,
			Summary:           storeBriefing.Summary,
			Tension:           storeBriefing.Tension,
			SuggestedFollowup: storeBriefing.SuggestedFollowup,
		}
	}

	res := &RunResult{
		FixtureName:  fixtureNameFromPath(fixturePath),
		FixturePath:  fixturePath,
		Briefing:     briefing,
		Cards:        cs,
		TotalLatency: totalLat,
		BriefingDeg:  briefing.Eyebrow == "draft pending",
	}
	if runErr != nil {
		res.CardsErr = runErr.Error()
	}
	if briefingErr != nil {
		res.BriefingErr = briefingErr.Error()
	}
	actualState := ""
	if storeBriefing != nil {
		actualState = storeBriefing.State
	}
	// V2.3.0 P2: per-state must_cards are keyed on the fixture's expected
	// state — the author's intent — not the detector's pick. If the detector
	// misfires, state_match flags it; per-state must_cards still validate
	// the fixture's authored expectations.
	must := f.MustCardsForState(synth.State(f.ExpectedState))
	res.Scoreboard = ScoreAll(briefing, cs, must, f.Memory, actualState, f.ExpectedState, false)
	return res, nil
}

// RunInjectFixture loads an inject fixture, builds a synth.InjectSignal
// from its inject_signal block, calls synth.SynthesizeInject directly
// (bypassing the morning Runner), persists card + fragment, and scores
// against MustCardsInject. Used by V2.3.0 P3 to evaluate the inject
// pipeline end-to-end on the corpus.
//
// Dispatch contract: the eval CLI routes a fixture here when both
// f.InjectSignal != nil AND f.ExpectedState == "message_inject". Other
// fixtures stay on RunFixture.
func RunInjectFixture(ctx context.Context, fixturePath string, opts RunOpts) (*RunResult, error) {
	f, err := LoadFixture(fixturePath)
	if err != nil {
		return nil, err
	}
	if f.InjectSignal == nil {
		return nil, fmt.Errorf("inject fixture %s missing inject_signal block", fixturePath)
	}

	estore, err := NewEphemeralStore("")
	if err != nil {
		return nil, err
	}
	defer estore.Close()

	if err := estore.Seed(ctx, f); err != nil {
		return nil, fmt.Errorf("seed: %w", err)
	}

	now, err := f.TodayTime()
	if err != nil {
		return nil, err
	}

	logger := opts.Logger
	if logger == nil {
		logger = logrus.NewEntry(logrus.New())
	}

	loc, err := time.LoadLocation(f.User.TZ)
	if err != nil {
		loc = time.UTC
	}
	projCfg := projection.Config{
		TZ:                    loc,
		LookbackDays:          14,
		RunWindowMinMinutes:   45,
		RunWindowMaxWindKmh:   25,
		RunWindowEarliestHour: 6,
		RunWindowLatestHour:   20,
		OpenThreadsMax:        20,
		Now:                   func() time.Time { return now },
	}

	signal := synth.InjectSignal{
		Kind:       f.InjectSignal.Kind,
		Subject:    f.InjectSignal.Subject,
		EvidenceID: f.InjectSignal.EvidenceID,
		At:         now,
	}

	deps := synth.InjectDeps{
		LLM:             opts.LLM,
		Reader:          estore.Store,
		Memory:          &store.MemoryRepo{DB: estore.DB, Table: "memory_facts"},
		Prompts:         opts.Prompts,
		ProjCfg:         projCfg,
		Date:            f.Today,
		Now:             now,
		Logger:          logger.WithField("c", "eval-inject"),
		LoopTimeout:     opts.CardsTimeout,
		BriefingTimeout: opts.BriefingTimeout,
		ToolTimeout:     opts.ToolTimeout,
	}

	start := time.Now()
	res, runErr := synth.SynthesizeInject(ctx, deps, signal)
	totalLat := time.Since(start)

	briefingRepo := &store.BriefingRepo{DB: estore.DB, Table: "briefings"}
	cardRepo := &store.CardRepo{DB: estore.DB, Table: "cards"}

	if runErr == nil {
		// Persist for read-back symmetry with the morning path.
		if err := cardRepo.Upsert(ctx, []store.Card{res.Card}); err != nil {
			return nil, fmt.Errorf("upsert inject card: %w", err)
		}
		if err := briefingRepo.UpsertInject(ctx, res.Fragment); err != nil {
			return nil, fmt.Errorf("upsert inject fragment: %w", err)
		}
	}

	// Read back the persisted card(s) — for the inject path this is
	// just the one card we just wrote, but we route through the same
	// store reader so the assertion shape matches the morning path.
	storeCards, _ := cardRepo.ListByDate(ctx, f.Today)
	cs := storeCardsToSynth(storeCards)

	briefing := synth.Briefing{}
	storeBriefing, briefingErr := briefingRepo.ByDateKind(ctx, f.Today, store.BriefingKindInject)
	if storeBriefing != nil {
		briefing = synth.Briefing{
			Date:              storeBriefing.Date,
			Eyebrow:           storeBriefing.Eyebrow,
			Title:             storeBriefing.Title,
			Summary:           storeBriefing.Summary,
			Tension:           storeBriefing.Tension,
			SuggestedFollowup: storeBriefing.SuggestedFollowup,
		}
	}

	result := &RunResult{
		FixtureName:  fixtureNameFromPath(fixturePath),
		FixturePath:  fixturePath,
		Briefing:     briefing,
		Cards:        cs,
		TotalLatency: totalLat,
		BriefingDeg:  briefing.Eyebrow == "draft pending",
	}
	if runErr != nil {
		result.CardsErr = runErr.Error()
	}
	if briefingErr != nil {
		result.BriefingErr = briefingErr.Error()
	}

	actualState := ""
	if storeBriefing != nil {
		actualState = storeBriefing.State
	}
	must := f.MustCardsForState(synth.StateMessageInject)
	result.Scoreboard = ScoreAll(briefing, cs, must, f.Memory, actualState, f.ExpectedState, true)
	return result, nil
}

// storeCardsToSynth converts persistence-layer Cards to the synth.Card
// shape the scorers expect. Meta/Actions JSON columns are unmarshalled.
func storeCardsToSynth(in []store.Card) synth.CardSet {
	out := synth.CardSet{Cards: make([]synth.Card, 0, len(in))}
	for _, c := range in {
		var meta []string
		if len(c.Meta) > 0 {
			_ = json.Unmarshal(c.Meta, &meta)
		}
		var actions []synth.Action
		if len(c.Actions) > 0 {
			_ = json.Unmarshal(c.Actions, &actions)
		}
		var expand map[string]string
		if len(c.Expand) > 0 {
			_ = json.Unmarshal(c.Expand, &expand)
		}
		out.Cards = append(out.Cards, synth.Card{
			ID:       c.ID,
			Date:     c.Date,
			Source:   c.Source,
			SrcLabel: c.SrcLabel,
			Rel:      c.Rel,
			Kind:     c.Kind,
			Title:    c.Title,
			Sub:      c.Sub,
			Meta:     meta,
			Actions:  actions,
			Expand:   expand,
			TraceID:  c.TraceID,
		})
	}
	return out
}

// jsonMarshal is a tiny indirection so scoring.go can serialize without a
// direct encoding/json import.
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// fixtureNameFromPath returns the file's basename without extension —
// "morning_calm" for ".../morning_calm.json".
func fixtureNameFromPath(p string) string {
	base := filepath.Base(p)
	if i := strings.LastIndexByte(base, '.'); i > 0 {
		return base[:i]
	}
	return base
}

// evalTickerSource is a fixture-derived TickerSource. The watchlist is the
// union of every ticker that appears in StockSnapshots or StockAlerts;
// thresholdPct picks up the alert's threshold (or defaults to 3% when no
// alert was seeded), so the MarketsContext "interesting" gate behaves the
// same way as production. Returns ok=false when the fixture seeds no
// stock data at all so MarketsContext.Compute short-circuits to nil and
// the cards prompt's markets block stays empty (preserving byte-equal
// pre-V2.7 behavior for fixtures that don't care about markets).
type evalTickerSource struct {
	tickers   []string
	threshold float64
	ok        bool
}

func (s evalTickerSource) StockConfig() ([]string, float64, bool, bool) {
	return s.tickers, s.threshold, false, s.ok
}

func fixtureTickerSource(f *Fixture) projection.TickerSource {
	if f == nil || (len(f.StockSnapshots) == 0 && len(f.StockAlerts) == 0) {
		return evalTickerSource{ok: false}
	}
	seen := map[string]struct{}{}
	tickers := make([]string, 0, len(f.StockSnapshots)+len(f.StockAlerts))
	for _, s := range f.StockSnapshots {
		if s.Ticker == "" {
			continue
		}
		if _, dup := seen[s.Ticker]; dup {
			continue
		}
		seen[s.Ticker] = struct{}{}
		tickers = append(tickers, s.Ticker)
	}
	threshold := 3.0
	for _, a := range f.StockAlerts {
		if a.Ticker == "" {
			continue
		}
		if _, dup := seen[a.Ticker]; !dup {
			seen[a.Ticker] = struct{}{}
			tickers = append(tickers, a.Ticker)
		}
		if a.ThresholdPct > 0 && a.ThresholdPct < threshold {
			threshold = a.ThresholdPct
		}
	}
	if len(tickers) == 0 {
		return evalTickerSource{ok: false}
	}
	return evalTickerSource{tickers: tickers, threshold: threshold, ok: true}
}
