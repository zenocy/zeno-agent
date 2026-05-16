package synth

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// stubAddTask records the target the tool dispatched and returns a
// canned ok/toast/err triple — exactly the shape the cmd/zeno wiring
// closure produces. Tests use this to drive AddTaskTool through every
// validation + dispatch branch without needing a real action.Handler.
type stubAddTask struct {
	calls []map[string]any
	ok    bool
	toast string
	err   error
}

func (s *stubAddTask) fn(_ context.Context, target map[string]any) (bool, string, error) {
	cp := make(map[string]any, len(target))
	for k, v := range target {
		cp[k] = v
	}
	s.calls = append(s.calls, cp)
	return s.ok, s.toast, s.err
}

func TestAddTaskTool_HappyPath_TitleOnly(t *testing.T) {
	stub := &stubAddTask{ok: true, toast: "Added task: buy milk"}
	tool := &AddTaskTool{Dispatch: stub.fn}

	out, err := tool.Execute(context.Background(), map[string]any{"title": "buy milk"})
	require.NoError(t, err)

	var got addTaskResult
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.True(t, got.OK)
	require.Equal(t, "buy milk", got.Title)
	require.Equal(t, "", got.Due)
	require.Equal(t, "Added task: buy milk", got.Toast)

	require.Len(t, stub.calls, 1)
	require.Equal(t, "buy milk", stub.calls[0]["title"])
	require.NotContains(t, stub.calls[0], "due")
	require.NotContains(t, stub.calls[0], "priority")
	require.NotContains(t, stub.calls[0], "tags")
}

// TestAddTaskTool_HappyPath_AllFields confirms that due / priority /
// tags survive the tool's coercion and reach the dispatcher in the
// shape AddTaskExec expects (string, string, []string).
func TestAddTaskTool_HappyPath_AllFields(t *testing.T) {
	stub := &stubAddTask{ok: true, toast: "Added task: pay rent"}
	tool := &AddTaskTool{Dispatch: stub.fn}

	out, err := tool.Execute(context.Background(), map[string]any{
		"title":    "pay rent",
		"due":      "2026-05-15",
		"priority": "high",
		"tags":     []any{"home", "#bills"}, // leading # should be stripped
	})
	require.NoError(t, err)

	var got addTaskResult
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Equal(t, "2026-05-15", got.Due)

	require.Len(t, stub.calls, 1)
	target := stub.calls[0]
	require.Equal(t, "pay rent", target["title"])
	require.Equal(t, "2026-05-15", target["due"])
	require.Equal(t, "high", target["priority"])
	require.Equal(t, []string{"home", "bills"}, target["tags"])
}

// TestAddTaskTool_NoDispatcher catches misconfigured wiring (a nil
// Dispatch closure) up front rather than letting it surface as a nil-
// pointer panic inside the loop.
func TestAddTaskTool_NoDispatcher(t *testing.T) {
	tool := &AddTaskTool{Dispatch: nil}
	_, err := tool.Execute(context.Background(), map[string]any{"title": "x"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing dispatcher")
}

func TestAddTaskTool_ValidationErrors(t *testing.T) {
	cases := []struct {
		name    string
		args    map[string]any
		wantSub string
	}{
		{
			name:    "missing title",
			args:    map[string]any{},
			wantSub: "title is required",
		},
		{
			name:    "empty title",
			args:    map[string]any{"title": "   "},
			wantSub: "title is required",
		},
		{
			name:    "title over budget",
			args:    map[string]any{"title": strings.Repeat("a", addTaskTitleMax+1)},
			wantSub: "exceeds",
		},
		{
			name:    "due wrong shape",
			args:    map[string]any{"title": "x", "due": "tomorrow"},
			wantSub: "YYYY-MM-DD",
		},
		{
			name:    "due not a real date",
			args:    map[string]any{"title": "x", "due": "2026-13-40"},
			wantSub: "not a real date",
		},
		{
			name:    "priority unknown",
			args:    map[string]any{"title": "x", "priority": "urgent"},
			wantSub: "priority must be one of",
		},
		{
			name:    "tag with bad chars",
			args:    map[string]any{"title": "x", "tags": []any{"work/home"}},
			wantSub: "tag",
		},
		{
			name:    "tags not strings",
			args:    map[string]any{"title": "x", "tags": []any{42}},
			wantSub: "tags must be strings",
		},
	}
	stub := &stubAddTask{ok: true, toast: "ok"}
	tool := &AddTaskTool{Dispatch: stub.fn}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tool.Execute(context.Background(), tc.args)
			require.Error(t, err)
			require.True(t, errors.Is(err, ErrAddTaskInvalidInput),
				"expected ErrAddTaskInvalidInput, got %v", err)
			require.Contains(t, err.Error(), tc.wantSub)
		})
	}
	require.Empty(t, stub.calls, "validation errors must not reach the dispatcher")
}

// TestAddTaskTool_DispatcherReturnsError surfaces hard executor
// failures (e.g. could not write tasks file) as a tool error so the
// repair flow can reflect the message back to the model.
func TestAddTaskTool_DispatcherReturnsError(t *testing.T) {
	stub := &stubAddTask{err: errors.New("permission denied")}
	tool := &AddTaskTool{Dispatch: stub.fn}

	_, err := tool.Execute(context.Background(), map[string]any{"title": "x"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "permission denied")
}

// TestAddTaskTool_DispatcherReturnsNotOK covers soft failures (tasks
// file not configured on this build) — the tool should surface the
// toast as the error so the model can quote it instead of claiming
// success.
func TestAddTaskTool_DispatcherReturnsNotOK(t *testing.T) {
	stub := &stubAddTask{ok: false, toast: "Tasks file not configured."}
	tool := &AddTaskTool{Dispatch: stub.fn}

	_, err := tool.Execute(context.Background(), map[string]any{"title": "x"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Tasks file not configured")
}

// TestAddTaskTool_FireAt covers the V2.11 alarm parameter. RFC3339
// and "+30m" relative forms both pass validation and are forwarded to
// the dispatcher target. Past timestamps and bad shapes are caught at
// the tool layer (the executor revalidates with `now` for future-only).
func TestAddTaskTool_FireAt(t *testing.T) {
	stub := &stubAddTask{ok: true, toast: "Added task: x"}
	tool := &AddTaskTool{Dispatch: stub.fn}

	cases := []struct {
		name    string
		fireAt  string
		wantErr bool
	}{
		{"rfc3339", "2026-05-12T18:30:00Z", false},
		{"relative_min", "+30m", false},
		{"relative_hour", "+2h", false},
		{"relative_day", "+1d", false},
		{"bad_shape", "tomorrow", true},
		{"empty_relative", "+", true},
		{"bad_rfc3339", "2026-05-12 18:30:00", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tool.Execute(context.Background(), map[string]any{
				"title":   "x",
				"fire_at": tc.fireAt,
			})
			if tc.wantErr {
				require.Error(t, err)
				require.True(t, errors.Is(err, ErrAddTaskInvalidInput))
			} else {
				require.NoError(t, err)
			}
		})
	}

	// Round-trip: when valid, fire_at lands in the dispatcher target
	// verbatim (the executor parses it).
	stub.calls = nil
	_, err := tool.Execute(context.Background(), map[string]any{
		"title":   "x",
		"fire_at": "+30m",
	})
	require.NoError(t, err)
	require.Equal(t, "+30m", stub.calls[0]["fire_at"])
}

// TestAddTaskTool_TagsAcceptSingleString tolerates clients that send
// "tags": "home" instead of ["home"] — exercised because some
// loose-typed tool callers (and replay fixtures) emit either shape.
func TestAddTaskTool_TagsAcceptSingleString(t *testing.T) {
	stub := &stubAddTask{ok: true, toast: "ok"}
	tool := &AddTaskTool{Dispatch: stub.fn}

	_, err := tool.Execute(context.Background(), map[string]any{
		"title": "x",
		"tags":  "errands",
	})
	require.NoError(t, err)
	require.Equal(t, []string{"errands"}, stub.calls[0]["tags"])
}

// TestAddTaskTool_Parameters pins the JSON-schema contract so the LLM
// always sees the same parameter list (cache key stability).
func TestAddTaskTool_Parameters(t *testing.T) {
	tool := &AddTaskTool{}
	params := tool.Parameters()
	names := make([]string, 0, len(params))
	for _, p := range params {
		names = append(names, p.Name)
	}
	require.Equal(t, []string{"title", "due", "priority", "tags", "fire_at"}, names)

	// title must be required; nothing else is.
	for _, p := range params {
		if p.Name == "title" {
			require.True(t, p.Required)
		} else {
			require.False(t, p.Required, "%s should be optional", p.Name)
		}
	}
}
