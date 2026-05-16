package projection

import (
	"context"

	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/store"
)

// MemoryFact is the projection-side shape for a derived-memory row injected
// into the cards / reactive prompts. It strips the persistence-only fields
// (run IDs, timestamps, soft-delete state) the synth doesn't need. ID is
// kept so the embedding-relevance ranker can look up cached vectors.
type MemoryFact struct {
	ID            string
	Subject       string
	Fact          string
	Category      string
	Confidence    string
	EvidenceCount int
}

// MemoryFactsConfig holds per-projection knobs. Memory needs a per-call cap
// (cards uses 20, reactive uses 8) which doesn't fit the shared
// projection.Config, so it lives on the projection itself.
type MemoryFactsConfig struct {
	Limit int
}

// MemoryFacts wraps the MemoryRepo and surfaces the top-N visible facts
// ordered by confidence (high→med→low) then recency. The projection bypasses
// the event-log Reader — memory state lives in its own table — but still
// implements the Projection[T] interface for plug-symmetry with the other
// projections.
type MemoryFacts struct {
	Repo   *store.MemoryRepo
	Config MemoryFactsConfig
}

// Name returns the projection identifier.
func (p MemoryFacts) Name() string { return "memory_facts" }

// Compute returns up to Config.Limit visible memory facts. Returns an empty
// slice (not nil error) when the repo is unset so callers can degrade
// gracefully — the same shape projections like RunWindow use when the
// underlying signal is missing.
func (p MemoryFacts) Compute(ctx context.Context, _ log.Reader) ([]MemoryFact, error) {
	if p.Repo == nil {
		return nil, nil
	}
	limit := p.Config.Limit
	if limit <= 0 {
		limit = 20
	}
	rows, err := p.Repo.ListTop(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]MemoryFact, 0, len(rows))
	for _, r := range rows {
		out = append(out, MemoryFact{
			ID:            r.ID,
			Subject:       r.Subject,
			Fact:          r.Fact,
			Category:      r.Category,
			Confidence:    r.Confidence,
			EvidenceCount: r.EvidenceCount,
		})
	}
	return out, nil
}
