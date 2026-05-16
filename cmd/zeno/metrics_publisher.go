package main

import (
	"context"
	"time"

	"github.com/sirupsen/logrus"
	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/eventbus"
	"github.com/zenocy/zeno-v2/internal/http/api"
	"github.com/zenocy/zeno-v2/internal/llm"
	zlog "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/metrics"
)

const (
	statsPublishInterval     = 10 * time.Second
	healthHeartbeatInterval  = 60 * time.Second
	healthLLMReachableBudget = 3 * time.Second
)

// startMetricsPublisher runs a goroutine that publishes
// `stats.snapshot` every statsPublishInterval and `health.changed` on
// every transition (plus a healthHeartbeatInterval heartbeat so a UI
// that just connected gets a baseline). The goroutine exits when ctx is
// cancelled.
//
// Replaces the React Query polls of /api/metrics/snapshot (every 30s)
// and /api/health (every 30s) with one server-side broadcast — the
// single emitter scales to N subscribers without N proportional load.
//
// Gated on bus.SubscriberCount() > 0 so a daemon with no UI tabs open
// doesn't wake up to compute aggregates nobody is listening for.
func startMetricsPublisher(
	ctx context.Context,
	bus *eventbus.Bus,
	mt *metrics.Metrics,
	db *gorm.DB,
	llmClient *llm.Client,
	reader zlog.Reader,
	startedAt time.Time,
	log *logrus.Entry,
) {
	if bus == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(statsPublishInterval)
		defer ticker.Stop()
		var lastHealth eventbus.HealthChangedEvent
		var lastHealthSent time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if bus.SubscriberCount() == 0 {
					continue
				}
				if mt != nil {
					bus.Publish(eventbus.StatsSnapshotEvent{Stats: mt.Snapshot()})
				}
				now := time.Now()
				h := computeHealth(ctx, db, llmClient, reader, startedAt)
				if healthMaterialChanged(lastHealth, h) || now.Sub(lastHealthSent) >= healthHeartbeatInterval {
					bus.Publish(h)
					lastHealth = h
					lastHealthSent = now
				}
			}
		}
	}()
	if log != nil {
		log.WithFields(logrus.Fields{
			"stats_interval":  statsPublishInterval.String(),
			"health_interval": healthHeartbeatInterval.String(),
		}).Info("metrics publisher started")
	}
}

// computeHealth mirrors HealthHandler.handle but writes into the
// eventbus event shape directly. Kept here (not in api/) so the
// transition-detection logic stays adjacent to the publisher.
func computeHealth(
	ctx context.Context,
	db *gorm.DB,
	llmClient *llm.Client,
	reader zlog.Reader,
	startedAt time.Time,
) eventbus.HealthChangedEvent {
	out := eventbus.HealthChangedEvent{
		Version: api.Version,
		Uptime:  time.Since(startedAt).Truncate(time.Second).String(),
	}
	if db != nil {
		if sqlDB, err := db.DB(); err == nil && sqlDB.Ping() == nil {
			out.DBOK = true
		}
	}
	if llmClient != nil {
		llmCtx, cancel := context.WithTimeout(ctx, healthLLMReachableBudget)
		defer cancel()
		if err := llmClient.Reachable(llmCtx); err != nil {
			out.LLMError = err.Error()
		} else {
			out.LLMReachable = true
		}
	}
	if reader != nil {
		readCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		if e, err := reader.Latest(readCtx, zlog.KindSynthRunCompleted); err == nil && e != nil {
			ts := e.TS.UTC()
			out.LastSynthAt = &ts
		}
		if e, err := reader.Latest(readCtx, zlog.KindSyncCompleted); err == nil && e != nil {
			ts := e.TS.UTC()
			out.LastSyncAt = &ts
		}
	}
	out.OK = out.DBOK // LLM may be down without zeno being unhealthy.
	return out
}

// healthMaterialChanged returns true when the user-visible health state
// (ok / db_ok / llm_reachable / llm_error / version) flipped between
// snapshots. Uptime / LastSynthAt / LastSyncAt drift continuously and
// would force a publish every tick, defeating the purpose of the gate.
func healthMaterialChanged(a, b eventbus.HealthChangedEvent) bool {
	return a.OK != b.OK ||
		a.DBOK != b.DBOK ||
		a.LLMReachable != b.LLMReachable ||
		a.LLMError != b.LLMError ||
		a.Version != b.Version
}
