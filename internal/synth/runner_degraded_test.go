package synth

import (
	"context"
	"encoding/json"
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

// TestRunner_BriefingRetry exercises the degraded-briefing fallback plus the
// deferred retry: cards persist, the first briefing attempt + repair both
// fail (empty content), so the runner persists a degraded row and schedules
// a retry. The retry (with a fast BriefingRetryDelay so the test runs in
// milliseconds) succeeds and replaces the briefing row in place.
func TestRunner_BriefingRetry(t *testing.T) {
	turns := loadTranscript(t, "briefing_retry")
	ts := newTranscriptServer(t, turns)
	defer ts.Close()

	dbPath := filepath.Join(t.TempDir(), "zeno.db")
	db, lstore, err := zlog.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, Migrate(db, true, false))

	now := time.Date(2026, 4, 25, 7, 30, 0, 0, time.UTC)
	ctx := context.Background()

	llmClient := llm.NewClient(llm.ClientConfig{
		Endpoint: ts.URL,
		Model:    "test",
		Timeout:  10 * time.Second,
	})
	prompts, err := LoadPrompts("")
	require.NoError(t, err)

	logger := logrus.New()
	logger.Out = io.Discard

	runner := &Runner{
		LLM:      llmClient,
		Reader:   lstore,
		DB:       db,
		EventLog: lstore,
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
		Prompts:            prompts,
		Now:                func() time.Time { return now },
		Logger:             logger.WithField("c", "synth-test"),
		CardsTable:         "cards",
		BriefingTable:      "briefings",
		TraceTable:         "traces",
		BriefingRetryDelay: 10 * time.Millisecond,
	}

	require.NoError(t, runner.Run(ctx))

	// Cards must persist regardless of the briefing failure.
	cards, err := (&store.CardRepo{DB: db, Table: "cards"}).ListByDate(ctx, "2026-04-25")
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(cards), 1)

	// Immediately after Run, the briefing row is the degraded placeholder.
	briefRepo := &store.BriefingRepo{DB: db, Table: "briefings"}
	br, err := briefRepo.ByDate(ctx, "2026-04-25")
	require.NoError(t, err)
	require.NotNil(t, br)
	require.Equal(t, "draft pending", br.Eyebrow)

	// The retry fires after BriefingRetryDelay (10ms). Poll until it lands.
	require.Eventually(t, func() bool {
		got, qerr := briefRepo.ByDate(ctx, "2026-04-25")
		return qerr == nil && got != nil && got.Eyebrow != "draft pending"
	}, 2*time.Second, 10*time.Millisecond, "retry should replace degraded briefing")

	br, err = briefRepo.ByDate(ctx, "2026-04-25")
	require.NoError(t, err)
	require.NotNil(t, br)
	require.Equal(t, "recovered · 2 things worth knowing", br.Eyebrow)
	require.Equal(t, 42, br.Tension)

	// Boundary events: scheduled and completed{success: true} both present.
	events, err := lstore.ByKind(ctx,
		zlog.KindSynthBriefingRetryScheduled,
		zlog.KindSynthBriefingRetryCompleted,
	)
	require.NoError(t, err)
	require.Len(t, events, 2)

	var scheduled, completed *zlog.Event
	for i := range events {
		switch events[i].Kind {
		case zlog.KindSynthBriefingRetryScheduled:
			scheduled = &events[i]
		case zlog.KindSynthBriefingRetryCompleted:
			completed = &events[i]
		}
	}
	require.NotNil(t, scheduled, "scheduled event must be present")
	require.NotNil(t, completed, "completed event must be present")

	var completedPayload map[string]any
	require.NoError(t, json.Unmarshal(completed.Payload, &completedPayload))
	require.Equal(t, true, completedPayload["success"])
}
