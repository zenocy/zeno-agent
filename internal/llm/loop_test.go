package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// scriptedCompleter returns ChatResults from a queue. Each Run pop a result;
// the test pre-loads the queue.
type scriptedCompleter struct {
	turns []ChatResult
	idx   int
	err   error
}

func (s *scriptedCompleter) ChatCompletion(_ context.Context, _ []Message, _ []ToolDefinition, _ ...ChatOption) (ChatResult, error) {
	if s.err != nil {
		return ChatResult{}, s.err
	}
	if s.idx >= len(s.turns) {
		return ChatResult{Content: "fallback final"}, nil
	}
	out := s.turns[s.idx]
	s.idx++
	return out, nil
}

// stubTool returns a fixed string and records the args it was called with.
type stubTool struct {
	name   string
	out    string
	calls  []map[string]any
	failOn map[string]bool
}

func (t *stubTool) Name() string        { return t.name }
func (t *stubTool) Description() string { return "stub" }
func (t *stubTool) Parameters() []ToolParamSpec {
	return []ToolParamSpec{{Name: "x", Type: "string", Required: true}}
}
func (t *stubTool) Execute(_ context.Context, args map[string]any) (string, error) {
	t.calls = append(t.calls, args)
	if t.failOn != nil {
		if v, ok := args["x"].(string); ok && t.failOn[v] {
			return "", errors.New("intentional failure")
		}
	}
	return t.out, nil
}

func TestRunLoop_NaturalExit(t *testing.T) {
	c := &scriptedCompleter{
		turns: []ChatResult{
			{Content: "Final answer."},
		},
	}
	reg := NewRegistry()
	res, err := RunLoop(context.Background(), c, "sys", "user", reg, LoopConfig{MaxIterations: 3})
	require.NoError(t, err)
	require.Equal(t, StopOK, res.Stopped)
	require.Equal(t, "Final answer.", res.Content)
	require.Equal(t, 1, res.Stats.Iterations)
}

func TestRunLoop_ToolThenFinal(t *testing.T) {
	tool := &stubTool{name: "read_thread", out: "thread body"}
	c := &scriptedCompleter{
		turns: []ChatResult{
			{ToolCalls: []ToolCall{{ID: "t1", Name: "read_thread", Arguments: map[string]any{"subject_hint": "redline"}}}},
			{Content: "thought: pivoting to calendar\n{\"cards\":[]}"},
		},
	}
	reg := NewRegistry(tool)
	res, err := RunLoop(context.Background(), c, "sys", "user", reg, LoopConfig{MaxIterations: 3})
	require.NoError(t, err)
	require.Equal(t, StopOK, res.Stopped)
	require.Contains(t, res.Content, "cards")
	require.NotContains(t, res.Content, "thought:")
	require.Len(t, tool.calls, 1)

	// Trace should have one tool step + one thought step.
	var toolSteps, thoughtSteps int
	for _, s := range res.Trace.Steps {
		if s.Kind == KindTool {
			toolSteps++
		}
		if s.Kind == KindThought {
			thoughtSteps++
		}
	}
	require.Equal(t, 1, toolSteps)
	require.GreaterOrEqual(t, thoughtSteps, 1)
}

func TestRunLoop_DuplicateCallStops(t *testing.T) {
	tool := &stubTool{name: "read_thread", out: "body"}
	c := &scriptedCompleter{
		turns: []ChatResult{
			{ToolCalls: []ToolCall{{ID: "t1", Name: "read_thread", Arguments: map[string]any{"subject_hint": "redline"}}}},
			{ToolCalls: []ToolCall{{ID: "t2", Name: "read_thread", Arguments: map[string]any{"subject_hint": "redline"}}}},
		},
	}
	reg := NewRegistry(tool)
	res, err := RunLoop(context.Background(), c, "sys", "user", reg, LoopConfig{MaxIterations: 5})
	require.NoError(t, err)
	require.Equal(t, StopDuplicate, res.Stopped)
}

func TestRunLoop_IterationCap(t *testing.T) {
	tool := &stubTool{name: "read_thread", out: "body"}
	turns := []ChatResult{}
	// 3 distinct tool calls (one per iteration), then a final-content turn for
	// the post-cap forced-final call.
	for i := 0; i < 3; i++ {
		turns = append(turns, ChatResult{
			ToolCalls: []ToolCall{{
				ID:        "t" + string(rune('0'+i)),
				Name:      "read_thread",
				Arguments: map[string]any{"subject_hint": "redline-" + string(rune('a'+i))},
			}},
		})
	}
	turns = append(turns, ChatResult{Content: "final-after-cap"})
	c := &scriptedCompleter{turns: turns}
	reg := NewRegistry(tool)
	res, err := RunLoop(context.Background(), c, "sys", "user", reg, LoopConfig{MaxIterations: 3})
	require.NoError(t, err)
	require.Equal(t, StopIterationCap, res.Stopped)
	require.Equal(t, "final-after-cap", res.Content)
}

func TestRunLoop_Repair(t *testing.T) {
	tool := &stubTool{name: "read_thread", out: "body"}
	c := &scriptedCompleter{
		turns: []ChatResult{
			{
				ToolCalls: []ToolCall{{ID: "t1", Name: "read_thread", Arguments: nil}},
				ToolArgsErrors: []ToolArgsParseError{{
					ToolCallID: "t1", Name: "read_thread", RawJSON: "{bad", ParseErrMsg: "expected key",
				}},
			},
			{ToolCalls: []ToolCall{{ID: "t2", Name: "read_thread", Arguments: map[string]any{"subject_hint": "redline"}}}},
			{Content: "ok"},
		},
	}
	reg := NewRegistry(tool)
	res, err := RunLoop(context.Background(), c, "sys", "user", reg, LoopConfig{MaxIterations: 6})
	require.NoError(t, err)
	require.Equal(t, StopOK, res.Stopped)
	require.GreaterOrEqual(t, res.Stats.RepairAttempts, 1)
}

func TestRunLoop_RepairExhausted(t *testing.T) {
	turns := []ChatResult{}
	for i := 0; i < 5; i++ {
		turns = append(turns, ChatResult{
			ToolCalls: []ToolCall{{ID: "same", Name: "read_thread"}},
			ToolArgsErrors: []ToolArgsParseError{{
				ToolCallID: "same", Name: "read_thread", RawJSON: "{bad", ParseErrMsg: "...",
			}},
		})
	}
	c := &scriptedCompleter{turns: turns}
	reg := NewRegistry(&stubTool{name: "read_thread"})
	res, err := RunLoop(context.Background(), c, "sys", "user", reg, LoopConfig{MaxIterations: 6})
	require.NoError(t, err)
	require.Equal(t, StopRepairExhausted, res.Stopped)
}

func TestRunLoop_Deadline(t *testing.T) {
	c := &scriptedCompleter{turns: []ChatResult{{Content: "should not run"}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, _ := RunLoop(ctx, c, "sys", "user", NewRegistry(), LoopConfig{MaxIterations: 3, Deadline: time.Hour})
	require.Equal(t, StopDeadline, res.Stopped)
}

func TestExtractThoughts(t *testing.T) {
	clean, thoughts := extractThoughts("thought: pivoting to calendar\nFinal text\nThought: another\nMore text")
	require.Equal(t, "Final text\nMore text", clean)
	require.Equal(t, []string{"pivoting to calendar", "another"}, thoughts)
}

func TestExtractMetaLines_RecognizesRememberLine(t *testing.T) {
	in := "Some prose.\nremember: partner: Sam\nClosing line."
	clean, thoughts, mems := extractMetaLines(in)
	require.Equal(t, "Some prose.\nClosing line.", clean)
	require.Empty(t, thoughts)
	require.Len(t, mems, 1)
	require.Equal(t, "partner", mems[0].Subject)
	require.Equal(t, "Sam", mems[0].Predicate)
	require.Equal(t, "remember: partner: Sam", mems[0].Raw)
}

func TestExtractMetaLines_MalformedRemember_Ignored(t *testing.T) {
	// remember: with no second colon → no candidate, line still stripped.
	clean, _, mems := extractMetaLines("Body line\nremember: foo\nMore body")
	require.Equal(t, "Body line\nMore body", clean, "malformed line is stripped from output")
	require.Empty(t, mems, "malformed candidate is dropped")

	// remember: subject only with trailing whitespace also drops.
	clean, _, mems = extractMetaLines("remember: subject_only_no_pred:    ")
	require.Empty(t, clean)
	require.Empty(t, mems)
}

func TestExtractMetaLines_MultipleRemembers_AllCaptured(t *testing.T) {
	in := strings.Join([]string{
		"remember: partner: Sam",
		"remember: runs: Tue/Thu mornings",
		"remember: anniversary: May 7",
		"remember: dinner: Otto's", // 4th — should be dropped, line still stripped
		"Final prose.",
	}, "\n")
	clean, _, mems := extractMetaLines(in)
	require.Equal(t, "Final prose.", clean)
	require.Len(t, mems, 3, "fourth remember beyond cap is dropped at parse time")
	require.Equal(t, "partner", mems[0].Subject)
	require.Equal(t, "runs", mems[1].Subject)
	require.Equal(t, "anniversary", mems[2].Subject)
}

func TestExtractMetaLines_RememberAndThought_BothPreserved(t *testing.T) {
	in := strings.Join([]string{
		"thought: pivoting to email",
		"remember: commute: bike when dry",
		"Final body.",
	}, "\n")
	clean, thoughts, mems := extractMetaLines(in)
	require.Equal(t, "Final body.", clean)
	require.Equal(t, []string{"pivoting to email"}, thoughts)
	require.Len(t, mems, 1)
	require.Equal(t, "commute", mems[0].Subject)
	require.Equal(t, "bike when dry", mems[0].Predicate)
}

func TestExtractMetaLines_SubjectNormalizedLowercase(t *testing.T) {
	clean, _, mems := extractMetaLines("REMEMBER:  Partner :  Sam Vega ")
	require.Empty(t, clean)
	require.Len(t, mems, 1)
	require.Equal(t, "partner", mems[0].Subject, "subject must be lowercased and trimmed")
	require.Equal(t, "Sam Vega", mems[0].Predicate, "predicate keeps original casing, just trims whitespace")
}

// TestRecordThought_WireFormat pins the JSON wire format for thought steps.
// The frontend Trace.tsx ThoughtStep renders {step.t}; if this serialization
// regresses to {"note":...} the trace UI silently renders empty <p> tags.
func TestRecordThought_WireFormat(t *testing.T) {
	acc := NewAccumulator()
	acc.RecordThought("the board call is the largest object")
	acc.RecordTool("READ", "redline thread", "")

	steps := acc.Steps()
	require.Len(t, steps, 2)

	thoughtBytes, err := json.Marshal(steps[0])
	require.NoError(t, err)
	require.Contains(t, string(thoughtBytes), `"kind":"thought"`)
	require.Contains(t, string(thoughtBytes), `"t":"the board call is the largest object"`)
	require.NotContains(t, string(thoughtBytes), `"note"`)

	toolBytes, err := json.Marshal(steps[1])
	require.NoError(t, err)
	require.Contains(t, string(toolBytes), `"kind":"tool"`)
	require.Contains(t, string(toolBytes), `"op":"READ"`)
	require.NotContains(t, string(toolBytes), `"t":`)
}

func TestCanonicalJSON_Stable(t *testing.T) {
	a := map[string]any{"b": 1, "a": "x", "c": []any{3, 1, 2}}
	b := map[string]any{"a": "x", "c": []any{3, 1, 2}, "b": 1}
	require.Equal(t, canonicalJSON(a), canonicalJSON(b))
}

func TestRunLoop_UnknownTool(t *testing.T) {
	c := &scriptedCompleter{
		turns: []ChatResult{
			{ToolCalls: []ToolCall{{ID: "t1", Name: "missing", Arguments: map[string]any{"x": "y"}}}},
			{Content: "done"},
		},
	}
	reg := NewRegistry()
	res, err := RunLoop(context.Background(), c, "sys", "user", reg, LoopConfig{MaxIterations: 3})
	require.NoError(t, err)
	require.Equal(t, StopOK, res.Stopped)
	// Trace should show the ERR step.
	found := false
	for _, s := range res.Trace.Steps {
		if s.Kind == KindTool && s.Op == "ERR" {
			found = true
		}
	}
	require.True(t, found, "expected ERR step for unknown tool")
}

func TestRunLoop_ToolError(t *testing.T) {
	tool := &stubTool{name: "read_thread", out: "body", failOn: map[string]bool{"bad": true}}
	c := &scriptedCompleter{
		turns: []ChatResult{
			{ToolCalls: []ToolCall{{ID: "t1", Name: "read_thread", Arguments: map[string]any{"x": "bad"}}}},
			{Content: "fallback"},
		},
	}
	reg := NewRegistry(tool)
	res, err := RunLoop(context.Background(), c, "sys", "user", reg, LoopConfig{MaxIterations: 3})
	require.NoError(t, err)
	require.Equal(t, StopOK, res.Stopped)
	// The tool error must be recorded as a trace note.
	found := false
	for _, s := range res.Trace.Steps {
		if s.Kind == KindTool && strings.Contains(s.Note, "intentional failure") {
			found = true
		}
	}
	require.True(t, found)
}

// TestRunLoop_LiveTrace_StreamsInOrder pins the V2.4 live-trace publish
// contract: when ctx carries a LiveTraceFunc, every trace step recorded
// during the loop ALSO fires the callback synchronously, in the same
// order they appear in the sealed Trace. The captured-step list and
// the sealed trace must be element-equal — same Kind, Op, Target,
// Note, T, MsAt for every step. This makes the live UI a perfect
// mirror of the durable trace.
func TestRunLoop_LiveTrace_StreamsInOrder(t *testing.T) {
	tool := &stubTool{name: "read_thread", out: "thread body"}
	c := &scriptedCompleter{
		turns: []ChatResult{
			// Iteration 1: a thinking thought + a tool call.
			{
				ThinkingContent: "Looking at the calendar to find tomorrow's pre-read.",
				ToolCalls: []ToolCall{
					{ID: "t1", Name: "read_thread", Arguments: map[string]any{"x": "redline"}},
				},
			},
			// Iteration 2: natural exit with one inline thought.
			{Content: "thought: synthesizing the briefing\nFinal answer."},
		},
	}
	reg := NewRegistry(tool)

	var captured []TraceStep
	cb := LiveTraceFunc(func(s TraceStep) { captured = append(captured, s) })
	ctx := ContextWithLiveTrace(context.Background(), cb)

	res, err := RunLoop(ctx, c, "sys", "user", reg, LoopConfig{MaxIterations: 3})
	require.NoError(t, err)
	require.Equal(t, StopOK, res.Stopped)

	// The sealed trace and the live-published list must be the same
	// slice — every step that lands in res.Trace.Steps came through
	// the LiveTraceFunc in publish order.
	require.Equal(t, len(res.Trace.Steps), len(captured),
		"live publish count must match sealed trace step count")
	for i, want := range res.Trace.Steps {
		require.Equalf(t, want, captured[i],
			"step %d: live (%v) must equal sealed (%v)", i, captured[i], want)
	}

	// Sanity: the trace contains the expected kinds (thought from
	// thinking + thought from extractMetaLines + tool from the call).
	var thoughts, tools int
	for _, s := range res.Trace.Steps {
		switch s.Kind {
		case KindThought:
			thoughts++
		case KindTool:
			tools++
		}
	}
	require.GreaterOrEqual(t, thoughts, 2, "should have at least the thinking-summary + the inline thought")
	require.Equal(t, 1, tools)
}

// TestRunLoop_LiveTrace_NilCallbackBehavesAsDefault pins the contract
// that without a LiveTraceFunc attached to ctx, the loop's behavior is
// byte-equal to V2.3 — no panics, sealed trace identical to the
// no-ctx baseline.
func TestRunLoop_LiveTrace_NilCallbackBehavesAsDefault(t *testing.T) {
	c := &scriptedCompleter{turns: []ChatResult{
		{Content: "Final answer."},
	}}
	reg := NewRegistry()

	// Default ctx (no LiveTraceFunc).
	res1, err := RunLoop(context.Background(), c, "sys", "user", reg, LoopConfig{MaxIterations: 3})
	require.NoError(t, err)

	// Reset the completer's idx and run again with an explicit nil
	// callback in ctx (the nil-extracted func from a different value
	// type). Both runs must produce identical Trace.Steps.
	c.idx = 0
	ctx := context.WithValue(context.Background(), liveTraceKey{}, LiveTraceFunc(nil))
	res2, err := RunLoop(ctx, c, "sys", "user", reg, LoopConfig{MaxIterations: 3})
	require.NoError(t, err)

	require.Equal(t, len(res1.Trace.Steps), len(res2.Trace.Steps))
	for i := range res1.Trace.Steps {
		require.Equal(t, res1.Trace.Steps[i].Kind, res2.Trace.Steps[i].Kind)
		require.Equal(t, res1.Trace.Steps[i].T, res2.Trace.Steps[i].T)
	}
}

// TestRunLoop_LiveTrace_PublishesDuplicateStop pins that even the
// terminal duplicate-detection path emits a live step before
// returning. UI must see "DUPSTOP" in real time so the user
// understands why the run cut short, not after a long silence.
func TestRunLoop_LiveTrace_PublishesDuplicateStop(t *testing.T) {
	tool := &stubTool{name: "read_thread", out: "body"}
	c := &scriptedCompleter{
		turns: []ChatResult{
			{ToolCalls: []ToolCall{{ID: "t1", Name: "read_thread", Arguments: map[string]any{"x": "same"}}}},
			{ToolCalls: []ToolCall{{ID: "t2", Name: "read_thread", Arguments: map[string]any{"x": "same"}}}},
		},
	}
	reg := NewRegistry(tool)

	var captured []TraceStep
	cb := LiveTraceFunc(func(s TraceStep) { captured = append(captured, s) })
	ctx := ContextWithLiveTrace(context.Background(), cb)

	res, err := RunLoop(ctx, c, "sys", "user", reg, LoopConfig{MaxIterations: 3})
	require.NoError(t, err)
	require.Equal(t, StopDuplicate, res.Stopped)

	// The DUPSTOP step must have arrived live, not just landed in the
	// sealed trace silently.
	dupSeen := false
	for _, s := range captured {
		if s.Op == "DUPSTOP" {
			dupSeen = true
		}
	}
	require.True(t, dupSeen, "duplicate-stop tool step must be published live")
}
