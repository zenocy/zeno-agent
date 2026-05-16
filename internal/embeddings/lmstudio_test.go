package embeddings

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLMStudioEmbedder_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/embeddings") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var body struct {
			Input string `json:"input"`
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Model != "nomic-embed-text-v2-moe" {
			t.Errorf("unexpected model: %s", body.Model)
		}
		if body.Input != "hello" {
			t.Errorf("unexpected input: %s", body.Input)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]any{
				{"object": "embedding", "index": 0, "embedding": []float32{0.6, 0.8, 0.0}},
			},
			"model": "nomic-embed-text-v2-moe",
			"usage": map[string]int{"prompt_tokens": 1, "total_tokens": 1},
		})
	}))
	defer srv.Close()

	emb := NewLMStudioEmbedder(srv.URL+"/v1", "nomic-embed-text-v2-moe", "", time.Second)
	vec, err := emb.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(vec) != 3 {
		t.Fatalf("expected 3 dims, got %d", len(vec))
	}
	// Re-normalised: input was already (0.6, 0.8, 0) which has norm 1.
	if vec[0] < 0.59 || vec[0] > 0.61 {
		t.Fatalf("unexpected normalised vec[0]: %v", vec[0])
	}
	if emb.Dims() != 3 {
		t.Fatalf("expected cached dims=3, got %d", emb.Dims())
	}
	if emb.ModelID() != "nomic-embed-text-v2-moe" {
		t.Fatalf("ModelID mismatch: %s", emb.ModelID())
	}
}

func TestLMStudioEmbedder_EmptyText(t *testing.T) {
	emb := NewLMStudioEmbedder("http://nope", "m", "", time.Second)
	if _, err := emb.Embed(context.Background(), ""); err != ErrEmptyText {
		t.Fatalf("expected ErrEmptyText, got %v", err)
	}
}

func TestLMStudioEmbedder_RequiresEndpointAndModel(t *testing.T) {
	emb := NewLMStudioEmbedder("", "m", "", time.Second)
	if _, err := emb.Embed(context.Background(), "x"); err == nil {
		t.Fatal("expected error for missing endpoint")
	}
}

func TestLMStudioEmbedder_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"message":"model not loaded"}}`, http.StatusInternalServerError)
	}))
	defer srv.Close()

	emb := NewLMStudioEmbedder(srv.URL+"/v1", "nomic-embed-text-v2-moe", "", time.Second)
	if _, err := emb.Embed(context.Background(), "hello"); err == nil {
		t.Fatal("expected error from 500")
	}
}

func TestLMStudioEmbedder_EmptyVectorIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []map[string]any{{"embedding": []float32{}}},
		})
	}))
	defer srv.Close()

	emb := NewLMStudioEmbedder(srv.URL+"/v1", "m", "", time.Second)
	if _, err := emb.Embed(context.Background(), "hello"); err == nil {
		t.Fatal("expected empty-vector error")
	}
}
