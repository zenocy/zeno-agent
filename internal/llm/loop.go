package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// chatCompleter is the narrow seam the loop uses to talk to the LLM client.
// *Client implements it; tests pass a stub that scripts the conversation.
type chatCompleter interface {
	ChatCompletion(ctx context.Context, messages []Message, tools []ToolDefinition, opts ...ChatOption) (ChatResult, error)
}

// LoopConfig tunes the tool-using loop. Zero values fall back to defaults.
type LoopConfig struct {
	MaxIterations int           // default 6
	Deadline      time.Duration // default 30s
	ToolTimeout   time.Duration // default 5s — per individual tool execution
	ChatOptions   []ChatOption  // applied to every ChatCompletion call in the loop (max_tokens, schema, etc.)
	Logger        *logrus.Entry

	// FinalCallBudget is the wall-clock budget reserved for the
	// "iteration cap reached, produce a final answer" synthesis call
	// that runs after MaxIterations is exhausted. Without a reserved
	// budget, a loop body that consumes the full Deadline always
	// starves this call — the user-visible answer disappears precisely
	// when the loop worked hardest to gather context. Default 15s.
	//
	// The final call's ctx is a fresh sub-context of the parent ctx
	// (the one supplied to RunLoop, *not* the Deadline-bounded loop
	// ctx) so the budget is additive on top of Deadline. The parent
	// ctx still bounds the total — a cron-budget-bounded parent will
	// cap the final call, but an unbounded reactive HTTP request will
	// let it run its full window.
	FinalCallBudget time.Duration

	// Stage labels metric series with the synth stage that owns this loop —
	// "cards" | "briefing" | "inject" | "reactive_ask" | "recognition".
	// Empty stage falls back to "unknown" inside the metrics package so a
	// missing label never tanks Prometheus.
	Stage string

	// Observer wires the loop's per-call / per-tool / per-repair events to
	// metrics counters. Each callback is optional; nil callbacks no-op.
	Observer LoopObserver
}

// LoopObserver carries the metric callbacks the loop fires. Defining the
// shape here (rather than importing internal/metrics) keeps llm a clean
// primitive — the synth runner glues this to metrics.Metrics methods.
type LoopObserver struct {
	OnLLMCall      func(stage, outcome string, dur time.Duration, promptTok, completionTok int)
	OnSchemaRepair func(stage, outcome string)
	OnTool         func(tool, outcome string, dur time.Duration)
	OnLoopIters    func(stage string, n int)
}

// LoopStats summarizes one loop run for the trace and for telemetry.
type LoopStats struct {
	Iterations       int
	PromptTokens     int
	CachedTokens     int
	CompletionTokens int
	RepairAttempts   int
}

// LoopResult is the loop's output. Stopped is one of:
//
//	ok | iteration_cap | deadline | duplicate | repair_exhausted | error
type LoopResult struct {
	Content  string
	Trace    Trace
	Stopped  string
	Stats    LoopStats
	Memories []MemoryCandidate // V2.2.0: derived-memory candidates extracted from `remember:` lines
}

// MemoryCandidate is one `remember: <subject>: <predicate>` line lifted from
// the model's emitted content. The cards loop's parser splits the line into a
// normalized subject (lowercase, trimmed) and a free-form predicate; the
// consolidator (internal/synth/consolidate.go) decides whether to insert,
// reinforce, promote, or drop. Raw is preserved for the `memory.candidates`
// audit event.
type MemoryCandidate struct {
	Subject   string `json:"subject"`
	Predicate string `json:"predicate"`
	Raw       string `json:"raw"`
}

// MaxMemoryCandidatesPerResponse caps how many `remember:` lines the parser
// captures from a single LLM response. Excess lines are dropped at parse
// time, not by the consolidator, so a misbehaving model can't blast the
// audit log. Pinned at the V2.2.0 plan-approval review.
const MaxMemoryCandidatesPerResponse = 3

// Stop reasons surfaced via LoopResult.Stopped.
const (
	StopOK              = "ok"
	StopIterationCap    = "iteration_cap"
	StopDeadline        = "deadline"
	StopDuplicate       = "duplicate"
	StopRepairExhausted = "repair_exhausted"
	StopError           = "error"
)

// RunLoop runs a tool-using conversation. The model may call tools from reg
// up to cfg.MaxIterations times. Malformed tool-call arguments trigger a
// bounded repair flow; the same canonical (name, args) called twice is
// detected and surfaces as a duplicate stop. Iteration cap forces a final
// no-tools call so the trace always carries Content (or surfaces an error).
func RunLoop(
	ctx context.Context,
	c chatCompleter,
	system, user string,
	reg *ToolRegistry,
	cfg LoopConfig,
) (LoopResult, error) {
	if cfg.MaxIterations <= 0 {
		cfg.MaxIterations = 6
	}
	if cfg.Deadline <= 0 {
		cfg.Deadline = 30 * time.Second
	}
	if cfg.ToolTimeout <= 0 {
		cfg.ToolTimeout = 5 * time.Second
	}
	if cfg.FinalCallBudget <= 0 {
		cfg.FinalCallBudget = 15 * time.Second
	}

	// Retain the parent ctx so the iteration-cap synthesis call can
	// run against its own budget independent of the loop's Deadline.
	// Without this, a loop body that consumes the full Deadline always
	// starves the synthesis call that produces the user-visible answer.
	parentCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, cfg.Deadline)
	defer cancel()

	msgs := []Message{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}
	repairs := NewRepairTrackerWithMax(MaxRepairAttemptsPerToolCall)
	dupSeen := map[string]int{}
	trace := NewAccumulator()
	var memories []MemoryCandidate

	// V2.4 live-trace publish: when ctx carries a LiveTraceFunc, every
	// trace step ALSO fires the callback synchronously. Wrappers below
	// sit in front of trace.RecordTool / trace.RecordThought so each
	// recording site (there are eight in this function) goes through a
	// single publish path — no need for the loop body to hold the cb.
	liveTrace := LiveTraceFromContext(ctx)
	recordTool := func(op, target, note string) {
		step := trace.RecordTool(op, target, note)
		if liveTrace != nil {
			liveTrace(step)
		}
	}
	recordToolWithRefs := func(op, target, note string, refs []string) {
		if len(refs) == 0 {
			recordTool(op, target, note)
			return
		}
		step := trace.RecordToolWithRefs(op, target, note, refs)
		if liveTrace != nil {
			liveTrace(step)
		}
	}
	recordThought := func(text string) {
		step := trace.RecordThought(text)
		if liveTrace != nil {
			liveTrace(step)
		}
	}

	stats := LoopStats{}
	var defs []ToolDefinition
	if reg != nil {
		defs = reg.Definitions()
	}

	for i := 0; i < cfg.MaxIterations; i++ {
		stats.Iterations = i + 1
		if err := ctx.Err(); err != nil {
			return finalize(trace, "", StopDeadline, stats, repairs), err
		}

		callStart := time.Now()
		cr, err := c.ChatCompletion(ctx, msgs, defs, cfg.ChatOptions...)
		callDur := time.Since(callStart)
		callMs := callDur.Milliseconds()
		callOutcome := outcomeFor(err)
		if cfg.Logger != nil {
			cfg.Logger.WithFields(map[string]any{
				"iteration":  i + 1,
				"ms":         callMs,
				"prompt_tok": cr.PromptTokens,
				"cached_tok": cr.CachedTokens,
				"comp_tok":   cr.CompletionTokens,
				"tools":      len(cr.ToolCalls),
			}).Info("llm: call complete")
		}
		if cfg.Observer.OnLLMCall != nil {
			cfg.Observer.OnLLMCall(cfg.Stage, callOutcome, callDur, cr.PromptTokens, cr.CompletionTokens)
		}
		if err != nil {
			if cfg.Observer.OnLoopIters != nil {
				cfg.Observer.OnLoopIters(cfg.Stage, stats.Iterations)
			}
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return finalize(trace, "", StopDeadline, stats, repairs), err
			}
			return finalize(trace, "", StopError, stats, repairs), err
		}
		stats.PromptTokens += cr.PromptTokens
		stats.CachedTokens += cr.CachedTokens
		stats.CompletionTokens += cr.CompletionTokens

		// Reasoning-model thinking: when the upstream split chain-of-thought
		// off into reasoning_content / <think> blocks, the client surfaces
		// it in cr.ThinkingContent. Capture it as a Trace thought step
		// (truncated to the first sentence so it reads literary, not stream-
		// of-consciousness) so the UI's trace surface isn't empty when the
		// model didn't emit explicit `thought:` lines.
		if t := summarizeThinking(cr.ThinkingContent); t != "" {
			recordThought(t)
		}

		// Repair: malformed tool-call argument JSON. Bounded per tool_call_id.
		if len(cr.ToolArgsErrors) > 0 {
			outcomes := PlanRepairs(repairs, cr.ToolArgsErrors)
			anyContinue := false
			for _, oc := range outcomes {
				if oc.Continue {
					msgs = append(msgs, oc.RepairMessage)
					recordThought(fmt.Sprintf("repair: re-emit JSON for %s", oc.ToolCallID))
					anyContinue = true
				}
			}
			stats.RepairAttempts = repairs.Total()
			if anyContinue {
				if cfg.Logger != nil {
					cfg.Logger.WithFields(logrus.Fields{
						"stage":   cfg.Stage,
						"attempt": stats.RepairAttempts,
					}).Info("synth: schema repair triggered")
				}
				if cfg.Observer.OnSchemaRepair != nil {
					cfg.Observer.OnSchemaRepair(cfg.Stage, "attempted")
				}
				// Don't consume an iteration: the next pass is the repair.
				i--
				continue
			}
			if cfg.Observer.OnSchemaRepair != nil {
				cfg.Observer.OnSchemaRepair(cfg.Stage, "exhausted")
			}
			if cfg.Observer.OnLoopIters != nil {
				cfg.Observer.OnLoopIters(cfg.Stage, stats.Iterations)
			}
			return finalize(trace, "", StopRepairExhausted, stats, repairs), nil
		}

		// Natural exit: no tool calls. Strip thought + remember lines from
		// content, record them, and return.
		if len(cr.ToolCalls) == 0 {
			clean, thoughts, mems := extractMetaLines(cr.Content)
			for _, t := range thoughts {
				recordThought(t)
			}
			memories = appendMemoriesCapped(memories, mems)
			recordCitations(cr.Citations, recordTool)
			if cfg.Observer.OnLoopIters != nil {
				cfg.Observer.OnLoopIters(cfg.Stage, stats.Iterations)
			}
			return finalizeWithMemories(trace, clean, StopOK, stats, repairs, memories), nil
		}

		// Append the assistant's tool-call message.
		msgs = append(msgs, Message{Role: "assistant", ToolCalls: cr.ToolCalls})

		// Capture any pre-tool-call thoughts and remember lines the model
		// emitted alongside the tool calls.
		if cr.Content != "" {
			_, thoughts, mems := extractMetaLines(cr.Content)
			for _, t := range thoughts {
				recordThought(t)
			}
			memories = appendMemoriesCapped(memories, mems)
		}

		// Execute each tool call, with dup-detect on canonical (name, args).
		for _, tc := range cr.ToolCalls {
			key := tc.Name + "|" + canonicalJSON(tc.Arguments)
			dupSeen[key]++
			if dupSeen[key] >= 2 {
				msgs = append(msgs, Message{
					Role:       "tool",
					ToolCallID: tc.ID,
					Content:    "Duplicate call detected — same args used previously. Use a different approach or finalize.",
				})
				recordTool("DUPSTOP", tc.Name, "duplicate")
				if cfg.Observer.OnLoopIters != nil {
					cfg.Observer.OnLoopIters(cfg.Stage, stats.Iterations)
				}
				return finalize(trace, "", StopDuplicate, stats, repairs), nil
			}

			tool, ok := reg.Get(tc.Name)
			if !ok {
				msg := "Error: unknown tool " + tc.Name
				msgs = append(msgs, Message{Role: "tool", ToolCallID: tc.ID, Content: msg})
				recordTool("ERR", tc.Name, "unknown")
				if cfg.Observer.OnTool != nil {
					cfg.Observer.OnTool(tc.Name, "unknown", 0)
				}
				continue
			}

			toolStart := time.Now()
			toolCtx, toolCancel := context.WithTimeout(ctx, cfg.ToolTimeout)
			toolCtx = WithRefsCollector(toolCtx) // V2.5.0 P3: tools may publish observation IDs
			out, err := tool.Execute(toolCtx, tc.Arguments)
			toolCancel()
			toolDur := time.Since(toolStart)
			refs := RefsFromContext(toolCtx)
			if err != nil {
				note := err.Error()
				toolOutcome := "error"
				if errors.Is(err, context.DeadlineExceeded) {
					note = fmt.Sprintf("tool exceeded %s timeout", cfg.ToolTimeout)
					toolOutcome = "timeout"
				}
				msgs = append(msgs, Message{
					Role: "tool", ToolCallID: tc.ID,
					Content: "Error: " + note,
				})
				recordToolWithRefs(opVerbFor(tc.Name), targetFromArgs(tc.Arguments), note, refs)
				if cfg.Logger != nil {
					cfg.Logger.WithFields(logrus.Fields{
						"tool":    tc.Name,
						"dur_ms":  toolDur.Milliseconds(),
						"outcome": toolOutcome,
					}).Debug("llm: tool dispatched")
				}
				if cfg.Observer.OnTool != nil {
					cfg.Observer.OnTool(tc.Name, toolOutcome, toolDur)
				}
				continue
			}
			msgs = append(msgs, Message{Role: "tool", ToolCallID: tc.ID, Content: out})
			recordToolWithRefs(opVerbFor(tc.Name), targetFromArgs(tc.Arguments), "", refs)
			if cfg.Logger != nil {
				cfg.Logger.WithFields(logrus.Fields{
					"tool":    tc.Name,
					"dur_ms":  toolDur.Milliseconds(),
					"outcome": "ok",
				}).Debug("llm: tool dispatched")
			}
			if cfg.Observer.OnTool != nil {
				cfg.Observer.OnTool(tc.Name, "ok", toolDur)
			}
		}
	}

	// Iteration cap: ask the model to finalize without tools. Runs
	// against a fresh sub-context of parentCtx with its own
	// FinalCallBudget so a fully-consumed loop Deadline doesn't
	// starve the synthesis call — slow remote providers (Gemini with
	// thinking + a 10K-token tool-result context) frequently need
	// 10-15s of dedicated headroom that the loop body has already
	// burned through.
	finalCtx, finalCancel := context.WithTimeout(parentCtx, cfg.FinalCallBudget)
	defer finalCancel()

	msgs = append(msgs, Message{
		Role:    "system",
		Content: "Iteration cap reached. Produce a final answer using only what you have; do not call any more tools.",
	})
	callStart := time.Now()
	cr, err := c.ChatCompletion(finalCtx, msgs, nil, cfg.ChatOptions...)
	callDur := time.Since(callStart)
	callMs := callDur.Milliseconds()
	callOutcome := outcomeFor(err)
	if cfg.Logger != nil {
		cfg.Logger.WithFields(map[string]any{
			"iteration":  "final",
			"ms":         callMs,
			"prompt_tok": cr.PromptTokens,
			"comp_tok":   cr.CompletionTokens,
		}).Info("llm: call complete")
	}
	if cfg.Observer.OnLLMCall != nil {
		cfg.Observer.OnLLMCall(cfg.Stage, callOutcome, callDur, cr.PromptTokens, cr.CompletionTokens)
	}
	if cfg.Observer.OnLoopIters != nil {
		cfg.Observer.OnLoopIters(cfg.Stage, stats.Iterations)
	}
	if err != nil {
		return finalize(trace, "", StopIterationCap, stats, repairs), err
	}
	stats.PromptTokens += cr.PromptTokens
	stats.CompletionTokens += cr.CompletionTokens
	clean, thoughts, mems := extractMetaLines(cr.Content)
	for _, t := range thoughts {
		recordThought(t)
	}
	memories = appendMemoriesCapped(memories, mems)
	return finalizeWithMemories(trace, clean, StopIterationCap, stats, repairs, memories), nil
}

// outcomeFor classifies a ChatCompletion error into a small enum suitable
// for Prometheus labels: ok | timeout | error. Bounded cardinality.
func outcomeFor(err error) string {
	if err == nil {
		return "ok"
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "timeout"
	}
	return "error"
}

// finalizeWithMemories is finalize plus the memory candidates the parser
// captured during the loop. The other failure paths (deadline / error /
// repair_exhausted / duplicate) keep using finalize because no candidates
// have been observed by then — those paths exit before any natural-exit
// content is parsed.
func finalizeWithMemories(trace *Accumulator, content, stopped string, stats LoopStats, repairs *RepairTracker, memories []MemoryCandidate) LoopResult {
	res := finalize(trace, content, stopped, stats, repairs)
	res.Memories = memories
	return res
}

// appendMemoriesCapped accumulates parser-emitted memories across multiple
// LLM turns within one loop, never exceeding the per-response cap. The
// per-response cap is already enforced inside extractMetaLines; this guard
// stops the loop from accumulating more than one response's worth even when
// the model emits remember: in both pre-tool and natural-exit content.
func appendMemoriesCapped(existing, more []MemoryCandidate) []MemoryCandidate {
	for _, m := range more {
		if len(existing) >= MaxMemoryCandidatesPerResponse {
			break
		}
		existing = append(existing, m)
	}
	return existing
}

func finalize(trace *Accumulator, content, stopped string, stats LoopStats, repairs *RepairTracker) LoopResult {
	stats.RepairAttempts = repairs.Total()
	return LoopResult{
		Content: content,
		Trace:   trace.Build(stopped),
		Stopped: stopped,
		Stats:   stats,
	}
}

// summarizeThinking is the package-internal alias for SummarizeText kept so
// the loop's existing callers don't need to change.
func summarizeThinking(s string) string { return SummarizeText(s) }

// recordCitations appends one trace step per Citation surfaced by a
// grounding-enabled provider response (currently only Gemini's Google
// Search grounding). Op verb is "CITE"; target is the source title or
// URI; note carries the URI when both title and URI are present so
// trace consumers can render a link without a second lookup. Empty
// citation list is a no-op.
//
// Citations live at LLM-call granularity, distinct from the per-tool-
// call refs collected via WithRefsCollector — overloading the latter
// would conflate "this tool produced observation IDs" with "the model
// cited these external sources."
func recordCitations(citations []Citation, recordTool func(op, target, note string)) {
	if recordTool == nil {
		return
	}
	for _, c := range citations {
		target := c.Title
		note := c.URI
		if target == "" {
			target = c.URI
			note = ""
		}
		if target == "" {
			continue
		}
		recordTool("CITE", target, note)
	}
}

// SummarizeText returns a short, single-sentence summary of arbitrary prose
// suitable for one Trace thought step. The full thinking from a reasoning
// model is often hundreds of words of stream-of-consciousness; we trim to
// the first sentence (terminated by `.`, `?`, `!`, or newline) and cap
// length to keep the trace UI readable. Returns "" when input is empty.
func SummarizeText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// First sentence boundary or first newline, whichever comes first.
	end := len(s)
	for i, r := range s {
		if r == '\n' {
			end = i
			break
		}
		if r == '.' || r == '?' || r == '!' {
			// Look one ahead for whitespace/EOS to avoid splitting inside
			// abbreviations like "U.S." — cheap heuristic, not perfect.
			if i+1 >= len(s) || s[i+1] == ' ' || s[i+1] == '\n' {
				end = i + 1
				break
			}
		}
	}
	out := strings.TrimSpace(s[:end])
	const maxLen = 200
	if len(out) > maxLen {
		out = out[:maxLen] + "…"
	}
	return out
}

// extractMetaLines returns the content with any `thought: ...` and
// `remember: <subject>: <predicate>` lines stripped, plus the parsed
// thoughts and memory candidates in order. Recognition of both prefixes is
// case-insensitive; the rest of each line is preserved verbatim.
//
// Malformed `remember:` lines (only one segment, e.g. `remember: foo`) are
// silently dropped — they're stripped from the cleaned content but produce
// no candidate. Memory capture caps at MaxMemoryCandidatesPerResponse;
// excess lines are still stripped but ignored.
func extractMetaLines(content string) (clean string, thoughts []string, memories []MemoryCandidate) {
	if content == "" {
		return "", nil, nil
	}
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		switch {
		case strings.HasPrefix(lower, "thought:"):
			thought := strings.TrimSpace(trimmed[len("thought:"):])
			if thought != "" {
				thoughts = append(thoughts, thought)
			}
		case strings.HasPrefix(lower, "remember:"):
			rest := strings.TrimSpace(trimmed[len("remember:"):])
			if mc, ok := parseMemoryCandidate(rest, trimmed); ok {
				if len(memories) < MaxMemoryCandidatesPerResponse {
					memories = append(memories, mc)
				}
			}
		default:
			out = append(out, line)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n")), thoughts, memories
}

// extractThoughts is a thin shim over extractMetaLines for callers that only
// need the existing thought-stripping behavior. Kept so older callers and
// tests that don't care about memories don't need to plumb a third return.
func extractThoughts(content string) (clean string, thoughts []string) {
	clean, thoughts, _ = extractMetaLines(content)
	return clean, thoughts
}

// parseMemoryCandidate parses the `<subject>: <predicate>` body of a
// remember: line. Subject is normalized to lowercase + trimmed; predicate is
// free-form text after the first colon. Returns ok=false on a body without
// a colon (malformed: subject only). Empty subject or empty predicate also
// fails.
func parseMemoryCandidate(rest, raw string) (MemoryCandidate, bool) {
	idx := strings.IndexByte(rest, ':')
	if idx < 0 {
		return MemoryCandidate{}, false
	}
	subject := strings.ToLower(strings.TrimSpace(rest[:idx]))
	predicate := strings.TrimSpace(rest[idx+1:])
	if subject == "" || predicate == "" {
		return MemoryCandidate{}, false
	}
	return MemoryCandidate{Subject: subject, Predicate: predicate, Raw: raw}, true
}

// canonicalJSON returns a stable JSON encoding of the args map: keys sorted
// recursively, slices preserved in input order. Used as the dup-detect key.
func canonicalJSON(v any) string {
	var b strings.Builder
	canonicalJSONInto(&b, v)
	return b.String()
}

func canonicalJSONInto(b *strings.Builder, v any) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			b.Write(kb)
			b.WriteByte(':')
			canonicalJSONInto(b, x[k])
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			canonicalJSONInto(b, e)
		}
		b.WriteByte(']')
	default:
		js, _ := json.Marshal(v)
		b.Write(js)
	}
}

// opVerbFor maps a tool name to the uppercase verb shown in the trace UI.
// Falls back to CALL for unknown tools.
func opVerbFor(name string) string {
	switch name {
	case "read_thread", "read_event":
		return "READ"
	case "read_weather_window":
		return "CHECK"
	default:
		return "CALL"
	}
}

// targetFromArgs picks a single short label from the tool args for the
// trace's Target field. Prefers `subject_hint` → `uid` → `start_iso` → first
// string value.
func targetFromArgs(args map[string]any) string {
	for _, k := range []string{"subject_hint", "uid", "start_iso"} {
		if v, ok := args[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	for _, v := range args {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ""
}
