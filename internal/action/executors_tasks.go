package action

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	logp "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/store"
)

// V2.11 — task executors back the unified SQLite tasks table. Replaces
// the V2.6 Markdown-rewriting executors that wrote to ~/zeno-tasks.md.
// CompleteTaskExec flips Completed=true; DeleteTaskExec soft-deletes.
// Both audit through the same KindTaskStatusChanged / KindTaskDeleted
// event kinds so dashboards continue to grep cleanly.

// ----------------------------------------------------------------------
// CompleteTaskExec — mark task as completed.
// ----------------------------------------------------------------------

type CompleteTaskExec struct {
	Tasks *store.TaskRepo
	Now   func() time.Time
}

func (e *CompleteTaskExec) Mode() Mode { return Mode1Click }

func (e *CompleteTaskExec) Execute(ctx context.Context, ec ExecCtx) (Result, error) {
	if e.Tasks == nil {
		return Result{OK: false, Toast: "Tasks store not configured."}, nil
	}
	uid := strings.TrimSpace(stringFromTarget(ec.Target, "uid"))
	if uid == "" {
		return Result{OK: false, Toast: "target.uid is required."}, nil
	}

	now := time.Now()
	if e.Now != nil {
		now = e.Now()
	}
	if !ec.Now.IsZero() {
		now = ec.Now
	}

	cur, err := e.Tasks.Get(ctx, uid)
	if err != nil {
		return Result{OK: false, Toast: "Could not look up task."}, err
	}
	if cur == nil {
		return Result{OK: false, Toast: "Task not found."}, nil
	}

	if err := e.Tasks.Complete(ctx, uid, now); err != nil {
		if errors.Is(err, store.ErrTaskNotFound) {
			return Result{OK: false, Toast: "Task not found."}, nil
		}
		return Result{OK: false, Toast: "Could not complete task."}, err
	}

	return Result{
		OK:        true,
		EventKind: logp.KindTaskStatusChanged,
		EventPayload: map[string]any{
			"uid":    uid,
			"title":  cur.Title,
			"change": "completed",
			"source": "ui",
		},
		Toast: fmt.Sprintf("Done: %s", cur.Title),
	}, nil
}

// ----------------------------------------------------------------------
// DeleteTaskExec — soft-delete a task row.
// ----------------------------------------------------------------------

type DeleteTaskExec struct {
	Tasks *store.TaskRepo
	Now   func() time.Time
}

func (e *DeleteTaskExec) Mode() Mode { return Mode1Click }

func (e *DeleteTaskExec) Execute(ctx context.Context, ec ExecCtx) (Result, error) {
	if e.Tasks == nil {
		return Result{OK: false, Toast: "Tasks store not configured."}, nil
	}
	uid := strings.TrimSpace(stringFromTarget(ec.Target, "uid"))
	if uid == "" {
		return Result{OK: false, Toast: "target.uid is required."}, nil
	}

	cur, err := e.Tasks.Get(ctx, uid)
	if err != nil {
		return Result{OK: false, Toast: "Could not look up task."}, err
	}
	if cur == nil {
		return Result{OK: false, Toast: "Task not found."}, nil
	}

	if err := e.Tasks.Delete(ctx, uid); err != nil {
		if errors.Is(err, store.ErrTaskNotFound) {
			return Result{OK: false, Toast: "Task not found."}, nil
		}
		return Result{OK: false, Toast: "Could not delete task."}, err
	}

	return Result{
		OK:        true,
		EventKind: logp.KindTaskDeleted,
		EventPayload: map[string]any{
			"uid":    uid,
			"title":  cur.Title,
			"source": "ui",
		},
		Toast: fmt.Sprintf("Deleted: %s", cur.Title),
	}, nil
}

// ----------------------------------------------------------------------
// EditTaskExec — update title and/or due_date on an existing task.
// Empty-string due_date clears the column.
// ----------------------------------------------------------------------

type EditTaskExec struct {
	Tasks *store.TaskRepo
}

func (e *EditTaskExec) Mode() Mode { return Mode1Click }

func (e *EditTaskExec) Execute(ctx context.Context, ec ExecCtx) (Result, error) {
	if e.Tasks == nil {
		return Result{OK: false, Toast: "Tasks store not configured."}, nil
	}
	uid := strings.TrimSpace(stringFromTarget(ec.Target, "uid"))
	if uid == "" {
		return Result{OK: false, Toast: "target.uid is required."}, nil
	}

	updates := map[string]any{}
	changed := []string{}

	if v, ok := ec.Target["title"]; ok {
		s, _ := v.(string)
		s = strings.TrimSpace(s)
		if s == "" {
			return Result{OK: false, Toast: "target.title cannot be empty."}, nil
		}
		updates["title"] = s
		changed = append(changed, "title")
	}

	if v, ok := ec.Target["due_date"]; ok {
		s, _ := v.(string)
		updates["due_date"] = strings.TrimSpace(s)
		changed = append(changed, "due_date")
	}

	if len(updates) == 0 {
		return Result{OK: false, Toast: "no fields to update."}, nil
	}

	cur, err := e.Tasks.Get(ctx, uid)
	if err != nil {
		return Result{OK: false, Toast: "Could not look up task."}, err
	}
	if cur == nil {
		return Result{OK: false, Toast: "Task not found."}, nil
	}

	if err := e.Tasks.Update(ctx, uid, updates); err != nil {
		if errors.Is(err, store.ErrTaskNotFound) {
			return Result{OK: false, Toast: "Task not found."}, nil
		}
		return Result{OK: false, Toast: "Could not update task."}, err
	}

	title := cur.Title
	if t, ok := updates["title"].(string); ok && t != "" {
		title = t
	}

	return Result{
		OK:        true,
		EventKind: logp.KindTaskEdited,
		EventPayload: map[string]any{
			"uid":    uid,
			"title":  title,
			"change": "edited",
			"fields": changed,
			"source": "ui",
		},
		Toast: fmt.Sprintf("Updated: %s", title),
	}, nil
}
