package projection

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/store"
)

// OpenTasksMax caps the slice this projection returns. The cards/reactive
// loops never want hundreds; the UI rail never renders more than a handful.
const OpenTasksMax = 50

// OpenTasksTask is one row of the OpenTasks projection. JSON shape is
// preserved across the V2.11 cutover so the UI keeps deserializing the
// same fields. Fields originally sourced from the V2.6 sensor's Task
// struct now come from the V2.11 store.Task row.
type OpenTasksTask struct {
	UID          string     `json:"uid"`
	Title        string     `json:"title"`
	Body         string     `json:"body,omitempty"`
	Completed    bool       `json:"completed"`
	DueDate      string     `json:"due_date,omitempty"`
	DoneDate     string     `json:"done_date,omitempty"`
	Priority     string     `json:"priority"`
	Tags         []string   `json:"tags,omitempty"`
	FireAt       *time.Time `json:"fire_at,omitempty"`
	FiredAt      *time.Time `json:"fired_at,omitempty"`
	SourceCardID string     `json:"source_card_id,omitempty"`
}

// OpenTasks reads the V2.11 unified tasks table and returns rows that
// are open or completed today. Sort: alarms-due-soon → overdue → today
// → soon → no-date → completed-today. Priority breaks ties (high → med
// → low). Capped at OpenTasksMax.
//
// The Tasks field is required; Compute returns ErrTasksRepoNotConfigured
// when nil so a partial wiring fails noisily instead of silently
// returning an empty slice.
type OpenTasks struct {
	Cfg   Config
	Tasks *store.TaskRepo
}

// Name returns the projection identifier.
func (p OpenTasks) Name() string { return "open_tasks" }

// Compute queries the tasks repo (filter=all so completed-today rows
// land in the same fold), applies the V2.6 sort + cap, and returns the
// shape callers consumed under V2.6/V2.10.
//
// The log.Reader argument is preserved for interface compatibility with
// other projections in this package but is not used by V2.11 — task
// state lives in the SQLite table, not the event log.
func (p OpenTasks) Compute(ctx context.Context, _ log.Reader) ([]OpenTasksTask, error) {
	if p.Tasks == nil {
		return nil, ErrTasksRepoNotConfigured
	}
	now := p.Cfg.now()
	tz := p.Cfg.tz()
	today := now.In(tz).Format("2006-01-02")

	rows, err := p.Tasks.List(ctx, store.TaskFilter{Status: "all", Today: today})
	if err != nil {
		return nil, err
	}

	out := make([]OpenTasksTask, 0, len(rows))
	for _, r := range rows {
		if r.Completed && (r.CompletedAt == nil || r.CompletedAt.In(tz).Format("2006-01-02") != today) {
			continue
		}
		out = append(out, taskToProjection(r))
	}

	sort.SliceStable(out, func(i, j int) bool {
		return rankTask(out[i], today) < rankTask(out[j], today)
	})

	if len(out) > OpenTasksMax {
		out = out[:OpenTasksMax]
	}
	return out, nil
}

// ErrTasksRepoNotConfigured surfaces a missing wiring at boot time.
// Returned by Compute when OpenTasks.Tasks is nil. Callers in production
// always wire the repo, so reaching this error means a bug.
var ErrTasksRepoNotConfigured = errTasksRepoNotConfigured()

func errTasksRepoNotConfigured() error {
	return projectionErr{"open_tasks: TaskRepo is required"}
}

type projectionErr struct{ msg string }

func (e projectionErr) Error() string { return e.msg }

// taskToProjection converts a store.Task into the OpenTasksTask shape
// the rest of the codebase expects. The doneDate field is derived from
// CompletedAt (formatted in the projection's TZ); tags arrive as JSON
// from the repo and are unmarshalled into a []string here so callers
// don't need to know about the storage shape.
func taskToProjection(t store.Task) OpenTasksTask {
	out := OpenTasksTask{
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

// rankTask produces a sortable composite key (lower = surface first).
// Bands: 0=alarm-due, 1=overdue, 2=due today, 3=due in next 3 days,
// 4=no due date, 5=completed today. Within a band, priority high < med
// < low.
func rankTask(t OpenTasksTask, today string) int {
	band := bandFor(t, today)
	pri := priorityRank(t.Priority)
	return band*10000 + pri*1000 + dueOffset(t.DueDate, today)
}

func bandFor(t OpenTasksTask, today string) int {
	if t.FireAt != nil && t.FiredAt == nil {
		return 0
	}
	if t.Completed {
		return 5
	}
	if t.DueDate == "" {
		return 4
	}
	if t.DueDate < today {
		return 1
	}
	if t.DueDate == today {
		return 2
	}
	if dueOffset(t.DueDate, today) <= 3 {
		return 3
	}
	return 4
}

func priorityRank(p string) int {
	switch p {
	case "high":
		return 0
	case "med", "":
		return 1
	case "low":
		return 2
	default:
		return 1
	}
}

// dueOffset returns days(due - today). Negative means overdue. Returns 0
// when either side fails to parse so malformed input never sorts wildly.
func dueOffset(due, today string) int {
	const layout = "2006-01-02"
	d, err := time.Parse(layout, due)
	if err != nil {
		return 0
	}
	t, err := time.Parse(layout, today)
	if err != nil {
		return 0
	}
	return int(d.Sub(t) / (24 * time.Hour))
}
