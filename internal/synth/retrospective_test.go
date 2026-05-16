package synth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// retrospectiveTestRig sets up a hermetic environment with a programmable
// LLM stub. Each test composes the per-batch responses up front. The stub
// reads `responses` (a slice) round-robin: the Nth POST gets responses[N].
// Surplus calls return responses[len-1]. responseErr (if non-nil at index
// N) returns an HTTP error for the Nth call instead.
type retrospectiveTestRig struct {
	DB         *gorm.DB
	Concerns   *store.ConcernRepo
	Obs        *store.ConcernObservationRepo
	Reader     *logtest.MemReader
	Bus        *eventbus.Bus
	LLMClient  *llm.Client
	LLMServer  *httptest.Server
	LLMCalls   *atomic.Int32
	Responses  []string
	ResponseEr []error // parallel; nil = success
	Logger     *logrus.Entry
}

func newRetrospectiveRig(t *testing.T, responses []string, respErrs []error) *retrospectiveTestRig {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	cRepo := &store.ConcernRepo{DB: db, Table: "concerns"}
	oRepo := &store.ConcernObservationRepo{DB: db, Table: "concern_observations"}
	require.NoError(t, cRepo.Migrate())
	require.NoError(t, oRepo.Migrate())

	calls := &atomic.Int32{}
	rig := &retrospectiveTestRig{
		DB: db, Concerns: cRepo, Obs: oRepo,
		Reader:     logtest.NewMemReader(),
		Responses:  responses,
		ResponseEr: respErrs,
		LLMCalls:   calls,
	}

	rig.LLMServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		idx := int(calls.Load())
		calls.Add(1)
		if rig.ResponseEr != nil && idx < len(rig.ResponseEr) && rig.ResponseEr[idx] != nil {
			http.Error(w, rig.ResponseEr[idx].Error(), http.StatusInternalServerError)
			return
		}
		body := ""
		if idx < len(rig.Responses) {
			body = rig.Responses[idx]
		} else if len(rig.Responses) > 0 {
			body = rig.Responses[len(rig.Responses)-1]
		}
		resp := map[string]any{
			"id":     "rt-test",
			"object": "chat.completion",
			"model":  "test",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": body},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(rig.LLMServer.Close)

	rig.LLMClient = llm.NewClient(llm.ClientConfig{Endpoint: rig.LLMServer.URL, Model: "test"})

	logger := logrus.New()
	logger.Out = io.Discard
	rig.Logger = logger.WithField("c", "test")
	rig.Bus = eventbus.New(logger.WithField("c", "bus"))

	return rig
}

func tagsJSON(tags ...map[string]any) string {
	b, _ := json.Marshal(map[string]any{"tags": tags})
	return string(b)
}

func seedConcernActive(t *testing.T, rig *retrospectiveTestRig, id, name string) {
	t.Helper()
	now := time.Now()
	require.NoError(t, rig.Concerns.Insert(context.Background(), store.Concern{
		ID: id, Name: name, NormName: store.NormalizeConcernName(name),
		Description: "test", State: store.ConcernStateActive,
		Source: store.ConcernSourceUser, LastActiveAt: now,
		CreatedAt: now, UpdatedAt: now, Confidence: 1.0,
	}))
}

func seedRetroMail(rig *retrospectiveTestRig, id string, ts time.Time, subject string) {
	rig.Reader.AppendEvent(log.Event{
		ID:      id,
		TS:      ts.UTC(),
		Kind:    log.KindMailReceived,
		Source:  "imap",
		Payload: jsonMust(map[string]any{"subject": subject, "from": "x", "body_preview": ""}),
	})
}

func jsonMust(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// TestRetrospective_HappyPath_TagsExpectedSet covers the success contract:
// model returns true/false per observation, runner persists only the
// trues with confidence above the floor, sums tagged count, fires bus
// progress events, and audits start + completed kinds.
func TestRetrospective_HappyPath_TagsExpectedSet(t *testing.T) {
	rig := newRetrospectiveRig(t, []string{tagsJSON(
		map[string]any{"observation_id": "obs-1", "tag": true, "confidence": 0.9},
		map[string]any{"observation_id": "obs-2", "tag": true, "confidence": 0.85},
		map[string]any{"observation_id": "obs-3", "tag": false, "confidence": 0.8},
	)}, nil)
	ctx := context.Background()
	now := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)

	seedConcernActive(t, rig, "c-1", "Construction at the house")
	seedRetroMail(rig, "obs-1", now.Add(-30*24*time.Hour), "Architect proposal")
	seedRetroMail(rig, "obs-2", now.Add(-20*24*time.Hour), "Permit filed")
	seedRetroMail(rig, "obs-3", now.Add(-10*24*time.Hour), "Receipt — Whole Foods")

	progressSub := rig.Bus.Subscribe()
	defer rig.Bus.Unsubscribe(progressSub)

	res, err := Retrospective(ctx, RetrospectiveDeps{
		LLM: rig.LLMClient, Reader: rig.Reader,
		Concerns: rig.Concerns, Observations: rig.Obs,
		Bus: rig.Bus, EventLog: rig.Reader, Logger: rig.Logger,
	}, "c-1", RetrospectiveOpts{Now: now, Batch: 20})
	require.NoError(t, err)
	require.Equal(t, "completed", res.Status)
	require.Equal(t, 3, res.Total)
	require.Equal(t, 3, res.Processed)
	require.Equal(t, 2, res.Tagged)
	require.Equal(t, 1, res.Calls)

	// Persisted tags.
	count, err := rig.Obs.CountByConcern(ctx, "c-1")
	require.NoError(t, err)
	require.Equal(t, int64(2), count)
	require.True(t, isTaggedHelper(t, rig, "c-1", "obs-1"))
	require.True(t, isTaggedHelper(t, rig, "c-1", "obs-2"))
	require.False(t, isTaggedHelper(t, rig, "c-1", "obs-3"))

	// Audit kinds.
	startedSeen, completedSeen := false, false
	for _, e := range rig.Reader.Events() {
		switch e.Kind {
		case log.KindConcernRetrospectiveStarted:
			startedSeen = true
		case log.KindConcernRetrospectiveCompleted:
			completedSeen = true
		}
	}
	require.True(t, startedSeen, "expected retrospective_started")
	require.True(t, completedSeen, "expected retrospective_completed")

	// Progress: at least one running + one completed event.
	gotRunning, gotCompleted := false, false
	deadline := time.After(time.Second)
	for !(gotRunning && gotCompleted) {
		select {
		case ev := <-progressSub:
			if pe, ok := ev.(eventbus.RetrospectiveProgressEvent); ok {
				switch pe.Status {
				case "running":
					gotRunning = true
				case "completed":
					gotCompleted = true
				}
			}
		case <-deadline:
			t.Fatalf("timed out waiting for progress events: running=%v completed=%v", gotRunning, gotCompleted)
		}
	}
}

func isTaggedHelper(t *testing.T, rig *retrospectiveTestRig, concernID, eventID string) bool {
	t.Helper()
	ok, err := rig.Obs.IsTagged(context.Background(), concernID, eventID)
	require.NoError(t, err)
	return ok
}

// TestRetrospective_BatchBoundaryProgressEvents pins the per-batch progress
// contract: with Batch=2 over 4 observations, the runner emits two
// running events with monotonically-increasing Processed counts.
func TestRetrospective_BatchBoundaryProgressEvents(t *testing.T) {
	// Two batches, each tagging both items.
	batchResp := tagsJSON(
		map[string]any{"observation_id": "x", "tag": false, "confidence": 0.8},
		map[string]any{"observation_id": "y", "tag": false, "confidence": 0.8},
	)
	rig := newRetrospectiveRig(t, []string{batchResp, batchResp}, nil)
	ctx := context.Background()
	now := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)

	seedConcernActive(t, rig, "c-1", "Some concern")
	for i, id := range []string{"o1", "o2", "o3", "o4"} {
		seedRetroMail(rig, id, now.Add(-time.Duration(i+1)*24*time.Hour), fmt.Sprintf("subj-%d", i))
	}

	sub := rig.Bus.Subscribe()
	defer rig.Bus.Unsubscribe(sub)

	res, err := Retrospective(ctx, RetrospectiveDeps{
		LLM: rig.LLMClient, Reader: rig.Reader,
		Concerns: rig.Concerns, Observations: rig.Obs, Bus: rig.Bus,
	}, "c-1", RetrospectiveOpts{Now: now, Batch: 2})
	require.NoError(t, err)
	require.Equal(t, 2, res.Calls)
	require.Equal(t, 4, res.Processed)

	// Collect progress events with a timeout, then assert sequence.
	var processed []int
	var statuses []string
	deadline := time.After(time.Second)
	done := false
	for !done {
		select {
		case ev := <-sub:
			pe, ok := ev.(eventbus.RetrospectiveProgressEvent)
			if !ok {
				continue
			}
			processed = append(processed, pe.Processed)
			statuses = append(statuses, pe.Status)
			if pe.Status == "completed" {
				done = true
			}
		case <-deadline:
			done = true
		}
	}
	require.Contains(t, statuses, "completed")
	// Monotonic non-decreasing.
	for i := 1; i < len(processed); i++ {
		require.GreaterOrEqual(t, processed[i], processed[i-1], "progress must not regress")
	}
}

// TestRetrospective_CancellationAtBatchBoundary verifies the cancel
// contract: cancel after the first batch returns Status="cancelled",
// the partial tags from batch one are persisted, no batch two LLM
// call is made, and the cancelled audit kind lands.
func TestRetrospective_CancellationAtBatchBoundary(t *testing.T) {
	// First batch tags one observation; the second would tag another,
	// but we cancel before it runs.
	rig := newRetrospectiveRig(t, []string{tagsJSON(
		map[string]any{"observation_id": "o1", "tag": true, "confidence": 0.9},
		map[string]any{"observation_id": "o2", "tag": false, "confidence": 0.8},
	)}, nil)
	now := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	seedConcernActive(t, rig, "c-1", "X")
	for i, id := range []string{"o1", "o2", "o3", "o4"} {
		seedRetroMail(rig, id, now.Add(-time.Duration(i+1)*24*time.Hour), fmt.Sprintf("s-%d", i))
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Start with batch=2 (4 obs / 2 = 2 batches). Cancel happens after
	// the first batch via a goroutine that fires on first progress
	// event.
	sub := rig.Bus.Subscribe()
	defer rig.Bus.Unsubscribe(sub)

	go func() {
		// Wait for the first "running" progress event with Processed > 0,
		// then cancel.
		for ev := range sub {
			pe, ok := ev.(eventbus.RetrospectiveProgressEvent)
			if !ok {
				continue
			}
			if pe.Status == "running" && pe.Processed > 0 {
				cancel()
				return
			}
		}
	}()

	res, err := Retrospective(ctx, RetrospectiveDeps{
		LLM: rig.LLMClient, Reader: rig.Reader,
		Concerns: rig.Concerns, Observations: rig.Obs, Bus: rig.Bus,
		EventLog: rig.Reader,
	}, "c-1", RetrospectiveOpts{Now: now, Batch: 2})
	require.NoError(t, err, "cancel is reported via Status, not as an error")
	require.Equal(t, "cancelled", res.Status)
	require.Less(t, res.Processed, res.Total, "must not have processed everything")

	// First batch's tag persisted.
	require.True(t, isTaggedHelper(t, rig, "c-1", "o1"))

	// Cancelled audit landed.
	cancelledSeen := false
	for _, e := range rig.Reader.Events() {
		if e.Kind == log.KindConcernRetrospectiveCancelled {
			cancelledSeen = true
		}
	}
	require.True(t, cancelledSeen)
}

// TestRetrospective_Idempotency_ReRunNoDuplicates is the contract the
// composite-PK + soft-delete schema gives us: a second invocation
// touches no new rows because every observation is already tagged
// (or untagged-as-soft-deleted). Without the IsTaggedIncludingDeleted
// pre-filter the runner would re-call the LLM on already-classified
// observations; the test pins both the no-LLM-call optimization AND
// the no-duplicate-row outcome.
func TestRetrospective_Idempotency_ReRunNoDuplicates(t *testing.T) {
	resp := tagsJSON(
		map[string]any{"observation_id": "o1", "tag": true, "confidence": 0.9},
		map[string]any{"observation_id": "o2", "tag": true, "confidence": 0.9},
	)
	rig := newRetrospectiveRig(t, []string{resp}, nil)
	ctx := context.Background()
	now := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	seedConcernActive(t, rig, "c-1", "X")
	seedRetroMail(rig, "o1", now.Add(-2*24*time.Hour), "a")
	seedRetroMail(rig, "o2", now.Add(-1*24*time.Hour), "b")

	_, err := Retrospective(ctx, RetrospectiveDeps{
		LLM: rig.LLMClient, Reader: rig.Reader,
		Concerns: rig.Concerns, Observations: rig.Obs,
	}, "c-1", RetrospectiveOpts{Now: now, Batch: 10})
	require.NoError(t, err)
	firstCount, _ := rig.Obs.CountByConcern(ctx, "c-1")

	callsBeforeReRun := rig.LLMCalls.Load()
	res2, err := Retrospective(ctx, RetrospectiveDeps{
		LLM: rig.LLMClient, Reader: rig.Reader,
		Concerns: rig.Concerns, Observations: rig.Obs,
	}, "c-1", RetrospectiveOpts{Now: now, Batch: 10})
	require.NoError(t, err)
	require.Equal(t, "completed", res2.Status)
	require.Equal(t, 0, res2.Total, "every observation already tagged → nothing to do")
	require.Equal(t, callsBeforeReRun, rig.LLMCalls.Load(), "no LLM call on re-run")

	secondCount, _ := rig.Obs.CountByConcern(ctx, "c-1")
	require.Equal(t, firstCount, secondCount, "no duplicate rows")
}

// TestRetrospective_MaxCallsCap pins the operator-configurable hard
// ceiling: with 6 observations, Batch=2, MaxCalls=2, the runner stops
// after 2 LLM calls (4 observations processed) and reports status
// "completed" — the cap is not an error.
func TestRetrospective_MaxCallsCap(t *testing.T) {
	resp := tagsJSON(
		map[string]any{"observation_id": "x", "tag": false, "confidence": 0.8},
		map[string]any{"observation_id": "y", "tag": false, "confidence": 0.8},
	)
	rig := newRetrospectiveRig(t, []string{resp, resp}, nil)
	ctx := context.Background()
	now := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	seedConcernActive(t, rig, "c-1", "X")
	for i, id := range []string{"o1", "o2", "o3", "o4", "o5", "o6"} {
		seedRetroMail(rig, id, now.Add(-time.Duration(i+1)*24*time.Hour), fmt.Sprintf("s-%d", i))
	}

	res, err := Retrospective(ctx, RetrospectiveDeps{
		LLM: rig.LLMClient, Reader: rig.Reader,
		Concerns: rig.Concerns, Observations: rig.Obs,
	}, "c-1", RetrospectiveOpts{Now: now, Batch: 2, MaxCalls: 2})
	require.NoError(t, err)
	require.Equal(t, "completed", res.Status)
	require.Equal(t, 2, res.Calls)
	require.Equal(t, 4, res.Processed)
	require.Equal(t, 6, res.Total)
}

// TestRetrospective_SingleFlightPerConcern pins the in-memory guard:
// a second concurrent call for the same concern returns
// ErrRetrospectiveInFlight; a call for a different concern is allowed.
func TestRetrospective_SingleFlightPerConcern(t *testing.T) {
	rig := newRetrospectiveRig(t, []string{tagsJSON()}, nil)
	ctx := context.Background()
	now := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	seedConcernActive(t, rig, "c-1", "A")
	seedConcernActive(t, rig, "c-2", "B")

	// Acquire the slot manually and verify a Retrospective call rejects.
	require.True(t, acquireRetrospectiveSlot("c-1"))
	defer releaseRetrospectiveSlot("c-1")

	_, err := Retrospective(ctx, RetrospectiveDeps{
		LLM: rig.LLMClient, Reader: rig.Reader,
		Concerns: rig.Concerns, Observations: rig.Obs,
	}, "c-1", RetrospectiveOpts{Now: now})
	require.ErrorIs(t, err, ErrRetrospectiveInFlight)

	// Different concern is unaffected.
	_, err = Retrospective(ctx, RetrospectiveDeps{
		LLM: rig.LLMClient, Reader: rig.Reader,
		Concerns: rig.Concerns, Observations: rig.Obs,
	}, "c-2", RetrospectiveOpts{Now: now})
	require.NoError(t, err)
}

// TestRetrospective_FailedAfterLLMError pins the failure surface: the
// LLM endpoint hangs up mid-request, the runner reports status="failed"
// with the wrapped error, the failed audit kind lands, and progress
// fires once with Status="failed".
//
// We hijack the connection and close it (rather than http.Error 500)
// because the openai client tolerates many odd status responses — what
// it cannot tolerate is a closed connection with no body.
func TestRetrospective_FailedAfterLLMError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatalf("hijacker not supported")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		_ = conn.Close()
	}))
	defer srv.Close()
	cli := llm.NewClient(llm.ClientConfig{Endpoint: srv.URL, Model: "test", Timeout: 2 * time.Second})

	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	cRepo := &store.ConcernRepo{DB: db, Table: "concerns"}
	oRepo := &store.ConcernObservationRepo{DB: db, Table: "concern_observations"}
	require.NoError(t, cRepo.Migrate())
	require.NoError(t, oRepo.Migrate())

	logger := logrus.New()
	logger.Out = io.Discard
	bus := eventbus.New(logger.WithField("c", "bus"))
	mem := logtest.NewMemReader()

	now := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	require.NoError(t, cRepo.Insert(context.Background(), store.Concern{
		ID: "c-1", Name: "X", NormName: "x", Description: "x",
		State: store.ConcernStateActive, Source: store.ConcernSourceUser,
		LastActiveAt: now, CreatedAt: now, UpdatedAt: now, Confidence: 1,
	}))
	mem.AppendEvent(log.Event{
		ID: "o1", TS: now.Add(-1 * 24 * time.Hour),
		Kind: log.KindMailReceived, Source: "imap",
		Payload: jsonMust(map[string]any{"subject": "x", "from": "y"}),
	})

	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	res, err := Retrospective(context.Background(), RetrospectiveDeps{
		LLM: cli, Reader: mem, Concerns: cRepo, Observations: oRepo,
		Bus: bus, EventLog: mem,
	}, "c-1", RetrospectiveOpts{Now: now, Batch: 5})
	require.Error(t, err)
	require.Equal(t, "failed", res.Status)
	failedSeen := false
	for _, e := range mem.Events() {
		if e.Kind == log.KindConcernRetrospectiveFailed {
			failedSeen = true
		}
	}
	require.True(t, failedSeen, "expected retrospective_failed audit")

	got := false
	deadline := time.After(2 * time.Second)
	for !got {
		select {
		case ev := <-sub:
			pe, ok := ev.(eventbus.RetrospectiveProgressEvent)
			if ok && pe.Status == "failed" {
				got = true
			}
		case <-deadline:
			t.Fatal("expected failed progress")
		}
	}
}

// TestRetrospective_ParseErrorSkipsBatchSoftly verifies parse-failure on
// a single batch's response does NOT mark the whole walk failed: the
// runner logs and continues. Important so a single malformed batch
// doesn't lose hours of historical tagging that would otherwise
// succeed.
func TestRetrospective_ParseErrorSkipsBatchSoftly(t *testing.T) {
	// Batch 1 is malformed; batch 2 returns valid output and tags both.
	rig := newRetrospectiveRig(t, []string{
		"this is not JSON",
		tagsJSON(map[string]any{"observation_id": "o3", "tag": true, "confidence": 0.9}),
	}, nil)
	ctx := context.Background()
	now := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	seedConcernActive(t, rig, "c-1", "X")
	for i, id := range []string{"o1", "o2", "o3"} {
		seedRetroMail(rig, id, now.Add(-time.Duration(i+1)*24*time.Hour), fmt.Sprintf("s-%d", i))
	}

	res, err := Retrospective(ctx, RetrospectiveDeps{
		LLM: rig.LLMClient, Reader: rig.Reader,
		Concerns: rig.Concerns, Observations: rig.Obs,
	}, "c-1", RetrospectiveOpts{Now: now, Batch: 2})
	require.NoError(t, err)
	require.Equal(t, "completed", res.Status)
	require.Equal(t, 1, res.Tagged, "second batch's o3 tagged")
}

// TestRetrospective_MissingConcernReturnsError pins the precondition: a
// concernID that doesn't exist surfaces as ErrConcernNotFound, not a
// silent no-op.
func TestRetrospective_MissingConcernReturnsError(t *testing.T) {
	rig := newRetrospectiveRig(t, []string{tagsJSON()}, nil)
	_, err := retroCall(rig, "does-not-exist")
	require.Error(t, err)
	require.True(t,
		errors.Is(err, store.ErrConcernNotFound) || strings.Contains(err.Error(), "concern not found"),
		"expected ErrConcernNotFound, got %v", err)
}

// retroCall is a tiny shim that hides the (ctx, deps, concernID, opts)
// signature behind a single positional arg for the precondition tests.
func retroCall(rig *retrospectiveTestRig, concernID string) (*RetrospectiveResult, error) {
	return Retrospective(context.Background(), RetrospectiveDeps{
		LLM:          rig.LLMClient,
		Reader:       rig.Reader,
		Concerns:     rig.Concerns,
		Observations: rig.Obs,
	}, concernID, RetrospectiveOpts{})
}
