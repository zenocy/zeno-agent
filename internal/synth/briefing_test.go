package synth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/llm"
	"github.com/zenocy/zeno-v2/internal/projection"
)

// captureBriefingRequest stands up a fake OpenAI-compatible server that
// records the system message of the first POST it sees and returns a valid
// briefing JSON. The test inspects the captured system content to assert
// whether the "/no_think" prefix was added.
func captureBriefingRequest(t *testing.T, model string, noThink bool) string {
	t.Helper()

	var capturedSystem string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		for _, m := range req.Messages {
			if m.Role == "system" {
				capturedSystem = m.Content
				break
			}
		}

		// Return a minimally valid briefing so SynthesizeBriefing's
		// validation path doesn't trip over the response.
		brief := map[string]any{
			"date":    "2026-04-28",
			"eyebrow": "calm morning",
			"title":   "A *calm* start.",
			"summary": "Three things matter today; the rest can wait. The *board call* is at 11:00.",
			"tension": 32,
		}
		raw, _ := json.Marshal(brief)
		resp := map[string]any{
			"id":      "test",
			"object":  "chat.completion",
			"model":   model,
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": string(raw)}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 30, "total_tokens": 40},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)

	prompts, err := LoadPrompts("")
	require.NoError(t, err, "load embedded prompts")

	client := llm.NewClient(llm.ClientConfig{
		Endpoint: srv.URL,
		Model:    model,
		NoThink:  noThink,
	})

	deps := BriefingDeps{
		LLM:     client,
		Prompts: prompts,
		Date:    "2026-04-28",
		Logger:  logrus.NewEntry(logrus.New()),
	}

	_, err = SynthesizeBriefing(context.Background(), deps, CardSet{Cards: []Card{}})
	require.NoError(t, err, "SynthesizeBriefing")

	return capturedSystem
}

func TestSynthesizeBriefing_NoThinkOff_Qwen3_NoPrefix(t *testing.T) {
	// NoThink off + qwen3 model → no /no_think prefix.
	got := captureBriefingRequest(t, "qwen3.6-35b-a3b", false)
	require.False(t, strings.HasPrefix(got, "/no_think"),
		"expected no /no_think prefix when NoThink is false; got: %q", firstLine(got))
}

func TestSynthesizeBriefing_NoThinkOn_Qwen3_PrefixAdded(t *testing.T) {
	// NoThink on + qwen3 model → /no_think prefix added.
	got := captureBriefingRequest(t, "qwen3.6-35b-a3b", true)
	require.True(t, strings.HasPrefix(got, "/no_think\n\n"),
		"expected /no_think prefix when NoThink is true on a Qwen3 model; got: %q", firstLine(got))
}

func TestSynthesizeBriefing_NoThinkOn_NonQwen3_NoPrefix(t *testing.T) {
	// NoThink on + non-qwen3 model → no prefix (gating is by family).
	got := captureBriefingRequest(t, "gemma3:4b", true)
	require.False(t, strings.HasPrefix(got, "/no_think"),
		"expected no /no_think prefix on non-Qwen3 model even when NoThink is true; got: %q", firstLine(got))
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// captureBriefingWithConcerns is a thin variant of captureBriefingRequest
// that takes an explicit Concerns slice (for V2.5 surfacing tests).
// Returns the rendered system content the model received.
func captureBriefingWithConcerns(t *testing.T, concerns []projection.Concern) string {
	t.Helper()
	var capturedSystem string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		for _, m := range req.Messages {
			if m.Role == "system" {
				capturedSystem = m.Content
				break
			}
		}
		brief := map[string]any{
			"date": "2026-04-28", "eyebrow": "calm morning", "title": "A *calm* start.",
			"summary": "Three things matter; the *board call* is at 11:00.", "tension": 32,
		}
		raw, _ := json.Marshal(brief)
		resp := map[string]any{
			"id": "t", "object": "chat.completion", "model": "qwen3.6-35b-a3b",
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": string(raw)}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 30, "total_tokens": 40},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)

	prompts, err := LoadPrompts("")
	require.NoError(t, err)
	client := llm.NewClient(llm.ClientConfig{Endpoint: srv.URL, Model: "qwen3.6-35b-a3b"})

	_, err = SynthesizeBriefing(context.Background(), BriefingDeps{
		LLM: client, Prompts: prompts, Date: "2026-04-28",
		Logger:   logrus.NewEntry(logrus.New()),
		Concerns: concerns,
	}, CardSet{Cards: []Card{}})
	require.NoError(t, err)
	return capturedSystem
}

// TestSynthesizeBriefing_ConcernsBlockRendered is the surfacing-side
// gate: when concerns are present, the rendered system prompt contains
// the concerns heading and each concern's name + description. This is
// what makes the briefing's voice rule actually applicable to a
// concrete fixture.
func TestSynthesizeBriefing_ConcernsBlockRendered(t *testing.T) {
	got := captureBriefingWithConcerns(t, []projection.Concern{
		{ID: "c1", Name: "Construction at the house", Description: "Kitchen tile and the inspection are open beats."},
	})
	require.Contains(t, got, "# Today's concerns")
	require.Contains(t, got, "Construction at the house")
	require.Contains(t, got, "Kitchen tile and the inspection are open beats")
	// Voice rule is present (came in via {{ .Voice }}).
	require.Contains(t, got, "Concerns are scaffolding")
}

// TestSynthesizeBriefing_NoConcernsBlock_OmittedCleanly is the
// regression gate against voice leakage: zero concerns means the
// concerns heading does NOT appear, so the model can't be tempted by
// an empty list. Also confirms the data-map nil-slice path renders
// cleanly via the template's `{{- if .Concerns }}` guard.
func TestSynthesizeBriefing_NoConcernsBlock_OmittedCleanly(t *testing.T) {
	got := captureBriefingWithConcerns(t, nil)
	require.NotContains(t, got, "# Today's concerns")
	// Voice rule (from .Voice) still loads — that's a global rule, not
	// gated on the concerns block.
	require.Contains(t, got, "Concerns are scaffolding")
}

// TestSynthesizeBriefing_MultipleConcernsAllRendered pins that the
// template iterates correctly. The voice rule's "at most one" cap is
// enforced by the LLM; the prompt presents all of them so the model
// can pick.
func TestSynthesizeBriefing_MultipleConcernsAllRendered(t *testing.T) {
	got := captureBriefingWithConcerns(t, []projection.Concern{
		{ID: "c1", Name: "Construction", Description: "kitchen tile."},
		{ID: "c2", Name: "Frankfurt trip", Description: "mid-June review."},
	})
	require.Contains(t, got, "Construction")
	require.Contains(t, got, "Frankfurt trip")
}

// briefingServer returns an httptest server that always responds with the
// caller-supplied body and status. Used by the error-path tests below.
func briefingServer(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func newBriefingDeps(t *testing.T, endpoint string) BriefingDeps {
	t.Helper()
	prompts, err := LoadPrompts("")
	require.NoError(t, err)
	client := llm.NewClient(llm.ClientConfig{
		Endpoint: endpoint,
		Model:    "qwen3.6-35b-a3b",
		Retry:    llm.RetryPolicy{MaxAttempts: 1}, // skip retry layer; we want errors immediately
	})
	return BriefingDeps{
		LLM: client, Prompts: prompts, Date: "2026-04-28",
		Logger: logrus.NewEntry(logrus.New()),
	}
}

// TestSynthesizeBriefing_LLMError_Surfaces5xx pins that an upstream LLM
// 5xx surfaces as a "briefing call" wrapped error — not a degraded card,
// not a panic. The runner relies on this to retry / fall back cleanly.
func TestSynthesizeBriefing_LLMError_Surfaces5xx(t *testing.T) {
	url := briefingServer(t, http.StatusInternalServerError, `{"error":{"message":"upstream blew up","type":"server_error"}}`)
	deps := newBriefingDeps(t, url)
	_, err := SynthesizeBriefing(context.Background(), deps, CardSet{Cards: []Card{}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "briefing call",
		"5xx must wrap as 'briefing call' so retry/log paths can recognize it")
}

// TestSynthesizeBriefing_MalformedJSON_TriggersRepair confirms a non-JSON
// model response engages the one-shot repair attempt. The repair server
// returns valid JSON on the second call so the test asserts (a) two calls
// were made and (b) the final briefing is non-empty.
func TestSynthesizeBriefing_MalformedJSON_TriggersRepair(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		var content string
		if calls == 1 {
			content = "this is not even JSON. the model just yapped."
		} else {
			brief, _ := json.Marshal(map[string]any{
				"date": "2026-04-28", "eyebrow": "calm morning", "title": "A *calm* start.",
				"summary": "Three things matter; the *board call* is at 11:00.", "tension": 32,
			})
			content = string(brief)
		}
		resp := map[string]any{
			"id": "t", "object": "chat.completion", "model": "qwen3.6-35b-a3b",
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)

	deps := newBriefingDeps(t, srv.URL)
	got, err := SynthesizeBriefing(context.Background(), deps, CardSet{Cards: []Card{}})
	require.NoError(t, err, "repair attempt with valid JSON must recover")
	require.Equal(t, 2, calls, "exactly one repair attempt expected")
	require.NotEmpty(t, got.Title)
}

// TestSynthesizeBriefing_RepairExhausted_ReturnsError pins that two
// consecutive malformed responses surface a clean error with the repair
// failure reason — not a degraded briefing — so the runner's degrade
// path can decide what to do.
func TestSynthesizeBriefing_RepairExhausted_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := map[string]any{
			"id": "t", "object": "chat.completion", "model": "qwen3.6-35b-a3b",
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "{not_json"}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)

	deps := newBriefingDeps(t, srv.URL)
	_, err := SynthesizeBriefing(context.Background(), deps, CardSet{Cards: []Card{}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "validate briefing")
}
