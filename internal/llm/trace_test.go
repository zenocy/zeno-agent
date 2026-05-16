package llm

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Build returns an empty Trace when nothing was recorded — but Stopped is
// preserved so callers can distinguish "loop terminated cleanly with zero
// steps" from "loop never ran." The Steps slice is non-nil but empty so
// JSON-encodes as `"steps": []` not `"steps": null`.
func TestAccumulator_Build_EmptyKeepsStoppedReason(t *testing.T) {
	a := NewAccumulator()
	tr := a.Build("ok")
	require.Equal(t, "ok", tr.Stopped)
	require.NotNil(t, tr.Steps)
	require.Len(t, tr.Steps, 0)

	out, err := json.Marshal(tr)
	require.NoError(t, err)
	require.Contains(t, string(out), `"steps":[]`)
}

// Each Record* call must stamp MsAt with monotonically non-decreasing
// elapsed time. This is what the live trace UI relies on to render a
// timeline; out-of-order steps would render the bars overlapping.
func TestAccumulator_RecordsAreMonotonic(t *testing.T) {
	a := NewAccumulator()
	a.RecordTool("READ", "thread-1", "")
	time.Sleep(2 * time.Millisecond)
	a.RecordThought("looking at evidence")
	time.Sleep(2 * time.Millisecond)
	a.RecordToolWithRefs("READ", "evidence-q", "", []string{"o1", "o2"})

	steps := a.Steps()
	require.Len(t, steps, 3)
	for i := 1; i < len(steps); i++ {
		require.True(t,
			steps[i].MsAt >= steps[i-1].MsAt,
			"step %d MsAt=%d must be >= prev %d", i, steps[i].MsAt, steps[i-1].MsAt)
	}
}

// Steps() returns a copy — mutating the returned slice must not affect
// future calls or Build's output.
func TestAccumulator_Steps_ReturnsCopy(t *testing.T) {
	a := NewAccumulator()
	a.RecordTool("READ", "x", "")
	got := a.Steps()
	got[0].Op = "MUTATED"

	again := a.Steps()
	require.Equal(t, "READ", again[0].Op, "Steps() must hand back a defensive copy")

	tr := a.Build("ok")
	require.Equal(t, "READ", tr.Steps[0].Op, "Build snapshot must not see external mutation")
}

// RecordTool returns the same step shape it appended; this is the
// invariant the live publisher relies on so durable + live carry the
// same body.
func TestAccumulator_Record_ReturnsAppendedStep(t *testing.T) {
	a := NewAccumulator()
	got := a.RecordTool("CHECK", "weather-window", "no rain in run window")
	require.Equal(t, KindTool, got.Kind)
	require.Equal(t, "CHECK", got.Op)
	require.Equal(t, "weather-window", got.Target)
	require.Equal(t, "no rain in run window", got.Note)

	thoughtGot := a.RecordThought("hmm, busy morning")
	require.Equal(t, KindThought, thoughtGot.Kind)
	require.Equal(t, "hmm, busy morning", thoughtGot.T)
	require.Equal(t, "", thoughtGot.Op)
}

// Build sets TotalMs from the accumulator's t0 — it should be > 0 in
// any realistic call, even if no steps were recorded between Now and Build.
func TestAccumulator_Build_TotalMsTracksWallClock(t *testing.T) {
	a := NewAccumulator()
	time.Sleep(3 * time.Millisecond)
	tr := a.Build("ok")
	require.GreaterOrEqual(t, tr.TotalMs, int64(2),
		"TotalMs should reflect at least the wall-clock sleep")
}
