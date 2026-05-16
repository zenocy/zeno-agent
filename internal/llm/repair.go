package llm

import (
	"fmt"
	"sync"
)

// MaxRepairAttemptsPerToolCall is the per-tool_call_id repair cap. Two
// attempts is the V1-tested sweet spot — one attempt is too brittle on local
// 7B/8B models, three attempts wastes budget when the model is genuinely
// stuck on a malformed call.
const MaxRepairAttemptsPerToolCall = 2

// RepairTracker records repair attempts per tool_call_id so a misbehaving
// call is bounded independently of the loop's iteration cap. Trackers are
// per-loop-run and not shared across runs.
type RepairTracker struct {
	mu          sync.Mutex
	attempts    map[string]int
	maxAttempts int
}

// NewRepairTrackerWithMax returns a tracker capped at max. max <= 0 falls
// back to MaxRepairAttemptsPerToolCall.
func NewRepairTrackerWithMax(max int) *RepairTracker {
	if max <= 0 {
		max = MaxRepairAttemptsPerToolCall
	}
	return &RepairTracker{attempts: map[string]int{}, maxAttempts: max}
}

// Attempts returns the number of repair attempts consumed so far for the
// given tool_call_id.
func (r *RepairTracker) Attempts(toolCallID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.attempts[toolCallID]
}

// Increment bumps the counter and returns the new value.
func (r *RepairTracker) Increment(toolCallID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempts[toolCallID]++
	return r.attempts[toolCallID]
}

// MaxAttempts returns the cap.
func (r *RepairTracker) MaxAttempts() int { return r.maxAttempts }

// Total returns repair attempts across all tool_call_ids — useful for the
// loop's stats line.
func (r *RepairTracker) Total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := 0
	for _, n := range r.attempts {
		t += n
	}
	return t
}

// BuildRepairMessage produces the synthetic user message appended to the
// conversation so the model can re-emit valid JSON for one specific failed
// tool call. The content names the tool, the raw JSON, and the parse error.
func BuildRepairMessage(pe ToolArgsParseError) Message {
	content := fmt.Sprintf(
		"Your last tool call to %q (id=%s) had malformed JSON arguments. "+
			"Parse error: %s. Raw arguments: %s. "+
			"Please re-issue ONLY that tool call with valid JSON arguments; "+
			"do not repeat other work from the previous message.",
		pe.Name, pe.ToolCallID, pe.ParseErrMsg, pe.RawJSON,
	)
	return Message{Role: "user", Content: content}
}

// RepairOutcome is the per-error decision returned by PlanRepair.
type RepairOutcome struct {
	ToolCallID string
	// Continue: append RepairMessage and re-run the provider call without
	// consuming a tool-loop iteration.
	Continue bool
	// Permanent: the tool_call_id hit the cap. Caller should surface the
	// error and stop the loop.
	Permanent     bool
	RepairMessage Message
	AttemptsSoFar int
}

// PlanRepair decides per parse-error whether to attempt one more repair or
// declare the call permanently broken. Increments the tracker on Continue.
func PlanRepair(tracker *RepairTracker, pe ToolArgsParseError) RepairOutcome {
	current := tracker.Attempts(pe.ToolCallID)
	if current >= tracker.MaxAttempts() {
		return RepairOutcome{
			ToolCallID:    pe.ToolCallID,
			Permanent:     true,
			AttemptsSoFar: current,
		}
	}
	n := tracker.Increment(pe.ToolCallID)
	return RepairOutcome{
		ToolCallID:    pe.ToolCallID,
		Continue:      true,
		RepairMessage: BuildRepairMessage(pe),
		AttemptsSoFar: n,
	}
}

// PlanRepairs runs PlanRepair on each parse error, preserving order so the
// synthetic messages can be appended deterministically.
func PlanRepairs(tracker *RepairTracker, errs []ToolArgsParseError) []RepairOutcome {
	out := make([]RepairOutcome, 0, len(errs))
	for _, pe := range errs {
		out = append(out, PlanRepair(tracker, pe))
	}
	return out
}
