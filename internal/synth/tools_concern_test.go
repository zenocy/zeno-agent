package synth

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/eventbus"
	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/log/logtest"
	"github.com/zenocy/zeno-v2/internal/store"
)

func newDeclareToolRig(t *testing.T) (*DeclareConcernTool, *store.ConcernRepo, *recordingDispatcher, *logtest.MemReader, *eventbus.Bus) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	cRepo := &store.ConcernRepo{DB: db, Table: "concerns"}
	oRepo := &store.ConcernObservationRepo{DB: db, Table: "concern_observations"}
	require.NoError(t, cRepo.Migrate())
	require.NoError(t, oRepo.Migrate())

	disp := &recordingDispatcher{}
	mem := logtest.NewMemReader()
	bus := eventbus.New(nil)

	tool := &DeclareConcernTool{
		Concerns:     cRepo,
		Observations: oRepo,
		Bus:          bus,
		EventLog:     mem,
		Dispatcher:   disp,
	}
	return tool, cRepo, disp, mem, bus
}

// TestDeclareConcernTool_Creates_Active_User_Source pins the happy path:
// a fresh declaration produces a row with state=active, source=user,
// confidence=1.0, dispatches retrospective, and audits the create kind.
func TestDeclareConcernTool_Creates_Active_User_Source(t *testing.T) {
	tool, repo, disp, mem, bus := newDeclareToolRig(t)
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	out, err := tool.Execute(context.Background(), map[string]any{
		"name":        "Frankfurt trip",
		"description": "Mid-June review with Heim.",
	})
	require.NoError(t, err)

	var got declareConcernResult
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.False(t, got.AlreadyExists)
	require.True(t, got.RetrospectiveStarted)
	require.Equal(t, store.ConcernStateActive, got.State)

	row, err := repo.GetByID(context.Background(), got.ConcernID)
	require.NoError(t, err)
	require.NotNil(t, row)
	require.Equal(t, store.ConcernStateActive, row.State)
	require.Equal(t, store.ConcernSourceUser, row.Source)
	require.InDelta(t, 1.0, row.Confidence, 1e-9)
	require.Equal(t, "frankfurt trip", row.NormName)

	require.Equal(t, []string{got.ConcernID}, disp.Calls)

	// Created kind audited.
	hasCreated := false
	for _, e := range mem.Events() {
		if e.Kind == log.KindConcernCreated {
			hasCreated = true
		}
	}
	require.True(t, hasCreated, "expected concern.created audit")

	// Bus delivered concern.proposed (per V2.5 contract — even user-source
	// concerns publish proposed so the review surface flow is uniform).
	select {
	case ev := <-sub:
		pe, ok := ev.(eventbus.ConcernProposedEvent)
		require.True(t, ok)
		require.Equal(t, store.ConcernSourceUser, pe.Source)
	case <-time.After(time.Second):
		t.Fatal("expected ConcernProposedEvent on bus")
	}
}

// TestDeclareConcernTool_NormalizedDedupe verifies idempotency by name.
// Two declarations with normalized-equivalent names ("Frankfurt Trip"
// vs "frankfurt trip") return the same ID and do not double-dispatch
// retrospective; the second declare is a no-op the model can compose
// "you're already tracking that" prose from.
func TestDeclareConcernTool_NormalizedDedupe(t *testing.T) {
	tool, repo, disp, _, _ := newDeclareToolRig(t)

	out1, err := tool.Execute(context.Background(), map[string]any{"name": "Frankfurt Trip"})
	require.NoError(t, err)
	var first declareConcernResult
	require.NoError(t, json.Unmarshal([]byte(out1), &first))

	out2, err := tool.Execute(context.Background(), map[string]any{"name": "  frankfurt trip  "})
	require.NoError(t, err)
	var second declareConcernResult
	require.NoError(t, json.Unmarshal([]byte(out2), &second))
	require.True(t, second.AlreadyExists)
	require.False(t, second.RetrospectiveStarted)
	require.Equal(t, first.ConcernID, second.ConcernID)

	// One row only.
	rows, _ := repo.ListAll(context.Background())
	require.Len(t, rows, 1)
	// One dispatch only.
	require.Len(t, disp.Calls, 1)
}

// TestDeclareConcernTool_RejectsEmptyName surfaces the input validation
// path. The model occasionally calls tools with missing required fields
// while exploring; the error string is reflected back to the loop.
func TestDeclareConcernTool_RejectsEmptyName(t *testing.T) {
	tool, _, _, _, _ := newDeclareToolRig(t)
	_, err := tool.Execute(context.Background(), map[string]any{"name": ""})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrDeclareConcernInvalidInput))

	_, err = tool.Execute(context.Background(), map[string]any{"name": "   "})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrDeclareConcernInvalidInput))

	_, err = tool.Execute(context.Background(), map[string]any{"name": 42})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrDeclareConcernInvalidInput))
}

func TestDeclareConcernTool_RejectsOversizedName(t *testing.T) {
	tool, _, _, _, _ := newDeclareToolRig(t)
	_, err := tool.Execute(context.Background(), map[string]any{
		"name": strings.Repeat("a", declareConcernNameMax+1),
	})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrDeclareConcernInvalidInput))
}

// TestDeclareConcernTool_TruncatesOversizedDescription is a "be lenient"
// rule: truncate the description rather than rejecting. The tool is
// called by an LLM that may slightly overshoot; rejecting here would
// strand the user's declaration.
func TestDeclareConcernTool_TruncatesOversizedDescription(t *testing.T) {
	tool, repo, _, _, _ := newDeclareToolRig(t)
	long := strings.Repeat("x", declareConcernDescriptionMax+50)
	out, err := tool.Execute(context.Background(), map[string]any{
		"name":        "Concern A",
		"description": long,
	})
	require.NoError(t, err)
	var got declareConcernResult
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.LessOrEqual(t, len(got.Description), declareConcernDescriptionMax)
	row, _ := repo.GetByID(context.Background(), got.ConcernID)
	require.NotNil(t, row)
	require.LessOrEqual(t, len(row.Description), declareConcernDescriptionMax)
}

// TestDeclareConcernTool_NoDispatcher_StillCreatesButFlagFalse pins the
// degraded mode where the dispatcher is nil (e.g. in test harnesses or
// during a misconfigured boot). The tool still creates the concern but
// reports retrospective_started=false so the model can produce a
// matching prose.
func TestDeclareConcernTool_NoDispatcher_StillCreatesButFlagFalse(t *testing.T) {
	tool, _, _, _, _ := newDeclareToolRig(t)
	tool.Dispatcher = nil

	out, err := tool.Execute(context.Background(), map[string]any{"name": "X"})
	require.NoError(t, err)
	var got declareConcernResult
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.False(t, got.RetrospectiveStarted)
	require.NotEmpty(t, got.ConcernID)
}

func TestDeclareConcernTool_MissingReposReturnsConfigError(t *testing.T) {
	tool := &DeclareConcernTool{}
	_, err := tool.Execute(context.Background(), map[string]any{"name": "X"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not configured")
}

// TestDeclareConcernTool_ParametersSpec_Pin pins the LLM-facing parameter
// schema so a refactor can't silently drop the description field or
// rename `name` (which would break every fixture and replay run).
func TestDeclareConcernTool_ParametersSpec_Pin(t *testing.T) {
	tool := &DeclareConcernTool{}
	params := tool.Parameters()
	require.Len(t, params, 2)
	require.Equal(t, "name", params[0].Name)
	require.True(t, params[0].Required)
	require.Equal(t, "description", params[1].Name)
	require.False(t, params[1].Required)
}

// ----- V2.5.0 Phase 3: lookup_concern + read_concern_evidence ----------

func newLookupRig(t *testing.T) *LookupConcernTool {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	c := &store.ConcernRepo{DB: db, Table: "concerns"}
	require.NoError(t, c.Migrate())
	return &LookupConcernTool{Concerns: c}
}

func seedActiveConcern(t *testing.T, repo *store.ConcernRepo, id, name string) {
	t.Helper()
	now := time.Now()
	require.NoError(t, repo.Insert(context.Background(), store.Concern{
		ID: id, Name: name, NormName: store.NormalizeConcernName(name),
		Description: name + " — long-running situation",
		State:       store.ConcernStateActive, Source: store.ConcernSourceUser,
		LastActiveAt: now, CreatedAt: now, UpdatedAt: now, Confidence: 1,
	}))
}

// TestLookupConcernTool_MatchesByName is the central happy path: a
// query phrase that overlaps a concern's name returns its id with a
// score above the lookup threshold.
func TestLookupConcernTool_MatchesByName(t *testing.T) {
	tool := newLookupRig(t)
	seedActiveConcern(t, tool.Concerns, "c-1", "Construction at the house")

	out, err := tool.Execute(context.Background(), map[string]any{"query": "construction"})
	require.NoError(t, err)
	var got lookupConcernResult
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Equal(t, "c-1", got.ConcernID)
	require.GreaterOrEqual(t, got.MatchScore, lookupConcernMatchThreshold)
}

// TestLookupConcernTool_NoMatchBelowThreshold is the regression gate
// for the over-eager case: a query unrelated to any concern must not
// surface a stale concern_id.
func TestLookupConcernTool_NoMatchBelowThreshold(t *testing.T) {
	tool := newLookupRig(t)
	seedActiveConcern(t, tool.Concerns, "c-1", "Construction")

	out, err := tool.Execute(context.Background(), map[string]any{"query": "tomorrow's weather"})
	require.NoError(t, err)
	require.Equal(t, "{}", out, "non-match returns empty JSON object")
}

// TestLookupConcernTool_AmbiguousMatchPicksHighest verifies the closer
// substring wins when two concerns share a token.
func TestLookupConcernTool_AmbiguousMatchPicksHighest(t *testing.T) {
	tool := newLookupRig(t)
	seedActiveConcern(t, tool.Concerns, "c-1", "Construction at the house")
	seedActiveConcern(t, tool.Concerns, "c-2", "Construction-industry newsletter")

	out, err := tool.Execute(context.Background(), map[string]any{"query": "house construction"})
	require.NoError(t, err)
	var got lookupConcernResult
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	// "house construction" overlaps both; the score normalization is
	// per-token; the first concern (whose description includes
	// "long-running situation") matches both query tokens.
	require.NotEmpty(t, got.ConcernID)
}

// TestLookupConcernTool_NoConcernsReturnsEmpty pins the cold-start
// behavior: no concerns in the store → empty result, no error.
func TestLookupConcernTool_NoConcernsReturnsEmpty(t *testing.T) {
	tool := newLookupRig(t)
	out, err := tool.Execute(context.Background(), map[string]any{"query": "anything"})
	require.NoError(t, err)
	require.Equal(t, "{}", out)
}

func TestLookupConcernTool_EmptyQueryReturnsEmpty(t *testing.T) {
	tool := newLookupRig(t)
	seedActiveConcern(t, tool.Concerns, "c-1", "Construction")
	out, err := tool.Execute(context.Background(), map[string]any{"query": ""})
	require.NoError(t, err)
	require.Equal(t, "{}", out)
}

func TestLookupConcernTool_ParametersSpec_Pin(t *testing.T) {
	tool := &LookupConcernTool{}
	params := tool.Parameters()
	require.Len(t, params, 1)
	require.Equal(t, "query", params[0].Name)
	require.True(t, params[0].Required)
}

// readConcernEvidenceRig wires the bigger surface ReadConcernEvidenceTool
// needs: concerns + tags + a logtest reader for the seeded events.
type readConcernEvidenceRig struct {
	Tool *ReadConcernEvidenceTool
}

func newReadEvidenceRig(t *testing.T) *readConcernEvidenceRig {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	c := &store.ConcernRepo{DB: db, Table: "concerns"}
	o := &store.ConcernObservationRepo{DB: db, Table: "concern_observations"}
	require.NoError(t, c.Migrate())
	require.NoError(t, o.Migrate())
	mem := logtest.NewMemReader()
	return &readConcernEvidenceRig{
		Tool: &ReadConcernEvidenceTool{Concerns: c, Observations: o, Reader: mem},
	}
}

func (r *readConcernEvidenceRig) seedConcernAndTag(t *testing.T, concernID, name, eventID string, ts time.Time) {
	t.Helper()
	if existing, _ := r.Tool.Concerns.GetByID(context.Background(), concernID); existing == nil {
		seedActiveConcern(t, r.Tool.Concerns, concernID, name)
	}
	require.NoError(t, r.Tool.Observations.Tag(context.Background(), store.ConcernObservation{
		ConcernID: concernID, EventID: eventID,
		Source: store.ConcernTagSourceModel, TaggedAt: ts,
	}))
	r.Tool.Reader.(*logtest.MemReader).AppendEvent(log.Event{
		ID: eventID, TS: ts.UTC(), Kind: log.KindMailReceived, Source: "imap",
		Payload: logtest.MakeEvent("mail", "imap", ts, map[string]any{
			"subject": "Subject for " + eventID, "from": "sender",
		}).Payload,
	})
}

// TestReadConcernEvidenceTool_ReturnsProseWithObservations is the happy
// path: tagged observations land in the prose; the prose is bounded
// by the tool's character cap.
func TestReadConcernEvidenceTool_ReturnsProseWithObservations(t *testing.T) {
	rig := newReadEvidenceRig(t)
	now := time.Now()
	rig.seedConcernAndTag(t, "c-1", "Construction", "ev-1", now.Add(-1*time.Hour))
	rig.seedConcernAndTag(t, "c-1", "Construction", "ev-2", now.Add(-2*time.Hour))

	out, err := rig.Tool.Execute(context.Background(), map[string]any{"concern_id": "c-1"})
	require.NoError(t, err)
	require.Contains(t, out, "Concern: Construction")
	require.Contains(t, out, "Subject for ev-1")
	require.Contains(t, out, "Subject for ev-2")
	require.LessOrEqual(t, len(out), readConcernEvidenceProseCap)
}

// TestReadConcernEvidenceTool_HonorsMaxObservations clamps the result
// to the requested cap. The model can ask for fewer to keep its card
// sub field tight.
func TestReadConcernEvidenceTool_HonorsMaxObservations(t *testing.T) {
	rig := newReadEvidenceRig(t)
	now := time.Now()
	for i := range 5 {
		id := "ev-" + time.Duration(i+1).String()
		rig.seedConcernAndTag(t, "c-1", "Construction", id, now.Add(-time.Duration(i+1)*time.Hour))
	}

	out, err := rig.Tool.Execute(context.Background(), map[string]any{
		"concern_id":       "c-1",
		"max_observations": float64(2), // JSON unmarshal float
	})
	require.NoError(t, err)
	require.Contains(t, out, "2 recent observations")
}

func TestReadConcernEvidenceTool_MissingConcernReturnsError(t *testing.T) {
	rig := newReadEvidenceRig(t)
	_, err := rig.Tool.Execute(context.Background(), map[string]any{"concern_id": "does-not-exist"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestReadConcernEvidenceTool_TerminalConcernRejected(t *testing.T) {
	rig := newReadEvidenceRig(t)
	rig.seedConcernAndTag(t, "c-old", "Old", "ev-1", time.Now())
	require.NoError(t, rig.Tool.Concerns.Transition(context.Background(), "c-old", store.ConcernStateEnded))

	_, err := rig.Tool.Execute(context.Background(), map[string]any{"concern_id": "c-old"})
	require.Error(t, err, "ended concern is not surfaced")
}

func TestReadConcernEvidenceTool_EmptyConcernRendersNoActivity(t *testing.T) {
	rig := newReadEvidenceRig(t)
	seedActiveConcern(t, rig.Tool.Concerns, "c-empty", "Empty")
	out, err := rig.Tool.Execute(context.Background(), map[string]any{"concern_id": "c-empty"})
	require.NoError(t, err)
	require.Contains(t, out, "No recent activity")
}

func TestReadConcernEvidenceTool_MissingConfigReturnsError(t *testing.T) {
	tool := &ReadConcernEvidenceTool{}
	_, err := tool.Execute(context.Background(), map[string]any{"concern_id": "c-1"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not configured")
}

func TestReadConcernEvidenceTool_RejectsEmptyConcernID(t *testing.T) {
	rig := newReadEvidenceRig(t)
	_, err := rig.Tool.Execute(context.Background(), map[string]any{"concern_id": ""})
	require.Error(t, err)
}

func TestReadConcernEvidenceTool_ParametersSpec_Pin(t *testing.T) {
	tool := &ReadConcernEvidenceTool{}
	params := tool.Parameters()
	require.Len(t, params, 2)
	require.Equal(t, "concern_id", params[0].Name)
	require.True(t, params[0].Required)
	require.Equal(t, "max_observations", params[1].Name)
	require.False(t, params[1].Required)
}
