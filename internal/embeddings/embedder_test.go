package embeddings

import (
	"context"
	"math"
	"testing"
)

func TestFakeEmbedder_Deterministic(t *testing.T) {
	e := NewFakeEmbedder(256)
	v1, err := e.Embed(context.Background(), "anniversary is May 7")
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	v2, err := e.Embed(context.Background(), "anniversary is May 7")
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(v1) != 256 || len(v2) != 256 {
		t.Fatalf("dims wrong: %d %d", len(v1), len(v2))
	}
	for i := range v1 {
		if v1[i] != v2[i] {
			t.Fatalf("non-deterministic at %d: %v vs %v", i, v1[i], v2[i])
		}
	}
}

func TestFakeEmbedder_DifferentInputsDiffer(t *testing.T) {
	e := NewFakeEmbedder(64)
	a, _ := e.Embed(context.Background(), "partner is Sam")
	b, _ := e.Embed(context.Background(), "runs Tuesdays and Thursdays")
	same := true
	for i := range a {
		if a[i] != b[i] {
			same = false
			break
		}
	}
	if same {
		t.Fatal("expected different vectors for different inputs")
	}
}

func TestFakeEmbedder_NormalisedToUnit(t *testing.T) {
	e := NewFakeEmbedder(128)
	v, err := e.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	norm := math.Sqrt(sum)
	if math.Abs(norm-1.0) > 1e-5 {
		t.Fatalf("expected unit vector, got norm=%v", norm)
	}
}

func TestFakeEmbedder_EmptyError(t *testing.T) {
	e := NewFakeEmbedder(16)
	if _, err := e.Embed(context.Background(), ""); err != ErrEmptyText {
		t.Fatalf("expected ErrEmptyText, got %v", err)
	}
}

func TestFakeEmbedder_ZeroDimsFallback(t *testing.T) {
	e := NewFakeEmbedder(0)
	if e.Dims() != 16 {
		t.Fatalf("expected fallback dims=16, got %d", e.Dims())
	}
}

func TestFakeEmbedder_ModelID(t *testing.T) {
	if NewFakeEmbedder(8).ModelID() != FakeModelID {
		t.Fatalf("ModelID mismatch")
	}
}

func TestHashContent_Stable(t *testing.T) {
	a := HashContent("partner is Sam")
	b := HashContent("partner is Sam")
	if a != b {
		t.Fatalf("HashContent unstable")
	}
	if len(a) != 16 {
		t.Fatalf("expected 16 hex chars, got %d", len(a))
	}
}

func TestHashContent_Different(t *testing.T) {
	if HashContent("a") == HashContent("b") {
		t.Fatal("expected different hashes")
	}
}

func TestL2Normalize_Idempotent(t *testing.T) {
	v := []float32{0.3, 0.4, 0}
	l2Normalize(v)
	expect := []float32{0.6, 0.8, 0}
	for i := range v {
		if math.Abs(float64(v[i]-expect[i])) > 1e-5 {
			t.Fatalf("wrong normalisation at %d: %v vs %v", i, v[i], expect[i])
		}
	}
	l2Normalize(v)
	for i := range v {
		if math.Abs(float64(v[i]-expect[i])) > 1e-5 {
			t.Fatalf("not idempotent at %d", i)
		}
	}
}

func TestL2Normalize_ZeroNoop(t *testing.T) {
	v := []float32{0, 0, 0}
	l2Normalize(v)
	for _, x := range v {
		if x != 0 {
			t.Fatalf("zero vector should not change")
		}
	}
}

func TestCosine_OrthogonalIsZero(t *testing.T) {
	a := []float32{1, 0, 0}
	b := []float32{0, 1, 0}
	if got := cosine(a, b); got != 0 {
		t.Fatalf("expected 0, got %v", got)
	}
}

func TestCosine_SameVectorIsOne(t *testing.T) {
	v := []float32{0.6, 0.8}
	if got := cosine(v, v); math.Abs(float64(got-1.0)) > 1e-5 {
		t.Fatalf("expected ~1.0, got %v", got)
	}
}
