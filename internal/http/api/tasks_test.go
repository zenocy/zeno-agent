package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/action"
	logp "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/store"
)

// noopLog satisfies logp.Writer for tests that don't care about events.
type noopLog struct{}

func (noopLog) Append(_ context.Context, _, _ string, _ any) (logp.Event, error) {
	return logp.Event{}, nil
}

// newTasksTestRig builds a real Echo + TasksHandler backed by a real
// action.Handler + executors that write to an in-memory SQLite DB.
// V2.11: replaces the V2.6 tempfile-backed rig.
func newTasksTestRig(t *testing.T) (*echo.Echo, *store.TaskRepo) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_journal_mode=WAL&_foreign_keys=on"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	repo := &store.TaskRepo{DB: db}
	require.NoError(t, repo.Migrate())

	now := func() time.Time { return time.Date(2026, 5, 9, 10, 0, 0, 0, time.UTC) }

	reg := action.NewRegistry()
	reg.Register("add_task", &action.AddTaskExec{Tasks: repo, Now: now})
	reg.Register("complete_task", &action.CompleteTaskExec{Tasks: repo, Now: now})
	reg.Register("delete_task", &action.DeleteTaskExec{Tasks: repo, Now: now})
	reg.Register("edit_task", &action.EditTaskExec{Tasks: repo})
	reg.Register("set_reminder", &action.SetReminderExec{
		Tasks: repo,
		TZ:    func() *time.Location { return time.UTC },
	})

	ah := &action.Handler{
		Registry: reg,
		EventLog: noopLog{},
		TZ:       tzUTC,
		Now:      now,
		Log:      quietHandlerEntry(),
	}

	e := echo.New()
	(&TasksHandler{
		Action: ah,
		Tasks:  repo,
		Log:    quietHandlerEntry(),
	}).Register(e)
	return e, repo
}

func TestTasksHandler_List_Empty(t *testing.T) {
	e, _ := newTasksTestRig(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	e.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, "[]", strings.TrimSpace(rr.Body.String()))
}

func TestTasksHandler_Create_InsertsRow(t *testing.T) {
	e, repo := newTasksTestRig(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tasks",
		strings.NewReader(`{"title":"Buy milk","due":"2026-05-10","priority":"high","tags":["errand"]}`))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())

	rows, err := repo.List(context.Background(), store.TaskFilter{Status: "all"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "Buy milk", rows[0].Title)
	require.Equal(t, "2026-05-10", rows[0].DueDate)
	require.Equal(t, "high", rows[0].Priority)
}

func TestTasksHandler_Create_FireAt(t *testing.T) {
	e, repo := newTasksTestRig(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tasks",
		strings.NewReader(`{"title":"kettle","fire_at":"+1h"}`))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())

	rows, err := repo.List(context.Background(), store.TaskFilter{Status: "has_alarm"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "kettle", rows[0].Title)
	require.NotNil(t, rows[0].FireAt)
}

func TestTasksHandler_Create_RejectsEmptyTitle(t *testing.T) {
	e, _ := newTasksTestRig(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tasks",
		strings.NewReader(`{"title":""}`))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(rr, req)
	require.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestTasksHandler_Complete_FlipsCompleted(t *testing.T) {
	e, repo := newTasksTestRig(t)
	uid := uuid.NewString()
	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: uid, Title: "Send invoice"}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/"+uid+"/complete", nil)
	e.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())

	got, err := repo.Get(context.Background(), uid)
	require.NoError(t, err)
	require.True(t, got.Completed)
}

func TestTasksHandler_Complete_NotFound(t *testing.T) {
	e, _ := newTasksTestRig(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/deadbeef/complete", nil)
	e.ServeHTTP(rr, req)
	require.Equal(t, http.StatusBadRequest, rr.Code)
	var body map[string]string
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	require.Contains(t, body["error"], "not found")
}

func TestTasksHandler_Delete_SoftDeletes(t *testing.T) {
	e, repo := newTasksTestRig(t)
	uid := uuid.NewString()
	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: uid, Title: "Second"}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/tasks/"+uid, nil)
	e.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())

	got, err := repo.Get(context.Background(), uid)
	require.NoError(t, err)
	require.Nil(t, got)
}

// TestTasksHandler_Reminder_UpdatesFireAt pins the V2.11 contract: the
// reminder route updates the task's fire_at column instead of inserting
// a separate reminders row.
func TestTasksHandler_Reminder_UpdatesFireAt(t *testing.T) {
	e, repo := newTasksTestRig(t)
	uid := uuid.NewString()
	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: uid, Title: "follow up"}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/"+uid+"/reminder",
		strings.NewReader(`{"when":"+1h"}`))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())

	got, err := repo.Get(context.Background(), uid)
	require.NoError(t, err)
	require.NotNil(t, got.FireAt, "reminder route must update the existing row's fire_at")

	// No duplicate row was inserted.
	all, err := repo.List(context.Background(), store.TaskFilter{Status: "all"})
	require.NoError(t, err)
	require.Len(t, all, 1)
}

func TestTasksHandler_Reminder_RequiresExistingTask(t *testing.T) {
	e, _ := newTasksTestRig(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/missing/reminder",
		strings.NewReader(`{"when":"+1h"}`))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(rr, req)
	require.Equal(t, http.StatusNotFound, rr.Code)
}

func TestTasksHandler_Edit_RenameAndDue(t *testing.T) {
	e, repo := newTasksTestRig(t)
	uid := uuid.NewString()
	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: uid, Title: "old"}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/tasks/"+uid,
		strings.NewReader(`{"title":"new","due_date":"2026-05-15"}`))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())

	got, err := repo.Get(context.Background(), uid)
	require.NoError(t, err)
	require.Equal(t, "new", got.Title)
	require.Equal(t, "2026-05-15", got.DueDate)
}

func TestTasksHandler_Edit_TitleOnly(t *testing.T) {
	e, repo := newTasksTestRig(t)
	uid := uuid.NewString()
	require.NoError(t, repo.Insert(context.Background(), store.Task{
		ID: uid, Title: "old", DueDate: "2026-05-12",
	}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/tasks/"+uid,
		strings.NewReader(`{"title":"renamed"}`))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())

	got, err := repo.Get(context.Background(), uid)
	require.NoError(t, err)
	require.Equal(t, "renamed", got.Title)
	require.Equal(t, "2026-05-12", got.DueDate, "due_date must survive a title-only PATCH")
}

func TestTasksHandler_Edit_ClearsDueDate(t *testing.T) {
	e, repo := newTasksTestRig(t)
	uid := uuid.NewString()
	require.NoError(t, repo.Insert(context.Background(), store.Task{
		ID: uid, Title: "task", DueDate: "2026-05-12",
	}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/tasks/"+uid,
		strings.NewReader(`{"due_date":""}`))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())

	got, err := repo.Get(context.Background(), uid)
	require.NoError(t, err)
	require.Empty(t, got.DueDate)
}

func TestTasksHandler_Edit_RejectsEmptyBody(t *testing.T) {
	e, repo := newTasksTestRig(t)
	uid := uuid.NewString()
	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: uid, Title: "x"}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/tasks/"+uid,
		strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(rr, req)
	require.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestTasksHandler_Edit_NotFound(t *testing.T) {
	e, _ := newTasksTestRig(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/tasks/deadbeef",
		strings.NewReader(`{"title":"new"}`))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(rr, req)
	require.Equal(t, http.StatusBadRequest, rr.Code)
	var body map[string]string
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	require.Contains(t, body["error"], "not found")
}
