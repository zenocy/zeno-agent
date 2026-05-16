package synth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/llm"
	zlog "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/projection"
)

func newConverseTestStore(t *testing.T) zlog.Reader {
	t.Helper()
	dbPath := t.TempDir() + "/zeno.db"
	_, store, err := zlog.Open(dbPath)
	require.NoError(t, err)
	return store
}

func newConverseLLMServer(t *testing.T, content string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := map[string]any{
			"id": "t", "object": "chat.completion", "model": "test",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": content},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func makeConverseDeps(t *testing.T, srvURL string) ConverseDeps {
	t.Helper()
	prompts, err := LoadPrompts("")
	require.NoError(t, err)

	logger := logrus.New()
	logger.Out = io.Discard

	llmClient := llm.NewClient(llm.ClientConfig{
		Endpoint: srvURL,
		Model:    "test",
		Timeout:  10 * time.Second,
	})

	return ConverseDeps{
		LLM:      llmClient,
		Reader:   newConverseTestStore(t),
		ProjCfg:  projection.Config{TZ: time.UTC},
		Prompts:  prompts,
		Date:     "2026-05-09",
		Now:      time.Now(),
		Deadline: 5 * time.Second,
		Logger:   logger.WithField("c", "converse-test"),
		Card: PinnedCard{
			ID:       "saru-redline",
			Title:    "Saru Patel · re: redline",
			Sub:      "Walked the redline with Lin. Two questions remain.",
			SrcLabel: "Email · Acuity",
		},
	}
}

func TestConverse_AnswerKindRoundTrip(t *testing.T) {
	reply, err := json.Marshal(SubCard{
		ID:      "sub-x1",
		Kind:    "answer",
		Eyebrow: "answer",
		Title:   "What Aria said about 1× preferred",
		Body:    "Aria flagged the 1× preferred as the headline trade-off and said she'd be comfortable conceding it for a tighter option pool.",
		Actions: []Action{{Label: "Done"}},
	})
	require.NoError(t, err)

	srv := newConverseLLMServer(t, string(reply))
	defer srv.Close()

	deps := makeConverseDeps(t, srv.URL)

	sub, _, _, err := Converse(context.Background(), deps, "What did Aria say about 1× preferred?")
	require.NoError(t, err)
	require.Equal(t, "answer", sub.Kind)
	require.Equal(t, "What Aria said about 1× preferred", sub.Title)
	require.NotEmpty(t, sub.Body)
}

func TestConverse_DraftKindRoundTrip(t *testing.T) {
	reply, err := json.Marshal(SubCard{
		ID:        "sub-x2",
		Kind:      "draft",
		Eyebrow:   "draft · ready to send",
		Title:     "Draft a reply confirming 11:00",
		Draft:     "Saru — confirming 11:00. I'll address the option pool sizing and 1× preferred directly. — M",
		DraftMeta: "tone: warm · concise",
		Actions:   []Action{{Label: "Send", Primary: true}, {Label: "Edit"}},
	})
	require.NoError(t, err)

	srv := newConverseLLMServer(t, string(reply))
	defer srv.Close()

	deps := makeConverseDeps(t, srv.URL)

	sub, _, _, err := Converse(context.Background(), deps, "Draft a reply confirming 11:00")
	require.NoError(t, err)
	require.Equal(t, "draft", sub.Kind)
	require.NotEmpty(t, sub.Draft)
	require.Equal(t, "tone: warm · concise", sub.DraftMeta)
	require.Len(t, sub.Actions, 2)
}

func TestConverse_CalendarKindRoundTrip(t *testing.T) {
	reply, err := json.Marshal(SubCard{
		ID:      "sub-x3",
		Kind:    "calendar",
		Eyebrow: "calendar · proposed",
		Title:   "Block 12:30 → 13:30",
		Cal: &SubCalendar{
			Title: "Run window",
			When:  "Tue · 12:30 → 13:30",
			Where: "Solo",
			Who:   "Solo",
		},
		Actions: []Action{{Label: "Confirm & send", Primary: true}, {Label: "Edit time"}},
	})
	require.NoError(t, err)

	srv := newConverseLLMServer(t, string(reply))
	defer srv.Close()

	deps := makeConverseDeps(t, srv.URL)

	sub, _, _, err := Converse(context.Background(), deps, "Block 12:30 to 13:30")
	require.NoError(t, err)
	require.Equal(t, "calendar", sub.Kind)
	require.NotNil(t, sub.Cal)
	require.Equal(t, "Run window", sub.Cal.Title)
}

// TestConverse_CalendarRichRoundTrip validates that the V2.x extended
// SubCalendar fields (attendees with status, conflict, reasoning,
// alternatives, recurring, daystrip) round-trip cleanly through the
// schema validator. The model is allowed to populate these on top of
// the legacy minimal fields when it has the evidence.
func TestConverse_CalendarRichRoundTrip(t *testing.T) {
	reply, err := json.Marshal(SubCard{
		ID:      "sub-x3-rich",
		Kind:    "calendar",
		Eyebrow: "calendar · proposed",
		Title:   "Block 17:00 → 18:30 Thursday",
		Cal: &SubCalendar{
			Title:        "Lia's school recital",
			When:         "Thu · 17:00 → 18:30",
			Where:        "Sutter Heights Elementary",
			Who:          "Sam, Lia",
			Start:        "17:00",
			End:          "18:30",
			TravelBefore: 15,
			Reminder:     "leave by 16:40",
			Attendees: []SubCalendarAttendee{
				{Name: "Sam", Role: "partner", Status: "accepted"},
				{Name: "Lia", Role: "daughter", Status: "host"},
			},
			Conflict:  &SubCalendarConflict{Ok: true, Text: "Range Ventures ends 15:45 — 75 min buffer."},
			Reasoning: []string{"Sam is free from 16:30.", "No focus blocks after 17:00 Thursdays."},
			Alternatives: []SubCalendarAlternative{
				{When: "Wed · 17:00 → 18:30", Note: "lighter Wednesday calendar"},
			},
			Recurring: &SubCalendarRecurring{Label: "Hold every Thursday", Default: false},
			Daystrip: &SubCalendarDaystrip{
				Label:   "Thu, May 14",
				StartHr: 9,
				EndHr:   21,
				Events: []SubCalendarDaystripEvent{
					{Start: 11.0, End: 12.0, Label: "Series B", Kind: "muted"},
					{Start: 16.75, End: 17.0, Label: "travel", Kind: "travel"},
					{Start: 17.0, End: 18.5, Label: "Recital", Kind: "proposed"},
				},
			},
		},
		Actions: []Action{{Label: "Confirm & send", Primary: true}, {Label: "Discard"}},
	})
	require.NoError(t, err)

	srv := newConverseLLMServer(t, string(reply))
	defer srv.Close()

	deps := makeConverseDeps(t, srv.URL)

	sub, _, _, err := Converse(context.Background(), deps, "Block 17:00 to 18:30 Thursday")
	require.NoError(t, err)
	require.Equal(t, "calendar", sub.Kind)
	require.NotNil(t, sub.Cal)
	require.Equal(t, "Lia's school recital", sub.Cal.Title)
	require.Len(t, sub.Cal.Attendees, 2)
	require.Equal(t, "host", sub.Cal.Attendees[1].Status)
	require.NotNil(t, sub.Cal.Conflict)
	require.True(t, sub.Cal.Conflict.Ok)
	require.NotNil(t, sub.Cal.Daystrip)
	require.Len(t, sub.Cal.Daystrip.Events, 3)
	require.Equal(t, "proposed", sub.Cal.Daystrip.Events[2].Kind)
	require.NotNil(t, sub.Cal.Recurring)
	require.Equal(t, 15, sub.Cal.TravelBefore)
}

// TestConverse_CalendarLegacyShapeStillDecodes proves that an old
// persisted SubCard JSON with only the four minimal fields decodes
// cleanly against the extended struct — the new optional fields stay
// nil/zero. This is the backwards-compat anchor for the conversation
// thread store.
func TestConverse_CalendarLegacyShapeStillDecodes(t *testing.T) {
	legacy := []byte(`{
		"id": "sub-legacy",
		"kind": "calendar",
		"eyebrow": "calendar · proposed",
		"title": "Block 12:30 → 13:30",
		"cal": {
			"title": "Run window",
			"when": "Tue · 12:30 → 13:30",
			"where": "Solo",
			"who": "Solo"
		},
		"actions": [{"label": "Confirm & send", "primary": true}]
	}`)

	srv := newConverseLLMServer(t, string(legacy))
	defer srv.Close()

	deps := makeConverseDeps(t, srv.URL)

	sub, _, _, err := Converse(context.Background(), deps, "Block 12:30 to 13:30")
	require.NoError(t, err)
	require.Equal(t, "calendar", sub.Kind)
	require.NotNil(t, sub.Cal)
	require.Equal(t, "Run window", sub.Cal.Title)
	require.Empty(t, sub.Cal.Attendees, "legacy shape leaves Attendees nil/empty")
	require.Nil(t, sub.Cal.Conflict, "legacy shape leaves Conflict nil")
	require.Nil(t, sub.Cal.Daystrip, "legacy shape leaves Daystrip nil")
	require.Equal(t, 0, sub.Cal.TravelBefore)
}

func TestConverse_ResearchKindWithSources(t *testing.T) {
	reply, err := json.Marshal(SubCard{
		ID:      "sub-x4",
		Kind:    "research",
		Eyebrow: "research · 2 sources",
		Title:   "What changed in the redline since Friday",
		Body:    "Two changes: option pool sizing moved from 12.5% pre to 14% post, and the 1× preferred footnote was removed.",
		Sources: []ResearchSource{
			{I: 1, T: "Saru thread · Acuity", W: "this morning"},
			{I: 2, T: "Friday redline", W: "Fri 19:02"},
		},
		Actions: []Action{{Label: "Save to docs", Primary: true}},
	})
	require.NoError(t, err)

	srv := newConverseLLMServer(t, string(reply))
	defer srv.Close()

	deps := makeConverseDeps(t, srv.URL)

	sub, _, _, err := Converse(context.Background(), deps, "What changed in the redline since Friday?")
	require.NoError(t, err)
	require.Equal(t, "research", sub.Kind)
	require.Len(t, sub.Sources, 2)
}

func TestConverse_PriorTurnsRenderInPrompt(t *testing.T) {
	// Capture the request body so we can assert that prior-turn
	// content actually surfaces in the rendered system prompt.
	var captured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured = string(body)
		reply, _ := json.Marshal(SubCard{
			ID: "sub-x5", Kind: "answer", Eyebrow: "answer",
			Title: "Continuing the thread", Body: "Yes, picking up where we left off.",
			Actions: []Action{{Label: "Done"}},
		})
		resp := map[string]any{
			"id": "t", "object": "chat.completion", "model": "test",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": string(reply)},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	deps := makeConverseDeps(t, srv.URL)
	deps.PriorTurns = []PriorTurn{
		{
			Prompt: "What did Aria say about 1× preferred?",
			Reply: SubCard{
				Kind: "answer", Title: "Aria flagged it as the headline",
				Body: "Comfortable conceding for tighter option pool.",
			},
		},
	}

	_, _, _, err := Converse(context.Background(), deps, "And what about Lin?")
	require.NoError(t, err)
	require.Contains(t, captured, "Aria flagged it as the headline",
		"prior-turn reply title must surface in the rendered system prompt")
	require.Contains(t, captured, "What did Aria say about 1× preferred",
		"prior-turn user prompt must surface in the rendered system prompt")
}

func TestConverse_DeadlineReturnsDegradedSubCard(t *testing.T) {
	srv := slowLLMServer(t, 500*time.Millisecond)
	defer srv.Close()

	deps := makeConverseDeps(t, srv.URL)
	deps.Deadline = 50 * time.Millisecond

	start := time.Now()
	sub, _, _, err := Converse(context.Background(), deps, "What changed?")
	elapsed := time.Since(start)

	require.NoError(t, err, "Converse should swallow timeouts and return a degraded sub-card")
	require.Less(t, elapsed, 2*time.Second, "Converse should honor the loop deadline")
	require.Equal(t, "answer", sub.Kind, "degraded sub-card uses kind=answer")
	require.NotEmpty(t, sub.Title)
}
