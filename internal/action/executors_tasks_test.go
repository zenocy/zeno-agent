package action

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	logp "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/store"
)

// fixedNow returns the same wall time on every call — used by the
// executor tests so timestamps are deterministic.
func fixedNow(t string) func() time.Time {
	return func() time.Time {
		ts, err := time.Parse("2006-01-02 15:04:05", t)
		if err != nil {
			panic(err)
		}
		return ts
	}
}

func newTaskRepo(t *testing.T) *store.TaskRepo {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_journal_mode=WAL&_foreign_keys=on"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	repo := &store.TaskRepo{DB: db}
	require.NoError(t, repo.Migrate())
	return repo
}

func seedTask(t *testing.T, repo *store.TaskRepo, title string) string {
	t.Helper()
	id := uuid.NewString()
	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: id, Title: title}))
	return id
}

// TestCompleteTaskExec_FlipsCompleted pins the V2.11 contract: the row
// is updated in place, Completed flips to true, and the outcome event
// kind / toast match the V2.6 file-based behavior.
func TestCompleteTaskExec_FlipsCompleted(t *testing.T) {
	repo := newTaskRepo(t)
	uid := seedTask(t, repo, "Buy milk")

	exec := &CompleteTaskExec{Tasks: repo, Now: fixedNow("2026-05-09 10:00:00")}
	res, err := exec.Execute(context.Background(), ExecCtx{
		Target: map[string]any{"uid": uid},
	})
	require.NoError(t, err)
	require.True(t, res.OK, "complete should succeed; toast=%q", res.Toast)
	require.Equal(t, logp.KindTaskStatusChanged, res.EventKind)
	require.Contains(t, res.Toast, "Buy milk")

	got, err := repo.Get(context.Background(), uid)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.True(t, got.Completed)
	require.NotNil(t, got.CompletedAt)
}

func TestCompleteTaskExec_NotFound(t *testing.T) {
	repo := newTaskRepo(t)
	exec := &CompleteTaskExec{Tasks: repo, Now: fixedNow("2026-05-09 10:00:00")}
	res, err := exec.Execute(context.Background(), ExecCtx{Target: map[string]any{"uid": "deadbeef"}})
	require.NoError(t, err)
	require.False(t, res.OK)
	require.Equal(t, "Task not found.", res.Toast)
}

func TestCompleteTaskExec_RequiresUID(t *testing.T) {
	repo := newTaskRepo(t)
	exec := &CompleteTaskExec{Tasks: repo, Now: fixedNow("2026-05-09 10:00:00")}
	res, err := exec.Execute(context.Background(), ExecCtx{Target: map[string]any{}})
	require.NoError(t, err)
	require.False(t, res.OK)
	require.Contains(t, res.Toast, "uid")
}

func TestCompleteTaskExec_NoStore(t *testing.T) {
	exec := &CompleteTaskExec{}
	res, err := exec.Execute(context.Background(), ExecCtx{Target: map[string]any{"uid": "x"}})
	require.NoError(t, err)
	require.False(t, res.OK)
	require.Contains(t, res.Toast, "not configured")
}

// TestCompleteTaskExec_Idempotent re-completes an already-completed
// task — the executor must succeed quietly. Documents the V2.11
// behavior change from V2.6 (where the file-based flip was implicitly
// idempotent because the [x] regex matched only [ ] lines).
func TestCompleteTaskExec_Idempotent(t *testing.T) {
	repo := newTaskRepo(t)
	uid := seedTask(t, repo, "x")

	exec := &CompleteTaskExec{Tasks: repo, Now: fixedNow("2026-05-09 10:00:00")}
	_, err := exec.Execute(context.Background(), ExecCtx{Target: map[string]any{"uid": uid}})
	require.NoError(t, err)
	res, err := exec.Execute(context.Background(), ExecCtx{Target: map[string]any{"uid": uid}})
	require.NoError(t, err)
	require.True(t, res.OK)
}

func TestDeleteTaskExec_SoftDeletes(t *testing.T) {
	repo := newTaskRepo(t)
	uid := seedTask(t, repo, "Second")

	exec := &DeleteTaskExec{Tasks: repo}
	res, err := exec.Execute(context.Background(), ExecCtx{Target: map[string]any{"uid": uid}})
	require.NoError(t, err)
	require.True(t, res.OK, "delete should succeed; toast=%q", res.Toast)
	require.Equal(t, logp.KindTaskDeleted, res.EventKind)
	require.Contains(t, res.Toast, "Second")

	got, err := repo.Get(context.Background(), uid)
	require.NoError(t, err)
	require.Nil(t, got, "deleted row must be hidden from Get")
}

func TestDeleteTaskExec_NotFound(t *testing.T) {
	repo := newTaskRepo(t)
	exec := &DeleteTaskExec{Tasks: repo}
	res, err := exec.Execute(context.Background(), ExecCtx{Target: map[string]any{"uid": "missing"}})
	require.NoError(t, err)
	require.False(t, res.OK)
	require.Equal(t, "Task not found.", res.Toast)
}

func TestEditTaskExec_TitleOnly(t *testing.T) {
	repo := newTaskRepo(t)
	uid := seedTask(t, repo, "old title")

	exec := &EditTaskExec{Tasks: repo}
	res, err := exec.Execute(context.Background(), ExecCtx{
		Target: map[string]any{"uid": uid, "title": "  new title  "},
	})
	require.NoError(t, err)
	require.True(t, res.OK, "toast=%q", res.Toast)
	require.Equal(t, logp.KindTaskEdited, res.EventKind)
	require.Contains(t, res.Toast, "new title")

	got, err := repo.Get(context.Background(), uid)
	require.NoError(t, err)
	require.Equal(t, "new title", got.Title)
	require.Empty(t, got.DueDate)
}

func TestEditTaskExec_DueDateSetAndClear(t *testing.T) {
	repo := newTaskRepo(t)
	uid := seedTask(t, repo, "task")

	exec := &EditTaskExec{Tasks: repo}

	// set
	res, err := exec.Execute(context.Background(), ExecCtx{
		Target: map[string]any{"uid": uid, "due_date": "2026-05-12"},
	})
	require.NoError(t, err)
	require.True(t, res.OK, "toast=%q", res.Toast)

	got, err := repo.Get(context.Background(), uid)
	require.NoError(t, err)
	require.Equal(t, "2026-05-12", got.DueDate)
	require.Equal(t, "task", got.Title, "title must be untouched when only due_date supplied")

	// clear
	res, err = exec.Execute(context.Background(), ExecCtx{
		Target: map[string]any{"uid": uid, "due_date": ""},
	})
	require.NoError(t, err)
	require.True(t, res.OK)

	got, err = repo.Get(context.Background(), uid)
	require.NoError(t, err)
	require.Empty(t, got.DueDate)
}

func TestEditTaskExec_BothFields(t *testing.T) {
	repo := newTaskRepo(t)
	uid := seedTask(t, repo, "old")

	exec := &EditTaskExec{Tasks: repo}
	res, err := exec.Execute(context.Background(), ExecCtx{
		Target: map[string]any{"uid": uid, "title": "renamed", "due_date": "2026-05-15"},
	})
	require.NoError(t, err)
	require.True(t, res.OK)

	got, err := repo.Get(context.Background(), uid)
	require.NoError(t, err)
	require.Equal(t, "renamed", got.Title)
	require.Equal(t, "2026-05-15", got.DueDate)

	fields, _ := res.EventPayload["fields"].([]string)
	require.ElementsMatch(t, []string{"title", "due_date"}, fields)
}

func TestEditTaskExec_RequiresUID(t *testing.T) {
	repo := newTaskRepo(t)
	exec := &EditTaskExec{Tasks: repo}
	res, err := exec.Execute(context.Background(), ExecCtx{Target: map[string]any{"title": "x"}})
	require.NoError(t, err)
	require.False(t, res.OK)
	require.Contains(t, res.Toast, "uid")
}

func TestEditTaskExec_NoFields(t *testing.T) {
	repo := newTaskRepo(t)
	uid := seedTask(t, repo, "task")
	exec := &EditTaskExec{Tasks: repo}
	res, err := exec.Execute(context.Background(), ExecCtx{Target: map[string]any{"uid": uid}})
	require.NoError(t, err)
	require.False(t, res.OK)
	require.Contains(t, res.Toast, "no fields")
}

func TestEditTaskExec_EmptyTitle(t *testing.T) {
	repo := newTaskRepo(t)
	uid := seedTask(t, repo, "task")
	exec := &EditTaskExec{Tasks: repo}
	res, err := exec.Execute(context.Background(), ExecCtx{
		Target: map[string]any{"uid": uid, "title": "   "},
	})
	require.NoError(t, err)
	require.False(t, res.OK)
	require.Contains(t, res.Toast, "title")
}

func TestEditTaskExec_NotFound(t *testing.T) {
	repo := newTaskRepo(t)
	exec := &EditTaskExec{Tasks: repo}
	res, err := exec.Execute(context.Background(), ExecCtx{
		Target: map[string]any{"uid": "missing", "title": "x"},
	})
	require.NoError(t, err)
	require.False(t, res.OK)
	require.Equal(t, "Task not found.", res.Toast)
}

func TestEditTaskExec_NoStore(t *testing.T) {
	exec := &EditTaskExec{}
	res, err := exec.Execute(context.Background(), ExecCtx{
		Target: map[string]any{"uid": "x", "title": "y"},
	})
	require.NoError(t, err)
	require.False(t, res.OK)
	require.Contains(t, res.Toast, "not configured")
}
