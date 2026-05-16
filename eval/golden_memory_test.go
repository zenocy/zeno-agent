package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestWriteMemorySidecar_WritesWhenFixtureHasMemory verifies the helper
// projects the inline memory block into memory.json alongside the frozen
// input. The freeze path runs this for every fixture; this test covers the
// branch in isolation so the freeze tests don't need a transcript-backed
// runner.
func TestWriteMemorySidecar_WritesWhenFixtureHasMemory(t *testing.T) {
	src := writeFixtureForTest(t, true)
	dir := t.TempDir()

	require.NoError(t, writeMemorySidecar(src, dir))

	out := filepath.Join(dir, "memory.json")
	raw, err := os.ReadFile(out)
	require.NoError(t, err)

	var rows []FixtureMemoryFact
	require.NoError(t, json.Unmarshal(raw, &rows))
	require.Len(t, rows, 2)
	require.Equal(t, "partner", rows[0].Subject)
	require.Equal(t, "Partner is Sam.", rows[0].Fact)
}

// TestWriteMemorySidecar_SkipsWhenFixtureHasNoMemory pins the no-stub
// behavior — fixtures without a memory block don't grow an empty
// memory.json so the golden directory listing stays meaningful.
func TestWriteMemorySidecar_SkipsWhenFixtureHasNoMemory(t *testing.T) {
	src := writeFixtureForTest(t, false)
	dir := t.TempDir()

	require.NoError(t, writeMemorySidecar(src, dir))

	_, err := os.Stat(filepath.Join(dir, "memory.json"))
	require.True(t, os.IsNotExist(err), "no memory.json should be written when fixture has no memory")
}

func writeFixtureForTest(t *testing.T, withMemory bool) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "fixture.json")
	body := map[string]any{
		"today": "2026-04-25",
		"user":  map[string]any{"name": "Test", "tz": "UTC"},
	}
	if withMemory {
		body["memory"] = []map[string]any{
			{"subject": "partner", "fact": "Partner is Sam.", "category": "relationship", "confidence": "high", "source": "user"},
			{"subject": "runs", "fact": "Tue/Thu mornings.", "category": "routine", "confidence": "med", "source": "synth"},
		}
	}
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(src, raw, 0o644))
	return src
}

// TestWriteMustCardsStateSidecar_WritesWhenFixtureDeclaresPerStateRules pins
// the V2.3.0 P5 audit-trail sidecar: when a fixture declares
// expected_state + per-state must_cards, the freeze writes a standalone
// must_cards_state.json with both the state name and the rule definitions.
func TestWriteMustCardsStateSidecar_WritesWhenFixtureDeclaresPerStateRules(t *testing.T) {
	src := writeStateFixtureForTest(t, "deep_work", true)
	dir := t.TempDir()

	require.NoError(t, writeMustCardsStateSidecar(src, dir))

	out := filepath.Join(dir, "must_cards_state.json")
	raw, err := os.ReadFile(out)
	require.NoError(t, err)

	var got mustCardsStateSidecar
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Equal(t, "deep_work", got.State)
	require.Len(t, got.MustCards, 1)
	require.Equal(t, "deep work focus card", got.MustCards[0].Name)
}

// TestWriteMustCardsStateSidecar_SkipsWhenNoExpectedState verifies that
// fixtures without expected_state (pre-V2.3 shape) don't grow a stub
// sidecar.
func TestWriteMustCardsStateSidecar_SkipsWhenNoExpectedState(t *testing.T) {
	src := writeFixtureForTest(t, false)
	dir := t.TempDir()

	require.NoError(t, writeMustCardsStateSidecar(src, dir))

	_, err := os.Stat(filepath.Join(dir, "must_cards_state.json"))
	require.True(t, os.IsNotExist(err))
}

// TestWriteMustCardsStateSidecar_SkipsWhenNoPerStateRules verifies that a
// fixture which declares expected_state but has no per-state must_cards
// rules for that state (e.g. morning_calm fixtures with only flat
// must_cards) does not grow an empty sidecar.
func TestWriteMustCardsStateSidecar_SkipsWhenNoPerStateRules(t *testing.T) {
	src := writeStateFixtureForTest(t, "morning_calm", false)
	dir := t.TempDir()

	require.NoError(t, writeMustCardsStateSidecar(src, dir))

	_, err := os.Stat(filepath.Join(dir, "must_cards_state.json"))
	require.True(t, os.IsNotExist(err))
}

func writeStateFixtureForTest(t *testing.T, state string, withDeepWorkRules bool) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "fixture.json")
	body := map[string]any{
		"today":          "2026-04-25",
		"user":           map[string]any{"name": "Test", "tz": "UTC"},
		"expected_state": state,
	}
	if withDeepWorkRules {
		body["must_cards_deep_work"] = []map[string]any{
			{
				"name":           "deep work focus card",
				"src":            "tasks",
				"title_contains": []string{"focus"},
				"sub_contains":   []string{"hold"},
			},
		}
	}
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(src, raw, 0o644))
	return src
}
