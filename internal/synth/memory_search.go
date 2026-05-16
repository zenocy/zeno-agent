package synth

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/embeddings"
	"github.com/zenocy/zeno-v2/internal/projection"
)

// confidenceWeight maps the persisted confidence tier to a [0,1] score
// component. Mirrors the high→med→low ordering used by ListTop so the
// pre-V2.2.1 baseline stays the natural fallback.
var confidenceWeight = map[string]float32{
	"high": 1.0,
	"med":  0.7,
	"low":  0.4,
}

// MemoryRanker re-orders the candidate pool by relevance to a query string.
// Score is a hybrid: cosine(query, fact) * WSim + confidenceWeight * WConf.
// On any failure (nil index, embedder error, all results below MinScore,
// empty query) it returns the input pool truncated to k — never nil — so
// reactive Ask and morning cards never lose memory injection because of a
// ranker miss.
type MemoryRanker struct {
	Index    *embeddings.MemoryIndex
	WSim     float32
	WConf    float32
	MinScore float32
	Logger   *logrus.Entry
}

// Rank returns at most k facts from pool, ordered by hybrid score desc with
// stable tie-break on the original pool index. Empty / unrankable inputs
// fall through to truncation.
func (r *MemoryRanker) Rank(ctx context.Context, query string, pool []projection.MemoryFact, k int) []projection.MemoryFact {
	if k <= 0 || len(pool) == 0 {
		return nil
	}
	if r == nil || r.Index == nil || strings.TrimSpace(query) == "" {
		return truncatePool(pool, k)
	}

	logger := r.Logger
	if logger == nil {
		logger = logrus.NewEntry(logrus.New())
	}
	start := time.Now()

	queryVec, err := r.Index.Embedder().Embed(ctx, query)
	if err != nil {
		logger.WithError(err).WithFields(logrus.Fields{
			"query_len": len(query),
			"pool_size": len(pool),
		}).Warn("rerank: embed query failed — falling back to ListTop order")
		return truncatePool(pool, k)
	}

	wSim := r.WSim
	if wSim == 0 {
		wSim = 1.0
	}
	wConf := r.WConf
	if wConf == 0 {
		wConf = 0.3
	}

	type scored struct {
		idx     int
		score   float32
		hasSim  bool
		similar float32
	}
	out := make([]scored, 0, len(pool))
	for i, f := range pool {
		s := scored{idx: i, score: wConf * confidenceWeight[f.Confidence]}
		if f.ID != "" {
			if vec, ok := r.Index.GetVector(f.ID); ok && len(vec) == len(queryVec) {
				sim := cosineFloat32(queryVec, vec)
				if sim < r.MinScore {
					continue
				}
				s.hasSim = true
				s.similar = sim
				s.score += wSim * sim
			}
		}
		out = append(out, s)
	}

	if len(out) == 0 {
		// Every candidate fell below MinScore. The fallback is "do not
		// regress to silence" — emit the original pool ordering.
		logger.WithFields(logrus.Fields{
			"pool_size":  len(pool),
			"min_score":  r.MinScore,
			"latency_ms": time.Since(start).Milliseconds(),
		}).Info("rerank: all candidates below min_score — falling back to ListTop order")
		return truncatePool(pool, k)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score == out[j].score {
			return out[i].idx < out[j].idx
		}
		return out[i].score > out[j].score
	})

	limit := min(k, len(out))
	picked := make([]projection.MemoryFact, 0, limit)
	for i := 0; i < limit; i++ {
		picked = append(picked, pool[out[i].idx])
	}

	topScore := float32(0)
	topSim := float32(0)
	if len(out) > 0 {
		topScore = out[0].score
		topSim = out[0].similar
	}
	logger.WithFields(logrus.Fields{
		"query_len":  len(query),
		"pool_size":  len(pool),
		"picked":     len(picked),
		"k":          k,
		"top_score":  topScore,
		"top_sim":    topSim,
		"w_sim":      wSim,
		"w_conf":     wConf,
		"latency_ms": time.Since(start).Milliseconds(),
	}).Info("rerank: ranked memory pool")
	return picked
}

// CardsSyntheticQuery builds the deterministic query string used by the
// morning cards ranker. Calendar event titles join thread subjects in fixed
// order (events sorted by start ascending, threads by subject ascending) so
// goldens stay byte-equal across runs. Returns "" when there are no
// signals — caller falls back to the unranked pool in that case.
func CardsSyntheticQuery(cal []projection.CalendarEvent, threads []projection.Thread) string {
	parts := make([]string, 0, len(cal)+len(threads))

	events := append([]projection.CalendarEvent(nil), cal...)
	sort.SliceStable(events, func(i, j int) bool { return events[i].Start.Before(events[j].Start) })
	for _, e := range events {
		t := strings.TrimSpace(e.Title)
		if t != "" {
			parts = append(parts, t)
		}
	}

	subs := make([]string, 0, len(threads))
	for _, t := range threads {
		s := strings.TrimSpace(t.Subject)
		if s != "" {
			subs = append(subs, s)
		}
	}
	sort.Strings(subs)
	parts = append(parts, subs...)

	return strings.Join(parts, " ")
}

func truncatePool(pool []projection.MemoryFact, k int) []projection.MemoryFact {
	if len(pool) <= k {
		return pool
	}
	return pool[:k]
}

func cosineFloat32(a, b []float32) float32 {
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return float32(dot)
}
