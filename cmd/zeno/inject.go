package main

import (
	"context"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/eventbus"
	"github.com/zenocy/zeno-v2/internal/http/api"
	"github.com/zenocy/zeno-v2/internal/jina"
	"github.com/zenocy/zeno-v2/internal/llm"
	logp "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/projection"
	"github.com/zenocy/zeno-v2/internal/schedule"
	"github.com/zenocy/zeno-v2/internal/sensor"
	"github.com/zenocy/zeno-v2/internal/store"
	"github.com/zenocy/zeno-v2/internal/synth"
)

// injectFnDeps bundles the dependencies the inject orchestrator closes
// over. Built once at boot, threaded into both cron-tick and manual paths.
type injectFnDeps struct {
	Reader         logp.Reader
	EventLog       logp.Writer
	ProjCfg        projection.Config
	Bus            *eventbus.Bus
	LLM            llm.Provider
	Memory         *store.MemoryRepo
	MemoryRanker   *synth.MemoryRanker
	Prompts        *synth.PromptSet
	CardRepo       *store.CardRepo
	BriefingRepo   *store.BriefingRepo
	ConcernRepo    *store.ConcernRepo            // V2.5.0: powers the concern-boost detector path
	ConcernTagRepo *store.ConcernObservationRepo // V2.5.0: optional, for observation counts
	DetectorCfg    sensor.InjectDetectorConfig
	Logger         *logrus.Entry
	Now            func() time.Time
	LoopObserver   llm.LoopObserver
	OnSynthRun     func(stage, outcome string, dur time.Duration)

	// V2.6: optional Jina web tools threaded into the inject loop.
	JinaClient synth.JinaClient
	JinaCache  *jina.Store
	SearchTTL  time.Duration
	ReadTTL    time.Duration

	// V2.8.1: action vocabulary the runner advertises to the LLM.
	WiredIntents []synth.WiredIntent

	// FinalCallBudget caps the loop's final wrap-up LLM call after
	// MaxIterations. Sourced from synth.final_call_budget_sec config.
	FinalCallBudget time.Duration
}

// buildInjectFn returns the schedule.InjectFunc the scheduler invokes on
// every /api/synth/now?kind=inject manual trigger and every reactive
// subscriber tick.
//
// signal contract:
//   - nil (cron tick): build deps from current projections, call
//     sensor.Detect, exit if no signal; otherwise SynthesizeInject.
//   - non-nil (manual debug, type *synth.InjectSignal): consult the
//     debounce gate (unless ctx carries api.InjectForceKey{}=true);
//     either return api.ErrInjectDebounced (mapped to HTTP 429) or
//     run SynthesizeInject directly.
//
// On a successful synth: persist card + fragment, append a
// synth.message_inject event to the log, publish the card on the bus
// for SSE delivery to any open browser tab. On synth failure: append
// synth.message_inject.failed and surface the error to the caller.
func buildInjectFn(d injectFnDeps) schedule.InjectFunc {
	return func(ctx context.Context, sigAny any) error {
		now := d.Now()
		tz := d.ProjCfg.TZ
		if tz == nil && d.ProjCfg.Clock != nil {
			tz = d.ProjCfg.Clock.Location()
		}
		if tz == nil {
			tz = time.UTC
		}
		date := now.In(tz).Format("2006-01-02")

		var signal *synth.InjectSignal

		if sigAny == nil {
			// Cron-tick path — run Detect first.
			detectorDeps, err := buildDetectorDeps(ctx, d, now, date)
			if err != nil {
				return fmt.Errorf("build detector deps: %w", err)
			}
			signal = sensor.Detect(detectorDeps, d.DetectorCfg)
			if signal == nil {
				if d.Logger != nil {
					d.Logger.Debug("inject: no signal this tick")
				}
				return nil
			}
		} else {
			// Manual debug path — caller supplied a synthetic signal.
			s, ok := sigAny.(*synth.InjectSignal)
			if !ok || s == nil {
				return fmt.Errorf("manual inject path expected *synth.InjectSignal, got %T", sigAny)
			}
			// Debounce check unless the manual path explicitly forces.
			force, _ := ctx.Value(api.InjectForceKey{}).(bool)
			if !force {
				lastFire := lookupLastInjectFire(ctx, d.Reader)
				if !lastFire.IsZero() && now.Sub(lastFire) < d.DetectorCfg.DebounceWindow {
					return api.ErrInjectDebounced
				}
			}
			signal = s
		}

		injectDeps := synth.InjectDeps{
			LLM:             d.LLM,
			Reader:          d.Reader,
			ProjCfg:         d.ProjCfg,
			Memory:          d.Memory,
			MemoryRanker:    d.MemoryRanker,
			Prompts:         d.Prompts,
			Date:            date,
			Now:             now,
			Logger:          d.Logger,
			Bus:             d.Bus, // V2.4: thread bus through so synth.started/completed publish
			LoopObserver:    d.LoopObserver,
			OnSynthRun:      d.OnSynthRun,
			JinaClient:      d.JinaClient,
			JinaCache:       d.JinaCache,
			SearchTTL:       d.SearchTTL,
			ReadTTL:         d.ReadTTL,
			WiredIntents:    d.WiredIntents,
			FinalCallBudget: d.FinalCallBudget,
		}
		result, err := synth.SynthesizeInject(ctx, injectDeps, *signal)
		if err != nil {
			if d.EventLog != nil {
				_, _ = d.EventLog.Append(ctx, logp.KindSynthMessageInjectFailed, "inject", map[string]any{
					"date":   date,
					"signal": signal,
					"error":  err.Error(),
				})
			}
			return fmt.Errorf("synth inject: %w", err)
		}

		// Persist + audit + publish.
		if err := d.CardRepo.Upsert(ctx, []store.Card{result.Card}); err != nil {
			return fmt.Errorf("persist inject card: %w", err)
		}
		if err := d.BriefingRepo.UpsertInject(ctx, result.Fragment); err != nil {
			return fmt.Errorf("persist inject fragment: %w", err)
		}
		if d.EventLog != nil {
			_, _ = d.EventLog.Append(ctx, logp.KindSynthMessageInject, "inject", map[string]any{
				"date":    date,
				"signal":  signal,
				"card_id": result.Card.ID,
			})
		}
		d.Bus.PublishCard(result.Card)
		if d.Logger != nil {
			d.Logger.WithField("card_id", result.Card.ID).
				WithField("kind", signal.Kind).
				Info("inject: card published")
		}
		return nil
	}
}

// buildDetectorDeps assembles InjectDetectorDeps from the current
// projections + today's persisted cards + the most recent inject fire
// time pulled from the event log.
func buildDetectorDeps(ctx context.Context, d injectFnDeps, now time.Time, date string) (sensor.InjectDetectorDeps, error) {
	cal, err := projection.TodaysCalendar{Cfg: d.ProjCfg}.Compute(ctx, d.Reader)
	if err != nil {
		return sensor.InjectDetectorDeps{}, fmt.Errorf("calendar projection: %w", err)
	}
	threads, err := projection.OpenEmailThreads{Cfg: d.ProjCfg}.Compute(ctx, d.Reader)
	if err != nil {
		return sensor.InjectDetectorDeps{}, fmt.Errorf("threads projection: %w", err)
	}
	cards, err := d.CardRepo.ListByDate(ctx, date)
	if err != nil {
		return sensor.InjectDetectorDeps{}, fmt.Errorf("list cards: %w", err)
	}
	// V2.5.0: project active concerns into the detector deps so the
	// concern-boost path has a list to substring-match against.
	// Best-effort — a list-failure should not kill the inject tick.
	var concerns []projection.Concern
	if d.ConcernRepo != nil {
		ac := projection.ActiveConcerns{
			Repo:   d.ConcernRepo,
			Config: projection.ActiveConcernsConfig{Limit: 5, Now: func() time.Time { return now }},
		}
		concerns, _ = ac.Compute(ctx, d.Reader)
	}
	// Recent stock.alert events feed the stock-breach detector path.
	// Best-effort — a read failure should not kill the inject tick;
	// the path simply skips and the other paths still get a chance.
	stockBreaches, _ := projection.RecentStockBreaches{
		Cfg:     projection.Config{Now: func() time.Time { return now }},
		Horizon: d.DetectorCfg.StockBreachHorizon,
	}.Compute(ctx, d.Reader)
	return sensor.InjectDetectorDeps{
		Threads:       threads,
		Calendar:      cal,
		Cards:         cards,
		Concerns:      concerns,
		StockBreaches: stockBreaches,
		LastFire:      lookupLastInjectFire(ctx, d.Reader),
		Now:           func() time.Time { return now },
		Logger:        d.Logger,
	}, nil
}

// lookupLastInjectFire returns the timestamp of the most recent
// synth.message_inject event, or zero if none. State lives in the
// observation log so it survives daemon restarts and there's no
// in-memory state to keep in sync.
func lookupLastInjectFire(ctx context.Context, reader logp.Reader) time.Time {
	if reader == nil {
		return time.Time{}
	}
	ev, err := reader.Latest(ctx, logp.KindSynthMessageInject)
	if err != nil || ev == nil {
		return time.Time{}
	}
	return ev.TS
}
