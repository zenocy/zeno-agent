package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/action"
	"github.com/zenocy/zeno-v2/internal/eventbus"
	"github.com/zenocy/zeno-v2/internal/projection"
	"github.com/zenocy/zeno-v2/internal/store"
)

// TasksHandler exposes UI-facing CRUD on the V2.11 unified tasks
// table. Replaces the V2.6 file-backed handler.
//
// All mutations route through Action.DispatchIntent so the same audit
// + outcome event rows fire whether the user clicks a card button or
// a panel button.
type TasksHandler struct {
	Action *action.Handler
	Tasks  *store.TaskRepo
	Bus    *eventbus.Bus
	Log    *logrus.Entry
}

// Register wires the routes onto the Echo instance.
func (h *TasksHandler) Register(e *echo.Echo) {
	e.GET("/api/tasks", h.list)
	e.POST("/api/tasks", h.create)
	e.POST("/api/tasks/:uid/complete", h.complete)
	e.DELETE("/api/tasks/:uid", h.delete)
	e.POST("/api/tasks/:uid/reminder", h.reminder)
	e.PATCH("/api/tasks/:uid", h.edit)
}

func (h *TasksHandler) list(c echo.Context) error {
	rows, err := h.Tasks.List(c.Request().Context(), store.TaskFilter{Status: "all"})
	if err != nil {
		return Internal(c, err)
	}
	out := make([]projection.OpenTasksTask, 0, len(rows))
	for _, r := range rows {
		out = append(out, taskRowToDTO(r))
	}
	return c.JSON(http.StatusOK, out)
}

type taskCreateReq struct {
	Title    string   `json:"title"`
	Body     string   `json:"body,omitempty"`
	Due      string   `json:"due,omitempty"`
	Priority string   `json:"priority,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	FireAt   string   `json:"fire_at,omitempty"`
}

func (h *TasksHandler) create(c echo.Context) error {
	var req taskCreateReq
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "invalid body")
	}
	if strings.TrimSpace(req.Title) == "" {
		return BadRequest(c, "title is required")
	}
	target := map[string]any{
		"title":    req.Title,
		"body":     req.Body,
		"due":      req.Due,
		"priority": req.Priority,
		"tags":     req.Tags,
	}
	if req.FireAt != "" {
		target["fire_at"] = req.FireAt
	}
	result, _ := h.Action.DispatchIntent(c.Request().Context(), action.DispatchInput{
		Intent: "add_task",
		Target: target,
	})
	if !result.OK {
		return BadRequest(c, result.Toast)
	}
	uid, _ := result.EventPayload["uid"].(string)
	if h.Bus != nil && uid != "" {
		if t, ok := h.fetchTask(c, uid); ok {
			h.Bus.Publish(eventbus.TaskCreatedEvent{Task: t})
		}
	}
	return c.JSON(http.StatusOK, result)
}

func (h *TasksHandler) complete(c echo.Context) error {
	uid := c.Param("uid")
	if uid == "" {
		return BadRequest(c, "uid is required")
	}
	result, _ := h.Action.DispatchIntent(c.Request().Context(), action.DispatchInput{
		Intent: "complete_task",
		Target: map[string]any{"uid": uid},
	})
	if !result.OK {
		return BadRequest(c, result.Toast)
	}
	if h.Bus != nil {
		if t, ok := h.fetchTask(c, uid); ok {
			h.Bus.Publish(eventbus.TaskCompletedEvent{Task: t})
		}
	}
	return c.JSON(http.StatusOK, result)
}

func (h *TasksHandler) delete(c echo.Context) error {
	uid := c.Param("uid")
	if uid == "" {
		return BadRequest(c, "uid is required")
	}
	result, _ := h.Action.DispatchIntent(c.Request().Context(), action.DispatchInput{
		Intent: "delete_task",
		Target: map[string]any{"uid": uid},
	})
	if !result.OK {
		return BadRequest(c, result.Toast)
	}
	if h.Bus != nil {
		h.Bus.Publish(eventbus.TaskDeletedEvent{UID: uid})
	}
	return c.JSON(http.StatusOK, result)
}

type reminderReq struct {
	When string `json:"when"` // RFC3339 or +<N>(m|h|d)
	Body string `json:"body,omitempty"`
}

// reminder attaches an alarm to an existing task. V2.11: previously
// inserted a separate reminders row; now updates the task's fire_at via
// SetReminderExec's update mode (target.task_uid).
func (h *TasksHandler) reminder(c echo.Context) error {
	uid := c.Param("uid")
	if uid == "" {
		return BadRequest(c, "uid is required")
	}
	var req reminderReq
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "invalid body")
	}
	if strings.TrimSpace(req.When) == "" {
		return BadRequest(c, "when is required")
	}
	if t, ok := h.fetchTask(c, uid); !ok || t.UID == "" {
		return NotFound(c, "task not found")
	}

	result, _ := h.Action.DispatchIntent(c.Request().Context(), action.DispatchInput{
		Intent: "set_reminder",
		Target: map[string]any{
			"when":     req.When,
			"task_uid": uid,
			"body":     req.Body,
		},
	})
	if !result.OK {
		return BadRequest(c, result.Toast)
	}
	if h.Bus != nil {
		if t, ok := h.fetchTask(c, uid); ok {
			h.Bus.Publish(eventbus.TaskReminderSetEvent{Task: t})
		}
	}
	return c.JSON(http.StatusOK, result)
}

type taskEditReq struct {
	Title   *string `json:"title,omitempty"`
	DueDate *string `json:"due_date,omitempty"`
}

// edit updates the title and/or due_date on an existing task. At least
// one field must be supplied. Empty-string due_date clears the column.
func (h *TasksHandler) edit(c echo.Context) error {
	uid := c.Param("uid")
	if uid == "" {
		return BadRequest(c, "uid is required")
	}
	var req taskEditReq
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "invalid body")
	}
	if req.Title == nil && req.DueDate == nil {
		return BadRequest(c, "at least one of title or due_date is required")
	}

	target := map[string]any{"uid": uid}
	if req.Title != nil {
		target["title"] = *req.Title
	}
	if req.DueDate != nil {
		target["due_date"] = *req.DueDate
	}

	result, _ := h.Action.DispatchIntent(c.Request().Context(), action.DispatchInput{
		Intent: "edit_task",
		Target: target,
	})
	if !result.OK {
		return BadRequest(c, result.Toast)
	}
	if h.Bus != nil {
		if t, ok := h.fetchTask(c, uid); ok {
			h.Bus.Publish(eventbus.TaskEditedEvent{Task: t})
		}
	}
	return c.JSON(http.StatusOK, result)
}

// fetchTask returns the projection-shape DTO for one task. Returns ok=false
// when the row is missing — callers skip publishing the SSE event in
// that case (durable state still survives in the table).
func (h *TasksHandler) fetchTask(c echo.Context, uid string) (projection.OpenTasksTask, bool) {
	if h.Tasks == nil {
		return projection.OpenTasksTask{}, false
	}
	row, err := h.Tasks.Get(c.Request().Context(), uid)
	if err != nil || row == nil {
		return projection.OpenTasksTask{}, false
	}
	return taskRowToDTO(*row), true
}

// taskRowToDTO converts a store.Task into the projection.OpenTasksTask
// JSON shape the UI consumes. The /api/tasks panel and the
// /api/projections/tasks/open route use the same shape.
func taskRowToDTO(t store.Task) projection.OpenTasksTask {
	out := projection.OpenTasksTask{
		UID:          t.ID,
		Title:        t.Title,
		Body:         t.Body,
		Completed:    t.Completed,
		DueDate:      t.DueDate,
		Priority:     t.Priority,
		FireAt:       t.FireAt,
		FiredAt:      t.FiredAt,
		SourceCardID: t.SourceCardID,
	}
	if t.CompletedAt != nil {
		out.DoneDate = t.CompletedAt.UTC().Format("2006-01-02")
	}
	if len(t.Tags) > 0 {
		var tags []string
		if err := json.Unmarshal(t.Tags, &tags); err == nil {
			out.Tags = tags
		}
	}
	return out
}
