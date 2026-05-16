package synth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	"github.com/zenocy/zeno-v2/internal/store"
)

// recognitionTestRig wires up a hermetic environment for one recognition
// pass: in-memory SQLite (concerns + tags migrated), a logtest.MemReader
// for both reading observations AND auditing recognition runs, a typed
// eventbus to verify publishes, and a stubbed LLM endpoint whose
// response is set per-test.
//
// The pattern mirrors extract_facts_test.go's stubExtractServer: the
// httptest server takes a single canned response, so each test composes
// the model's "answer" up front.
type recognitionTestRig struct {
	DB        *gorm.DB
	Concerns  *store.ConcernRepo
	Obs       *store.ConcernObservationRepo
	Reader    *logtest.MemReader
	Bus       *eventbus.Bus
	LLMClient *llm.Client
	LLMServer *httptest.Server
	LLMCalls  *atomic.Int32 // increments on each ChatCompletion request
	LastReq   *atomic.Value // last *http.Request body bytes (for prompt assertions)
	Logger    *logrus.Entry
}

func newRecognitionRig(t *testing.T, response string) *recognitionTestRig {
	t.Helper()

	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)

	cRepo := &store.ConcernRepo{DB: db, Table: "concerns"}
	tRepo := &store.ConcernObservationRepo{DB: db, Table: "concern_observations"}
	require.NoError(t, cRepo.Migrate())
	require.NoError(t, tRepo.Migrate())

	calls := &atomic.Int32{}
	last := &atomic.Value{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		last.Store(string(body))
		calls.Add(1)
		resp := map[string]any{
			"id":     "rec-test",
			"object": "chat.completion",
			"model":  "test",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": response},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)

	cli := llm.NewClient(llm.ClientConfig{Endpoint: srv.URL, Model: "test"})

	logger := logrus.New()
	logger.Out = io.Discard

	bus := eventbus.New(logger.WithField("c", "bus"))

	return &recognitionTestRig{
		DB:        db,
		Concerns:  cRepo,
		Obs:       tRepo,
		Reader:    logtest.NewMemReader(),
		Bus:       bus,
		LLMClient: cli,
		LLMServer: srv,
		LLMCalls:  calls,
		LastReq:   last,
		Logger:    logger.WithField("c", "test"),
	}
}

// proposalsJSON encodes a proposals envelope as the LLM would return it.
func proposalsJSON(proposals ...ProposedConcern) string {
	b, _ := json.Marshal(map[string]any{"proposals": proposals})
	return string(b)
}

func seedMail(rig *recognitionTestRig, ts time.Time, subject, from, preview string) {
	rig.Reader.AppendEvent(logtest.MakeEvent(log.KindMailReceived, "imap", ts, map[string]any{
		"folder":       "INBOX",
		"uid":          uint32(ts.Unix() & 0x7fffffff),
		"uidvalidity":  uint32(1),
		"from":         from,
		"subject":      subject,
		"date":         ts,
		"body_preview": preview,
	}))
}

// TestRecognize_HappyPath_ProducesProposalAndPersists exercises the
// success path: model proposes one high-confidence concern, runner
// inserts it as proposed, pre-tags the observations, fires bus events,
// and audits the run kind. This is the contract every other test
// modulates.
func TestRecognize_HappyPath_ProducesProposalAndPersists(t *testing.T) {
	rig := newRecognitionRig(t, proposalsJSON(ProposedConcern{
		Name:           "Construction at the house",
		Description:    "Kitchen tile and the inspection are the open beats; contractor active.",
		Confidence:     0.85,
		ObservationIDs: []string{"obs-1", "obs-2"},
	}))

	ctx := context.Background()
	now := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	seedMail(rig, now.Add(-3*24*time.Hour), "Drywall schedule", "hector", "crew pushed by a day")
	seedMail(rig, now.Add(-1*24*time.Hour), "Inspector confirmed", "hector", "Wednesday 10:00")

	// Subscribe to bus before running so we capture publishes synchronously.
	sub := rig.Bus.Subscribe()
	defer rig.Bus.Unsubscribe(sub)

	res, err := Recognize(ctx, RecognizeDeps{
		LLM:          rig.LLMClient,
		Reader:       rig.Reader,
		Concerns:     rig.Concerns,
		Observations: rig.Obs,
		Bus:          rig.Bus,
		EventLog:     rig.Reader, // double-duty: log reader + writer
		Logger:       rig.Logger,
	}, RecognizeOpts{Now: now})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Len(t, res.Accepted, 1)
	require.Empty(t, res.Rejected)
	require.Equal(t, 2, res.ObservationN)

	// Concern persisted as proposed, model-source.
	row, err := rig.Concerns.GetByID(ctx, res.Accepted[0].ConcernID)
	require.NoError(t, err)
	require.NotNil(t, row)
	require.Equal(t, store.ConcernStateProposed, row.State)
	require.Equal(t, store.ConcernSourceModel, row.Source)
	require.Equal(t, "construction at the house", row.NormName)
	require.InDelta(t, 0.85, row.Confidence, 1e-9)

	// Pre-tags landed in the join.
	tagged, err := rig.Obs.ListByConcern(ctx, row.ID, 0)
	require.NoError(t, err)
	require.Len(t, tagged, 2)

	// Bus delivered concern.proposed + concern.tagged.
	gotProposed, gotTagged := false, false
	deadline := time.After(time.Second)
	for !(gotProposed && gotTagged) {
		select {
		case ev := <-sub:
			switch e := ev.(type) {
			case eventbus.ConcernProposedEvent:
				require.Equal(t, row.ID, e.ConcernID)
				gotProposed = true
			case eventbus.ConcernTaggedEvent:
				require.Equal(t, "recognition", e.BatchOrigin)
				gotTagged = true
			}
		case <-deadline:
			t.Fatalf("timed out waiting for bus events: proposed=%v tagged=%v", gotProposed, gotTagged)
		}
	}

	// Audit kind landed.
	kinds := map[string]int{}
	for _, e := range rig.Reader.Events() {
		kinds[e.Kind]++
	}
	require.GreaterOrEqual(t, kinds[log.KindConcernRecognitionRun], 1)
}

// TestRecognize_DailyCapTruncates verifies the cap isn't a soft hint —
// the third proposal must be rejected with reason="daily_cap" even
// when its confidence is high. This is what stops the model from
// dumping a dozen concerns on the user's first morning.
func TestRecognize_DailyCapTruncates(t *testing.T) {
	rig := newRecognitionRig(t, proposalsJSON(
		ProposedConcern{Name: "Concern A", Description: "First", Confidence: 0.9, ObservationIDs: []string{"o1"}},
		ProposedConcern{Name: "Concern B", Description: "Second", Confidence: 0.9, ObservationIDs: []string{"o2"}},
		ProposedConcern{Name: "Concern C", Description: "Third", Confidence: 0.9, ObservationIDs: []string{"o3"}},
	))
	ctx := context.Background()
	now := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	seedMail(rig, now.Add(-1*time.Hour), "anything", "x", "y")

	res, err := Recognize(ctx, RecognizeDeps{
		LLM: rig.LLMClient, Reader: rig.Reader,
		Concerns: rig.Concerns, Observations: rig.Obs,
	}, RecognizeOpts{Now: now, DailyCap: 2})
	require.NoError(t, err)
	require.Len(t, res.Accepted, 2)
	require.Len(t, res.Rejected, 1)
	require.Equal(t, "daily_cap", res.Rejected[0].Reason)
}

// TestRecognize_LowConfidenceFiltered guarantees the floor isn't
// negotiable: a 0.6 proposal with MinConfidence=0.7 is rejected with
// reason="low_confidence" even when it cites real observations.
func TestRecognize_LowConfidenceFiltered(t *testing.T) {
	rig := newRecognitionRig(t, proposalsJSON(
		ProposedConcern{Name: "Maybe a thing", Description: "Hedging", Confidence: 0.6, ObservationIDs: []string{"o1"}},
	))
	ctx := context.Background()
	now := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	seedMail(rig, now.Add(-1*time.Hour), "x", "y", "z")

	res, err := Recognize(ctx, RecognizeDeps{
		LLM: rig.LLMClient, Reader: rig.Reader,
		Concerns: rig.Concerns, Observations: rig.Obs,
	}, RecognizeOpts{Now: now, MinConfidence: 0.7})
	require.NoError(t, err)
	require.Empty(t, res.Accepted)
	require.Len(t, res.Rejected, 1)
	require.Equal(t, "low_confidence", res.Rejected[0].Reason)
}

// TestRecognize_DenylistSkipsRecentlyDismissed pins the 90-day denylist
// behavior: a soft-deleted concern's normalized name blocks re-proposal
// for the configured window, and a name dismissed > window-ago does not
// block. Without this contract recognition would re-propose a concern
// the user explicitly dismissed last week.
func TestRecognize_DenylistSkipsRecentlyDismissed(t *testing.T) {
	rig := newRecognitionRig(t, proposalsJSON(
		ProposedConcern{Name: "Construction", Description: "x", Confidence: 0.9, ObservationIDs: []string{"o1"}},
	))
	ctx := context.Background()
	now := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	seedMail(rig, now.Add(-1*time.Hour), "x", "y", "z")

	// Pre-seed a soft-deleted "construction" concern dismissed 30 days ago.
	dismissed := store.Concern{
		ID:           "old-1",
		Name:         "Construction",
		NormName:     "construction",
		Description:  "old",
		State:        store.ConcernStateProposed,
		Source:       store.ConcernSourceModel,
		LastActiveAt: now.Add(-31 * 24 * time.Hour),
		CreatedAt:    now.Add(-31 * 24 * time.Hour),
		UpdatedAt:    now.Add(-31 * 24 * time.Hour),
	}
	require.NoError(t, rig.Concerns.Insert(ctx, dismissed))
	// Soft-delete and override deleted_at to a known timestamp 30 days ago.
	require.NoError(t, rig.Concerns.SoftDelete(ctx, "old-1"))
	require.NoError(t, rig.DB.Exec(
		"UPDATE concerns SET deleted_at = ? WHERE id = ?",
		now.Add(-30*24*time.Hour), "old-1",
	).Error)

	res, err := Recognize(ctx, RecognizeDeps{
		LLM: rig.LLMClient, Reader: rig.Reader,
		Concerns: rig.Concerns, Observations: rig.Obs,
	}, RecognizeOpts{Now: now})
	require.NoError(t, err)
	require.Empty(t, res.Accepted)
	require.Len(t, res.Rejected, 1)
	require.Equal(t, "denylisted", res.Rejected[0].Reason)
}

func TestRecognize_DenylistExpiresAfterWindow(t *testing.T) {
	rig := newRecognitionRig(t, proposalsJSON(
		ProposedConcern{Name: "Construction", Description: "x", Confidence: 0.9, ObservationIDs: []string{"o1"}},
	))
	ctx := context.Background()
	now := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	seedMail(rig, now.Add(-1*time.Hour), "x", "y", "z")

	dismissed := store.Concern{
		ID: "old-1", Name: "Construction", NormName: "construction",
		Description: "old", State: store.ConcernStateProposed,
		Source: store.ConcernSourceModel, LastActiveAt: now.Add(-200 * 24 * time.Hour),
		CreatedAt: now.Add(-200 * 24 * time.Hour), UpdatedAt: now.Add(-200 * 24 * time.Hour),
	}
	require.NoError(t, rig.Concerns.Insert(ctx, dismissed))
	require.NoError(t, rig.Concerns.SoftDelete(ctx, "old-1"))
	// 100 days ago > 90-day window → denylist no longer applies.
	require.NoError(t, rig.DB.Exec(
		"UPDATE concerns SET deleted_at = ? WHERE id = ?",
		now.Add(-100*24*time.Hour), "old-1",
	).Error)

	res, err := Recognize(ctx, RecognizeDeps{
		LLM: rig.LLMClient, Reader: rig.Reader,
		Concerns: rig.Concerns, Observations: rig.Obs,
	}, RecognizeOpts{Now: now})
	require.NoError(t, err)
	require.Len(t, res.Accepted, 1)
}

// TestRecognize_DuplicateNormNameSkipped covers a separate path from the
// denylist: an *active* concern with the same normalized name. The
// runner must not insert a duplicate row.
func TestRecognize_DuplicateNormNameSkipped(t *testing.T) {
	rig := newRecognitionRig(t, proposalsJSON(
		ProposedConcern{Name: "Frankfurt Trip", Description: "x", Confidence: 0.9, ObservationIDs: []string{"o1"}},
	))
	ctx := context.Background()
	now := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	seedMail(rig, now.Add(-1*time.Hour), "x", "y", "z")

	existing := store.Concern{
		ID: "exists-1", Name: "Frankfurt trip", NormName: "frankfurt trip",
		Description: "existing", State: store.ConcernStateActive,
		Source: store.ConcernSourceUser, LastActiveAt: now,
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, rig.Concerns.Insert(ctx, existing))

	res, err := Recognize(ctx, RecognizeDeps{
		LLM: rig.LLMClient, Reader: rig.Reader,
		Concerns: rig.Concerns, Observations: rig.Obs,
	}, RecognizeOpts{Now: now})
	require.NoError(t, err)
	require.Empty(t, res.Accepted)
	require.Len(t, res.Rejected, 1)
	require.Equal(t, "duplicate_norm_name", res.Rejected[0].Reason)
}

// TestRecognize_NoObservationsSkipsLLM verifies the cheap exit: empty
// log, no LLM call, no proposals, audit kind logged. This is the
// behavior that keeps a fresh install from making a daily LLM call
// before it has any data.
func TestRecognize_NoObservationsSkipsLLM(t *testing.T) {
	rig := newRecognitionRig(t, proposalsJSON())
	ctx := context.Background()
	now := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)

	res, err := Recognize(ctx, RecognizeDeps{
		LLM: rig.LLMClient, Reader: rig.Reader,
		Concerns: rig.Concerns, Observations: rig.Obs,
		EventLog: rig.Reader,
	}, RecognizeOpts{Now: now})
	require.NoError(t, err)
	require.Empty(t, res.Accepted)
	require.Equal(t, int32(0), rig.LLMCalls.Load(), "no LLM call when log is empty")

	// Audit row reflects skip.
	found := false
	for _, e := range rig.Reader.Events() {
		if e.Kind == log.KindConcernRecognitionRun {
			found = true
			require.Contains(t, string(e.Payload), "skipped_no_observations")
		}
	}
	require.True(t, found, "expected recognition_run audit row")
}

// TestRecognize_HonorsCancel exercises the context-cancel path: caller
// cancels before the LLM call, runner returns ctx.Err()-ish error, no
// DB writes. This matches the cron's signal-handler semantics.
func TestRecognize_HonorsCancel(t *testing.T) {
	rig := newRecognitionRig(t, proposalsJSON(
		ProposedConcern{Name: "X", Description: "y", Confidence: 0.9, ObservationIDs: []string{"o1"}},
	))
	now := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	seedMail(rig, now.Add(-1*time.Hour), "x", "y", "z")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Recognize(ctx, RecognizeDeps{
		LLM: rig.LLMClient, Reader: rig.Reader,
		Concerns: rig.Concerns, Observations: rig.Obs,
	}, RecognizeOpts{Now: now})
	require.Error(t, err)
}

// TestRecognize_RejectsBadLLMResponse pins the parse-failure path: model
// returns garbage, runner audits the failure and returns an error
// without inserting anything.
func TestRecognize_RejectsBadLLMResponse(t *testing.T) {
	rig := newRecognitionRig(t, "this is not JSON")
	ctx := context.Background()
	now := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	seedMail(rig, now.Add(-1*time.Hour), "x", "y", "z")

	_, err := Recognize(ctx, RecognizeDeps{
		LLM: rig.LLMClient, Reader: rig.Reader,
		Concerns: rig.Concerns, Observations: rig.Obs,
		EventLog: rig.Reader,
	}, RecognizeOpts{Now: now})
	require.Error(t, err)

	// No concern persisted.
	rows, _ := rig.Concerns.ListAll(ctx)
	require.Empty(t, rows)
	// Audit "parse_failed".
	found := false
	for _, e := range rig.Reader.Events() {
		if e.Kind == log.KindConcernRecognitionRun && strings.Contains(string(e.Payload), "parse_failed") {
			found = true
		}
	}
	require.True(t, found, "expected parse_failed audit row")
}

// TestRecognize_RejectsObservationsOutsideLookback proves the lookback
// window is honored: an observation older than Lookback is invisible
// to the prompt, so a very narrow window with otherwise-sufficient
// data produces zero proposals.
func TestRecognize_RejectsObservationsOutsideLookback(t *testing.T) {
	rig := newRecognitionRig(t, proposalsJSON())
	ctx := context.Background()
	now := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	// Observation 30 days ago; lookback only 7 days.
	seedMail(rig, now.Add(-30*24*time.Hour), "ancient", "x", "y")

	res, err := Recognize(ctx, RecognizeDeps{
		LLM: rig.LLMClient, Reader: rig.Reader,
		Concerns: rig.Concerns, Observations: rig.Obs,
	}, RecognizeOpts{Now: now, Lookback: 7 * 24 * time.Hour})
	require.NoError(t, err)
	require.Equal(t, 0, res.ObservationN)
	require.Equal(t, int32(0), rig.LLMCalls.Load(), "out-of-window obs → empty prompt → no LLM call")
}
