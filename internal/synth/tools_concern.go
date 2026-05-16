package synth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zenocy/zeno-v2/internal/idgen"

	"github.com/zenocy/zeno-v2/internal/eventbus"
	"github.com/zenocy/zeno-v2/internal/llm"
	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/projection"
	"github.com/zenocy/zeno-v2/internal/store"
)

// V2.5.0 Phase 2 — declare_concern tool for the reactive Ask loop.
//
// When a user types something like *"track my Frankfurt trip"* into Ask,
// the model can call this tool to register a concern. Concerns declared
// this way skip the `proposed` gate (the user already approved the act
// of declaration) and land in `active` with `source=user`. The tool
// fires retrospective tagging asynchronously via the injected
// dispatcher, so by the time the model composes its response card the
// retrospective walk is already in flight; the live trace panel
// (V2.4) surfaces progress.

const (
	declareConcernNameMax        = 80
	declareConcernDescriptionMax = 240
)

// DeclareConcernTool implements llm.Tool. It is registered into the
// reactive Ask loop only when its dependencies (Concerns, Observations,
// Dispatcher) are non-nil — otherwise the registry skips it so eval
// fixtures and replay don't accidentally create concern rows.
type DeclareConcernTool struct {
	Concerns     *store.ConcernRepo
	Observations *store.ConcernObservationRepo
	Bus          *eventbus.Bus // optional
	EventLog     log.Writer    // optional
	Dispatcher   RetrospectiveDispatcher
	Now          func() time.Time // 0 → time.Now
}

func (t *DeclareConcernTool) Name() string { return "declare_concern" }

func (t *DeclareConcernTool) Description() string {
	return "Declare a long-running situation (a 'concern') the user wants to track across emails and events. Use when the user explicitly asks to track, remember, or follow a situation by name. Returns the concern's id and a confirmation."
}

func (t *DeclareConcernTool) Parameters() []llm.ToolParamSpec {
	return []llm.ToolParamSpec{
		{
			Name:        "name",
			Type:        "string",
			Description: "Short human-readable name for the situation (e.g. 'Construction at the house', 'Frankfurt trip'). Required.",
			Required:    true,
		},
		{
			Name:        "description",
			Type:        "string",
			Description: "Optional one-sentence Zeno-voiced summary, ≤240 chars. Calm and declarative; never PM-shaped.", // allow-pm-language
			Required:    false,
		},
	}
}

// declareConcernResult is the JSON the tool returns to the loop. The
// model uses these fields to compose its confirmation card prose.
type declareConcernResult struct {
	ConcernID            string `json:"concern_id"`
	Name                 string `json:"name"`
	Description          string `json:"description"`
	State                string `json:"state"`
	AlreadyExists        bool   `json:"already_exists"`
	RetrospectiveStarted bool   `json:"retrospective_started"`
}

// ErrDeclareConcernInvalidInput is the sentinel for tool input
// validation errors. Returned as a tool error so the loop reflects
// the validation message back to the model rather than crashing.
var ErrDeclareConcernInvalidInput = errors.New("declare_concern: invalid input")

func (t *DeclareConcernTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	if t.Concerns == nil || t.Observations == nil {
		return "", errors.New("declare_concern: tool not configured (missing repos)")
	}

	name, ok := args["name"].(string)
	if !ok {
		return "", fmt.Errorf("%w: name is required and must be a string", ErrDeclareConcernInvalidInput)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("%w: name must not be empty", ErrDeclareConcernInvalidInput)
	}
	if len(name) > declareConcernNameMax {
		return "", fmt.Errorf("%w: name exceeds %d chars", ErrDeclareConcernInvalidInput, declareConcernNameMax)
	}

	description := ""
	if v, ok := args["description"].(string); ok {
		description = strings.TrimSpace(v)
	}
	if len(description) > declareConcernDescriptionMax {
		// Truncate rather than reject — the model should be coached, not
		// failed. The `…` glyph is 3 bytes; reserve those so the final
		// string is exactly within the byte budget.
		description = description[:declareConcernDescriptionMax-3] + "…"
	}

	now := time.Now()
	if t.Now != nil {
		now = t.Now()
	}
	norm := store.NormalizeConcernName(name)

	// Dedupe: idempotent declare. If a visible concern already exists with
	// the same normalized name, return its ID without creating a duplicate
	// or firing another retrospective. The model can compose a "you're
	// already tracking that" card from the AlreadyExists flag.
	existing, err := t.Concerns.GetByNormName(ctx, norm, false)
	if err != nil {
		return "", fmt.Errorf("declare_concern: dedupe lookup: %w", err)
	}
	if existing != nil {
		out := declareConcernResult{
			ConcernID:            existing.ID,
			Name:                 existing.Name,
			Description:          existing.Description,
			State:                existing.State,
			AlreadyExists:        true,
			RetrospectiveStarted: false,
		}
		b, _ := json.Marshal(out)
		return string(b), nil
	}

	c := store.Concern{
		ID:           idgen.New(),
		Name:         name,
		NormName:     norm,
		Description:  description,
		State:        store.ConcernStateActive,
		Source:       store.ConcernSourceUser,
		Confidence:   1.0,
		LastActiveAt: now,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := t.Concerns.Insert(ctx, c); err != nil {
		return "", fmt.Errorf("declare_concern: insert: %w", err)
	}

	if t.EventLog != nil {
		_, _ = t.EventLog.Append(ctx, log.KindConcernCreated, "synth", map[string]any{
			"concern_id": c.ID, "name": c.Name, "source": c.Source, "state": c.State,
		})
	}
	if t.Bus != nil {
		t.Bus.Publish(eventbus.ConcernProposedEvent{
			ConcernID:   c.ID,
			Name:        c.Name,
			Description: c.Description,
			Source:      c.Source,
			Confidence:  c.Confidence,
		})
	}

	retroStarted := false
	if t.Dispatcher != nil {
		t.Dispatcher.Dispatch(c.ID)
		retroStarted = true
	}

	out := declareConcernResult{
		ConcernID:            c.ID,
		Name:                 c.Name,
		Description:          c.Description,
		State:                c.State,
		AlreadyExists:        false,
		RetrospectiveStarted: retroStarted,
	}
	b, _ := json.Marshal(out)
	return string(b), nil
}

// recordingDispatcher captures Dispatch calls — used by tests and by the
// CLI's --dry-run path to preview what would be tagged without spending
// LLM calls.
type recordingDispatcher struct {
	Calls []string
}

func (r *recordingDispatcher) Dispatch(concernID string) {
	r.Calls = append(r.Calls, concernID)
}

// V2.5.0 Phase 3 — lookup_concern + read_concern_evidence tools.
//
// These two tools land the concern-scoped query path into the reactive
// Ask loop. Typical model trace for "what's happening with construction?":
//
//   1. lookup_concern("construction") → {concern_id, name, match_score}
//   2. read_concern_evidence(concern_id) → prose evidence (≤600 chars)
//   3. Compose final card grounded in the evidence prose.
//
// Both tools are registered into the reactive registry only when the
// concerns repos are non-nil — eval and replay paths leave them nil so
// the V2.4 byte-equal hot path holds.

// lookupConcernMatchThreshold is the minimum match score required for
// a positive match. Below this, lookup_concern returns an empty
// object so the model treats the query as un-related to any concern.
const lookupConcernMatchThreshold = 0.5

// LookupConcernTool implements llm.Tool. Resolves a free-form query
// to (at most) one active or paused concern. Substring-based for V2.5
// — the embedding-cosine path is a Phase 5 enhancement when the
// MemoryRanker is wired through.
type LookupConcernTool struct {
	Concerns *store.ConcernRepo
}

func (t *LookupConcernTool) Name() string { return "lookup_concern" }

func (t *LookupConcernTool) Description() string {
	return "Resolve a user query to a concern by name. Use when the query mentions a long-running situation (e.g. 'what's happening with construction?'). Returns the matched concern's id and name; empty when no concern matches."
}

func (t *LookupConcernTool) Parameters() []llm.ToolParamSpec {
	return []llm.ToolParamSpec{
		{
			Name:        "query",
			Type:        "string",
			Description: "The user's question or topic phrase (e.g. 'construction', 'frankfurt trip').",
			Required:    true,
		},
	}
}

type lookupConcernResult struct {
	ConcernID  string  `json:"concern_id,omitempty"`
	Name       string  `json:"name,omitempty"`
	MatchScore float64 `json:"match_score,omitempty"`
}

func (t *LookupConcernTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	if t.Concerns == nil {
		return "{}", nil
	}
	query, _ := args["query"].(string)
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return "{}", nil
	}

	active, err := t.Concerns.ListActive(ctx)
	if err != nil {
		return "", fmt.Errorf("lookup_concern: list active: %w", err)
	}
	paused, err := t.Concerns.ListByState(ctx, store.ConcernStatePaused)
	if err != nil {
		return "", fmt.Errorf("lookup_concern: list paused: %w", err)
	}
	candidates := append(active, paused...)
	if len(candidates) == 0 {
		return "{}", nil
	}

	bestID := ""
	bestName := ""
	bestScore := 0.0
	for _, c := range candidates {
		score := substringMatchScore(query, strings.ToLower(c.Name+" "+c.Description))
		if score > bestScore {
			bestScore = score
			bestID = c.ID
			bestName = c.Name
		}
	}
	if bestScore < lookupConcernMatchThreshold {
		return "{}", nil
	}
	out := lookupConcernResult{ConcernID: bestID, Name: bestName, MatchScore: bestScore}
	b, _ := json.Marshal(out)
	return string(b), nil
}

// substringMatchScore is a cheap substring-overlap heuristic. Every
// query token that appears in the haystack adds 1; the score is
// normalized to [0,1] by dividing by query token count.
//
// Phase 5 may swap this for an embedding-cosine path; for now the
// substring heuristic resolves the canonical "construction" /
// "frankfurt" cases reliably without an embedder dependency.
func substringMatchScore(query, haystack string) float64 {
	tokens := strings.Fields(query)
	if len(tokens) == 0 {
		return 0
	}
	hits := 0
	for _, tok := range tokens {
		if len(tok) < 2 {
			continue
		}
		if strings.Contains(haystack, tok) {
			hits++
		}
	}
	return float64(hits) / float64(len(tokens))
}

// ReadConcernEvidenceTool implements llm.Tool. Returns prose evidence
// for one concern — the top-N most-recent tagged observations,
// formatted as compact bullets the model can drop into a card's `sub`.
type ReadConcernEvidenceTool struct {
	Concerns     *store.ConcernRepo
	Observations *store.ConcernObservationRepo
	Reader       log.Reader
	Now          func() time.Time
}

func (t *ReadConcernEvidenceTool) Name() string { return "read_concern_evidence" }

func (t *ReadConcernEvidenceTool) Description() string {
	return "Pull the most-recent observations tagged to one concern. Use after lookup_concern returns a concern_id. Returns prose evidence (≤600 chars) suitable for a card's sub field."
}

func (t *ReadConcernEvidenceTool) Parameters() []llm.ToolParamSpec {
	return []llm.ToolParamSpec{
		{
			Name:        "concern_id",
			Type:        "string",
			Description: "The concern id from lookup_concern.",
			Required:    true,
		},
		{
			Name:        "max_observations",
			Type:        "integer",
			Description: "Optional cap on returned items (default 5, max 8).",
			Required:    false,
		},
	}
}

const readConcernEvidenceMaxObservations = 8
const readConcernEvidenceProseCap = 600

func (t *ReadConcernEvidenceTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	if t.Concerns == nil || t.Observations == nil || t.Reader == nil {
		return "", errors.New("read_concern_evidence: tool not configured")
	}
	concernID, _ := args["concern_id"].(string)
	concernID = strings.TrimSpace(concernID)
	if concernID == "" {
		return "", errors.New("read_concern_evidence: concern_id required")
	}
	maxObs := 5
	if v, ok := args["max_observations"].(float64); ok && v > 0 {
		maxObs = int(v)
	}
	if maxObs > readConcernEvidenceMaxObservations {
		maxObs = readConcernEvidenceMaxObservations
	}

	now := time.Now()
	if t.Now != nil {
		now = t.Now()
	}

	ev, err := projection.QueryConcernEvidence(ctx, projection.QueryConcernEvidenceDeps{
		Concerns: t.Concerns,
		Tags:     t.Observations,
		Reader:   t.Reader,
	}, concernID, projection.QueryConcernEvidenceOpts{
		MaxObservations: maxObs,
		Now:             now,
	})
	if err != nil {
		return "", fmt.Errorf("read_concern_evidence: %w", err)
	}
	if ev == nil {
		return "", fmt.Errorf("read_concern_evidence: concern %s not found or terminal", concernID)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Concern: %s. ", ev.ConcernName)
	if len(ev.Observations) == 0 {
		sb.WriteString("No recent activity.")
	} else {
		fmt.Fprintf(&sb, "%d recent observations: ", len(ev.Observations))
		refs := make([]string, 0, len(ev.Observations))
		for i, o := range ev.Observations {
			if i > 0 {
				sb.WriteString("; ")
			}
			fmt.Fprintf(&sb, "[%s] %s", o.Date.Format("2006-01-02"), o.Title)
			refs = append(refs, o.EventID)
		}
		sb.WriteString(".")
		// V2.5.0 P3: publish observation IDs through the loop's
		// context-bound refs collector. The loop reads these and
		// stamps the trace step's Refs field. The eval harness folds
		// these into ConcernRetrievalQuality scoring.
		llm.AppendRefsToContext(ctx, refs...)
	}
	out := sb.String()
	if len(out) > readConcernEvidenceProseCap {
		out = out[:readConcernEvidenceProseCap-1] + "…"
	}
	return out, nil
}

// Compile-time interface checks.
var (
	_ llm.Tool = (*LookupConcernTool)(nil)
	_ llm.Tool = (*ReadConcernEvidenceTool)(nil)
	_ llm.Tool = (*DeclareConcernTool)(nil)
)
