package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/store"
)

func TestMemoryExport_RoundtripsThroughSeedShape(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	repo := &store.MemoryRepo{DB: db}
	require.NoError(t, repo.Migrate())

	ctx := context.Background()
	now := time.Now()
	require.NoError(t, repo.Upsert(ctx, []store.MemoryFact{
		{ID: "p", Subject: "partner", Fact: "Partner is Sam.", Category: "relationship", Confidence: "high", Source: "user", EvidenceCount: 1, FirstSeen: now, LastReinforced: now},
		{ID: "r", Subject: "runs", Fact: "Tue/Thu mornings.", Category: "routine", Confidence: "med", Source: "synth", EvidenceCount: 3, FirstSeen: now, LastReinforced: now},
	}))

	rows, err := repo.ListTop(ctx, 100)
	require.NoError(t, err)

	// JSON: the output must round-trip through []memoryExportRow which
	// has the same shape as eval/seed/memory.json so it can be reseeded
	// via `zeno replay --memory-fixture`.
	var buf bytes.Buffer
	require.NoError(t, writeMemoryJSON(&buf, rows))

	var decoded []memoryExportRow
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))
	require.Len(t, decoded, 2)
	require.Equal(t, "partner", decoded[0].Subject)
	require.Equal(t, "Partner is Sam.", decoded[0].Fact)
	require.Equal(t, "high", decoded[0].Confidence)
	require.Equal(t, "user", decoded[0].Source)

	// Soft-delete one and re-export — only one row should appear.
	require.NoError(t, repo.SoftDelete(ctx, "p"))
	rows, _ = repo.ListTop(ctx, 100)
	buf.Reset()
	require.NoError(t, writeMemoryJSON(&buf, rows))
	decoded = nil
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))
	require.Len(t, decoded, 1, "soft-deleted rows must not appear in export")
	require.Equal(t, "runs", decoded[0].Subject)
}

func TestMemoryExport_CSVHasHeaderAndRows(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	repo := &store.MemoryRepo{DB: db}
	require.NoError(t, repo.Migrate())

	ctx := context.Background()
	now := time.Now()
	require.NoError(t, repo.Insert(ctx, store.MemoryFact{
		ID: "p", Subject: "partner", Fact: "Partner is Sam.",
		Category: "relationship", Confidence: "high", Source: "user", EvidenceCount: 1,
		FirstSeen: now, LastReinforced: now,
	}))
	rows, _ := repo.ListTop(ctx, 100)

	var buf bytes.Buffer
	require.NoError(t, writeMemoryCSV(&buf, rows))
	out := buf.String()
	require.Contains(t, out, "subject,fact,category,confidence,source,evidence_count")
	require.Contains(t, out, "partner,Partner is Sam.,relationship,high,user,1")
}

func TestMemoryExport_OpenWriter_StdoutAndFile(t *testing.T) {
	w, closer, err := openExportWriter("-")
	require.NoError(t, err)
	require.NotNil(t, closer)
	require.Equal(t, os.Stdout, w)

	path := filepath.Join(t.TempDir(), "out.json")
	w, closer, err = openExportWriter(path)
	require.NoError(t, err)
	closer()
	_, err = os.Stat(path)
	require.NoError(t, err, "file writer must create the file")
}
