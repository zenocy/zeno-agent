package synth

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/zenocy/zeno-v2/internal/llm"
	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/projection"
	"github.com/zenocy/zeno-v2/internal/store"
)

// ReadTasksTool exposes the OpenTasks projection to the LLM cards/reactive
// loops. The tool returns a short human-readable summary (not raw JSON) so
// token budget stays calm and the model has the same prose-first surface
// it gets for the email/calendar tools.
type ReadTasksTool struct {
	Tasks  *store.TaskRepo
	Reader log.Reader // unused as of V2.11; retained so legacy wiring compiles
	Now    func() time.Time
	TZ     *time.Location
}

func (t *ReadTasksTool) Name() string { return "read_tasks" }

func (t *ReadTasksTool) Description() string {
	return "List the user's open tasks (and tasks completed today). Filter by status, tag, and limit. Tasks come from the local-Markdown task file the user maintains."
}

func (t *ReadTasksTool) Parameters() []llm.ToolParamSpec {
	return []llm.ToolParamSpec{
		{
			Name:        "filter",
			Type:        "string",
			Description: "open | due_today | overdue | completed_today | has_alarm; default open. has_alarm returns tasks with an unfired fire_at — \"what's coming up\".",
			Required:    false,
		},
		{
			Name:        "tag",
			Type:        "string",
			Description: "Restrict to tasks carrying this #tag (case-insensitive, no leading #).",
			Required:    false,
		},
		{
			Name:        "limit",
			Type:        "integer",
			Description: "Max rows to return; default 5, max 20.",
			Required:    false,
		},
	}
}

// Execute reuses the OpenTasks projection (so HTTP, eval, and tool paths
// agree on the dedupe + ordering rules) and applies filter/tag/limit on top.
func (t *ReadTasksTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	filter := strings.ToLower(strings.TrimSpace(argString(args, "filter")))
	if filter == "" {
		filter = "open"
	}
	switch filter {
	case "open", "due_today", "overdue", "completed_today", "has_alarm":
	default:
		return "", fmt.Errorf("filter %q must be one of: open, due_today, overdue, completed_today, has_alarm", filter)
	}
	tag := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(argString(args, "tag")), "#"))

	limit := 5
	if v, ok := args["limit"]; ok {
		switch x := v.(type) {
		case float64:
			limit = int(x)
		case int:
			limit = x
		case int64:
			limit = int(x)
		}
	}
	if limit <= 0 {
		limit = 5
	}
	if limit > 20 {
		limit = 20
	}

	tz := t.TZ
	if tz == nil {
		tz = time.UTC
	}
	now := t.now()
	today := now.In(tz).Format("2006-01-02")

	cfg := projection.Config{
		TZ:           tz,
		LookbackDays: 30,
		Now:          func() time.Time { return now },
	}
	tasks, err := projection.OpenTasks{Cfg: cfg, Tasks: t.Tasks}.Compute(ctx, t.Reader)
	if err != nil {
		return "", err
	}

	filtered := make([]projection.OpenTasksTask, 0, len(tasks))
	for _, ts := range tasks {
		if !filterMatch(ts, filter, today) {
			continue
		}
		if tag != "" && !hasTag(ts.Tags, tag) {
			continue
		}
		filtered = append(filtered, ts)
		if len(filtered) >= limit {
			break
		}
	}

	if len(filtered) == 0 {
		return formatEmptyTasksResult(filter, tag), nil
	}
	return formatTasksList(filtered, today), nil
}

func (t *ReadTasksTool) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

func filterMatch(t projection.OpenTasksTask, filter, today string) bool {
	switch filter {
	case "open":
		return !t.Completed
	case "due_today":
		return !t.Completed && t.DueDate == today
	case "overdue":
		return !t.Completed && t.DueDate != "" && t.DueDate < today
	case "has_alarm":
		return t.FireAt != nil && t.FiredAt == nil
	case "completed_today":
		return t.Completed && t.DoneDate == today
	}
	return false
}

func hasTag(tags []string, want string) bool {
	for _, tg := range tags {
		if strings.ToLower(tg) == want {
			return true
		}
	}
	return false
}

func formatEmptyTasksResult(filter, tag string) string {
	if tag != "" {
		return fmt.Sprintf("No tasks match filter=%s with #%s.", filter, tag)
	}
	return fmt.Sprintf("No tasks match filter=%s.", filter)
}

func formatTasksList(tasks []projection.OpenTasksTask, today string) string {
	overdueN := 0
	for _, ts := range tasks {
		if !ts.Completed && ts.DueDate != "" && ts.DueDate < today {
			overdueN++
		}
	}
	noun := "task"
	if len(tasks) != 1 {
		noun = "tasks"
	}
	header := fmt.Sprintf("%d %s", len(tasks), noun)
	if overdueN > 0 {
		header += fmt.Sprintf(" (%d overdue)", overdueN)
	}
	header += ":"

	var b strings.Builder
	b.WriteString(header)
	for _, ts := range tasks {
		b.WriteString("\n- ")
		b.WriteString(taskBadge(ts, today))
		b.WriteString(" ")
		b.WriteString(ts.Title)
		extras := taskExtras(ts)
		if extras != "" {
			b.WriteString(" (")
			b.WriteString(extras)
			b.WriteString(")")
		}
	}
	return capOutput(b.String(), 4096)
}

func taskBadge(t projection.OpenTasksTask, today string) string {
	if t.Completed && t.DoneDate == today {
		return "[done today]"
	}
	if t.DueDate == "" {
		return "[no date]"
	}
	if t.DueDate == today {
		return "[today]"
	}
	if t.DueDate < today {
		days := dueOffsetDays(today, t.DueDate)
		if days == 1 {
			return "[overdue 1d]"
		}
		return fmt.Sprintf("[overdue %dd]", days)
	}
	days := dueOffsetDays(t.DueDate, today)
	if days == 1 {
		return "[in 1d]"
	}
	return fmt.Sprintf("[in %dd]", days)
}

// dueOffsetDays returns |a - b| in days. Both arguments are YYYY-MM-DD;
// returns 0 if either fails to parse.
func dueOffsetDays(a, b string) int {
	const layout = "2006-01-02"
	at, err := time.Parse(layout, a)
	if err != nil {
		return 0
	}
	bt, err := time.Parse(layout, b)
	if err != nil {
		return 0
	}
	d := int(at.Sub(bt) / (24 * time.Hour))
	if d < 0 {
		d = -d
	}
	return d
}

func taskExtras(t projection.OpenTasksTask) string {
	parts := make([]string, 0, 2)
	if len(t.Tags) > 0 {
		hashed := make([]string, len(t.Tags))
		for i, tg := range t.Tags {
			hashed[i] = "#" + tg
		}
		parts = append(parts, strings.Join(hashed, " "))
	}
	if t.Priority != "" && t.Priority != "med" {
		parts = append(parts, t.Priority)
	}
	return strings.Join(parts, ", ")
}
