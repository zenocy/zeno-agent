package embeddings

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	openai "github.com/sashabaranov/go-openai"
	"github.com/sirupsen/logrus"
)

// LMStudioEmbedder calls an OpenAI-compatible /v1/embeddings endpoint
// (LM Studio in production) for one input string at a time. Single-text
// only because the call sites embed one query per request; we do not
// batch.
//
// Failure mode is fail-fast: a 3-second default timeout, no retries. The
// MemoryRanker layer above logs and falls back to ListTop ordering when
// any embed call returns an error.
type LMStudioEmbedder struct {
	Endpoint   string        // e.g. http://192.168.88.45:1234/v1
	Model      string        // e.g. nomic-embed-text-v2-moe
	APIKey     string        // optional; LM Studio ignores it
	Timeout    time.Duration // 0 → 3s
	Dimensions int           // 0 → use model's native dimensionality; >0 sent as the OpenAI "dimensions" parameter for Matryoshka-style models (Qwen3-Embedding, text-embedding-3-*) that can truncate output
	HTTP       *http.Client  // optional; constructor builds one if nil
	Logger     *logrus.Entry // optional; nil → no embedder logging

	api        *openai.Client
	once       sync.Once
	dimsMu     sync.RWMutex
	cachedDims int
}

// NewLMStudioEmbedder constructs an embedder. Endpoint and Model must be
// non-empty; everything else has sensible defaults.
func NewLMStudioEmbedder(endpoint, model, apiKey string, timeout time.Duration) *LMStudioEmbedder {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &LMStudioEmbedder{
		Endpoint: endpoint,
		Model:    model,
		APIKey:   apiKey,
		Timeout:  timeout,
	}
}

func (e *LMStudioEmbedder) lazyInit() {
	e.once.Do(func() {
		if e.HTTP == nil {
			e.HTTP = &http.Client{Timeout: e.Timeout}
		}
		cfg := openai.DefaultConfig(e.APIKey)
		cfg.BaseURL = strings.TrimRight(e.Endpoint, "/")
		cfg.HTTPClient = e.HTTP
		e.api = openai.NewClientWithConfig(cfg)
	})
}

// Embed implements Embedder. Empty text returns ErrEmptyText.
func (e *LMStudioEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if text == "" {
		return nil, ErrEmptyText
	}
	if e.Endpoint == "" || e.Model == "" {
		return nil, errors.New("embeddings: LMStudioEmbedder requires Endpoint and Model")
	}
	e.lazyInit()

	timeout := e.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	req := openai.EmbeddingRequest{
		Input: text,
		Model: openai.EmbeddingModel(e.Model),
	}
	if e.Dimensions > 0 {
		req.Dimensions = e.Dimensions
	}
	resp, err := e.api.CreateEmbeddings(callCtx, req)
	if err != nil {
		if e.Logger != nil {
			e.Logger.WithError(err).WithFields(logrus.Fields{
				"model":      e.Model,
				"text_len":   len(text),
				"latency_ms": time.Since(start).Milliseconds(),
			}).Warn("embed: lmstudio call failed")
		}
		return nil, fmt.Errorf("embeddings: lmstudio embed: %w", err)
	}
	if len(resp.Data) == 0 || len(resp.Data[0].Embedding) == 0 {
		return nil, errors.New("embeddings: lmstudio returned empty vector")
	}
	vec := append([]float32(nil), resp.Data[0].Embedding...)
	// Some local servers do not return L2-normalised vectors. Cosine assumes
	// they are, so normalise defensively. nomic-embed-text variants are
	// normalised by default, but a future model swap shouldn't break ranking.
	l2Normalize(vec)

	e.dimsMu.Lock()
	if e.cachedDims == 0 {
		e.cachedDims = len(vec)
	}
	e.dimsMu.Unlock()

	if e.Logger != nil {
		e.Logger.WithFields(logrus.Fields{
			"model":      e.Model,
			"text_len":   len(text),
			"dims":       len(vec),
			"latency_ms": time.Since(start).Milliseconds(),
		}).Debug("embed: lmstudio call ok")
	}

	return vec, nil
}

// Dims returns the cached output dimension, or 0 if Embed has not yet
// succeeded. Callers tolerate zero (the index validates dims at UpsertVector
// time, not at embedder-init time).
func (e *LMStudioEmbedder) Dims() int {
	e.dimsMu.RLock()
	defer e.dimsMu.RUnlock()
	return e.cachedDims
}

// ModelID implements Embedder. Returns the configured model name, with the
// requested output dimensionality appended when Dimensions > 0. The persistent
// cache uses this as its invalidation key, so a dims change on a Matryoshka-
// style model (same name, different requested width) correctly evicts the
// cached vectors instead of leaving the in-memory index with mixed widths.
func (e *LMStudioEmbedder) ModelID() string {
	if e.Dimensions > 0 {
		return fmt.Sprintf("%s@%d", e.Model, e.Dimensions)
	}
	return e.Model
}
