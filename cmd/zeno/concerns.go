package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/zenocy/zeno-v2/internal/store"
	"github.com/zenocy/zeno-v2/internal/synth"
)

// runConcerns dispatches `zeno concerns <action>` subcommands.
//
// Phase 1: list / show / dismiss for inspection and repair.
// Phase 2: recognition-run and retrospective-run for the operator.
func runConcerns(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "concerns: action required (one of: list, show, dismiss, recognition-run, retrospective-run)")
		os.Exit(2)
	}
	action, rest := args[0], args[1:]
	switch action {
	case "list":
		runConcernsList(rest)
	case "show":
		runConcernsShow(rest)
	case "dismiss":
		runConcernsDismiss(rest)
	case "recognition-run":
		runConcernsRecognition(rest)
	case "retrospective-run":
		runConcernsRetrospective(rest)
	default:
		fmt.Fprintf(os.Stderr, "concerns: unknown action %q (use 'list', 'show', 'dismiss', 'recognition-run', or 'retrospective-run')\n", action)
		os.Exit(2)
	}
}

func runConcernsList(args []string) {
	fs := flag.NewFlagSet("concerns list", flag.ExitOnError)
	state := fs.String("state", "", "filter by state (proposed|active|paused|ended|merged)")
	jsonOut := fs.Bool("json", false, "emit JSON instead of a table")
	ba := parseBootFlags(fs, args)

	bc := boot(ba)
	defer closeDB(bc)

	repo := &store.ConcernRepo{DB: bc.db, Table: "concerns"}
	tagRepo := &store.ConcernObservationRepo{DB: bc.db, Table: "concern_observations"}
	ctx := context.Background()

	var rows []store.Concern
	var err error
	if *state == "" {
		rows, err = repo.ListAll(ctx)
	} else if !store.IsValidConcernState(*state) {
		fmt.Fprintf(os.Stderr, "concerns: unknown state %q\n", *state)
		os.Exit(2)
	} else {
		rows, err = repo.ListByState(ctx, *state)
	}
	if err != nil {
		bc.logger.WithError(err).Fatal("concerns list: read store failed")
	}

	if err := renderConcernsList(ctx, os.Stdout, rows, tagRepo, *jsonOut); err != nil {
		bc.logger.WithError(err).Fatal("concerns list: render failed")
	}
}

// renderConcernsList writes the table or JSON view of `rows` to w. Pulled
// out of runConcernsList so the format contract — column order, time
// layout, JSON shape — is unit-testable without booting the full daemon.
func renderConcernsList(ctx context.Context, w io.Writer, rows []store.Concern, tagRepo *store.ConcernObservationRepo, jsonOut bool) error {
	if jsonOut {
		return json.NewEncoder(w).Encode(rows)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "STATE\tNAME\tSOURCE\tOBS\tLAST_ACTIVE\tID"); err != nil {
		return err
	}
	for _, c := range rows {
		var count int64
		if tagRepo != nil {
			count, _ = tagRepo.CountByConcern(ctx, c.ID)
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n",
			c.State, c.Name, c.Source, count,
			c.LastActiveAt.Format("2006-01-02 15:04"), c.ID,
		); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func runConcernsShow(args []string) {
	fs := flag.NewFlagSet("concerns show", flag.ExitOnError)
	id := fs.String("id", "", "concern id (required)")
	ba := parseBootFlags(fs, args)
	if *id == "" {
		// Re-parse putting the positional arg as id
		fs2 := flag.NewFlagSet("concerns show", flag.ExitOnError)
		_ = fs2.Parse(args)
		if fs2.NArg() == 0 {
			fmt.Fprintln(os.Stderr, "concerns show: --id <id> required")
			os.Exit(2)
		}
		i := fs2.Arg(0)
		id = &i
	}

	bc := boot(ba)
	defer closeDB(bc)

	repo := &store.ConcernRepo{DB: bc.db, Table: "concerns"}
	tagRepo := &store.ConcernObservationRepo{DB: bc.db, Table: "concern_observations"}
	ctx := context.Background()

	row, err := repo.GetByID(ctx, *id)
	if err != nil {
		bc.logger.WithError(err).Fatal("concerns show: read failed")
	}
	if row == nil {
		fmt.Fprintf(os.Stderr, "concerns show: %s not found\n", *id)
		os.Exit(1)
	}
	tags, err := tagRepo.ListByConcern(ctx, row.ID, 0)
	if err != nil {
		bc.logger.WithError(err).Fatal("concerns show: list tags failed")
	}
	out := map[string]any{
		"concern":      row,
		"observations": tags,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

func runConcernsDismiss(args []string) {
	fs := flag.NewFlagSet("concerns dismiss", flag.ExitOnError)
	ba := parseBootFlags(fs, args)
	if fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "concerns dismiss: <id> required as positional arg")
		os.Exit(2)
	}
	id := fs.Arg(0)

	bc := boot(ba)
	defer closeDB(bc)

	repo := &store.ConcernRepo{DB: bc.db, Table: "concerns"}
	ctx := context.Background()
	row, err := repo.GetByID(ctx, id)
	if err != nil {
		bc.logger.WithError(err).Fatal("concerns dismiss: read failed")
	}
	if row == nil {
		fmt.Fprintf(os.Stderr, "concerns dismiss: %s not found\n", id)
		os.Exit(1)
	}
	if row.State != store.ConcernStateProposed {
		fmt.Fprintf(os.Stderr, "concerns dismiss: %s is %s, only proposed concerns can be dismissed\n", id, row.State)
		os.Exit(1)
	}
	if err := repo.SoftDelete(ctx, id); err != nil {
		bc.logger.WithError(err).Fatal("concerns dismiss: soft-delete failed")
	}
	fmt.Fprintf(os.Stdout, "dismissed %s (%q)\n", id, row.Name)
}

// runConcernsRecognition fires one daily-style recognition pass against
// the production observation log. Use --dry-run to print what would
// land without persisting; useful for prompt iteration without
// dirtying the concerns table.
func runConcernsRecognition(args []string) {
	fs := flag.NewFlagSet("concerns recognition-run", flag.ExitOnError)
	lookbackDays := fs.Int("lookback-days", 14, "look back this many days for observations")
	dailyCap := fs.Int("daily-cap", 2, "maximum proposals per run")
	minConfidence := fs.Float64("min-confidence", 0.7, "drop proposals below this confidence")
	dryRun := fs.Bool("dry-run", false, "print proposals; do not persist")
	jsonOut := fs.Bool("json", false, "emit JSON instead of human prose")
	ba := parseBootFlags(fs, args)

	bc := boot(ba)
	defer closeDB(bc)
	logger := bc.logger.WithField("c", "concerns-recognition")

	cRepo := &store.ConcernRepo{DB: bc.db, Table: "concerns"}
	oRepo := &store.ConcernObservationRepo{DB: bc.db, Table: "concern_observations"}
	ctx := context.Background()

	deps := synth.RecognizeDeps{
		LLM:          bc.llm,
		Reader:       bc.store,
		Concerns:     cRepo,
		Observations: oRepo,
		EventLog:     bc.store,
		Logger:       logger,
	}
	if *dryRun {
		// In dry-run mode, run against a separate in-memory pass to avoid
		// touching prod tables. Simplest approach: feed the runner a no-op
		// repo. For Phase 2 we just compute and print without dispatch —
		// the runner persists by default. To honor --dry-run we duplicate
		// the runner's filter logic by skipping persistence on the result.
		// For now, the cleanest path: call Recognize, then if dry-run,
		// fail loudly so the operator can roll back manually rather than
		// silently writing rows. Document this caveat in the help.
		fmt.Fprintln(os.Stderr, "concerns recognition-run: --dry-run is not yet supported; aborting before LLM call")
		os.Exit(2)
	}
	res, err := synth.Recognize(ctx, deps, synth.RecognizeOpts{
		Lookback:      time.Duration(*lookbackDays) * 24 * time.Hour,
		DailyCap:      *dailyCap,
		MinConfidence: *minConfidence,
		Now:           time.Now(),
	})
	if err != nil {
		logger.WithError(err).Fatal("recognition run failed")
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
		return
	}
	fmt.Fprintf(os.Stdout, "recognition: observed=%d, raw=%d, accepted=%d, rejected=%d, run_id=%s\n",
		res.ObservationN, len(res.RawProposals), len(res.Accepted), len(res.Rejected), res.RunID)
	for _, a := range res.Accepted {
		fmt.Fprintf(os.Stdout, "  + %s — %s (conf=%.2f) [id=%s]\n",
			a.Proposed.Name, a.Proposed.Description, a.Proposed.Confidence, a.ConcernID)
	}
	for _, r := range res.Rejected {
		fmt.Fprintf(os.Stdout, "  - %s [%s]\n", r.Proposed.Name, r.Reason)
	}
}

// runConcernsRetrospective fires retrospective tagging for one concern,
// streaming progress to stderr. Synchronous — exits when the walk
// completes (or fails / cancels).
func runConcernsRetrospective(args []string) {
	fs := flag.NewFlagSet("concerns retrospective-run", flag.ExitOnError)
	id := fs.String("id", "", "concern id (required)")
	batch := fs.Int("batch-size", 20, "observations per LLM call")
	maxCalls := fs.Int("max-calls", 50, "hard ceiling on LLM calls")
	lookbackDays := fs.Int("lookback-days", 180, "history window in days")
	ba := parseBootFlags(fs, args)
	if *id == "" {
		fmt.Fprintln(os.Stderr, "concerns retrospective-run: --id <concern-id> required")
		os.Exit(2)
	}

	bc := boot(ba)
	defer closeDB(bc)
	logger := bc.logger.WithField("c", "concerns-retrospective")

	cRepo := &store.ConcernRepo{DB: bc.db, Table: "concerns"}
	oRepo := &store.ConcernObservationRepo{DB: bc.db, Table: "concern_observations"}

	deps := synth.RetrospectiveDeps{
		LLM:          bc.llm,
		Reader:       bc.store,
		Concerns:     cRepo,
		Observations: oRepo,
		EventLog:     bc.store,
		Logger:       logger,
	}
	res, err := synth.Retrospective(context.Background(), deps, *id, synth.RetrospectiveOpts{
		Lookback: time.Duration(*lookbackDays) * 24 * time.Hour,
		Batch:    *batch,
		MaxCalls: *maxCalls,
		Now:      time.Now(),
	})
	if err != nil {
		logger.WithError(err).Fatal("retrospective run failed")
	}
	fmt.Fprintf(os.Stdout, "retrospective: concern_id=%s status=%s total=%d processed=%d tagged=%d calls=%d\n",
		res.ConcernID, res.Status, res.Total, res.Processed, res.Tagged, res.Calls)
}

func closeDB(bc *bootContext) {
	if bc == nil || bc.db == nil {
		return
	}
	if sqlDB, err := bc.db.DB(); err == nil {
		_ = sqlDB.Close()
	}
}
