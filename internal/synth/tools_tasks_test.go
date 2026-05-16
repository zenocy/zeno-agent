package synth

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/log/logtest"
	"github.com/zenocy/zeno-v2/internal/store"
)

// V2.11: ReadTasksTool reads from the unified TaskRepo via the
// OpenTasks projection. The V2.6 event-log fold is gone; the tool's
// public contract (filters, output prose) survives unchanged so the
// LLM prompt cache stays warm across the cutover.

func newToolRepo(t *testing.T) *store.TaskRepo {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_journal_mode=WAL&_foreign_keys=on"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	repo := &store.TaskRepo{DB: db}
	require.NoError(t, repo.Migrate())
	return repo
}

func newTasksToolWithRepo(repo *store.TaskRepo, tz *time.Location, today string) *ReadTasksTool {
	d, _ := time.ParseInLocation("2006-01-02", today, tz)
	d = d.Add(10 * time.Hour)
	return &ReadTasksTool{
		Tasks:  repo,
		Reader: logtest.NewMemReader(),
		Now:    func() time.Time { return d },
		TZ:     tz,
	}
}

func TestReadTasks_FilterOpen(t *testing.T) {
	repo := newToolRepo(t)
	tz, _ := time.LoadLocation("America/Los_Angeles")
	doneAt := time.Date(2026, 4, 30, 10, 0, 0, 0, tz)
	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: "a", Title: "live one", Priority: "med"}))
	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: "b", Title: "old one", Completed: true, CompletedAt: &doneAt, Priority: "med"}))

	tool := newTasksToolWithRepo(repo, tz, "2026-05-05")
	out, err := tool.Execute(context.Background(), map[string]any{"filter": "open"})
	require.NoError(t, err)
	require.Contains(t, out, "live one")
	require.NotContains(t, out, "old one", "completed task should not surface under open")
}

func TestReadTasks_FilterOverdue(t *testing.T) {
	repo := newToolRepo(t)
	tz, _ := time.LoadLocation("America/Los_Angeles")
	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: "od", Title: "OD", DueDate: "2026-05-01", Priority: "med"}))
	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: "fut", Title: "FUT", DueDate: "2026-05-10", Priority: "med"}))

	tool := newTasksToolWithRepo(repo, tz, "2026-05-05")
	out, err := tool.Execute(context.Background(), map[string]any{"filter": "overdue"})
	require.NoError(t, err)
	require.Contains(t, out, "OD")
	require.NotContains(t, out, "FUT", "future task leaked into overdue")
	require.Contains(t, out, "overdue")
}

func TestReadTasks_FilterCompletedToday(t *testing.T) {
	repo := newToolRepo(t)
	tz, _ := time.LoadLocation("America/Los_Angeles")
	doneToday := time.Date(2026, 5, 5, 14, 0, 0, 0, tz)
	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: "today", Title: "shipped", Completed: true, CompletedAt: &doneToday, Priority: "med"}))

	tool := newTasksToolWithRepo(repo, tz, "2026-05-05")
	out, err := tool.Execute(context.Background(), map[string]any{"filter": "completed_today"})
	require.NoError(t, err)
	require.Contains(t, out, "shipped")
	require.Contains(t, out, "done today")
}

func TestReadTasks_DefaultFilterIsOpen(t *testing.T) {
	repo := newToolRepo(t)
	tz, _ := time.LoadLocation("America/Los_Angeles")
	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: "live", Title: "live", Priority: "med"}))

	tool := newTasksToolWithRepo(repo, tz, "2026-05-05")
	out, err := tool.Execute(context.Background(), map[string]any{})
	require.NoError(t, err)
	require.Contains(t, out, "live")
}

func TestReadTasks_TagFilter(t *testing.T) {
	repo := newToolRepo(t)
	tz, _ := time.LoadLocation("America/Los_Angeles")
	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: "h", Title: "with tag", Tags: []byte(`["home"]`), Priority: "med"}))
	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: "n", Title: "no tag", Priority: "med"}))

	tool := newTasksToolWithRepo(repo, tz, "2026-05-05")
	out, err := tool.Execute(context.Background(), map[string]any{"tag": "home"})
	require.NoError(t, err)
	require.Contains(t, out, "with tag")
	require.NotContains(t, out, "no tag")
}

func TestReadTasks_TagFilterAcceptsLeadingHash(t *testing.T) {
	repo := newToolRepo(t)
	tz, _ := time.LoadLocation("America/Los_Angeles")
	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: "h", Title: "with tag", Tags: []byte(`["home"]`), Priority: "med"}))

	tool := newTasksToolWithRepo(repo, tz, "2026-05-05")
	out, err := tool.Execute(context.Background(), map[string]any{"tag": "#home"})
	require.NoError(t, err)
	require.Contains(t, out, "with tag")
}

func TestReadTasks_TagFilterCaseInsensitive(t *testing.T) {
	repo := newToolRepo(t)
	tz, _ := time.LoadLocation("America/Los_Angeles")
	// Tags are normalized to lowercase by the executor on insert. Test
	// that a mixed-case query still matches.
	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: "h", Title: "with tag", Tags: []byte(`["home"]`), Priority: "med"}))

	tool := newTasksToolWithRepo(repo, tz, "2026-05-05")
	out, err := tool.Execute(context.Background(), map[string]any{"tag": "Home"})
	require.NoError(t, err)
	require.Contains(t, out, "with tag")
}

func TestReadTasks_LimitCappedAt20(t *testing.T) {
	repo := newToolRepo(t)
	tz, _ := time.LoadLocation("America/Los_Angeles")
	for i := 0; i < 30; i++ {
		require.NoError(t, repo.Insert(context.Background(), store.Task{ID: "x" + string(rune('a'+i)), Title: "task"}))
	}

	tool := newTasksToolWithRepo(repo, tz, "2026-05-05")
	out, err := tool.Execute(context.Background(), map[string]any{"limit": 100})
	require.NoError(t, err)
	require.Equal(t, 20, strings.Count(out, "\n- "), "limit must cap at 20 even when caller asks for 100")
}

func TestReadTasks_LimitZeroFallsBackTo5(t *testing.T) {
	repo := newToolRepo(t)
	tz, _ := time.LoadLocation("America/Los_Angeles")
	for i := 0; i < 10; i++ {
		require.NoError(t, repo.Insert(context.Background(), store.Task{ID: "x" + string(rune('a'+i)), Title: "task"}))
	}

	tool := newTasksToolWithRepo(repo, tz, "2026-05-05")
	out, err := tool.Execute(context.Background(), map[string]any{"limit": 0})
	require.NoError(t, err)
	require.Equal(t, 5, strings.Count(out, "\n- "), "limit=0 must default to 5")
}

func TestReadTasks_EmptyResult(t *testing.T) {
	repo := newToolRepo(t)
	tz, _ := time.LoadLocation("America/Los_Angeles")

	tool := newTasksToolWithRepo(repo, tz, "2026-05-05")
	out, err := tool.Execute(context.Background(), map[string]any{"filter": "open"})
	require.NoError(t, err)
	require.Equal(t, "No tasks match filter=open.", out)
}

func TestReadTasks_BadgeForms(t *testing.T) {
	repo := newToolRepo(t)
	tz, _ := time.LoadLocation("America/Los_Angeles")
	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: "today", Title: "today task", DueDate: "2026-05-05"}))
	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: "in1", Title: "tomorrow task", DueDate: "2026-05-06"}))
	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: "in3", Title: "in 3d", DueDate: "2026-05-08"}))
	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: "nodate", Title: "no date"}))

	tool := newTasksToolWithRepo(repo, tz, "2026-05-05")
	out, err := tool.Execute(context.Background(), map[string]any{"filter": "open", "limit": 10})
	require.NoError(t, err)
	require.Contains(t, out, "[today]")
	require.Contains(t, out, "[in 1d]")
	require.Contains(t, out, "[in 3d]")
	require.Contains(t, out, "[no date]")
}

func TestReadTasks_HasAlarmFilter(t *testing.T) {
	repo := newToolRepo(t)
	tz, _ := time.LoadLocation("America/Los_Angeles")
	fa := time.Date(2026, 5, 5, 14, 0, 0, 0, tz)
	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: "a", Title: "alarm task", FireAt: &fa}))
	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: "p", Title: "plain"}))

	tool := newTasksToolWithRepo(repo, tz, "2026-05-05")
	out, err := tool.Execute(context.Background(), map[string]any{"filter": "has_alarm"})
	require.NoError(t, err)
	require.Contains(t, out, "alarm task")
	require.NotContains(t, out, "plain")
}

func TestReadTasks_BadFilterRejected(t *testing.T) {
	repo := newToolRepo(t)
	tz, _ := time.LoadLocation("America/Los_Angeles")
	tool := newTasksToolWithRepo(repo, tz, "2026-05-05")
	_, err := tool.Execute(context.Background(), map[string]any{"filter": "bogus"})
	require.Error(t, err)
}
