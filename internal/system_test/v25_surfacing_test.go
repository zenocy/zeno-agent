package system_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/eventbus"
	"github.com/zenocy/zeno-v2/internal/llm"
	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/log/logtest"
	"github.com/zenocy/zeno-v2/internal/projection"
	"github.com/zenocy/zeno-v2/internal/store"
	"github.com/zenocy/zeno-v2/internal/synth"
)

// surfacingRig wires the minimum stack the surfacing E2E paths need:
// in-memory log + concerns + tags repos + a programmable LLM stub.
// No HTTP server, no bus subscriber — Phase 3 surfacing tests focus
// on the synth-side stitching (data map, persistence, trace refs)
// rather than the wire surface.
type surfacingRig struct {
	DB           *gorm.DB
	Reader       *logtest.MemReader
	Concerns     *store.ConcernRepo
	Observations *store.ConcernObservationRepo
	Bus          *eventbus.Bus
	LLMServer    *httptest.Server
	LLMResponses []string
	LLMReqs      *[]string
	Client       *llm.Client
	Logger       *logrus.Entry
}

func newSurfacingRig(t *testing.T, responses ...string) *surfacingRig {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	cRepo := &store.ConcernRepo{DB: db, Table: "concerns"}
	oRepo := &store.ConcernObservationRepo{DB: db, Table: "concern_observations"}
	require.NoError(t, cRepo.Migrate())
	require.NoError(t, oRepo.Migrate())

	logger := logrus.New()
	logger.Out = io.Discard

	idx := 0
	captured := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured = append(captured, string(body))
		var content string
		if idx < len(responses) {
			content = responses[idx]
		} else if len(responses) > 0 {
			content = responses[len(responses)-1]
		}
		idx++
		resp := map[string]any{
			"id": "t", "object": "chat.completion", "model": "test",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": content},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 30, "total_tokens": 40},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)

	cli := llm.NewClient(llm.ClientConfig{Endpoint: srv.URL, Model: "test"})

	return &surfacingRig{
		DB:           db,
		Reader:       logtest.NewMemReader(),
		Concerns:     cRepo,
		Observations: oRepo,
		Bus:          eventbus.New(logger.WithField("c", "bus")),
		LLMServer:    srv,
		LLMResponses: responses,
		LLMReqs:      &captured,
		Client:       cli,
		Logger:       logger.WithField("c", "test"),
	}
}

func (r *surfacingRig) seedConcern(t *testing.T, id, name string) {
	t.Helper()
	now := time.Now()
	require.NoError(t, r.Concerns.Insert(context.Background(), store.Concern{
		ID: id, Name: name, NormName: store.NormalizeConcernName(name),
		Description: name + " — long-running situation",
		State:       store.ConcernStateActive, Source: store.ConcernSourceUser,
		LastActiveAt: now, CreatedAt: now, UpdatedAt: now, Confidence: 1,
	}))
}

func (r *surfacingRig) tag(t *testing.T, concernID, eventID string) {
	t.Helper()
	require.NoError(t, r.Observations.Tag(context.Background(), store.ConcernObservation{
		ConcernID: concernID, EventID: eventID,
		Source: store.ConcernTagSourceUser, TaggedAt: time.Now(),
	}))
}

// TestSurfacing_E2E_BriefingPromptCarriesConcerns is the briefing-side
// gate: when concerns are seeded, the rendered briefing system prompt
// the LLM sees contains the concern names. Validates the data-map
// thread-through from Runner → BriefingDeps → template.
func TestSurfacing_E2E_BriefingPromptCarriesConcerns(t *testing.T) {
	// Stub responses: cards (CardSet — minItems=2) then briefing.
	cardsResp, _ := json.Marshal(map[string]any{
		"cards": []map[string]any{
			{
				"id": "c1", "date": "2026-04-28",
				"src": "calendar", "src_label": "Acuity — Series B narrative review",
				"rel": "high", "title": "*Acuity Capital* — Series B review", "sub": "Saru leads with the pricing slide; Lin will press cohort retention. Reply to Saru first.",
				"actions": []map[string]any{{"label": "Dismiss"}},
			},
			{
				"id": "c2", "date": "2026-04-28",
				"src": "mail", "src_label": "Kitchen tile — vendor options",
				"rel": "med", "title": "Kitchen *tile* vendors", "sub": "Two ranges from Hector — ceramic and porcelain. Lead time on porcelain is eleven days.",
				"actions": []map[string]any{{"label": "Dismiss"}},
			},
		},
	})
	briefResp, _ := json.Marshal(map[string]any{
		"date": "2026-04-28", "eyebrow": "this morning", "title": "A *calm* start.",
		"summary": "The board call sets today.", "tension": 32,
	})
	rig := newSurfacingRig(t, string(cardsResp), string(briefResp))
	rig.seedConcern(t, "c-1", "Construction at the house")

	// Build a runner with concerns wiring; the cards/briefing prompts
	// will pull active concerns into their data maps.
	prompts, err := synth.LoadPrompts("")
	require.NoError(t, err)

	require.NoError(t, synth.Migrate(rig.DB, true, false))
	pinnedNow := time.Date(2026, 4, 28, 7, 0, 0, 0, time.UTC)
	runner := &synth.Runner{
		LLM: rig.Client, Reader: rig.Reader, DB: rig.DB,
		EventLog: rig.Reader, Bus: rig.Bus, Prompts: prompts,
		Now:                 func() time.Time { return pinnedNow },
		Logger:              rig.Logger,
		Concerns:            rig.Concerns,
		ConcernObservations: rig.Observations,
		ProjCfg: projection.Config{
			TZ:  time.UTC,
			Now: func() time.Time { return pinnedNow },
		},
	}
	_ = runner.Run(context.Background())

	// At least the briefing system message must contain the concern.
	// Search every captured request body for the concern name.
	found := false
	for _, body := range *rig.LLMReqs {
		if strings.Contains(body, "Construction at the house") {
			found = true
			break
		}
	}
	require.True(t, found, "expected at least one prompt to mention the seeded concern")
}

// TestSurfacing_E2E_CardInheritsConcernIDOnPersist is the cards-side
// gate: a calendar event tagged to a concern produces a persisted
// store.Card row whose ConcernID matches the tag. This is the wire
// the review surface (Phase 4) reads from.
func TestSurfacing_E2E_CardInheritsConcernIDOnPersist(t *testing.T) {
	// Seed a calendar event in the log so the projection sees it.
	calStart := time.Date(2026, 4, 28, 11, 0, 0, 0, time.UTC)
	rig := newSurfacingRig(t, jsonStr(map[string]any{
		"cards": []map[string]any{
			{
				"id": "c1", "date": "2026-04-28",
				"src": "calendar", "src_label": "Acuity — Series B narrative review",
				"rel": "high", "title": "*Acuity* review at 11", "sub": "Series B narrative review with Saru and Lin in the room.",
				"actions": []map[string]any{{"label": "Dismiss"}},
			},
			{
				"id": "c2", "date": "2026-04-28",
				"src": "personal", "src_label": "filler", "rel": "low",
				"title": "Quiet *afternoon*", "sub": "Three windows of unstructured time after the meeting.",
				"actions": []map[string]any{{"label": "Dismiss"}},
			},
		},
	}), jsonStr(map[string]any{
		"date": "2026-04-28", "eyebrow": "this morning", "title": "A *calm* start.",
		"summary": "Board call sets today.", "tension": 32,
	}))
	rig.Reader.AppendEvent(log.Event{
		ID: "evt-acuity", TS: calStart, Kind: log.KindCalEventSeen, Source: "caldav",
		Payload: jsonBytes(map[string]any{
			"uid": "evt-acuity", "title": "Acuity — Series B narrative review",
			"start": calStart, "end": calStart.Add(45 * time.Minute), "tag": "work",
		}),
	})
	rig.seedConcern(t, "c-acuity", "Acuity Series B")
	rig.tag(t, "c-acuity", "evt-acuity")

	prompts, err := synth.LoadPrompts("")
	require.NoError(t, err)
	require.NoError(t, synth.Migrate(rig.DB, true, false))

	pinnedNow := calStart.Add(-4 * time.Hour)
	runner := &synth.Runner{
		LLM: rig.Client, Reader: rig.Reader, DB: rig.DB,
		EventLog: rig.Reader, Bus: rig.Bus, Prompts: prompts,
		Now:                 func() time.Time { return pinnedNow },
		Logger:              rig.Logger,
		Concerns:            rig.Concerns,
		ConcernObservations: rig.Observations,
		ProjCfg: projection.Config{
			TZ:  time.UTC,
			Now: func() time.Time { return pinnedNow },
		},
	}
	require.NoError(t, runner.Run(context.Background()))

	// Persisted card row carries concern_id matching the tag.
	cardRepo := &store.CardRepo{DB: rig.DB, Table: "cards"}
	rows, err := cardRepo.ListByDate(context.Background(), "2026-04-28")
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	got := false
	for _, c := range rows {
		if c.ConcernID != nil && *c.ConcernID == "c-acuity" {
			got = true
		}
	}
	require.True(t, got, "expected at least one persisted card to inherit concern_id=c-acuity")
}

func jsonStr(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func jsonBytes(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
