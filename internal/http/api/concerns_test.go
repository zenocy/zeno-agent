package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/eventbus"
	"github.com/zenocy/zeno-v2/internal/log/logtest"
	"github.com/zenocy/zeno-v2/internal/store"
)

func buildConcernsHandler(t *testing.T) (*echo.Echo, *store.ConcernRepo, *store.ConcernObservationRepo, *eventbus.Bus, *logtest.MemReader) {
	t.Helper()
	db := openHandlerTestDB(t)
	cRepo := &store.ConcernRepo{DB: db}
	oRepo := &store.ConcernObservationRepo{DB: db}
	bus := eventbus.New(logrus.NewEntry(quietLogger()))
	mem := logtest.NewMemReader()
	e := echo.New()
	(&ConcernsHandler{
		Concerns:     cRepo,
		Observations: oRepo,
		Bus:          bus,
		EventLog:     mem,
		Now:          func() time.Time { return time.Date(2026, 5, 3, 9, 0, 0, 0, time.UTC) },
		Log:          quietHandlerEntry(),
	}).Register(e)
	return e, cRepo, oRepo, bus, mem
}

func quietLogger() *logrus.Logger {
	l := logrus.New()
	l.SetOutput(quietWriter{})
	return l
}

type quietWriter struct{}

func (quietWriter) Write(p []byte) (int, error) { return len(p), nil }

// hasAuditKind reports whether the in-memory event log saw at least one
// event of the given kind. The MemReader exposes Events() but no Has-by-kind
// helper, so we provide one inline.
func hasAuditKind(mem *logtest.MemReader, kind string) bool {
	for _, e := range mem.Events() {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

func doJSON(t *testing.T, e *echo.Echo, method, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var rdr *bytes.Buffer
	if body != nil {
		buf, err := json.Marshal(body)
		require.NoError(t, err)
		rdr = bytes.NewBuffer(buf)
	} else {
		rdr = bytes.NewBuffer(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Body.Len() == 0 {
		return rec, nil
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		// not JSON; return nil
		return rec, nil
	}
	return rec, got
}

func TestConcernsHandler_CreateUserAutoActive(t *testing.T) {
	e, cRepo, _, _, mem := buildConcernsHandler(t)
	rec, body := doJSON(t, e, http.MethodPost, "/api/concerns", map[string]any{
		"name":        "Construction",
		"description": "kitchen + tile + contractor scheduling",
		"source":      "user",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, "active", body["state"], "user-source must auto-promote to active")
	require.Equal(t, "Construction", body["name"])
	id := body["id"].(string)

	// Persisted in repo
	row, err := cRepo.GetByID(context.Background(), id)
	require.NoError(t, err)
	require.NotNil(t, row)

	// Audit event written
	require.True(t, hasAuditKind(mem, "concern.created"), "audit event missing")
}

func TestConcernsHandler_CreateModelStaysProposed(t *testing.T) {
	e, _, _, bus, _ := buildConcernsHandler(t)
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	rec, body := doJSON(t, e, http.MethodPost, "/api/concerns", map[string]any{
		"name":        "Frankfurt trip",
		"description": "summer flight + meetings",
		"source":      "model",
	})
	require.Equal(t, http.StatusCreated, rec.Code)
	require.Equal(t, "proposed", body["state"], "model-source must stay proposed")

	// Bus saw a ConcernProposedEvent.
	select {
	case ev := <-sub:
		_, ok := ev.(eventbus.ConcernProposedEvent)
		require.True(t, ok, "expected ConcernProposedEvent, got %T", ev)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("no event published for model-source create")
	}
}

func TestConcernsHandler_CreateRejectsDuplicateName(t *testing.T) {
	e, _, _, _, _ := buildConcernsHandler(t)
	body := map[string]any{
		"name": "Construction", "description": "first", "source": "user",
	}
	rec, _ := doJSON(t, e, http.MethodPost, "/api/concerns", body)
	require.Equal(t, http.StatusCreated, rec.Code)
	rec2, got := doJSON(t, e, http.MethodPost, "/api/concerns", map[string]any{
		"name": "construction", "description": "different but same norm name", "source": "user",
	})
	require.Equal(t, http.StatusConflict, rec2.Code)
	require.Contains(t, got["error"], "already exists")
}

func TestConcernsHandler_CreateValidation(t *testing.T) {
	e, _, _, _, _ := buildConcernsHandler(t)
	bad := []map[string]any{
		{"name": "", "description": "x", "source": "user"},
		{"name": "x", "description": "", "source": "user"},
		{"name": "x", "description": "y", "source": "robot"},
	}
	for i, b := range bad {
		rec, _ := doJSON(t, e, http.MethodPost, "/api/concerns", b)
		require.Equal(t, http.StatusBadRequest, rec.Code, "case %d: body=%s", i, rec.Body.String())
	}
}

func TestConcernsHandler_ListFilters(t *testing.T) {
	e, cRepo, _, _, _ := buildConcernsHandler(t)
	ctx := context.Background()
	mustInsert(t, cRepo, ctx, "A", store.ConcernStateActive)
	mustInsert(t, cRepo, ctx, "B", store.ConcernStateActive)
	mustInsert(t, cRepo, ctx, "C", store.ConcernStateProposed)

	rec, body := doJSON(t, e, http.MethodGet, "/api/concerns", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	all := body["concerns"].([]any)
	require.Len(t, all, 3)

	rec, body = doJSON(t, e, http.MethodGet, "/api/concerns?state=active", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	active := body["concerns"].([]any)
	require.Len(t, active, 2)

	rec, _ = doJSON(t, e, http.MethodGet, "/api/concerns?state=in_progress", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, "PM-language state name rejected")
}

func TestConcernsHandler_GetAndPatch(t *testing.T) {
	e, cRepo, _, _, _ := buildConcernsHandler(t)
	ctx := context.Background()
	id := mustInsert(t, cRepo, ctx, "Hiring search", store.ConcernStateActive)

	rec, body := doJSON(t, e, http.MethodGet, "/api/concerns/"+id, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "Hiring search", body["name"])

	newName := "Engineering lead search"
	newDesc := "panel scheduling + candidate threads"
	rec, body = doJSON(t, e, http.MethodPatch, "/api/concerns/"+id, map[string]any{
		"name":        newName,
		"description": newDesc,
	})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, newName, body["name"])
	require.Equal(t, newDesc, body["description"])

	// 404 for unknown ID.
	rec, _ = doJSON(t, e, http.MethodGet, "/api/concerns/does-not-exist", nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestConcernsHandler_PatchRejectsNameCollision(t *testing.T) {
	e, cRepo, _, _, _ := buildConcernsHandler(t)
	ctx := context.Background()
	idA := mustInsert(t, cRepo, ctx, "Alpha", store.ConcernStateActive)
	mustInsert(t, cRepo, ctx, "Beta", store.ConcernStateActive)

	rec, _ := doJSON(t, e, http.MethodPatch, "/api/concerns/"+idA, map[string]any{
		"name": "  beta  ",
	})
	require.Equal(t, http.StatusConflict, rec.Code, "norm collision rejected")
}

func TestConcernsHandler_TransitionValidAndInvalid(t *testing.T) {
	e, cRepo, _, bus, _ := buildConcernsHandler(t)
	ctx := context.Background()
	id := mustInsert(t, cRepo, ctx, "Lifecycle", store.ConcernStateProposed)

	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	rec, body := doJSON(t, e, http.MethodPost, "/api/concerns/"+id+"/state", map[string]any{
		"state": "active",
	})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "active", body["state"])

	// Bus saw the state-changed event.
	select {
	case ev := <-sub:
		evChange, ok := ev.(eventbus.ConcernStateChangedEvent)
		require.True(t, ok, "expected ConcernStateChangedEvent, got %T", ev)
		require.Equal(t, "proposed", evChange.PriorState)
		require.Equal(t, "active", evChange.NewState)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("no event published for transition")
	}

	// proposed → paused was rejected; now active → in_progress (unknown) → 400.
	rec, _ = doJSON(t, e, http.MethodPost, "/api/concerns/"+id+"/state", map[string]any{
		"state": "in_progress",
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, "PM-language state rejected")

	// active → merged is rejected at the handler (use /merge).
	rec, _ = doJSON(t, e, http.MethodPost, "/api/concerns/"+id+"/state", map[string]any{
		"state": "merged",
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestConcernsHandler_MergeReassignsObservations(t *testing.T) {
	e, cRepo, oRepo, _, mem := buildConcernsHandler(t)
	ctx := context.Background()
	src := mustInsert(t, cRepo, ctx, "Frankfurt", store.ConcernStateActive)
	tgt := mustInsert(t, cRepo, ctx, "Travel Q3", store.ConcernStateActive)

	for _, ev := range []string{"e1", "e2"} {
		require.NoError(t, oRepo.Tag(ctx, store.ConcernObservation{
			ConcernID: src, EventID: ev, Source: store.ConcernTagSourceUser,
		}))
	}

	rec, body := doJSON(t, e, http.MethodPost, "/api/concerns/"+src+"/merge", map[string]any{
		"into_id": tgt,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "merged", body["state"])
	require.Equal(t, tgt, body["merged_into_id"])

	tgtCount, err := oRepo.CountByConcern(ctx, tgt)
	require.NoError(t, err)
	require.EqualValues(t, 2, tgtCount, "observations re-tagged to target")

	require.True(t, hasAuditKind(mem, "concern.merged"))
}

func TestConcernsHandler_SplitPartitionsAndEndsSource(t *testing.T) {
	e, cRepo, oRepo, _, mem := buildConcernsHandler(t)
	ctx := context.Background()
	src := mustInsert(t, cRepo, ctx, "Mixed", store.ConcernStateActive)

	for _, ev := range []string{"a1", "a2", "b1", "leftover"} {
		require.NoError(t, oRepo.Tag(ctx, store.ConcernObservation{
			ConcernID: src, EventID: ev, Source: store.ConcernTagSourceUser,
		}))
	}

	rec, body := doJSON(t, e, http.MethodPost, "/api/concerns/"+src+"/split", map[string]any{
		"splits": []map[string]any{
			{"name": "Side A", "description": "ay", "observation_ids": []string{"a1", "a2"}},
			{"name": "Side B", "description": "bee", "observation_ids": []string{"b1"}},
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	src2, _ := body["source"].(map[string]any)
	require.Equal(t, "ended", src2["state"], "source ended after split")
	news, _ := body["new"].([]any)
	require.Len(t, news, 2)

	// Source still has only "leftover" tagged.
	srcRows, err := oRepo.ListByConcern(ctx, src, 100)
	require.NoError(t, err)
	require.Len(t, srcRows, 1)
	require.Equal(t, "leftover", srcRows[0].EventID)

	require.True(t, hasAuditKind(mem, "concern.split"))
}

func TestConcernsHandler_SplitRejectsAmbiguousAssignment(t *testing.T) {
	e, cRepo, oRepo, _, _ := buildConcernsHandler(t)
	ctx := context.Background()
	src := mustInsert(t, cRepo, ctx, "Mixed", store.ConcernStateActive)
	require.NoError(t, oRepo.Tag(ctx, store.ConcernObservation{
		ConcernID: src, EventID: "e1", Source: store.ConcernTagSourceUser,
	}))

	rec, _ := doJSON(t, e, http.MethodPost, "/api/concerns/"+src+"/split", map[string]any{
		"splits": []map[string]any{
			{"name": "A", "description": "a", "observation_ids": []string{"e1"}},
			{"name": "B", "description": "b", "observation_ids": []string{"e1"}},
		},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestConcernsHandler_TagsAndUntag(t *testing.T) {
	e, cRepo, oRepo, bus, mem := buildConcernsHandler(t)
	ctx := context.Background()
	id := mustInsert(t, cRepo, ctx, "Tag test", store.ConcernStateActive)

	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	rec, body := doJSON(t, e, http.MethodPost, "/api/concerns/"+id+"/tags", map[string]any{
		"event_ids": []string{"ev1", "ev2", "ev2"},
		"source":    "user",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.EqualValues(t, 3, body["tagged"], "duplicate event_id within batch deduped at DB level via PK")

	// Idempotent re-tag.
	rec, body = doJSON(t, e, http.MethodPost, "/api/concerns/"+id+"/tags", map[string]any{
		"event_ids": []string{"ev1"},
		"source":    "user",
	})
	require.Equal(t, http.StatusOK, rec.Code)

	count, err := oRepo.CountByConcern(ctx, id)
	require.NoError(t, err)
	require.EqualValues(t, 2, count, "re-tag must not multiply rows")

	require.True(t, hasAuditKind(mem, "concern.tagged"))

	// last_active_at bumped.
	row, _ := cRepo.GetByID(ctx, id)
	require.WithinDuration(t, time.Date(2026, 5, 3, 9, 0, 0, 0, time.UTC), row.LastActiveAt, time.Second)

	// Drain bus events.
	for i := 0; i < 2; i++ {
		select {
		case <-sub:
		case <-time.After(300 * time.Millisecond):
			t.Fatalf("expected tag event %d", i)
		}
	}

	// Untag.
	url := "/api/concerns/" + id + "/tags/ev1"
	rec, _ = doJSON(t, e, http.MethodDelete, url, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	count, _ = oRepo.CountByConcern(ctx, id)
	require.EqualValues(t, 1, count)
	require.True(t, hasAuditKind(mem, "concern.untagged"))
}

func TestConcernsHandler_ListObservations(t *testing.T) {
	e, cRepo, oRepo, _, _ := buildConcernsHandler(t)
	ctx := context.Background()
	id := mustInsert(t, cRepo, ctx, "obs-test", store.ConcernStateActive)
	for _, ev := range []string{"a", "b", "c"} {
		require.NoError(t, oRepo.Tag(ctx, store.ConcernObservation{
			ConcernID: id, EventID: ev, Source: store.ConcernTagSourceModel,
		}))
	}
	rec, body := doJSON(t, e, http.MethodGet, "/api/concerns/"+id+"/observations", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	rows := body["observations"].([]any)
	require.Len(t, rows, 3)
}

// recordingApiDispatcher captures Dispatch calls so the approve test
// can verify the retrospective path was triggered without spinning up
// a real synth.RetrospectiveDispatcher.
type recordingApiDispatcher struct {
	calls []string
}

func (r *recordingApiDispatcher) Dispatch(concernID string) {
	r.calls = append(r.calls, concernID)
}

func buildConcernsHandlerWithDispatcher(t *testing.T) (*echo.Echo, *store.ConcernRepo, *recordingApiDispatcher, *logtest.MemReader, *eventbus.Bus) {
	t.Helper()
	db := openHandlerTestDB(t)
	cRepo := &store.ConcernRepo{DB: db}
	oRepo := &store.ConcernObservationRepo{DB: db}
	bus := eventbus.New(logrus.NewEntry(quietLogger()))
	mem := logtest.NewMemReader()
	disp := &recordingApiDispatcher{}
	e := echo.New()
	(&ConcernsHandler{
		Concerns:     cRepo,
		Observations: oRepo,
		Bus:          bus,
		EventLog:     mem,
		Now:          func() time.Time { return time.Date(2026, 5, 3, 9, 0, 0, 0, time.UTC) },
		Log:          quietHandlerEntry(),
		Dispatcher:   disp,
	}).Register(e)
	return e, cRepo, disp, mem, bus
}

// TestConcernsHandler_Approve_TransitionsAndDispatches pins the happy
// path: a proposed concern transitions to active, the approved audit
// kind lands, the dispatcher is called once, the response carries
// retrospective_dispatched=true.
func TestConcernsHandler_Approve_TransitionsAndDispatches(t *testing.T) {
	e, cRepo, disp, mem, bus := buildConcernsHandlerWithDispatcher(t)
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)
	ctx := context.Background()

	id := uuid.New().String()
	require.NoError(t, cRepo.Insert(ctx, store.Concern{
		ID: id, Name: "Construction", NormName: "construction",
		Description: "x", State: store.ConcernStateProposed,
		Source: store.ConcernSourceModel, Confidence: 0.85,
		LastActiveAt: time.Now(), CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}))

	rec, body := doJSON(t, e, http.MethodPost, "/api/concerns/"+id+"/approve", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, true, body["retrospective_dispatched"])
	require.Equal(t, []string{id}, disp.calls)

	row, _ := cRepo.GetByID(ctx, id)
	require.NotNil(t, row)
	require.Equal(t, store.ConcernStateActive, row.State)

	require.True(t, hasAuditKind(mem, "concern.approved"))

	// Bus delivered concern.state_changed.
	got := false
	for !got {
		select {
		case ev := <-sub:
			if pe, ok := ev.(eventbus.ConcernStateChangedEvent); ok && pe.ConcernID == id {
				require.Equal(t, store.ConcernStateProposed, pe.PriorState)
				require.Equal(t, store.ConcernStateActive, pe.NewState)
				got = true
			}
		case <-time.After(time.Second):
			t.Fatal("expected ConcernStateChangedEvent")
		}
	}
}

// TestConcernsHandler_Approve_RejectsNonProposed pins the precondition.
// Approving an already-active concern returns 409 — the contract is
// "only proposed can be approved", a deliberate hard rule that keeps
// the audit trail clean.
func TestConcernsHandler_Approve_RejectsNonProposed(t *testing.T) {
	e, cRepo, _, _, _ := buildConcernsHandlerWithDispatcher(t)
	ctx := context.Background()
	id := mustInsert(t, cRepo, ctx, "Active concern", store.ConcernStateActive)
	rec, body := doJSON(t, e, http.MethodPost, "/api/concerns/"+id+"/approve", nil)
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Equal(t, "active", body["state"])
}

func TestConcernsHandler_Approve_NotFound(t *testing.T) {
	e, _, _, _, _ := buildConcernsHandlerWithDispatcher(t)
	rec, _ := doJSON(t, e, http.MethodPost, "/api/concerns/does-not-exist/approve", nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestConcernsHandler_Approve_NoDispatcher_StillTransitions pins the
// degraded mode: dispatcher nil → transition still happens, audit lands,
// response carries retrospective_dispatched=false. Important for the
// boot path where retrospective wiring fails but the user can still
// approve concerns.
func TestConcernsHandler_Approve_NoDispatcher_StillTransitions(t *testing.T) {
	// buildConcernsHandler builds without a Dispatcher.
	e, cRepo, _, _, mem := buildConcernsHandler(t)
	ctx := context.Background()
	id := mustInsert(t, cRepo, ctx, "Proposed", store.ConcernStateProposed)
	rec, body := doJSON(t, e, http.MethodPost, "/api/concerns/"+id+"/approve", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, false, body["retrospective_dispatched"])
	require.True(t, hasAuditKind(mem, "concern.approved"))
}

// TestConcernsHandler_Dismiss_SoftDeletesAndAuditsKind pins the happy
// path of dismiss. Soft-delete preserves the row for the 90-day
// denylist; the row remains queryable with Unscoped (verified via the
// existing recognition denylist tests).
func TestConcernsHandler_Dismiss_SoftDeletesAndAuditsKind(t *testing.T) {
	e, cRepo, _, mem, _ := buildConcernsHandlerWithDispatcher(t)
	ctx := context.Background()
	id := mustInsert(t, cRepo, ctx, "Noise", store.ConcernStateProposed)

	rec, body := doJSON(t, e, http.MethodPost, "/api/concerns/"+id+"/dismiss", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, true, body["dismissed"])

	// Soft-deleted: GetByID returns nil (Unscoped would return it).
	row, _ := cRepo.GetByID(ctx, id)
	require.Nil(t, row)

	require.True(t, hasAuditKind(mem, "concern.dismissed"))
}

func TestConcernsHandler_Dismiss_RejectsNonProposed(t *testing.T) {
	e, cRepo, _, _, _ := buildConcernsHandlerWithDispatcher(t)
	ctx := context.Background()
	id := mustInsert(t, cRepo, ctx, "Active", store.ConcernStateActive)
	rec, _ := doJSON(t, e, http.MethodPost, "/api/concerns/"+id+"/dismiss", nil)
	require.Equal(t, http.StatusConflict, rec.Code)
}

func TestConcernsHandler_Dismiss_NotFound(t *testing.T) {
	e, _, _, _, _ := buildConcernsHandlerWithDispatcher(t)
	rec, _ := doJSON(t, e, http.MethodPost, "/api/concerns/missing/dismiss", nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// mustInsert is a test-only sugar that places a row directly via the repo,
// bypassing the validation/audit path so test setup stays terse.
func mustInsert(t *testing.T, repo *store.ConcernRepo, ctx context.Context, name, state string) string {
	t.Helper()
	id := uuid.New().String()
	require.NoError(t, repo.Insert(ctx, store.Concern{
		ID:           id,
		Name:         name,
		NormName:     store.NormalizeConcernName(name),
		Description:  fmt.Sprintf("%s — placeholder", name),
		State:        state,
		Source:       store.ConcernSourceUser,
		Confidence:   1.0,
		LastActiveAt: time.Now(),
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}))
	return id
}

// V2.5.0 Phase 5: ready_to_retire derivation in the DTO. Derived from
// LastActiveAt against AutoRetireDays. Only `active` qualifies.
func TestConcernsHandler_ReadyToRetire_Derivation(t *testing.T) {
	db := openHandlerTestDB(t)
	cRepo := &store.ConcernRepo{DB: db}
	oRepo := &store.ConcernObservationRepo{DB: db}
	bus := eventbus.New(logrus.NewEntry(quietLogger()))
	mem := logtest.NewMemReader()
	now := time.Date(2026, 5, 3, 9, 0, 0, 0, time.UTC)
	e := echo.New()
	(&ConcernsHandler{
		Concerns:       cRepo,
		Observations:   oRepo,
		Bus:            bus,
		EventLog:       mem,
		Now:            func() time.Time { return now },
		Log:            quietHandlerEntry(),
		AutoRetireDays: 90,
	}).Register(e)

	ctx := context.Background()
	insert := func(name, state string, lastActive time.Time) string {
		id := uuid.New().String()
		require.NoError(t, cRepo.Insert(ctx, store.Concern{
			ID: id, Name: name,
			NormName:     store.NormalizeConcernName(name),
			Description:  "test",
			State:        state,
			Source:       store.ConcernSourceUser,
			Confidence:   1.0,
			LastActiveAt: lastActive,
			CreatedAt:    now, UpdatedAt: now,
		}))
		return id
	}

	below := insert("Recently Idle", store.ConcernStateActive, now.Add(-89*24*time.Hour))
	at := insert("At Threshold", store.ConcernStateActive, now.Add(-90*24*time.Hour))
	past := insert("Long Idle", store.ConcernStateActive, now.Add(-100*24*time.Hour))
	paused := insert("Paused Ancient", store.ConcernStatePaused, now.Add(-200*24*time.Hour))

	rec, body := doJSON(t, e, http.MethodGet, "/api/concerns", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	concerns, ok := body["concerns"].([]any)
	require.True(t, ok)

	flag := map[string]bool{}
	for _, raw := range concerns {
		row := raw.(map[string]any)
		v, _ := row["ready_to_retire"].(bool)
		flag[row["id"].(string)] = v
	}
	require.False(t, flag[below], "89d below threshold")
	require.True(t, flag[at], "exactly 90d at threshold")
	require.True(t, flag[past], "100d past threshold")
	require.False(t, flag[paused], "paused never qualifies")
}
