package synth

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/llm"
	zlog "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/projection"
	"github.com/zenocy/zeno-v2/internal/store"
)

// injectTestSetup builds the shared environment for an inject test:
// transcript-backed LLM client, populated event log with one VIP email
// thread + one calendar event, fresh prompts, deps ready to call.
func injectTestSetup(t *testing.T, transcriptName string) (InjectDeps, InjectSignal, *logrus.Entry, func()) {
	t.Helper()
	turns := loadTranscript(t, transcriptName)
	ts := newTranscriptServer(t, turns)

	dbPath := filepath.Join(t.TempDir(), "inject.db")
	db, lstore, err := zlog.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, Migrate(db, true, false))

	now := time.Date(2026, 4, 30, 10, 0, 0, 0, time.UTC)
	ctx := context.Background()

	// Seed an inbox thread the inject will reference.
	_, err = lstore.Append(ctx, zlog.KindMailReceived, "imap", map[string]any{
		"folder":       "INBOX",
		"uid":          1,
		"uidvalidity":  1,
		"message_id":   "<inject-1@test>",
		"from":         "Saru Patel <saru@acuity.test>",
		"to":           []string{"mira@halsen.test"},
		"subject":      "URGENT — board call moved to 10:30",
		"date":         now.Add(-30 * time.Minute),
		"body_preview": "Need redline answer by 10:30 — option pool and 1x non-participating preferred.",
	})
	require.NoError(t, err)

	// Seed the underlying calendar event.
	_, err = lstore.Append(ctx, zlog.KindCalEventSeen, "caldav", map[string]any{
		"uid":           "evt-acuity",
		"title":         "Acuity — Series B review (board)",
		"location":      "Zoom",
		"tag":           "work",
		"start":         time.Date(2026, 4, 30, 11, 0, 0, 0, time.UTC),
		"end":           time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC),
		"attendees":     []string{"Saru Patel", "Lin Vega", "Park Choi"},
		"last_modified": now.Add(-15 * time.Minute),
	})
	require.NoError(t, err)

	// Seed a weather snapshot so the run-window projection doesn't error.
	_, err = lstore.Append(ctx, zlog.KindWeatherSnapshot, "weather", map[string]any{
		"captured_at": now.Add(-30 * time.Minute),
		"timezone":    "UTC",
		"hourly": []map[string]any{
			{"time": time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC), "code": 1, "wind_kmh": 8.0, "precip_mm": 0.0},
		},
	})
	require.NoError(t, err)

	llmClient := llm.NewClient(llm.ClientConfig{
		Endpoint: ts.URL,
		Model:    "test",
		Timeout:  10 * time.Second,
	})
	prompts, err := LoadPrompts("")
	require.NoError(t, err)

	logger := logrus.New()
	logger.Out = io.Discard
	entry := logger.WithField("c", "inject-test")

	deps := InjectDeps{
		LLM:     llmClient,
		Reader:  lstore,
		Memory:  &store.MemoryRepo{DB: db, Table: "memory_facts"},
		Prompts: prompts,
		ProjCfg: projection.Config{
			TZ:                    time.UTC,
			LookbackDays:          14,
			RunWindowMinMinutes:   45,
			RunWindowMaxWindKmh:   25,
			RunWindowEarliestHour: 6,
			RunWindowLatestHour:   20,
			OpenThreadsMax:        20,
			Now:                   func() time.Time { return now },
		},
		Date:   "2026-04-30",
		Now:    now,
		Logger: entry,
	}
	signal := InjectSignal{
		Kind:       "email",
		Subject:    "URGENT — board call moved to 10:30",
		EvidenceID: "<inject-1@test>",
		At:         now,
	}
	cleanup := func() { ts.Close() }
	return deps, signal, entry, cleanup
}

// TestSynthesizeInject_HappyPath pins the canonical 1-card + 1-paragraph
// output: the model emits a valid InjectCardSet with exactly one card,
// SynthesizeInject takes it, sets Origin="inject", renders the briefing
// fragment, and returns both.
func TestSynthesizeInject_HappyPath(t *testing.T) {
	deps, sig, _, cleanup := injectTestSetup(t, "inject_happy")
	defer cleanup()

	res, err := SynthesizeInject(context.Background(), deps, sig)
	require.NoError(t, err)
	require.Equal(t, "inject", res.Card.Origin, "inject cards must carry Origin=\"inject\" — DeleteStale keys off it")
	require.Equal(t, "2026-04-30", res.Card.Date)
	require.Contains(t, res.Card.Title, "Saru")
	require.Equal(t, "high", res.Card.Rel)
	require.NotEmpty(t, res.Card.RunID, "RunID must be set for audit trail")

	// Fragment must be in the message_inject voice band (Phase 2 rubric:
	// tension 80–100). Title carries the signal subject; eyebrow names the
	// inject signal itself.
	require.Equal(t, "message_inject", res.Fragment.State)
	require.GreaterOrEqual(t, res.Fragment.Tension, 75)
	require.LessOrEqual(t, res.Fragment.Tension, 100)
	require.NotEmpty(t, res.Fragment.Title)
	require.NotEmpty(t, res.Fragment.Summary)
}

// TestSynthesizeInject_UpdateMode pins the V2.x update-in-place mapping: an
// update-mode signal carrying an entity key produces a card whose ID IS that
// entity key and whose Origin is empty (NOT "inject") — so the persist
// Upsert refreshes the existing morning card rather than appending a badged
// inject duplicate.
func TestSynthesizeInject_UpdateMode(t *testing.T) {
	deps, sig, _, cleanup := injectTestSetup(t, "inject_happy")
	defer cleanup()

	sig.Mode = InjectModeUpdate
	sig.EntityKey = "thread:urgent-board-call-moved-to-10-30"

	res, err := SynthesizeInject(context.Background(), deps, sig)
	require.NoError(t, err)
	require.Equal(t, sig.EntityKey, res.Card.ID, "update-mode card uses the entity key as ID")
	require.Equal(t, sig.EntityKey, res.Card.EntityKey)
	require.Empty(t, res.Card.Origin, "update-mode must NOT stamp Origin=inject — it refreshes the morning card")
}

// TestSynthesizeInject_TooManyCards_KeepsFirst pins the 2-card → 1-card
// collapse: the model emits two cards (against the prompt instruction);
// SynthesizeInject takes the first and drops the rest with a log note.
func TestSynthesizeInject_TooManyCards_KeepsFirst(t *testing.T) {
	deps, sig, _, cleanup := injectTestSetup(t, "inject_too_many")
	defer cleanup()

	res, err := SynthesizeInject(context.Background(), deps, sig)
	require.NoError(t, err)
	require.Contains(t, res.Card.Title, "Saru", "first card kept")
	require.Equal(t, "inject", res.Card.Origin)
}

// TestSynthesizeInject_DegradedFallback pins the LLM-failure path: when
// the loop never returns a parseable card, SynthesizeInject must NOT
// error — it must return a degraded card built from the signal alone so
// the user sees something, plus a degraded fragment in the inject voice
// band. The whole call still succeeds; the caller's audit log differentiates.
func TestSynthesizeInject_DegradedFallback(t *testing.T) {
	deps, sig, _, cleanup := injectTestSetup(t, "inject_garbage")
	defer cleanup()

	res, err := SynthesizeInject(context.Background(), deps, sig)
	require.NoError(t, err, "soft failure must not propagate as error")
	require.Equal(t, "inject", res.Card.Origin)
	require.Contains(t, res.Card.Title, "URGENT", "degraded card title comes from signal subject")
	require.Equal(t, "mail", res.Card.Source, "degraded card src derives from signal kind")

	// The briefing fragment in this transcript happens to be valid (the
	// degraded code path only triggers for the cards loop), so we expect
	// a real fragment with a real tension band. If both fail, the
	// degraded fragment fallback at the bottom of SynthesizeInject still
	// keeps tension within the message_inject band.
	require.Equal(t, "message_inject", res.Fragment.State)
	require.GreaterOrEqual(t, res.Fragment.Tension, 75)
}
