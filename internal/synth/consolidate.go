package synth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/embeddings"
	"github.com/zenocy/zeno-v2/internal/llm"
	"github.com/zenocy/zeno-v2/internal/store"
)

// ConsolidateDeps bundles the consolidator's external dependencies.
// EmbeddingStore + EmbeddingIndex are optional; when both are non-nil the
// consolidator updates the relevance index after a successful write so the
// next reactive Ask sees fresh vectors. Failures here log and emit
// memory.embedding.failed but never fail the consolidation.
type ConsolidateDeps struct {
	Repo           *store.MemoryRepo
	EmbeddingStore *embeddings.Store
	EmbeddingIndex *embeddings.MemoryIndex
	Now            func() time.Time
	Logger         *logrus.Entry
}

// ConsolidateConfig holds the tunable thresholds for promotion and eviction.
// Zero values fall back to V2.2.0 plan-approval defaults.
type ConsolidateConfig struct {
	PromoteToMedAt  int // default 3
	PromoteToHighAt int // default 7
	MaxFacts        int // default 50
	MaxPerRun       int // candidates ignored beyond this; default 3 (also enforced at parse)
}

// ConsolidateDelta is the audit shape recorded in the memory.consolidated
// event. Subjects (not IDs) are used in most fields for human readability;
// Evicted uses IDs because the rows are gone from the visible set after the
// soft-delete and IDs are stable across confirmations.
type ConsolidateDelta struct {
	Added       []string `json:"added,omitempty"`        // subjects newly inserted
	Incremented []string `json:"incremented,omitempty"`  // subjects whose evidence count went up
	Promoted    []string `json:"promoted,omitempty"`     // subjects whose confidence tier changed (subset of Incremented)
	Evicted     []string `json:"evicted,omitempty"`      // IDs soft-deleted at cap
	Skipped     []string `json:"skipped,omitempty"`      // subjects dropped via denylist (soft-deleted lookup)
	SkippedSeen []string `json:"skipped_seen,omitempty"` // subjects whose LastSeenRunID == runID (idempotent re-run)
}

// Consolidate folds a run's memory candidates into the durable MemoryFact
// store. Pure deterministic logic — no LLM call. The algorithm per
// candidate (in order):
//
//  1. Idempotency: existing fact with LastSeenRunID == runID → SkippedSeen.
//  2. Denylist: existing fact with DeletedAt set → Skipped.
//  3. Reinforce: existing visible fact → IncrementEvidence; if the new
//     count crosses PromoteToMedAt or PromoteToHighAt, PromoteConfidence.
//  4. Insert: no existing fact → create at Confidence=low, Source=synth,
//     EvidenceCount=1.
//  5. Eviction: after all candidates, if visible row count > MaxFacts,
//     evict (low-confidence, oldest-LastReinforced) non-user rows down to
//     the cap.
//
// Candidates beyond MaxPerRun are dropped (defense in depth — the parser
// already caps per-response).
func Consolidate(
	ctx context.Context,
	deps ConsolidateDeps,
	cfg ConsolidateConfig,
	runID string,
	candidates []llm.MemoryCandidate,
) (ConsolidateDelta, error) {
	if deps.Repo == nil {
		return ConsolidateDelta{}, fmt.Errorf("consolidate: repo required")
	}
	if cfg.PromoteToMedAt <= 0 {
		cfg.PromoteToMedAt = 3
	}
	if cfg.PromoteToHighAt <= 0 {
		cfg.PromoteToHighAt = 7
	}
	if cfg.MaxFacts <= 0 {
		cfg.MaxFacts = 50
	}
	if cfg.MaxPerRun <= 0 {
		cfg.MaxPerRun = 3
	}
	now := time.Now
	if deps.Now != nil {
		now = deps.Now
	}
	logger := deps.Logger
	if logger == nil {
		logger = logrus.NewEntry(logrus.New())
	}

	if len(candidates) > cfg.MaxPerRun {
		candidates = candidates[:cfg.MaxPerRun]
	}

	delta := ConsolidateDelta{}
	for _, cand := range candidates {
		subj := strings.ToLower(strings.TrimSpace(cand.Subject))
		if subj == "" {
			continue
		}
		existing, err := deps.Repo.GetBySubject(ctx, subj, true)
		if err != nil {
			return delta, fmt.Errorf("lookup subject %q: %w", subj, err)
		}

		// Idempotency: same run, same subject → already counted this run.
		if existing != nil && existing.LastSeenRunID == runID {
			delta.SkippedSeen = append(delta.SkippedSeen, subj)
			continue
		}

		// Denylist: user soft-deleted this subject; do not re-emerge.
		if existing != nil && existing.DeletedAt.Valid {
			delta.Skipped = append(delta.Skipped, subj)
			continue
		}

		if existing != nil {
			// Reinforce existing visible fact.
			if err := deps.Repo.IncrementEvidence(ctx, existing.ID, runID); err != nil {
				return delta, fmt.Errorf("increment %q: %w", existing.ID, err)
			}
			delta.Incremented = append(delta.Incremented, subj)
			newCount := existing.EvidenceCount + 1
			newConf := promoteConfidence(existing.Confidence, newCount, cfg)
			if newConf != existing.Confidence {
				if err := deps.Repo.PromoteConfidence(ctx, existing.ID, newConf); err != nil {
					return delta, fmt.Errorf("promote %q: %w", existing.ID, err)
				}
				delta.Promoted = append(delta.Promoted, subj)
			}
			continue
		}

		// Insert new low-confidence fact.
		fact := store.MemoryFact{
			ID:             memoryFactID(subj, cand.Predicate),
			Subject:        subj,
			Fact:           strings.TrimSpace(cand.Predicate),
			Category:       "misc",
			Confidence:     "low",
			Source:         "synth",
			EvidenceCount:  1,
			FirstSeenRunID: runID,
			LastSeenRunID:  runID,
			FirstSeen:      now(),
			LastReinforced: now(),
		}
		if err := deps.Repo.Insert(ctx, fact); err != nil {
			return delta, fmt.Errorf("insert %q: %w", subj, err)
		}
		delta.Added = append(delta.Added, subj)
	}

	// Eviction at cap. EvictExcess is idempotent across re-runs (a no-op
	// when total ≤ cap) so re-running the consolidator on the same day
	// doesn't re-evict.
	evicted, err := deps.Repo.EvictExcess(ctx, cfg.MaxFacts)
	if err != nil {
		return delta, fmt.Errorf("evict at cap: %w", err)
	}
	if len(evicted) > 0 {
		delta.Evicted = append(delta.Evicted, evicted...)
		logger.WithField("evicted", len(evicted)).Info("consolidate: evicted at cap")
	}

	updateConsolidatorEmbeddings(ctx, deps, delta, logger)

	return delta, nil
}

// updateConsolidatorEmbeddings keeps the vector cache in step with the
// consolidator's writes. Only Added subjects need new vectors (Incremented
// only bumps evidence count; the fact text is unchanged). Evicted IDs lose
// their cached vector. Best-effort: per-row failures warn-and-continue so
// embedding outages never fail the synth run.
func updateConsolidatorEmbeddings(
	ctx context.Context,
	deps ConsolidateDeps,
	delta ConsolidateDelta,
	logger *logrus.Entry,
) {
	if deps.EmbeddingStore == nil || deps.EmbeddingIndex == nil {
		return
	}
	if len(delta.Added) == 0 && len(delta.Evicted) == 0 {
		return
	}
	start := time.Now()
	modelID := deps.EmbeddingIndex.Embedder().ModelID()

	addedOK, addedFail := 0, 0
	for _, subj := range delta.Added {
		row, err := deps.Repo.GetBySubject(ctx, subj, false)
		if err != nil || row == nil {
			logger.WithField("subject", subj).WithError(err).Warn("embed: lookup after add failed")
			addedFail++
			continue
		}
		if err := persistEmbedding(ctx, deps, row.ID, row.Fact, modelID); err != nil {
			logger.WithFields(logrus.Fields{
				"subject": subj,
				"id":      row.ID,
			}).WithError(err).Warn("embed: persist after add failed")
			addedFail++
			continue
		}
		logger.WithFields(logrus.Fields{
			"subject":  subj,
			"id":       row.ID,
			"fact_len": len(row.Fact),
		}).Debug("embed: indexed new consolidator fact")
		addedOK++
	}
	for _, id := range delta.Evicted {
		deps.EmbeddingIndex.Remove(id)
		if err := deps.EmbeddingStore.Delete(ctx, id); err != nil {
			logger.WithField("id", id).WithError(err).Warn("embed: delete after evict failed")
			continue
		}
		logger.WithField("id", id).Debug("embed: removed evicted fact")
	}
	logger.WithFields(logrus.Fields{
		"added_ok":   addedOK,
		"added_fail": addedFail,
		"evicted":    len(delta.Evicted),
		"latency_ms": time.Since(start).Milliseconds(),
	}).Info("embed: consolidator post-write hook complete")
}

// persistEmbedding embeds factText, stores the vector, and updates the
// in-memory index. Caller has already filtered for non-nil Store + Index.
func persistEmbedding(
	ctx context.Context,
	deps ConsolidateDeps,
	id, factText, modelID string,
) error {
	if id == "" || factText == "" {
		return nil
	}
	if err := deps.EmbeddingIndex.Upsert(ctx, id, factText); err != nil {
		return err
	}
	vec, ok := deps.EmbeddingIndex.GetVector(id)
	if !ok || len(vec) == 0 {
		return fmt.Errorf("vector missing after upsert")
	}
	blob, err := embeddings.EncodeVector(vec)
	if err != nil {
		return err
	}
	return deps.EmbeddingStore.Upsert(ctx, embeddings.MemoryEmbedding{
		ID:          id,
		ContentHash: embeddings.HashContent(factText),
		ModelID:     modelID,
		Dims:        len(vec),
		Vector:      blob,
		UpdatedAt:   time.Now().UTC(),
	})
}

// promoteConfidence maps an evidence count to the highest tier it earns:
// ≥PromoteToHighAt → "high"; ≥PromoteToMedAt → "med"; else current. Never
// demotes — demotion is user-delete-only in V2.2.0.
func promoteConfidence(current string, evidenceCount int, cfg ConsolidateConfig) string {
	target := current
	switch {
	case evidenceCount >= cfg.PromoteToHighAt:
		target = "high"
	case evidenceCount >= cfg.PromoteToMedAt:
		if current == "low" {
			target = "med"
		}
	}
	// Don't demote: if current is already "high" we never go back to "med".
	if confidenceRank(target) < confidenceRank(current) {
		return current
	}
	return target
}

// confidenceRank maps a confidence string to a comparable integer. Used by
// promoteConfidence to enforce the no-demotion rule.
func confidenceRank(c string) int {
	switch c {
	case "high":
		return 2
	case "med":
		return 1
	default:
		return 0
	}
}

// memoryFactID builds the same deterministic ID scheme used by the eval
// fixture seed loader and cmd/zeno/replay.go's --memory-fixture path. Re-
// emitting the same (subject, predicate) on a different run produces the
// same row identity — which is what the consolidator's same-subject lookup
// already relies on; the ID is just a stable secondary identifier for
// debugging.
func memoryFactID(subject, fact string) string {
	subj := strings.ToLower(strings.TrimSpace(subject))
	sum := sha256.Sum256([]byte(fact))
	return subj + "-" + hex.EncodeToString(sum[:4])
}
