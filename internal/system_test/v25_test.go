package system_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/eventbus"
	"github.com/zenocy/zeno-v2/internal/http/api"
	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/store"
	"github.com/zenocy/zeno-v2/internal/synth"
)

// concernsRig builds a minimal end-to-end stack for V2.5 concerns tests:
// real SQLite DB with synth.Migrate run (concerns + concern_observations
// tables in place), real Echo with ConcernsHandler + TodayStreamHandler,
// real eventbus.Bus, and a real httptest server.
//
// The richer V2.4 Harness in helpers.go isn't reused here because (a) it
// pulls in the full sensor + scheduler stack the concerns tests don't
// need, and (b) it doesn't run synth.Migrate — so the concerns tables
// would be missing. concernsRig is the V2.5-shaped sibling.
type concernsRig struct {
	t            *testing.T
	DB           *gorm.DB
	Store        log.Store
	Bus          *eventbus.Bus
	Server       *httptest.Server
	Concerns     *store.ConcernRepo
	Observations *store.ConcernObservationRepo
}

func newConcernsRig(t *testing.T) *concernsRig {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "zeno.db")
	db, st, err := log.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, synth.Migrate(db, true, false))

	logger := logrus.New()
	logger.Out = io.Discard

	bus := eventbus.New(logger.WithField("c", "eventbus"))
	concernRepo := &store.ConcernRepo{DB: db, Table: "concerns"}
	obsRepo := &store.ConcernObservationRepo{DB: db, Table: "concern_observations"}

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	(&api.ConcernsHandler{
		Concerns:     concernRepo,
		Observations: obsRepo,
		Bus:          bus,
		EventLog:     st,
		Now:          func() time.Time { return time.Date(2026, 5, 3, 9, 0, 0, 0, time.UTC) },
		Log:          logger.WithField("c", "concerns"),
	}).Register(e)
	(&api.TodayStreamHandler{Bus: bus, Logger: logger.WithField("c", "today-stream")}).Register(e)

	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return &concernsRig{
		t: t, DB: db, Store: st, Bus: bus, Server: srv,
		Concerns: concernRepo, Observations: obsRepo,
	}
}

// post sends a JSON body to the rig's server and returns the response code
// + decoded body (nil if the body isn't JSON or is empty).
func (r *concernsRig) post(path string, body any) (int, map[string]any) {
	r.t.Helper()
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		require.NoError(r.t, err)
		rdr = bytes.NewBuffer(buf)
	}
	req, err := http.NewRequest("POST", r.Server.URL+path, rdr)
	require.NoError(r.t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(r.t, err)
	defer func() { _ = resp.Body.Close() }()
	body2, _ := io.ReadAll(resp.Body)
	if len(body2) == 0 {
		return resp.StatusCode, nil
	}
	var got map[string]any
	if err := json.Unmarshal(body2, &got); err != nil {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, got
}

func (r *concernsRig) get(path string) (int, map[string]any) {
	r.t.Helper()
	resp, err := http.Get(r.Server.URL + path)
	require.NoError(r.t, err)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if len(body) == 0 {
		return resp.StatusCode, nil
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, got
}

// TestConcerns_E2E_CreateProposesAndSurfacesOverSSE walks the full path a
// recognition-pipeline-proposed concern takes through the V2.5 wire layer:
//
//  1. SSE subscriber connects to /api/today/stream.
//  2. POST /api/concerns with source=model lands a proposed concern.
//  3. The handler publishes a ConcernProposedEvent to the bus.
//  4. The SSE subscriber sees `event: concern.proposed` carrying the
//     concern's name + description + confidence.
//  5. POST /api/concerns/:id/state with state=active transitions it.
//  6. The SSE subscriber sees `event: concern.state_changed` with the
//     prior_state="proposed" + new_state="active".
//
// This is the system-level analog of the V2.4 SSE typed-events test —
// the wire shapes the React review surface (Phase 4) will rely on.
func TestConcerns_E2E_CreateProposesAndSurfacesOverSSE(t *testing.T) {
	rig := newConcernsRig(t)

	r, cleanup := openConcernsSSE(t, rig)
	defer cleanup()
	require.Eventually(t, func() bool { return rig.Bus.SubscriberCount() == 1 }, time.Second, 10*time.Millisecond,
		"SSE handler must subscribe before the publish")

	code, body := rig.post("/api/concerns", map[string]any{
		"name":        "Construction at the house",
		"description": "kitchen tile + contractor scheduling + architect drawings",
		"source":      "model",
	})
	require.Equal(t, http.StatusCreated, code, body)
	require.Equal(t, "proposed", body["state"], "model-source landed in proposed")
	id, _ := body["id"].(string)
	require.NotEmpty(t, id)

	deadline := time.Now().Add(2 * time.Second)
	ev, data, err := readSSEEvent(t, r, deadline)
	require.NoError(t, err)
	require.Equal(t, "concern.proposed", ev)
	require.Contains(t, data, `"concern_id":"`+id+`"`)
	require.Contains(t, data, `"name":"Construction at the house"`)
	require.Contains(t, data, `"source":"model"`)

	// Approve via /state — proposed → active.
	code, _ = rig.post("/api/concerns/"+id+"/state", map[string]any{"state": "active"})
	require.Equal(t, http.StatusOK, code)

	deadline = time.Now().Add(2 * time.Second)
	ev, data, err = readSSEEvent(t, r, deadline)
	require.NoError(t, err)
	require.Equal(t, "concern.state_changed", ev)
	require.Contains(t, data, `"prior_state":"proposed"`)
	require.Contains(t, data, `"new_state":"active"`)
}

// TestConcerns_E2E_TaggingBumpsLastActiveAndPublishes pins the
// observation-tagging happy path: POST /api/concerns/:id/tags inserts
// rows into concern_observations, bumps the concern's last_active_at,
// emits an audit event, and publishes a ConcernTaggedEvent that the SSE
// subscriber receives.
func TestConcerns_E2E_TaggingBumpsLastActiveAndPublishes(t *testing.T) {
	rig := newConcernsRig(t)

	// Seed an active concern via the API path (user-source auto-actives).
	code, body := rig.post("/api/concerns", map[string]any{
		"name": "Frankfurt trip", "description": "summer flight + meetings", "source": "user",
	})
	require.Equal(t, http.StatusCreated, code, body)
	id, _ := body["id"].(string)

	// User-create skips the bus publish (only model-source proposals
	// fire ConcernProposedEvent), so the SSE channel is empty until we
	// trigger the tag below.
	r, cleanup := openConcernsSSE(t, rig)
	defer cleanup()
	require.Eventually(t, func() bool { return rig.Bus.SubscriberCount() == 1 }, time.Second, 10*time.Millisecond)

	code, _ = rig.post("/api/concerns/"+id+"/tags", map[string]any{
		"event_ids": []string{"ev-a", "ev-b"},
		"source":    "user",
	})
	require.Equal(t, http.StatusOK, code)

	deadline := time.Now().Add(2 * time.Second)
	ev, data, err := readSSEEvent(t, r, deadline)
	require.NoError(t, err)
	require.Equal(t, "concern.tagged", ev)
	require.Contains(t, data, `"concern_id":"`+id+`"`)
	require.Contains(t, data, `"event_ids":["ev-a","ev-b"]`)

	// last_active_at was bumped by the handler — observable via GET.
	code, fetched := rig.get("/api/concerns/" + id)
	require.Equal(t, http.StatusOK, code)
	la, _ := fetched["last_active_at"].(string)
	bumped, err := time.Parse(time.RFC3339Nano, la)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 5, 3, 9, 0, 0, 0, time.UTC), bumped.UTC(),
		"handler's Now() pin propagates to last_active_at")
	require.EqualValues(t, 2, fetched["observation_count"])

	// Audit events landed in the durable log.
	require.Greater(t, countLogKind(rig.Store, "concern.created"), 0)
	require.Greater(t, countLogKind(rig.Store, "concern.tagged"), 0)
}

// TestConcerns_E2E_MergeMovesObservationsAcrossSSE pins that merging two
// active concerns publishes a state_changed event for the source, and the
// observation rows are reachable on the target afterwards.
func TestConcerns_E2E_MergeMovesObservationsAcrossSSE(t *testing.T) {
	rig := newConcernsRig(t)

	code, srcBody := rig.post("/api/concerns", map[string]any{
		"name": "Frankfurt", "description": "a", "source": "user",
	})
	require.Equal(t, http.StatusCreated, code)
	srcID, _ := srcBody["id"].(string)
	code, tgtBody := rig.post("/api/concerns", map[string]any{
		"name": "Travel — Q3 2026", "description": "b", "source": "user",
	})
	require.Equal(t, http.StatusCreated, code)
	tgtID, _ := tgtBody["id"].(string)

	// Tag two events on the source.
	code, _ = rig.post("/api/concerns/"+srcID+"/tags", map[string]any{
		"event_ids": []string{"e1", "e2"}, "source": "user",
	})
	require.Equal(t, http.StatusOK, code)

	// SSE opens after the tag publish, so the subscriber's first event
	// is the merge (publishes pre-subscribe never reach a stream that
	// didn't exist yet — see eventbus.Bus.Publish).
	r, cleanup := openConcernsSSE(t, rig)
	defer cleanup()
	require.Eventually(t, func() bool { return rig.Bus.SubscriberCount() == 1 }, time.Second, 10*time.Millisecond)

	code, _ = rig.post("/api/concerns/"+srcID+"/merge", map[string]any{"into_id": tgtID})
	require.Equal(t, http.StatusOK, code)

	deadline := time.Now().Add(2 * time.Second)
	ev, data, err := readSSEEvent(t, r, deadline)
	require.NoError(t, err)
	require.Equal(t, "concern.state_changed", ev)
	require.Contains(t, data, `"new_state":"merged"`)
	require.Contains(t, data, `"merged_into_id":"`+tgtID+`"`)

	// Source has zero visible observations; target has both.
	srcCount, _ := rig.Observations.CountByConcern(context.Background(), srcID)
	require.EqualValues(t, 0, srcCount)
	tgtCount, _ := rig.Observations.CountByConcern(context.Background(), tgtID)
	require.EqualValues(t, 2, tgtCount)
}

// TestConcerns_Migration_ProdAndReplayTablesExist exercises synth.Migrate
// with both prod and replay flags and verifies all V2.5 tables — including
// the *_replay variants — are reachable via the corresponding repos.
func TestConcerns_Migration_ProdAndReplayTablesExist(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "migrate.db")
	db, _, err := log.Open(dbPath)
	require.NoError(t, err)
	defer func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	}()
	require.NoError(t, synth.Migrate(db, true, true))

	cases := []struct {
		concern, obs string
	}{
		{"concerns", "concern_observations"},
		{"concerns_replay", "concern_observations_replay"},
	}
	ctx := context.Background()
	for _, c := range cases {
		// Read a no-row result; the query path validates the table exists
		// + columns match the model. Either failure surfaces as an error.
		cRepo := &store.ConcernRepo{DB: db, Table: c.concern}
		_, err := cRepo.ListByState(ctx, store.ConcernStateActive)
		require.NoErrorf(t, err, "table %q must be migrated and queryable", c.concern)

		oRepo := &store.ConcernObservationRepo{DB: db, Table: c.obs}
		_, err = oRepo.ListByConcern(ctx, "no-such-id", 0)
		require.NoErrorf(t, err, "table %q must be migrated and queryable", c.obs)
	}
}

// recordingDispatcher captures Dispatch calls so the approve E2E test
// can assert the retrospective path was triggered without spinning up
// a real synth.RetrospectiveDispatcher.
type sysRecordingDispatcher struct {
	calls []string
}

func (r *sysRecordingDispatcher) Dispatch(concernID string) {
	r.calls = append(r.calls, concernID)
}

// newConcernsRigWithDispatcher mirrors newConcernsRig but attaches a
// recording dispatcher to the ConcernsHandler so /approve fires can
// be verified end-to-end.
func newConcernsRigWithDispatcher(t *testing.T) (*concernsRig, *sysRecordingDispatcher) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "zeno.db")
	db, st, err := log.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, synth.Migrate(db, true, false))

	logger := logrus.New()
	logger.Out = io.Discard

	bus := eventbus.New(logger.WithField("c", "eventbus"))
	concernRepo := &store.ConcernRepo{DB: db, Table: "concerns"}
	obsRepo := &store.ConcernObservationRepo{DB: db, Table: "concern_observations"}
	disp := &sysRecordingDispatcher{}

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	(&api.ConcernsHandler{
		Concerns:     concernRepo,
		Observations: obsRepo,
		Bus:          bus,
		EventLog:     st,
		Now:          func() time.Time { return time.Date(2026, 5, 3, 9, 0, 0, 0, time.UTC) },
		Log:          logger.WithField("c", "concerns"),
		Dispatcher:   disp,
	}).Register(e)
	(&api.TodayStreamHandler{Bus: bus, Logger: logger.WithField("c", "today-stream")}).Register(e)

	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	return &concernsRig{
		t: t, DB: db, Store: st, Bus: bus, Server: srv,
		Concerns: concernRepo, Observations: obsRepo,
	}, disp
}

// TestConcerns_E2E_Approve_TransitionsAndDispatchesOverSSE walks the
// V2.5 Phase 2 happy path: a model-source proposed concern is created,
// approved via the /approve endpoint, the dispatcher fires once,
// the audit log captures concern.approved, and the SSE subscriber
// sees concern.state_changed.
func TestConcerns_E2E_Approve_TransitionsAndDispatchesOverSSE(t *testing.T) {
	rig, disp := newConcernsRigWithDispatcher(t)

	r, cleanup := openConcernsSSE(t, rig)
	defer cleanup()
	require.Eventually(t, func() bool { return rig.Bus.SubscriberCount() == 1 }, time.Second, 10*time.Millisecond)

	code, body := rig.post("/api/concerns", map[string]any{
		"name":        "Frankfurt trip",
		"description": "Mid-June review with Heim.",
		"source":      "model",
	})
	require.Equal(t, http.StatusCreated, code, body)
	id, _ := body["id"].(string)
	require.NotEmpty(t, id)

	// Drain the proposed event so the next read is the state_changed.
	deadline := time.Now().Add(2 * time.Second)
	ev, _, err := readSSEEvent(t, r, deadline)
	require.NoError(t, err)
	require.Equal(t, "concern.proposed", ev)

	code, body = rig.post("/api/concerns/"+id+"/approve", nil)
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, true, body["retrospective_dispatched"])
	require.Equal(t, []string{id}, disp.calls)

	deadline = time.Now().Add(2 * time.Second)
	ev, data, err := readSSEEvent(t, r, deadline)
	require.NoError(t, err)
	require.Equal(t, "concern.state_changed", ev)
	require.Contains(t, data, `"prior_state":"proposed"`)
	require.Contains(t, data, `"new_state":"active"`)

	require.Equal(t, 1, countLogKind(rig.Store, log.KindConcernApproved))
}

// TestConcerns_E2E_Dismiss_SoftDeletesAndAudits exercises the /dismiss
// path: a proposed concern is dismissed, the soft-delete row is
// hidden from GetByID (proving the denylist seed is in place), and
// the concern.dismissed audit kind lands.
func TestConcerns_E2E_Dismiss_SoftDeletesAndAudits(t *testing.T) {
	rig, _ := newConcernsRigWithDispatcher(t)

	code, body := rig.post("/api/concerns", map[string]any{
		"name":        "Newsletter follow-up",
		"description": "Probably noise.",
		"source":      "model",
	})
	require.Equal(t, http.StatusCreated, code, body)
	id, _ := body["id"].(string)

	code, body = rig.post("/api/concerns/"+id+"/dismiss", nil)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, true, body["dismissed"])

	// Soft-deleted: subsequent GET 404s.
	code, _ = rig.get("/api/concerns/" + id)
	require.Equal(t, http.StatusNotFound, code)

	require.Equal(t, 1, countLogKind(rig.Store, log.KindConcernDismissed))
}

// TestConcerns_E2E_Dismiss_RejectsNonProposed pins the precondition
// at the wire layer.
func TestConcerns_E2E_Dismiss_RejectsNonProposed(t *testing.T) {
	rig, _ := newConcernsRigWithDispatcher(t)
	code, body := rig.post("/api/concerns", map[string]any{
		"name":        "Active concern",
		"description": "User-declared, auto-actives.",
		"source":      "user",
	})
	require.Equal(t, http.StatusCreated, code, body)
	id, _ := body["id"].(string)
	code, _ = rig.post("/api/concerns/"+id+"/dismiss", nil)
	require.Equal(t, http.StatusConflict, code)
}

// openConcernsSSE attaches an SSE reader to the rig's server.
func openConcernsSSE(t *testing.T, r *concernsRig) (*bufio.Reader, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", r.Server.URL+"/api/today/stream", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	cleanup := func() { cancel(); _ = resp.Body.Close() }
	return bufio.NewReader(resp.Body), cleanup
}

// countLogKind returns the number of audit events of the given kind in
// the durable observation log. Used to assert the API path emitted the
// expected audit trail.
func countLogKind(s log.Store, kind string) int {
	events, err := s.ByKind(context.Background(), kind)
	if err != nil {
		return 0
	}
	return len(events)
}
