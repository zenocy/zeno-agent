package llm

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRecordToolWithRefs_PopulatesRefsField pins the simple case: a
// tool step recorded with refs lands them in the step's Refs field.
func TestRecordToolWithRefs_PopulatesRefsField(t *testing.T) {
	a := NewAccumulator()
	step := a.RecordToolWithRefs("READ", "thing", "", []string{"e1", "e2"})
	require.Equal(t, []string{"e1", "e2"}, step.Refs)
	tr := a.Build("ok")
	require.Len(t, tr.Steps, 1)
	require.Equal(t, []string{"e1", "e2"}, tr.Steps[0].Refs)
}

// TestTraceStep_RefsOmittedWhenEmpty is the V2.4 byte-equality gate:
// a step recorded via the V2.4 RecordTool path (no refs) must NOT
// include a `refs` field in its JSON serialization. The omitempty tag
// is the load-bearing affordance — without it, every existing fixture
// would diff.
func TestTraceStep_RefsOmittedWhenEmpty(t *testing.T) {
	a := NewAccumulator()
	a.RecordTool("READ", "thing", "")

	tr := a.Build("ok")
	b, err := json.Marshal(tr.Steps[0])
	require.NoError(t, err)
	require.NotContains(t, string(b), `"refs"`,
		"V2.4 RecordTool path must serialize without a refs field")
}

func TestTraceStep_RefsSerializedWhenPresent(t *testing.T) {
	a := NewAccumulator()
	a.RecordToolWithRefs("READ", "thing", "", []string{"e1"})
	tr := a.Build("ok")
	b, err := json.Marshal(tr.Steps[0])
	require.NoError(t, err)
	require.Contains(t, string(b), `"refs":["e1"]`)
}

// TestRefsCollector_RoundTrip pins the context-bound collector path:
// a tool that calls AppendRefsToContext during its Execute publishes
// refs the loop can read after the call returns. This is the carrier
// between Phase 3 tools and the trace step.
func TestRefsCollector_RoundTrip(t *testing.T) {
	ctx := WithRefsCollector(context.Background())
	AppendRefsToContext(ctx, "e1", "e2")
	AppendRefsToContext(ctx, "e3")
	got := RefsFromContext(ctx)
	require.Equal(t, []string{"e1", "e2", "e3"}, got)
}

// TestRefsFromContext_NoCollectorReturnsNil pins the silent-fail path:
// a tool running outside the loop (or in a V2.4-era code path that
// doesn't wrap with WithRefsCollector) sees nil, no panic.
func TestRefsFromContext_NoCollectorReturnsNil(t *testing.T) {
	got := RefsFromContext(context.Background())
	require.Nil(t, got)
}

// TestAppendRefsToContext_NoCollectorIsNoOp lets V2.4 tools call the
// helper unconditionally (defensively) without crashing.
func TestAppendRefsToContext_NoCollectorIsNoOp(t *testing.T) {
	require.NotPanics(t, func() {
		AppendRefsToContext(context.Background(), "e1")
	})
}

// TestRefsCollector_ReturnedSliceIsCopy pins the immutability of the
// returned slice — a caller mutating the returned slice must not
// poison the collector for subsequent reads.
func TestRefsCollector_ReturnedSliceIsCopy(t *testing.T) {
	ctx := WithRefsCollector(context.Background())
	AppendRefsToContext(ctx, "e1")
	got := RefsFromContext(ctx)
	got[0] = "TAMPERED"
	again := RefsFromContext(ctx)
	require.Equal(t, []string{"e1"}, again)
}
