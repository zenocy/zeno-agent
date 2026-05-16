package eval

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/zenocy/zeno-v2/internal/store"
)

func TestLoadFixture_DeepWorkWithTasksRoundTrips(t *testing.T) {
	// Verify the new fixture file parses cleanly and exposes the expected
	// open_tasks block (regression guard against schema drift).
	repoRoot := filepath.Join("corpus", "deep_work_with_tasks.json")
	f, err := LoadFixture(repoRoot)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if f.ExpectedState != "deep_work" {
		t.Fatalf("expected_state want deep_work, got %q", f.ExpectedState)
	}
	if len(f.OpenTasks) == 0 {
		t.Fatalf("open_tasks must be populated in deep_work_with_tasks fixture")
	}
	// At least one high-priority task to exercise the cards-bias overlay.
	hasHigh := false
	for _, ot := range f.OpenTasks {
		if ot.Priority == "high" {
			hasHigh = true
			break
		}
	}
	if !hasHigh {
		t.Fatalf("fixture should declare at least one high-priority task")
	}
}

func TestLoadFixture_EndOfDayWithTasksRoundTrips(t *testing.T) {
	repoRoot := filepath.Join("corpus", "end_of_day_with_tasks.json")
	f, err := LoadFixture(repoRoot)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if f.ExpectedState != "end_of_day" {
		t.Fatalf("expected_state want end_of_day, got %q", f.ExpectedState)
	}
	// One completed-today task + one slipped task — both required for
	// end_of_day cards bias to have something to grip.
	doneToday, slipped := 0, 0
	for _, ot := range f.OpenTasks {
		if ot.Completed && ot.DoneDate == f.Today {
			doneToday++
		}
		if !ot.Completed {
			slipped++
		}
	}
	if doneToday == 0 {
		t.Fatalf("fixture should include at least one task done today")
	}
	if slipped == 0 {
		t.Fatalf("fixture should include at least one open (slipped) task")
	}
}

func TestEphemeralStore_Seed_OpenTasks(t *testing.T) {
	dir := t.TempDir()
	s, err := NewEphemeralStore(dir)
	if err != nil {
		t.Fatalf("new ephemeral: %v", err)
	}
	defer func() { _ = s.Close() }()

	f := &Fixture{
		Today: "2026-05-05",
		User:  FixtureUser{Name: "Test", TZ: "America/Los_Angeles"},
		OpenTasks: []FixtureTask{
			{Title: "Open task", DueDate: "2026-05-10", Priority: "high", Tags: []string{"work"}},
			{Title: "Done today", Completed: true, DoneDate: "2026-05-05"},
			{Title: "Custom UID", UID: "custom-uid-xyz"},
		},
	}
	if err := s.Seed(context.Background(), f); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// V2.11: tasks live in the SQLite tasks table; the seed inserts
	// rows directly. Read them back via TaskRepo to check fidelity.
	repo := &store.TaskRepo{DB: s.DB}
	rows, err := repo.List(context.Background(), store.TaskFilter{Status: "all"})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("want 3 task rows, got %d", len(rows))
	}

	gotByTitle := map[string]store.Task{}
	for _, r := range rows {
		gotByTitle[r.Title] = r
	}

	open := gotByTitle["Open task"]
	if open.DueDate != "2026-05-10" {
		t.Fatalf("open task due_date wrong: %v", open.DueDate)
	}
	if open.Priority != "high" {
		t.Fatalf("open task priority wrong: %v", open.Priority)
	}
	var tags []string
	if err := json.Unmarshal(open.Tags, &tags); err != nil || len(tags) != 1 || tags[0] != "work" {
		t.Fatalf("open task tags wrong: %v", string(open.Tags))
	}

	done := gotByTitle["Done today"]
	if !done.Completed {
		t.Fatalf("done task should be completed")
	}
	if done.CompletedAt == nil || done.CompletedAt.Format("2006-01-02") != "2026-05-05" {
		t.Fatalf("done date wrong: %v", done.CompletedAt)
	}
	// Default priority should be med when omitted.
	if done.Priority != "med" {
		t.Fatalf("default priority should be med, got %v", done.Priority)
	}

	// Custom UID is honored verbatim.
	custom := gotByTitle["Custom UID"]
	if custom.ID != "custom-uid-xyz" {
		t.Fatalf("custom uid not preserved: %v", custom.ID)
	}

	// Auto-derived UID matches the V2.6 sha256(title|due_date)[:6] scheme
	// — taskFixtureUID is the runtime computation, kept stable across the
	// V2.11 cutover so re-seeded fixtures don't churn IDs.
	wantUID := taskFixtureUID("Open task", "2026-05-10")
	if open.ID != wantUID {
		t.Fatalf("auto-derived UID mismatch: want %q, got %v", wantUID, open.ID)
	}
}

func TestEphemeralStore_Seed_OpenTasksRejectsEmptyTitle(t *testing.T) {
	dir := t.TempDir()
	s, err := NewEphemeralStore(dir)
	if err != nil {
		t.Fatalf("new ephemeral: %v", err)
	}
	defer func() { _ = s.Close() }()

	f := &Fixture{
		Today: "2026-05-05",
		User:  FixtureUser{Name: "Test", TZ: "America/Los_Angeles"},
		OpenTasks: []FixtureTask{
			{Title: "   "}, // whitespace only
		},
	}
	if err := s.Seed(context.Background(), f); err == nil {
		t.Fatalf("expected error on empty title")
	}
}
