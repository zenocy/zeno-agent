package embeddings

import (
	"context"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"
)

// FactRow is the slim shape Warmup expects per visible fact.
type FactRow struct {
	ID   string
	Fact string
}

// FactLister is satisfied by any caller that can list every visible
// memory fact's id + text. internal/store/memory.go.MemoryRepo provides
// ListAllVisible which the caller wraps into this shape.
type FactLister interface {
	ListAllFactRows(ctx context.Context) ([]FactRow, error)
}

// WarmupStats summarises a single warmup pass. The bucket counters split
// Failed by failure mode so an operator can see at a glance whether a
// warmup that lost facts did so to corrupt cache rows, embed-endpoint
// errors, or storage failures.
type WarmupStats struct {
	Loaded     int
	Reembedded int
	Evicted    int
	Failed     int
	Duration   time.Duration

	SkippedCorrupt     int // step 2: cached vector blob couldn't be decoded
	SkippedDimMismatch int // step 2: cached vector decoded but dims didn't match index
	EmbedFailed        int // step 3: embedder endpoint returned an error
	IndexUpsertFailed  int // step 3: in-memory index rejected the vector
	EncodeFailed       int // step 3: encoding the vector for persistence failed
	StoreUpsertFailed  int // step 3: SQLite upsert failed
}

// Warmup synchronises the in-memory index and the persistent store with
// the live fact set:
//
//  1. Evict every persisted row whose model_id is not the embedder's
//     current ModelID. A model swap therefore wipes the cache before
//     repopulation.
//  2. Load all rows for the current model_id, decode their vectors, and
//     fill the in-memory index.
//  3. Walk the live fact set. For each fact, compare its content_hash
//     against the index. Re-embed and persist anything missing or
//     changed. Per-fact errors warn and continue.
//
// Failures during step 1 or 2 are returned as hard errors (the index will
// be empty and the ranker will fall back to ListTop, but the operator
// needs to see the failure). Step 3 is best-effort.
func Warmup(
	ctx context.Context,
	store *Store,
	idx *MemoryIndex,
	lister FactLister,
	logger *logrus.Entry,
) (WarmupStats, error) {
	if logger == nil {
		logger = logrus.NewEntry(logrus.New())
	}
	stats := WarmupStats{}
	start := time.Now()

	if store == nil || idx == nil {
		return stats, fmt.Errorf("embeddings: Warmup requires non-nil store and index")
	}
	modelID := idx.Embedder().ModelID()
	if modelID == "" {
		return stats, fmt.Errorf("embeddings: Warmup requires embedder.ModelID() != \"\"")
	}

	evicted, err := store.DeleteByModelID(ctx, modelID)
	if err != nil {
		return stats, fmt.Errorf("evict stale-model rows: %w", err)
	}
	stats.Evicted = int(evicted)

	rows, err := store.Load(ctx, modelID)
	if err != nil {
		return stats, fmt.Errorf("load cached vectors: %w", err)
	}
	// Per-fact failures during step 2 are logged at DEBUG so the terminal
	// summary stays the only place those counts are visible at INFO; raise
	// to WARN only if any bucket > 0 in the post-loop summary.
	var firstCorruptErr, firstDimErr error
	var firstCorruptID, firstDimID string
	for _, r := range rows {
		vec, decErr := DecodeVector(r.Vector, r.Dims)
		if decErr != nil {
			stats.SkippedCorrupt++
			stats.Failed++
			if firstCorruptErr == nil {
				firstCorruptErr, firstCorruptID = decErr, r.ID
			}
			continue
		}
		if upErr := idx.UpsertVector(r.ID, r.ContentHash, vec); upErr != nil {
			stats.SkippedDimMismatch++
			stats.Failed++
			if firstDimErr == nil {
				firstDimErr, firstDimID = upErr, r.ID
			}
			continue
		}
		stats.Loaded++
	}

	facts, err := lister.ListAllFactRows(ctx)
	if err != nil {
		return stats, fmt.Errorf("list visible facts: %w", err)
	}
	var firstEmbedErr, firstIdxErr, firstEncodeErr, firstStoreErr error
	var firstEmbedID, firstIdxID, firstEncodeID, firstStoreID string
	for _, f := range facts {
		if f.ID == "" || f.Fact == "" {
			continue
		}
		hash := HashContent(f.Fact)
		if existing, ok := idx.GetHash(f.ID); ok && existing == hash {
			continue
		}
		vec, embErr := idx.Embedder().Embed(ctx, f.Fact)
		if embErr != nil {
			stats.EmbedFailed++
			stats.Failed++
			if firstEmbedErr == nil {
				firstEmbedErr, firstEmbedID = embErr, f.ID
			}
			continue
		}
		if upErr := idx.UpsertVector(f.ID, hash, vec); upErr != nil {
			stats.IndexUpsertFailed++
			stats.Failed++
			if firstIdxErr == nil {
				firstIdxErr, firstIdxID = upErr, f.ID
			}
			continue
		}
		blob, encErr := EncodeVector(vec)
		if encErr != nil {
			stats.EncodeFailed++
			stats.Failed++
			if firstEncodeErr == nil {
				firstEncodeErr, firstEncodeID = encErr, f.ID
			}
			continue
		}
		row := MemoryEmbedding{
			ID:          f.ID,
			ContentHash: hash,
			ModelID:     modelID,
			Dims:        len(vec),
			Vector:      blob,
			UpdatedAt:   time.Now().UTC(),
		}
		if upErr := store.Upsert(ctx, row); upErr != nil {
			stats.StoreUpsertFailed++
			stats.Failed++
			if firstStoreErr == nil {
				firstStoreErr, firstStoreID = upErr, f.ID
			}
			continue
		}
		stats.Reembedded++
	}

	stats.Duration = time.Since(start)
	fields := logrus.Fields{
		"loaded":               stats.Loaded,
		"reembedded":           stats.Reembedded,
		"evicted":              stats.Evicted,
		"failed":               stats.Failed,
		"skipped_corrupt":      stats.SkippedCorrupt,
		"skipped_dim_mismatch": stats.SkippedDimMismatch,
		"embed_failed":         stats.EmbedFailed,
		"index_upsert_failed":  stats.IndexUpsertFailed,
		"encode_failed":        stats.EncodeFailed,
		"store_upsert_failed":  stats.StoreUpsertFailed,
		"ms":                   stats.Duration.Milliseconds(),
	}
	if stats.Failed > 0 {
		// One representative example per bucket — full per-fact detail is
		// available at DEBUG via re-running with logging.level: debug.
		entry := logger.WithFields(fields)
		if firstCorruptErr != nil {
			entry = entry.WithField("first_corrupt_id", firstCorruptID).WithField("first_corrupt_err", firstCorruptErr.Error())
		}
		if firstDimErr != nil {
			entry = entry.WithField("first_dim_id", firstDimID).WithField("first_dim_err", firstDimErr.Error())
		}
		if firstEmbedErr != nil {
			entry = entry.WithField("first_embed_id", firstEmbedID).WithField("first_embed_err", firstEmbedErr.Error())
		}
		if firstIdxErr != nil {
			entry = entry.WithField("first_idx_id", firstIdxID).WithField("first_idx_err", firstIdxErr.Error())
		}
		if firstEncodeErr != nil {
			entry = entry.WithField("first_encode_id", firstEncodeID).WithField("first_encode_err", firstEncodeErr.Error())
		}
		if firstStoreErr != nil {
			entry = entry.WithField("first_store_id", firstStoreID).WithField("first_store_err", firstStoreErr.Error())
		}
		entry.Warn("embeddings: warmup complete with failures")
	} else {
		logger.WithFields(fields).Info("embeddings: warmup complete")
	}
	return stats, nil
}
