package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/zenocy/zeno-v2/internal/store"
)

// runMemory dispatches `zeno memory <action>` subcommands. V2.2.0 ships only
// `export`; future actions (`import`, `inspect`, etc.) slot in here.
func runMemory(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "memory: action required (one of: export)")
		os.Exit(2)
	}
	action, rest := args[0], args[1:]
	switch action {
	case "export":
		runMemoryExport(rest)
	default:
		fmt.Fprintf(os.Stderr, "memory: unknown action %q (use 'export')\n", action)
		os.Exit(2)
	}
}

// runMemoryExport streams non-soft-deleted memory facts to stdout (or to
// --out) as JSON or CSV. Mirrors eval/seed/memory.json's shape so the export
// round-trips through `zeno replay --memory-fixture`.
//
// Read-only on the prod DB; safe to run while `zeno serve` holds the same
// file because SQLite WAL mode allows concurrent readers.
func runMemoryExport(args []string) {
	fs := flag.NewFlagSet("memory export", flag.ExitOnError)
	format := fs.String("format", "json", "output format: json | csv")
	out := fs.String("out", "-", "output destination ('-' for stdout)")
	ba := parseBootFlags(fs, args)

	bc := boot(ba)
	defer func() {
		if sqlDB, err := bc.db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	}()

	repo := &store.MemoryRepo{DB: bc.db, Table: "memory_facts"}
	// memoryListLimit (200) on the API mirrors what the UI surfaces; the
	// export goes wider — pull up to the hard cap × 4 so even a recently-
	// uncapped DB exports cleanly. Visible-only by default.
	rows, err := repo.ListTop(context.Background(), 1000)
	if err != nil {
		bc.logger.WithError(err).Fatal("memory export: read store failed")
	}

	w, closer, err := openExportWriter(*out)
	if err != nil {
		bc.logger.WithError(err).Fatalf("memory export: open %q", *out)
	}
	defer closer()

	switch *format {
	case "json":
		if err := writeMemoryJSON(w, rows); err != nil {
			bc.logger.WithError(err).Fatal("memory export: write json")
		}
	case "csv":
		if err := writeMemoryCSV(w, rows); err != nil {
			bc.logger.WithError(err).Fatal("memory export: write csv")
		}
	default:
		fmt.Fprintf(os.Stderr, "memory export: unknown format %q (use 'json' or 'csv')\n", *format)
		os.Exit(2)
	}

	bc.logger.WithField("count", len(rows)).WithField("format", *format).Info("memory export: done")
}

// openExportWriter resolves --out: '-' → stdout (no-op closer), otherwise a
// new file truncated on open. The closer is safe to call once on the deferred
// path; close errors are surfaced via the logger inside the helper.
func openExportWriter(path string) (io.Writer, func(), error) {
	if path == "-" || path == "" {
		return os.Stdout, func() {}, nil
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, nil, err
	}
	return f, func() { _ = f.Close() }, nil
}

// memoryExportRow mirrors eval/seed/memory.json's shape exactly so the
// output is round-trippable through the replay --memory-fixture flag.
type memoryExportRow struct {
	Subject       string `json:"subject"`
	Fact          string `json:"fact"`
	Category      string `json:"category"`
	Confidence    string `json:"confidence"`
	Source        string `json:"source"`
	EvidenceCount int    `json:"evidence_count"`
}

func writeMemoryJSON(w io.Writer, rows []store.MemoryFact) error {
	out := make([]memoryExportRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, memoryExportRow{
			Subject:       r.Subject,
			Fact:          r.Fact,
			Category:      r.Category,
			Confidence:    r.Confidence,
			Source:        r.Source,
			EvidenceCount: r.EvidenceCount,
		})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func writeMemoryCSV(w io.Writer, rows []store.MemoryFact) error {
	cw := csv.NewWriter(w)
	defer cw.Flush()
	if err := cw.Write([]string{"subject", "fact", "category", "confidence", "source", "evidence_count"}); err != nil {
		return err
	}
	for _, r := range rows {
		if err := cw.Write([]string{
			r.Subject, r.Fact, r.Category, r.Confidence, r.Source, strconv.Itoa(r.EvidenceCount),
		}); err != nil {
			return err
		}
	}
	return cw.Error()
}
