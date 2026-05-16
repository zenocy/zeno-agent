package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// V2.5.0 Phase 5: writeConcernsSidecar mirrors writeMemorySidecar. When
// the source fixture seeds concerns, a concerns.json appears next to
// the frozen input.json. When it doesn't, no stub is written.

func TestWriteConcernsSidecar_PresentWhenSeeded(t *testing.T) {
	dir := t.TempDir()
	fixturePath := filepath.Join(dir, "fx.json")
	require.NoError(t, os.WriteFile(fixturePath, []byte(`{
        "today": "2026-05-01",
        "user": {"name": "Test", "tz": "UTC"},
        "calendar": [],
        "email_threads": [],
        "seed_concerns": [
            {"id": "c-1", "name": "Frankfurt trip", "description": "summer flights", "state": "active", "source": "user"}
        ]
    }`), 0o644))

	out := filepath.Join(dir, "out")
	require.NoError(t, os.MkdirAll(out, 0o755))
	require.NoError(t, writeConcernsSidecar(fixturePath, out))

	body, err := os.ReadFile(filepath.Join(out, "concerns.json"))
	require.NoError(t, err)
	var got []map[string]any
	require.NoError(t, json.Unmarshal(body, &got))
	require.Len(t, got, 1)
	require.Equal(t, "Frankfurt trip", got[0]["name"])
}

func TestWriteConcernsSidecar_AbsentWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	fixturePath := filepath.Join(dir, "fx.json")
	require.NoError(t, os.WriteFile(fixturePath, []byte(`{
        "today": "2026-05-01",
        "user": {"name": "Test", "tz": "UTC"},
        "calendar": [],
        "email_threads": []
    }`), 0o644))

	out := filepath.Join(dir, "out")
	require.NoError(t, os.MkdirAll(out, 0o755))
	require.NoError(t, writeConcernsSidecar(fixturePath, out))

	_, err := os.Stat(filepath.Join(out, "concerns.json"))
	require.True(t, os.IsNotExist(err), "no concerns.json must be written when none seeded")
}
