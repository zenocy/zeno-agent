// Package embeddings provides semantic-similarity utilities for memory-fact
// ranking. The public surface is small:
//
//   - Embedder:    abstracts whichever model produces vectors (LM Studio in
//     production, FakeEmbedder in tests).
//   - MemoryIndex: cosine-similarity nearest-neighbour index over fact text.
//   - Store:       GORM-backed persistence of the vector cache, keyed by
//     (id, content_hash, model_id).
//
// None of this package depends on synth or store — it is a standalone
// library so the ranker, the HTTP handler, and future callers can reuse it
// without circular imports.
package embeddings

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
)

// FakeModelID is the logical identifier returned by FakeEmbedder. Stored in
// memory_embeddings.model_id when tests populate the cache directly so the
// production warmup can detect and evict fake-embedded rows.
const FakeModelID = "fake-sha256"

// Embedder turns a string into a fixed-dimension dense float32 vector.
//
// Implementations must be safe for concurrent use. Dims may return 0 until
// the first successful Embed call (e.g. when the dimension is discovered
// from a remote endpoint), but once it returns a non-zero value it must
// stay constant for the lifetime of the instance.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	Dims() int
	ModelID() string
}

// ErrEmptyText is returned when an empty string is passed to Embed.
var ErrEmptyText = errors.New("embeddings: cannot embed empty text")

// HashContent returns a stable, short hash of fact text. Used as the cache
// key for content_hash columns and the index dedupe check. 16 hex chars
// (64 bits) is more than enough for the V2.2 50-fact total cap.
func HashContent(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// cosine returns the cosine similarity of a and b, assuming both have been
// L2-normalised (the dot product equals cosine for unit vectors). Callers
// must validate equal length before calling.
func cosine(a, b []float32) float32 {
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return float32(dot)
}

// l2Normalize divides v by its L2 norm in place. No-op if the norm is zero.
func l2Normalize(v []float32) {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	norm := float32(math.Sqrt(sum))
	if norm == 0 {
		return
	}
	for i := range v {
		v[i] /= norm
	}
}

// FakeEmbedder is a deterministic test embedder. It hashes the input with
// SHA-256 and expands the 32 bytes into a `dims`-dimensional float32 vector
// by reading float32s from the repeated hash stream. Properties tests rely
// on:
//
//  1. Identical input → identical output (always).
//  2. Different input → different output (with overwhelming probability).
//  3. No network, no files, no CGO — safe to use anywhere.
//
// The vector is L2-normalised so cosine similarity equals the dot product.
type FakeEmbedder struct {
	dims int
}

// NewFakeEmbedder returns a deterministic test embedder with the given
// output dimension. dims <= 0 falls back to 16.
func NewFakeEmbedder(dims int) *FakeEmbedder {
	if dims <= 0 {
		dims = 16
	}
	return &FakeEmbedder{dims: dims}
}

// Embed implements Embedder.
func (f *FakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if text == "" {
		return nil, ErrEmptyText
	}
	vec := make([]float32, f.dims)
	seed := sha256.Sum256([]byte(text))
	pos := 0
	block := seed
	for pos < f.dims {
		for i := 0; i+4 <= len(block) && pos < f.dims; i += 4 {
			u := binary.LittleEndian.Uint32(block[i : i+4])
			vec[pos] = float32(int32(u)) / float32(math.MaxInt32)
			pos++
		}
		if pos < f.dims {
			block = sha256.Sum256(block[:])
		}
	}
	l2Normalize(vec)
	return vec, nil
}

// Dims implements Embedder.
func (f *FakeEmbedder) Dims() int { return f.dims }

// ModelID implements Embedder.
func (f *FakeEmbedder) ModelID() string { return FakeModelID }
