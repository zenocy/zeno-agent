package synth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/llm"
	zlog "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/projection"
)

// slowLLMServer hangs every chat completion past the test deadline so the
// loop's context-with-timeout fires and returns to Ask, which must then
// surface a degraded card rather than an error.
func slowLLMServer(t *testing.T, hold time.Duration) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(hold):
			http.Error(w, "should not reach here in this test", http.StatusInternalServerError)
		case <-r.Context().Done():
			// Client cancelled (the loop's deadline fired). End cleanly.
			return
		}
	}))
}

func TestAsk_DeadlineReturnsDegradedCard(t *testing.T) {
	srv := slowLLMServer(t, 500*time.Millisecond)
	defer srv.Close()

	dbPath := t.TempDir() + "/zeno.db"
	_, lstore, err := zlog.Open(dbPath)
	require.NoError(t, err)

	prompts, err := LoadPrompts("")
	require.NoError(t, err)

	logger := logrus.New()
	logger.Out = io.Discard

	llmClient := llm.NewClient(llm.ClientConfig{
		Endpoint: srv.URL,
		Model:    "test",
		Timeout:  10 * time.Second,
	})

	deps := ReactiveDeps{
		LLM:     llmClient,
		Reader:  lstore,
		ProjCfg: projection.Config{TZ: time.UTC},
		Prompts: prompts,
		Date:    "2026-04-25",
		Now:     time.Now(),
		// Force the loop to time out almost immediately.
		Deadline: 50 * time.Millisecond,
		Logger:   logger.WithField("c", "reactive-test"),
	}

	start := time.Now()
	card, _, _, err := Ask(context.Background(), deps, "what's the weather today?")
	elapsed := time.Since(start)

	// Ask must never bubble an error to the caller — the UI relies on this.
	require.NoError(t, err, "Ask should swallow timeouts and return a degraded card")

	// And it must return well before the upstream `hold` would have completed.
	require.Less(t, elapsed, 2*time.Second, "Ask should honor the loop deadline, not wait on slow LLM")

	// Shape: degraded card per degradedCard().
	require.Equal(t, "ask", card.Source)
	require.Equal(t, "Generated", card.SrcLabel)
	require.Equal(t, "low", card.Rel)
	require.Equal(t, "2026-04-25", card.Date)
	require.NotEmpty(t, card.ID, "degraded card must have a slug ID")
	require.NotEmpty(t, card.Title)
	require.NotEmpty(t, card.Actions, "degraded card must include at least Dismiss")
}

// TestAsk_OversizedSubPassesThrough pins the post-incident behavior:
// a `sub` longer than the prompt's 400-char budget is no longer a
// validation error. The model's answer surfaces as-is — no repair LLM
// round-trip, no degraded card. Guards against future re-tightening of
// the schema that would silently revert this.
func TestAsk_OversizedSubPassesThrough(t *testing.T) {
	const oversizeRunes = 470
	longSub := strings.Repeat("a", oversizeRunes)

	cardJSON, err := json.Marshal(Card{
		ID:       "generated-x8k2",
		Date:     "2026-05-06",
		Source:   "ask",
		SrcLabel: "Generated",
		Rel:      "med",
		Title:    "Tile quantities confirmed",
		Sub:      longSub,
		Meta:     []string{},
		Actions:  []Action{{Label: "Dismiss"}},
	})
	require.NoError(t, err)

	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&calls, 1)
		resp := map[string]any{
			"id": "t", "object": "chat.completion", "model": "qwen3-test",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": string(cardJSON)},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	dbPath := t.TempDir() + "/zeno.db"
	_, lstore, err := zlog.Open(dbPath)
	require.NoError(t, err)

	prompts, err := LoadPrompts("")
	require.NoError(t, err)

	logger := logrus.New()
	logger.Out = io.Discard

	llmClient := llm.NewClient(llm.ClientConfig{
		Endpoint: srv.URL,
		Model:    "test",
		Timeout:  10 * time.Second,
	})

	deps := ReactiveDeps{
		LLM:      llmClient,
		Reader:   lstore,
		ProjCfg:  projection.Config{TZ: time.UTC},
		Prompts:  prompts,
		Date:     "2026-05-06",
		Now:      time.Now(),
		Deadline: 5 * time.Second,
		Logger:   logger.WithField("c", "reactive-test"),
	}

	card, _, _, err := Ask(context.Background(), deps, "what's the latest with construction?")
	require.NoError(t, err)
	require.EqualValues(t, 1, atomic.LoadInt64(&calls), "no repair round-trip should fire for an over-budget sub")

	// Card should not be the degraded fallback.
	require.NotEqual(t, "Couldn't reach an answer in time.", card.Title)

	// Sub passes through unchanged — no truncation, no clamp.
	require.Equal(t, oversizeRunes, len([]rune(card.Sub)),
		"sub must pass through at full length; rejecting an over-budget sub throws away a usable answer")
}

// stubLLMServer returns a test server that always replies with the
// given content string wrapped in a chat-completion envelope. Used to
// pin Ask's behavior on specific model outputs.
func stubLLMServer(t *testing.T, content string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := map[string]any{
			"id": "t", "object": "chat.completion", "model": "qwen3-test",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": content},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

// TestAsk_PopulatesBody_InAppSurface pins the in-app text-chat behavior:
// when Conversation is nil (the in-app surface) the reactive prompt
// invites the model to populate `body` with multi-paragraph elaboration,
// and Ask must surface it on the returned Card. Guards against a future
// schema change that silently drops the field.
func TestAsk_PopulatesBody_InAppSurface(t *testing.T) {
	body := "First paragraph with concrete *detail*.\n\nSecond paragraph adds context.\n\nThird beat ends decisively."
	cardJSON, err := json.Marshal(Card{
		ID:       "generated-iran",
		Date:     "2026-05-06",
		Source:   "ask",
		SrcLabel: "Generated",
		Rel:      "med",
		Title:    "A fragile pause in the Iran conflict",
		Sub:      "Vance keeps military action *locked and loaded* if talks stall.",
		Body:     body,
		Meta:     []string{},
		Actions:  []Action{{Label: "Dismiss"}},
	})
	require.NoError(t, err)

	srv := stubLLMServer(t, string(cardJSON))
	defer srv.Close()

	dbPath := t.TempDir() + "/zeno.db"
	_, lstore, err := zlog.Open(dbPath)
	require.NoError(t, err)

	prompts, err := LoadPrompts("")
	require.NoError(t, err)

	logger := logrus.New()
	logger.Out = io.Discard

	llmClient := llm.NewClient(llm.ClientConfig{
		Endpoint: srv.URL,
		Model:    "test",
		Timeout:  10 * time.Second,
	})

	deps := ReactiveDeps{
		LLM:      llmClient,
		Reader:   lstore,
		ProjCfg:  projection.Config{TZ: time.UTC},
		Prompts:  prompts,
		Date:     "2026-05-06",
		Now:      time.Now(),
		Deadline: 5 * time.Second,
		Logger:   logger.WithField("c", "reactive-test"),
		// Conversation == nil → in-app surface.
	}

	card, _, _, err := Ask(context.Background(), deps, "what's the latest in the war of iran?")
	require.NoError(t, err)
	require.Equal(t, body, card.Body, "in-app reactive Ask must surface multi-paragraph body verbatim")
	require.Empty(t, card.Speech, "in-app surface must not populate the WhatsApp speech field")
}

// TestAsk_OmitsBody_WhatsAppSurface pins the WhatsApp behavior: when
// Conversation is non-nil the prompt's WhatsApp register suppresses
// `body` (it's for the in-app surface only) and asks for `speech`
// instead. Even if a hypothetical model leaks body, this test
// documents the contract — the reactive flow must accept a
// body-less card on WhatsApp without crashing or repairing.
func TestAsk_OmitsBody_WhatsAppSurface(t *testing.T) {
	cardJSON, err := json.Marshal(Card{
		ID:       "generated-wa",
		Date:     "2026-05-06",
		Source:   "ask",
		SrcLabel: "Generated",
		Rel:      "med",
		Title:    "Quick read on the talks",
		Sub:      "Vance keeps military action locked and loaded if talks stall.",
		Speech:   "Trump paused the strike; Vance says talks first, force second.",
		Meta:     []string{},
		Actions:  []Action{{Label: "Dismiss"}},
		// Body intentionally absent — WhatsApp register suppresses it.
	})
	require.NoError(t, err)

	srv := stubLLMServer(t, string(cardJSON))
	defer srv.Close()

	dbPath := t.TempDir() + "/zeno.db"
	_, lstore, err := zlog.Open(dbPath)
	require.NoError(t, err)

	prompts, err := LoadPrompts("")
	require.NoError(t, err)

	logger := logrus.New()
	logger.Out = io.Discard

	llmClient := llm.NewClient(llm.ClientConfig{
		Endpoint: srv.URL,
		Model:    "test",
		Timeout:  10 * time.Second,
	})

	deps := ReactiveDeps{
		LLM:      llmClient,
		Reader:   lstore,
		ProjCfg:  projection.Config{TZ: time.UTC},
		Prompts:  prompts,
		Date:     "2026-05-06",
		Now:      time.Now(),
		Deadline: 5 * time.Second,
		Logger:   logger.WithField("c", "reactive-test"),
		Conversation: &ConversationContext{
			IsDM:       true,
			SenderName: "Andreas",
		},
	}

	card, _, _, err := Ask(context.Background(), deps, "what's the latest in the war of iran?")
	require.NoError(t, err)
	require.Empty(t, card.Body, "WhatsApp surface must leave body empty — only the in-app surface populates it")
	require.NotEmpty(t, card.Speech, "WhatsApp surface must still populate speech for verbatim send")
}

// TestAsk_PopulatesSources pins source-citation surfacing: when the
// model emits a `sources` array (because it called search_web /
// read_url), Ask must surface the list on the returned Card so the
// UI can render clickable links below the body.
func TestAsk_PopulatesSources(t *testing.T) {
	sources := []Source{
		{T: "Reuters: Trump delays Iran strike", U: "https://www.reuters.com/world/middle-east/iran-pause-2026"},
		{T: "Bloomberg: Gulf pressure", U: "https://www.bloomberg.com/news/iran-gulf-allies"},
	}
	cardJSON, err := json.Marshal(Card{
		ID:       "generated-iran-src",
		Date:     "2026-05-06",
		Source:   "ask",
		SrcLabel: "Generated",
		Rel:      "med",
		Title:    "A fragile pause in the Iran conflict",
		Sub:      "Vance keeps military action locked and loaded if talks stall.",
		Body:     "First paragraph.\n\nSecond paragraph.",
		Sources:  sources,
		Meta:     []string{},
		Actions:  []Action{{Label: "Dismiss"}},
	})
	require.NoError(t, err)

	srv := stubLLMServer(t, string(cardJSON))
	defer srv.Close()

	dbPath := t.TempDir() + "/zeno.db"
	_, lstore, err := zlog.Open(dbPath)
	require.NoError(t, err)

	prompts, err := LoadPrompts("")
	require.NoError(t, err)

	logger := logrus.New()
	logger.Out = io.Discard

	llmClient := llm.NewClient(llm.ClientConfig{
		Endpoint: srv.URL,
		Model:    "test",
		Timeout:  10 * time.Second,
	})

	deps := ReactiveDeps{
		LLM:      llmClient,
		Reader:   lstore,
		ProjCfg:  projection.Config{TZ: time.UTC},
		Prompts:  prompts,
		Date:     "2026-05-06",
		Now:      time.Now(),
		Deadline: 5 * time.Second,
		Logger:   logger.WithField("c", "reactive-test"),
	}

	card, _, _, err := Ask(context.Background(), deps, "what's the latest in the war of iran?")
	require.NoError(t, err)
	require.Len(t, card.Sources, 2, "Ask must surface the model's sources list verbatim")
	require.Equal(t, "Reuters: Trump delays Iran strike", card.Sources[0].T)
	require.Equal(t, "https://www.reuters.com/world/middle-east/iran-pause-2026", card.Sources[0].U)
}

func TestSourcesFromCitations(t *testing.T) {
	t.Run("empty input", func(t *testing.T) {
		require.Nil(t, sourcesFromCitations(nil))
		require.Nil(t, sourcesFromCitations([]llm.Citation{}))
	})
	t.Run("skips entries with empty URI", func(t *testing.T) {
		got := sourcesFromCitations([]llm.Citation{
			{Title: "no uri"},
			{Title: "ok", URI: "https://a.example"},
		})
		require.Len(t, got, 1)
		require.Equal(t, "ok", got[0].T)
		require.Equal(t, "https://a.example", got[0].U)
	})
	t.Run("falls back to URI when title is empty", func(t *testing.T) {
		got := sourcesFromCitations([]llm.Citation{
			{URI: "https://a.example"},
		})
		require.Len(t, got, 1)
		require.Equal(t, "https://a.example", got[0].T,
			"missing title must fall back to URI so the UI doesn't render an empty anchor")
	})
	t.Run("dedups by URI", func(t *testing.T) {
		got := sourcesFromCitations([]llm.Citation{
			{Title: "A", URI: "https://a.example"},
			{Title: "A again", URI: "https://a.example"},
			{Title: "B", URI: "https://b.example"},
		})
		require.Len(t, got, 2, "duplicate URIs collapse to one entry")
	})
	t.Run("caps at 5", func(t *testing.T) {
		in := []llm.Citation{}
		for i := 0; i < 8; i++ {
			in = append(in, llm.Citation{
				Title: fmt.Sprintf("S%d", i),
				URI:   fmt.Sprintf("https://example.com/%d", i),
			})
		}
		require.Len(t, sourcesFromCitations(in), 5)
	})
}

func TestPostProcessCard_OverridesSourcesFromCitations(t *testing.T) {
	// Regression: when Gemini native google_search grounding fires, the
	// model never sees the underlying URLs and fabricates the `u` field.
	// postProcessCard must replace the model-emitted Sources with the
	// citation-derived list so the UI renders real (redirect) URLs.
	card := Card{
		Title: "An answer",
		Sub:   "Sub text long enough for postprocessing pass.",
		Sources: []Source{
			{T: "Fabricated", U: "Fabricated"}, // model hallucinated U
		},
	}
	citations := []llm.Citation{
		{Title: "Real source A", URI: "https://vertexaisearch.cloud.google.com/grounding-api-redirect/abc"},
		{Title: "Real source B", URI: "https://vertexaisearch.cloud.google.com/grounding-api-redirect/def"},
	}
	postProcessCard(&card, "2026-05-20", nil, citations, nil)

	require.Len(t, card.Sources, 2)
	require.Equal(t, "Real source A", card.Sources[0].T)
	require.Equal(t, "https://vertexaisearch.cloud.google.com/grounding-api-redirect/abc", card.Sources[0].U)
}

func TestPostProcessCard_KeepsModelSourcesWhenNoCitations(t *testing.T) {
	// OpenAI / OpenRouter flows: no native grounding, model populates
	// Sources from search_web tool output. postProcessCard must not
	// touch them.
	original := []Source{
		{T: "Reuters", U: "https://reuters.com/x"},
	}
	card := Card{
		Title:   "An answer",
		Sub:     "Sub text long enough for postprocessing pass.",
		Sources: original,
	}
	postProcessCard(&card, "2026-05-20", nil, nil, nil)

	require.Equal(t, original, card.Sources,
		"non-Gemini flows keep the model-emitted sources unchanged")
}
