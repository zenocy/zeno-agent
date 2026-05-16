package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/clock"
	"github.com/zenocy/zeno-v2/internal/store"
	"github.com/zenocy/zeno-v2/internal/synth"
)

// runReplay re-runs synth against a historical date. Output writes to the
// _replay tables so prompt iteration doesn't pollute prod cards.
func runReplay(args []string) {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	dateStr := fs.String("date", "", "target date YYYY-MM-DD (required)")
	hourStr := fs.String("hour", "07:00", "local time-of-day to simulate (HH:MM)")
	memFixture := fs.String("memory-fixture", "", "JSON file of MemoryFact rows to seed memory_facts_replay (optional)")
	ba := parseBootFlags(fs, args)

	if *dateStr == "" {
		fmt.Fprintln(os.Stderr, "replay: --date is required (YYYY-MM-DD)")
		os.Exit(2)
	}

	bc := boot(ba)
	prompts, err := synth.LoadPrompts(ba.promptsDir)
	if err != nil {
		bc.logger.WithError(err).Fatal("load prompts")
	}

	if *memFixture != "" {
		if err := seedMemoryReplay(context.Background(), bc.db, *memFixture); err != nil {
			bc.logger.WithError(err).Fatalf("seed --memory-fixture %q", *memFixture)
		}
		bc.logger.WithField("fixture", *memFixture).Info("replay: memory_facts_replay seeded")
	}

	hh, mm, err := parseHHMM(*hourStr)
	if err != nil {
		bc.logger.WithError(err).Fatalf("parse --hour %q", *hourStr)
	}
	tz := bc.clk.Location()
	day, err := time.ParseInLocation("2006-01-02", *dateStr, tz)
	if err != nil {
		bc.logger.WithError(err).Fatalf("parse --date %q", *dateStr)
	}
	asOf := time.Date(day.Year(), day.Month(), day.Day(), hh, mm, 0, 0, tz)

	bc.logger.WithField("date", *dateStr).WithField("as_of", asOf.Format(time.RFC3339)).
		Info("replay: starting")

	// Replay must compute every "today's calendar"-style projection against
	// the historical wall clock, so swap the bootContext's live Clock for a
	// Fixed pinned to asOf in the user's TZ.
	replayClk := clock.NewFixed(asOf, tz)
	reader := &synth.SliceReader{Inner: bc.store, Until: asOf}
	projCfg := projCfgFromConfig(bc.cfg, replayClk)

	runner := &synth.Runner{
		LLM:      bc.llm,
		Reader:   reader,
		DB:       bc.db,
		EventLog: nil, // replays don't write boundary events to the prod log
		ProjCfg:  projCfg,
		Prompts:  prompts,
		Now:      func() time.Time { return asOf },
		Logger:   bc.logger.WithField("c", "replay"),
		// Route to the _replay sibling tables so prod stays clean.
		CardsTable:    "cards_replay",
		BriefingTable: "briefings_replay",
		TraceTable:    "traces_replay",
		MemoryTable:   "memory_facts_replay",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := runner.Run(ctx); err != nil {
		bc.logger.WithError(err).Fatal("replay: synth run failed")
	}
	bc.logger.WithField("date", *dateStr).Info("replay: done")
}

func parseHHMM(s string) (int, int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, 0, err
	}
	return t.Hour(), t.Minute(), nil
}

// seedMemoryReplay truncates memory_facts_replay and reloads it from a JSON
// fixture. Used by `zeno replay --memory-fixture <path>` so prompt iteration
// against memory-aware briefings starts from a known state. Missing
// provenance fields (FirstSeen, LastReinforced, EvidenceCount) get sensible
// defaults at load time so fixture authors don't need to think about them;
// missing IDs are derived deterministically from subject+fact so re-running
// with the same fixture produces the same row IDs.
func seedMemoryReplay(ctx context.Context, db *gorm.DB, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read fixture: %w", err)
	}
	var rows []store.MemoryFact
	if err := json.Unmarshal(raw, &rows); err != nil {
		return fmt.Errorf("decode fixture: %w", err)
	}
	if len(rows) == 0 {
		return fmt.Errorf("fixture %q is empty", path)
	}
	now := time.Now()
	for i := range rows {
		if rows[i].Subject == "" {
			return fmt.Errorf("fixture row %d missing subject", i)
		}
		if rows[i].Fact == "" {
			return fmt.Errorf("fixture row %d (%s) missing fact", i, rows[i].Subject)
		}
		if rows[i].ID == "" {
			rows[i].ID = deriveMemoryID(rows[i].Subject, rows[i].Fact)
		}
		if rows[i].Confidence == "" {
			rows[i].Confidence = "low"
		}
		if rows[i].Source == "" {
			rows[i].Source = "synth"
		}
		if rows[i].Category == "" {
			rows[i].Category = "misc"
		}
		if rows[i].EvidenceCount == 0 {
			rows[i].EvidenceCount = 1
		}
		if rows[i].FirstSeen.IsZero() {
			rows[i].FirstSeen = now
		}
		if rows[i].LastReinforced.IsZero() {
			rows[i].LastReinforced = now
		}
	}
	// Hard delete every row (including any soft-deleted ones) so the seed
	// is a true reset rather than an overlay on prior replay state.
	if err := db.WithContext(ctx).Exec("DELETE FROM memory_facts_replay").Error; err != nil {
		return fmt.Errorf("truncate memory_facts_replay: %w", err)
	}
	repo := &store.MemoryRepo{DB: db, Table: "memory_facts_replay"}
	if err := repo.Upsert(ctx, rows); err != nil {
		return fmt.Errorf("upsert seed rows: %w", err)
	}
	return nil
}

// deriveMemoryID builds a stable ID from a normalized subject + a short hash
// of the full fact text. Lets fixture authors omit the ID while still getting
// reproducible row identities across re-seeds.
func deriveMemoryID(subject, fact string) string {
	subj := strings.ToLower(strings.TrimSpace(subject))
	sum := sha256.Sum256([]byte(fact))
	return subj + "-" + hex.EncodeToString(sum[:4])
}
