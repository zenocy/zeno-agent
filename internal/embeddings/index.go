package embeddings

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// IndexEntry is one row in the in-memory index: the current vector for a
// fact plus the hash of the content it was embedded from, so the warmup
// path can diff the live store against the cache without re-embedding.
type IndexEntry struct {
	ID          string
	ContentHash string
	Vector      []float32
}

// Match is one result from MemoryIndex.Search.
type Match struct {
	ID    string
	Score float32 // cosine similarity in [-1, 1]
}

// MemoryIndex is an in-memory cosine-similarity nearest-neighbour index
// over memory-fact text. It is not a vector DB — N is bounded by the V2.2
// 50-fact total cap, so a linear scan per query is fine and avoids a heavy
// dependency.
//
// Safe for concurrent Read (Search) and Write (Upsert/Remove). Writes take
// a write lock; reads take a read lock.
type MemoryIndex struct {
	mu       sync.RWMutex
	embedder Embedder
	entries  map[string]IndexEntry
}

// NewMemoryIndex constructs an empty index bound to embedder.
func NewMemoryIndex(embedder Embedder) *MemoryIndex {
	return &MemoryIndex{
		embedder: embedder,
		entries:  make(map[string]IndexEntry),
	}
}

// Embedder returns the embedder in use. Used by warmup to read ModelID.
func (idx *MemoryIndex) Embedder() Embedder { return idx.embedder }

// Size returns the number of facts currently indexed.
func (idx *MemoryIndex) Size() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return len(idx.entries)
}

// Has reports whether the index currently holds an entry for id.
func (idx *MemoryIndex) Has(id string) bool {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	_, ok := idx.entries[id]
	return ok
}

// GetHash returns the content hash stored for id, and whether the entry
// exists. Used by callers that want to skip an embed call when the content
// hasn't changed (the consolidator after a same-day re-run).
func (idx *MemoryIndex) GetHash(id string) (string, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	e, ok := idx.entries[id]
	if !ok {
		return "", false
	}
	return e.ContentHash, true
}

// GetVector returns a copy of the stored vector for id, and whether the
// entry exists.
func (idx *MemoryIndex) GetVector(id string) ([]float32, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	e, ok := idx.entries[id]
	if !ok {
		return nil, false
	}
	out := make([]float32, len(e.Vector))
	copy(out, e.Vector)
	return out, true
}

// UpsertVector stores a precomputed vector for a fact. Used by the warmup
// path (loading from SQLite) and by tests that need controlled vectors.
//
// If the embedder's Dims is non-zero and disagrees with len(vector) the
// call returns an error so a mid-flight model swap surfaces loudly. A
// zero embedder.Dims (e.g. LMStudioEmbedder before its first call) is
// tolerated — the next Embed will set it.
func (idx *MemoryIndex) UpsertVector(id, contentHash string, vector []float32) error {
	if id == "" {
		return errors.New("embeddings: empty memory id")
	}
	if len(vector) == 0 {
		return errors.New("embeddings: empty vector")
	}
	if d := idx.embedder.Dims(); d > 0 && len(vector) != d {
		return fmt.Errorf("embeddings: vector dims %d do not match embedder dims %d", len(vector), d)
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	cp := make([]float32, len(vector))
	copy(cp, vector)
	idx.entries[id] = IndexEntry{ID: id, ContentHash: contentHash, Vector: cp}
	return nil
}

// Upsert embeds content and stores the resulting vector under id. If the
// fact already has an entry whose hash matches the new content's hash,
// the embed call is skipped (cache hit). Empty content removes the entry.
func (idx *MemoryIndex) Upsert(ctx context.Context, id, content string) error {
	if id == "" {
		return errors.New("embeddings: empty memory id")
	}
	if content == "" {
		idx.Remove(id)
		return nil
	}
	hash := HashContent(content)
	if existing, ok := idx.GetHash(id); ok && existing == hash {
		return nil
	}
	vec, err := idx.embedder.Embed(ctx, content)
	if err != nil {
		return fmt.Errorf("embeddings: embed memory %q: %w", id, err)
	}
	return idx.UpsertVector(id, hash, vec)
}

// Remove deletes the entry for id if present.
func (idx *MemoryIndex) Remove(id string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	delete(idx.entries, id)
}

// Search returns the top-k facts most similar to query text, filtered by
// minScore. Returns nil if the query cannot be embedded or the index is
// empty.
//
// k <= 0 means unlimited. minScore in [-1, 1]; pass any negative value to
// disable the threshold.
func (idx *MemoryIndex) Search(ctx context.Context, text string, k int, minScore float32) ([]Match, error) {
	if text == "" {
		return nil, nil
	}
	queryVec, err := idx.embedder.Embed(ctx, text)
	if err != nil {
		return nil, fmt.Errorf("embeddings: embed memory query: %w", err)
	}
	return idx.SearchVector(queryVec, k, minScore)
}

// SearchVector is the same as Search but takes a pre-computed query vector.
// Exported so tests can build queries with known angles.
func (idx *MemoryIndex) SearchVector(queryVec []float32, k int, minScore float32) ([]Match, error) {
	if len(queryVec) == 0 {
		return nil, errors.New("embeddings: empty query vector")
	}

	idx.mu.RLock()
	defer idx.mu.RUnlock()

	if len(idx.entries) == 0 {
		return nil, nil
	}

	matches := make([]Match, 0, len(idx.entries))
	for _, e := range idx.entries {
		if len(e.Vector) != len(queryVec) {
			return nil, fmt.Errorf("embeddings: dim mismatch (query=%d entry=%d)", len(queryVec), len(e.Vector))
		}
		score := cosine(queryVec, e.Vector)
		if score < minScore {
			continue
		}
		matches = append(matches, Match{ID: e.ID, Score: score})
	}

	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Score == matches[j].Score {
			return matches[i].ID < matches[j].ID
		}
		return matches[i].Score > matches[j].Score
	})

	if k > 0 && len(matches) > k {
		matches = matches[:k]
	}
	return matches, nil
}

// IDs returns all indexed IDs in lexical order. Useful for diagnostic
// logging and tests.
func (idx *MemoryIndex) IDs() []string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	out := make([]string, 0, len(idx.entries))
	for id := range idx.entries {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
