package synth

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/llm"
	zlog "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/projection"
	"github.com/zenocy/zeno-v2/internal/store"
)

// askValidCardJSON is the canonical Ask response shape — kept inline so
// the reactive_bus_test fixture doesn't depend on a transcript file
// (the runner transcripts use prompts that don't match the Ask path).
const askValidCardJSON = `{"id":"answer-1","date":"2026-04-25","src":"ask","src_label":"Generated","rel":"med","title":"A *quiet* day.","sub":"Calendar is empty and threads are settled. Spend it on the deep work you keep deferring.","meta":[],"actions":[{"label":"Dismiss"}]}`

// reactiveAskFixture builds an Ask call wired to a transcript server
// pre-loaded with one valid response. Returned ReactiveDeps points at it.
func reactiveAskFixture(t *testing.T) (ReactiveDeps, func()) {
	t.Helper()
	turns := []transcriptTurn{
		{Resp: transcriptResp{Content: askValidCardJSON}},
	}
	ts := newTranscriptServer(t, turns)

	dbPath := t.TempDir() + "/zeno.db"
	db, lstore, err := zlog.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, Migrate(db, true, false))

	prompts, err := LoadPrompts("")
	require.NoError(t, err)
	logger := logrus.New()
	logger.Out = io.Discard

	llmClient := llm.NewClient(llm.ClientConfig{
		Endpoint: ts.URL,
		Model:    "test",
		Timeout:  10 * time.Second,
	})

	deps := ReactiveDeps{
		LLM:    llmClient,
		Reader: lstore,
		Memory: &store.MemoryRepo{DB: db, Table: "memory_facts"},
		ProjCfg: projection.Config{
			TZ:                    time.UTC,
			LookbackDays:          14,
			RunWindowEarliestHour: 6,
			RunWindowLatestHour:   20,
			OpenThreadsMax:        20,
			Now:                   func() time.Time { return time.Date(2026, 4, 25, 9, 0, 0, 0, time.UTC) },
		},
		Prompts:  prompts,
		Date:     "2026-04-25",
		Now:      time.Date(2026, 4, 25, 9, 0, 0, 0, time.UTC),
		Deadline: 5 * time.Second,
		Logger:   logger.WithField("c", "reactive-bus-test"),
	}
	return deps, func() { ts.Close() }
}

// TestAsk_ForwardsLiveTraceFromContext pins the contract that the
// AskHandler will rely on: when a caller attaches LiveTraceFunc and
// StreamContentFunc to ctx, synth.Ask's internal RunLoop must forward
// each step to LiveTraceFunc and each content delta to StreamContentFunc.
// We don't go through the bus here — we capture into slices directly so
// the test can match them against the sealed Trace byte-equal.
func TestAsk_ForwardsLiveTraceFromContext(t *testing.T) {
	deps, cleanup := reactiveAskFixture(t)
	defer cleanup()

	var (
		mu              sync.Mutex
		capturedSteps   []llm.TraceStep
		capturedContent string
	)

	ctx := context.Background()
	ctx = llm.ContextWithLiveTrace(ctx, func(s llm.TraceStep) {
		mu.Lock()
		defer mu.Unlock()
		capturedSteps = append(capturedSteps, s)
	})
	ctx = llm.ContextWithStreamContent(ctx, func(delta string) {
		mu.Lock()
		defer mu.Unlock()
		capturedContent += delta
	})

	_, trace, _, _ := Ask(ctx, deps, "what about today?")

	mu.Lock()
	defer mu.Unlock()

	// Live-published steps must equal the sealed Trace.Steps element-wise.
	// We compare lengths first to give a useful failure if the loop
	// bypassed the live publisher.
	require.Equal(t, len(trace.Steps), len(capturedSteps),
		"live publisher must see every step that lands in the sealed trace")
	for i, s := range trace.Steps {
		require.Equal(t, s, capturedSteps[i], "step %d mismatch", i)
	}

	// Content callback must have received deltas. We don't assert the
	// concatenation byte-equal because Ask post-processes the content
	// (canonicalize, strip code fences). Confirming non-empty is enough.
	require.NotEmpty(t, capturedContent,
		"StreamContentFunc must receive content deltas during synthesis")
}

// TestAsk_NoCallbacksAttached_ProducesV23Behavior — V2.3 backward compat:
// no callbacks in ctx → no streaming routing → Ask still returns a card.
func TestAsk_NoCallbacksAttached_ProducesV23Behavior(t *testing.T) {
	deps, cleanup := reactiveAskFixture(t)
	defer cleanup()

	card, trace, _, err := Ask(context.Background(), deps, "what about today?")
	require.NoError(t, err, "Ask should never bubble an error from the synthesis loop")
	require.NotEmpty(t, card.ID)
	require.NotEmpty(t, trace.Stopped)
}
