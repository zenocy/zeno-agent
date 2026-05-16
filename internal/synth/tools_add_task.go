package synth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/zenocy/zeno-v2/internal/llm"
)

// V2.10 — add_task tool for the reactive Ask loop.
//
// When the user explicitly asks the reactive loop to add something to
// their todos ("add buy milk", "remind me to pay rent"), the model can
// call this tool to commit the task during synthesis. The task is
// appended to the configured tasks Markdown file via the same executor
// the card-action button uses (action.AddTaskExec, intent "add_task"),
// dispatched through action.Handler so the audit + outcome events are
// bit-equal to the click-path.
//
// Speculative or low-confidence adds should still be proposed via the
// existing `add_task` card-action verb — the prompt block in
// reactive.tmpl steers the model between the two paths.
//
// Note on package boundary: the action package already imports synth
// (for AskFn → synth.Card), so synth must not import back into action.
// AddTaskFn is a small callback the wiring in cmd/zeno/main.go fills in
// with a closure around action.Handler.DispatchIntent. Tests inject a
// stub function. This keeps the dependency graph one-way.

// AddTaskFn dispatches the "add_task" intent. It returns the
// executor's OK flag, the user-facing toast, and any executor error.
// The wiring layer is responsible for converting target into the
// action.DispatchInput the handler expects.
type AddTaskFn func(ctx context.Context, target map[string]any) (ok bool, toast string, err error)

const (
	addTaskTitleMax = 240
)

var (
	// ErrAddTaskInvalidInput is the sentinel for input validation errors.
	// Returned wrapped so the loop's repair flow reflects the validation
	// message back to the model rather than crashing the loop.
	ErrAddTaskInvalidInput = errors.New("add_task: invalid input")

	addTaskDateRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	addTaskTagRE  = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// AddTaskTool exposes the "add_task" intent to the LLM tool loop.
type AddTaskTool struct {
	Dispatch AddTaskFn
}

func (t *AddTaskTool) Name() string { return "add_task" }

func (t *AddTaskTool) Description() string {
	return "Add a task to the user's todo list. Call this when the user explicitly asks to add, capture, or remind them of a task. Pass fire_at when the user wants to be pinged at a specific time (\"remind me in 30 minutes\", \"ping me at 6pm\"). The task is committed immediately. Do not call this tool for speculative or suggestive adds — propose an `add_task` action button on the card instead."
}

func (t *AddTaskTool) Parameters() []llm.ToolParamSpec {
	return []llm.ToolParamSpec{
		{
			Name:        "title",
			Type:        "string",
			Description: "Task title as the user phrased it. Required. ≤240 chars. Do not include @due / #tag / !priority tokens here — pass those as separate fields.",
			Required:    true,
		},
		{
			Name:        "due",
			Type:        "string",
			Description: "Optional due date in YYYY-MM-DD.",
			Required:    false,
		},
		{
			Name:        "priority",
			Type:        "string",
			Description: "Optional priority. Defaults to med if omitted.",
			Required:    false,
			Enum:        []string{"low", "med", "high"},
		},
		{
			Name:        "tags",
			Type:        "array",
			Description: "Optional list of tag tokens (no leading #). Letters, digits, underscore, hyphen only.",
			Required:    false,
			Items:       &llm.ToolParamItems{Type: "string"},
		},
		{
			Name:        "fire_at",
			Type:        "string",
			Description: "Optional alarm time. Either RFC3339 (2026-05-12T18:30:00Z) or a relative offset like \"+30m\", \"+2h\", \"+1d\". When set, the sweeper fires the task as a reminder card at that moment.",
			Required:    false,
		},
	}
}

// addTaskResult is the JSON the tool returns to the loop. The model
// uses these fields to compose its confirmation prose; `toast` is the
// same string the UI surfaces when a card click runs add_task, so the
// reply matches the click-path UX.
type addTaskResult struct {
	OK    bool   `json:"ok"`
	Title string `json:"title"`
	Due   string `json:"due,omitempty"`
	Toast string `json:"toast"`
}

func (t *AddTaskTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	if t.Dispatch == nil {
		return "", errors.New("add_task: tool not configured (missing dispatcher)")
	}

	title := argString(args, "title")
	if title == "" {
		return "", fmt.Errorf("%w: title is required and must be a non-empty string", ErrAddTaskInvalidInput)
	}
	if len(title) > addTaskTitleMax {
		return "", fmt.Errorf("%w: title exceeds %d chars", ErrAddTaskInvalidInput, addTaskTitleMax)
	}

	target := map[string]any{"title": title}

	if due := argString(args, "due"); due != "" {
		if !addTaskDateRE.MatchString(due) {
			return "", fmt.Errorf("%w: due must be YYYY-MM-DD", ErrAddTaskInvalidInput)
		}
		if _, err := time.Parse("2006-01-02", due); err != nil {
			return "", fmt.Errorf("%w: due is not a real date", ErrAddTaskInvalidInput)
		}
		target["due"] = due
	}

	if priority := argString(args, "priority"); priority != "" {
		switch strings.ToLower(priority) {
		case "low", "med", "high":
			target["priority"] = strings.ToLower(priority)
		default:
			return "", fmt.Errorf("%w: priority must be one of low, med, high", ErrAddTaskInvalidInput)
		}
	}

	if rawTags, ok := args["tags"]; ok {
		tags, err := normalizeAddTaskTags(rawTags)
		if err != nil {
			return "", err
		}
		if len(tags) > 0 {
			target["tags"] = tags
		}
	}

	if fireAt := argString(args, "fire_at"); fireAt != "" {
		// Validate the format here so the model gets a clean error
		// instead of "Could not parse fire_at" from the executor.
		if !validAddTaskFireAt(fireAt) {
			return "", fmt.Errorf("%w: fire_at must be RFC3339 or +<N>(m|h|d), got %q", ErrAddTaskInvalidInput, fireAt)
		}
		target["fire_at"] = fireAt
	}

	ok, toast, err := t.Dispatch(ctx, target)
	if err != nil {
		// Executor-level failure (e.g. couldn't open file). Surface as a
		// tool error so the model can apologize rather than claim success.
		return "", fmt.Errorf("add_task: dispatch: %w", err)
	}
	if !ok {
		// Soft failure (e.g. tasks file not configured on this build).
		// Return the toast as the tool error so the model can quote it.
		if toast == "" {
			toast = "could not add task"
		}
		return "", fmt.Errorf("add_task: %s", toast)
	}

	out := addTaskResult{
		OK:    true,
		Title: title,
		Due:   argString(args, "due"),
		Toast: toast,
	}
	b, _ := json.Marshal(out)
	return string(b), nil
}

// validAddTaskFireAt accepts the same forms the AddTaskExec parser
// does: RFC3339 timestamps and "+<N>(m|h|d)" relative offsets. The
// executor revalidates with the actual `now` to enforce future-only;
// here we only check the shape so the model gets a fast error.
func validAddTaskFireAt(raw string) bool {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "+") {
		// Permit "+30m" / "+2h" / "+1d" — the executor calls
		// store.ParseRelative which is the source of truth.
		return len(raw) > 1
	}
	_, err := time.Parse(time.RFC3339, raw)
	return err == nil
}

// normalizeAddTaskTags coerces the LLM's tags arg (which arrives from
// JSON as []any of strings) into a []string, stripping leading "#" and
// rejecting tokens that wouldn't parse cleanly back from the Markdown.
func normalizeAddTaskTags(raw any) ([]string, error) {
	arr, ok := raw.([]any)
	if !ok {
		// Some clients send a single string instead of an array. Be
		// permissive: treat one string as a one-element list.
		if s, sok := raw.(string); sok {
			arr = []any{s}
		} else {
			return nil, fmt.Errorf("%w: tags must be an array of strings", ErrAddTaskInvalidInput)
		}
	}
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		s, sok := v.(string)
		if !sok {
			return nil, fmt.Errorf("%w: tags must be strings", ErrAddTaskInvalidInput)
		}
		s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "#"))
		if s == "" {
			continue
		}
		if !addTaskTagRE.MatchString(s) {
			return nil, fmt.Errorf("%w: tag %q must match [A-Za-z0-9_-]+", ErrAddTaskInvalidInput, s)
		}
		out = append(out, s)
	}
	return out, nil
}
