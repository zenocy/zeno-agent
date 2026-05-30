package synth

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/jina"
	"github.com/zenocy/zeno-v2/internal/llm"
	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/projection"
	"github.com/zenocy/zeno-v2/internal/store"
)

// WiredIntent is the V2.8.1 carrier for the action vocabulary the
// runner advertises to the LLM. Each entry is one intent the live
// `internal/action` registry has an Executor for; the prompt template
// renders the full slice as the "Action verbs" section so the LLM
// only learns about verbs that will actually fire on click. Lives in
// the synth package so internal/action can populate it without
// creating a synth → action import cycle.
type WiredIntent struct {
	Intent      string
	Mode        string // "one_click" | "preflight" | "confirm"
	Description string
}

// IntentSet returns a quick-lookup map for membership checks against
// a slice of WiredIntent. Allocated each call; cheap (≤25 entries).
func IntentSet(intents []WiredIntent) map[string]struct{} {
	out := make(map[string]struct{}, len(intents))
	for _, w := range intents {
		out[w.Intent] = struct{}{}
	}
	return out
}

// CardsMemoryLimit is the cap on derived-memory facts injected into the
// cards-loop system prompt. Pinned at the V2.2.0 plan-approval review:
// 20 facts × ~15 words = ~300 tokens of context, comfortably tolerable on
// Qwen3-35B; tighten to 12 if smaller models stress.
const CardsMemoryLimit = 20

// CardsPoolLimit is the candidate pool size pulled from the memory store
// before re-ranking by today's signals. Larger than the prompt cap so the
// ranker can re-order across most of the V2.2 50-fact total cap; smaller
// than 50 to keep the embedder pass cheap on cold starts where the index
// is empty.
const CardsPoolLimit = 50

// CardsDeps bundles everything the cards synthesis step needs. Constructed
// once by Runner per synth run.
type CardsDeps struct {
	LLM           llm.Provider
	Reader        log.Reader
	Tasks         *store.TaskRepo // V2.11: backs read_tasks; nil → tool returns empty
	ProjCfg       projection.Config
	Memory        *store.MemoryRepo // optional; nil → memory section renders empty
	MemoryRanker  *MemoryRanker     // optional; nil → ListTop ordering (V2.2.0 baseline)
	Prompts       *PromptSet
	Date          string // YYYY-MM-DD in local TZ
	Now           time.Time
	State         State // V2.3.0: register the cards prompt is biased under (cards_system.tmpl renders .StateBias)
	Logger        *logrus.Entry
	LoopTimeout     time.Duration // cards-loop deadline; 0 → 30s
	ToolTimeout     time.Duration // per-tool execution timeout; 0 → 5s
	FinalCallBudget time.Duration // per-call deadline for the loop's final wrap-up LLM call; 0 → 15s (llm.LoopConfig default)
	MaxIterations   int           // max cards-loop LLM calls; 0 → 4

	// V2.5.0 Phase 3: optional concern context.
	//
	// Concerns is the projected list (top-N by recency) injected into the
	// prompt's "Today's concerns" block — gives the model orientation
	// without listing observation IDs. Nil/empty → block renders nothing.
	//
	// ConcernRepo + ConcernObservationRepo are used by the post-process
	// pass `ResolveCardConcern` to attach a `ConcernID` to each persisted
	// card whose source observation is concern-tagged. Both nil → the
	// inheritance pass is skipped entirely; cards persist with
	// `ConcernID = nil` and the surfacing path is byte-equal to V2.4.
	Concerns               []projection.Concern
	ConcernRepo            *store.ConcernRepo
	ConcernObservationRepo *store.ConcernObservationRepo

	// LoopObserver receives per-call / per-tool / per-repair metric events
	// from the underlying LLM tool loop. Nil disables.
	LoopObserver llm.LoopObserver

	// V2.6: optional Jina web tools. When JinaClient is non-nil the
	// search_web + read_url tools are registered into the cards loop
	// so the morning briefing can pull a referenced URL or do a quick
	// public-web lookup. JinaCache is normally wired; SearchTTL/ReadTTL
	// fall back to 6h/24h when zero.
	JinaClient JinaClient
	JinaCache  *jina.Store
	SearchTTL  time.Duration
	ReadTTL    time.Duration

	// Phase 4: Markets context. When non-nil and the projection
	// returns a non-nil summary (something interesting today), the
	// rendered prompt gets a "Today's watchlist movements" block and
	// the model can emit a markets card. Nil → block omitted →
	// model never sees markets data and won't emit a markets card.
	MarketsCtx projection.MarketsContext

	// V2.8.1: action vocabulary the runner advertises to the LLM.
	// Empty (nil/length-0) means the prompt's "Action verbs" section
	// renders empty and the post-process pass falls through to the
	// V2.8 "all 16 canonical intents are valid" behavior — preserves
	// the replay path which doesn't have a registry.
	WiredIntents []WiredIntent

	// V2.13.0: list of event UIDs the cards loop must NOT re-propose a
	// "text attendee to confirm" `send_whatsapp` card for. Populated by
	// the runner from open ExpectedReply rows + same-day proactive sends
	// keyed on context_id. Empty disables the suppress block (legacy
	// behavior).
	ProposalSuppress []string

	// V2.x: entities surfaced in the last few days (one row per entity
	// key). Rendered into the "already surfaced recently" prompt block so
	// the model UPDATES an entity with what's new or DROPS it when nothing
	// changed, instead of regenerating a near-duplicate card. Empty
	// disables the block. The entity-key Upsert is the hard dedup
	// guarantee; this block is the soft "suppress unchanged" layer.
	RecentCards []store.RecentEntity
}

// SynthesizeCards builds the system prompt from today's projections, runs the
// tool-using loop, and returns a validated CardSet plus the loop's trace
// and any `remember:` memory candidates the model emitted (V2.2.0).
func SynthesizeCards(ctx context.Context, d CardsDeps) (CardSet, llm.Trace, []llm.MemoryCandidate, error) {
	cal, err := projection.TodaysCalendar{Cfg: d.ProjCfg}.Compute(ctx, d.Reader)
	if err != nil {
		return CardSet{}, llm.Trace{}, nil, fmt.Errorf("projection calendar: %w", err)
	}
	threads, err := projection.OpenEmailThreads{Cfg: d.ProjCfg}.Compute(ctx, d.Reader)
	if err != nil {
		return CardSet{}, llm.Trace{}, nil, fmt.Errorf("projection threads: %w", err)
	}
	window, err := projection.RunWindow{Cfg: d.ProjCfg}.Compute(ctx, d.Reader)
	if err != nil {
		return CardSet{}, llm.Trace{}, nil, fmt.Errorf("projection run window: %w", err)
	}
	// Best-effort: a markets-projection failure shouldn't kill the
	// morning synth. If the read fails we just fall through with
	// markets=nil and the prompt's `{{- if .Markets }}` block stays
	// empty.
	markets, _ := d.MarketsCtx.Compute(ctx, d.Reader)
	poolLimit := CardsMemoryLimit
	if d.MemoryRanker != nil {
		poolLimit = CardsPoolLimit
	}
	memory, err := projection.MemoryFacts{Repo: d.Memory, Config: projection.MemoryFactsConfig{Limit: poolLimit}}.Compute(ctx, d.Reader)
	if err != nil {
		return CardSet{}, llm.Trace{}, nil, fmt.Errorf("projection memory: %w", err)
	}
	if d.MemoryRanker != nil {
		if q := CardsSyntheticQuery(cal, threads); q != "" {
			memory = d.MemoryRanker.Rank(ctx, q, memory, CardsMemoryLimit)
		} else if len(memory) > CardsMemoryLimit {
			memory = memory[:CardsMemoryLimit]
		}
	}

	tz := d.ProjCfg.TZ
	if tz == nil {
		tz = time.UTC
	}

	systemBuf := &bytes.Buffer{}
	if err := d.Prompts.CardsSystem.Execute(systemBuf, map[string]any{
		"VoiceShort":       d.Prompts.VoiceShort,
		"State":            string(d.State),
		"StateBias":        d.Prompts.StateBias[d.State],
		"Date":             d.Date,
		"TZ":               tz.String(),
		"Calendar":         cal,
		"Threads":          threads,
		"RunWindow":        window,
		"Memory":           memory,
		"Concerns":         d.Concerns,
		"Markets":          markets,
		"WiredIntents":     d.WiredIntents,
		"ProposalSuppress": d.ProposalSuppress,
		"RecentCards":      d.RecentCards,
	}); err != nil {
		return CardSet{}, llm.Trace{}, nil, fmt.Errorf("render cards prompt: %w", err)
	}

	user := fmt.Sprintf(
		"Synthesize today's cards. Today is %s. Use the read tools to expand any thread or event you reference.",
		d.Date,
	)

	reg := llm.NewRegistry(
		&ReadThreadTool{Reader: d.Reader, Now: func() time.Time { return d.Now }},
		&ReadEventTool{Reader: d.Reader, TZ: tz},
		&ReadWeatherWindowTool{Reader: d.Reader, TZ: tz},
		&ReadTasksTool{Tasks: d.Tasks, Reader: d.Reader, Now: func() time.Time { return d.Now }, TZ: tz},
		&ReadStockAlertTool{Reader: d.Reader},
	)
	// V2.x: when the provider has native search grounding (Gemini's
	// google_search) and it's enabled in config, skip the third-party
	// search_web tool — the model gets a more capable grounding path
	// via WithGoogleSearch() on every loop call, and citations come
	// back via ChatResult.Citations instead of through a tool round-
	// trip. read_url is still useful (Gemini has no native fetch) so
	// it stays registered whenever JinaClient is wired.
	nativeSearch := d.LLM != nil && d.LLM.NativeSearchEnabled()
	if d.JinaClient != nil {
		ttls := jinaTTLs{Search: d.SearchTTL, Read: d.ReadTTL}
		if !nativeSearch {
			reg.Register(&SearchWebTool{Client: d.JinaClient, Cache: d.JinaCache, TTLs: ttls})
		}
		reg.Register(&ReadURLTool{Client: d.JinaClient, Cache: d.JinaCache, TTLs: ttls})
	}

	loopDeadline := d.LoopTimeout
	if loopDeadline <= 0 {
		loopDeadline = 30 * time.Second
	}
	maxIter := d.MaxIterations
	if maxIter <= 0 {
		maxIter = 4
	}
	toolTimeout := d.ToolTimeout
	if d.JinaClient != nil && (toolTimeout <= 0 || toolTimeout < 20*time.Second) {
		toolTimeout = 20 * time.Second
	}
	var loopChatOpts []llm.ChatOption
	if nativeSearch {
		loopChatOpts = append(loopChatOpts, llm.WithGoogleSearch())
	}
	result, err := llm.RunLoop(ctx, d.LLM, systemBuf.String(), user, reg, llm.LoopConfig{
		MaxIterations:    maxIter,
		Deadline:         loopDeadline,
		ToolTimeout:      toolTimeout,
		FinalCallBudget:  d.FinalCallBudget,
		ChatOptions:      loopChatOpts,
		Logger:           d.Logger,
		Stage:            "cards",
		Observer:         d.LoopObserver,
	})
	if err != nil {
		return CardSet{}, result.Trace, result.Memories, fmt.Errorf("run cards loop (stopped=%s): %w", result.Stopped, err)
	}

	cardSet, err := parseAndValidateCardSet(result.Content)
	if err == nil {
		postProcessCards(&cardSet, d.Date, cal, threads, IntentSet(d.WiredIntents), d.Logger)
		applyConcernInheritance(ctx, &cardSet, d, cal, threads)
		return cardSet, result.Trace, result.Memories, nil
	}

	// Hold on to the initial parse (when it's at least valid JSON, even
	// if it failed schema validation). If the repair pass returns
	// truncated/invalid output — common on Gemini Flash 3 preview
	// where a minItems repair burns the full token budget on
	// reasoning and returns mid-string JSON — fall back to this
	// rather than degrading the whole briefing.
	initialParsed, initialParseErr := parseCardSetPermissive(result.Content)

	// One repair pass — feed the validation error back as a system message
	// and ask for a new attempt without tools.
	repaired, repairContent, repairErr := repairCards(ctx, d, systemBuf.String(), user, result.Content, err)
	if repairErr == nil {
		postProcessCards(&repaired, d.Date, cal, threads, IntentSet(d.WiredIntents), d.Logger)
		applyConcernInheritance(ctx, &repaired, d, cal, threads)
		return repaired, result.Trace, result.Memories, nil
	}

	// Repair failed. Fall back to the initial parse if it has at least
	// one card to surface — better to ship slightly off-spec content
	// than a degraded briefing. Validation errors that broke the
	// initial parse (e.g. minItems=2 on a quiet morning, or a too-long
	// title) don't matter here; the UI tolerates them and the user
	// gets *something* back.
	if initialParseErr == nil && len(initialParsed.Cards) > 0 {
		if d.Logger != nil {
			d.Logger.WithError(repairErr).WithField("cards_in_initial", len(initialParsed.Cards)).
				Warn("cards: repair failed — falling back to initial parsed response")
		}
		postProcessCards(&initialParsed, d.Date, cal, threads, IntentSet(d.WiredIntents), d.Logger)
		applyConcernInheritance(ctx, &initialParsed, d, cal, threads)
		return initialParsed, result.Trace, result.Memories, nil
	}

	return CardSet{}, result.Trace, result.Memories, fmt.Errorf(
		"validate cards: %w (initial response: %s | repair response: %s)",
		repairErr,
		snippet(result.Content),
		snippet(repairContent),
	)
}

// applyConcernInheritance is the V2.5.0 Phase 3 post-process that
// attaches a `ConcernID` to each card whose source observation is
// concern-tagged. Skipped cleanly when concerns wiring is absent —
// V2.4-era boots that haven't enabled concerns get byte-equal output.
//
// Calendar cards inherit by event UID match. Other source types
// (mail, personal, tasks, ask) are deferred until a stable
// observation-ID resolution path lands.
func applyConcernInheritance(
	ctx context.Context,
	cs *CardSet,
	d CardsDeps,
	cal []projection.CalendarEvent,
	threads []projection.Thread,
) {
	if d.ConcernRepo == nil || d.ConcernObservationRepo == nil {
		return
	}
	for i := range cs.Cards {
		if cs.Cards[i].ConcernID != nil {
			continue
		}
		cs.Cards[i].ConcernID = ResolveCardConcern(
			ctx, d.ConcernRepo, d.ConcernObservationRepo,
			cs.Cards[i], cal, threads, d.Logger,
		)
	}
}

// snippet returns a single-line, length-bounded preview of a model response
// suitable for embedding in error logs. Whitespace is collapsed so multi-line
// thinking blocks do not stretch the log entry.
func snippet(s string) string {
	const max = 400
	cleaned := strings.Join(strings.Fields(s), " ")
	if cleaned == "" {
		return "<empty>"
	}
	if len(cleaned) > max {
		return cleaned[:max] + "…"
	}
	return cleaned
}

// parseAndValidateCardSet strips code fences, parses, applies the small set
// of post-parse defaults (empty actions → [Dismiss], nil meta → []), and
// runs the JSON against the compiled CardSet schema. Defaults run before
// validation so a model that drops the actions field doesn't fail on a
// schema constraint that's about UI affordances rather than voice.
func parseAndValidateCardSet(raw string) (CardSet, error) {
	parsed, parseErr := parseCardSetPermissive(raw)
	if parseErr != nil {
		return CardSet{}, parseErr
	}
	if err := validateCardSet(parsed); err != nil {
		return CardSet{}, err
	}
	return parsed, nil
}

// parseCardSetPermissive is the parse half of parseAndValidateCardSet,
// without the strict schema check. It returns whatever it can decode
// from `raw` (with defaults applied) so callers can fall back to a
// "parsed but failed validation" CardSet when the repair pass also
// fails — happens on Gemini's thinking-mode repair runs that hit
// finish_reason=length and truncate mid-string.
func parseCardSetPermissive(raw string) (CardSet, error) {
	cleaned := stripCodeFences(raw)
	if cleaned == "" {
		return CardSet{}, fmt.Errorf("empty content")
	}
	var out CardSet
	if err := json.Unmarshal([]byte(cleaned), &out); err != nil {
		return CardSet{}, err
	}
	applyCardDefaults(&out)
	return out, nil
}

// validateCardSet runs the strict schema check on a parsed CardSet.
// Split from parsing so the caller can keep the parsed value around
// when validation fails — see usable-fallback path in CardsLoop.
func validateCardSet(cs CardSet) error {
	rebuilt, err := json.Marshal(cs)
	if err != nil {
		return err
	}
	return ValidateJSON(CardSetSchema(), rebuilt)
}

// applyCardDefaults fills in fields the model can drop without it being a
// voice problem. Actions are UI affordances; an empty meta array is fine
// but a nil array fails the schema's array-type check. A nil Cards slice is
// likewise normalised to an empty array so the validation error surfaces as
// the diagnostic "minItems" violation rather than the misleading "expected
// array, but got null" — `[]Card(nil)` marshals as JSON `null`, `[]Card{}`
// marshals as `[]`.
func applyCardDefaults(cs *CardSet) {
	if cs.Cards == nil {
		cs.Cards = []Card{}
	}
	for i := range cs.Cards {
		if len(cs.Cards[i].Actions) == 0 {
			cs.Cards[i].Actions = []Action{{Label: "Dismiss", Intent: "dismiss"}}
		}
		if cs.Cards[i].Meta == nil {
			cs.Cards[i].Meta = []string{}
		}
	}
}

// repairCards sends one synthetic system message naming the validation error
// and re-asks the model to produce only the JSON. No tools this time.
//
// When the client has json_schema mode enabled, this call sends the compiled
// CardSet schema so the model is constrained to emit `{"cards":[...]}` by
// construction — most repair failures are shape drift (model returns a bare
// array, a single card, or wraps under a different key) and schema-mode
// resolves them in one shot.
func repairCards(ctx context.Context, d CardsDeps, system, user, prevContent string, validationErr error) (CardSet, string, error) {
	msgs := []llm.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
		{Role: "assistant", Content: prevContent},
		{Role: "system", Content: cardsRepairMessage(validationErr)},
	}
	opts := []llm.ChatOption{}
	if d.LLM.JSONSchemaEnabled() {
		opts = append(opts, llm.WithJSONSchema("cards", CardSetSchemaMap()))
	}
	cr, err := d.LLM.ChatCompletion(ctx, msgs, nil, opts...)
	if err != nil {
		return CardSet{}, "", err
	}
	cs, vErr := parseAndValidateCardSet(cr.Content)
	return cs, cr.Content, vErr
}

// cardsRepairMessage builds the system message that re-asks the model
// after a validation error. For minLength failures (the most common
// content-side error on local models — a one-word sub or a 2-char title),
// append targeted guidance that names the floor and tells the model how
// to expand. Other errors fall through to the generic re-emit instruction.
func cardsRepairMessage(validationErr error) string {
	errStr := validationErr.Error()
	base := fmt.Sprintf(
		"Your previous JSON failed validation: %s. Re-emit ONLY a valid JSON CardSet object — no prose, no code fences.",
		errStr,
	)
	if strings.Contains(strings.ToLower(errStr), "minlength") {
		base += " A card field is too short. Title must be at least 4 characters; " +
			"sub must be at least 20 characters — write one concrete sentence (a time, a name, " +
			"a count, a next step). If you produced a one-word or two-word sub, expand it with " +
			"context drawn from the card's title and the calendar/thread it represents."
	}
	return base
}

// postProcessCards canonicalizes title markdown, regenerates IDs from
// titles, drops actions whose intent isn't in the wired set, and
// normalizes card sources against the projection inputs. Date is
// overwritten to the run's date so the upsert key is stable. Source
// normalization corrects the two routing mistakes the model on local
// 7B/8B endpoints reliably makes — see normalizeCardSources.
//
// V2.8.1: wired carries the live registry's intent set. Empty → drop
// pass is a no-op (preserves the V2.8.0 "all canonical intents valid"
// behavior; replay path).
func postProcessCards(cs *CardSet, date string, cal []projection.CalendarEvent, threads []projection.Thread, wired map[string]struct{}, logger *logrus.Entry) {
	for i := range cs.Cards {
		cs.Cards[i].Title = canonicalizeMarkdown(cs.Cards[i].Title)
		cs.Cards[i].Sub = canonicalizeMarkdown(cs.Cards[i].Sub)
		cs.Cards[i].Date = date

		// Server-generate IDs for stable upsert. The model's id is ignored
		// — local 7B/8B models drift on slugs. When the card resolves to a
		// known entity (event UID, thread, ticker, digest, proposal) the
		// entity key IS the ID, so a refresh or next-day run about the same
		// entity Upserts in place instead of minting a near-duplicate (the
		// V2.x repetition fix). Unanchored cards fall back to the title
		// slug (legacy behavior).
		cs.Cards[i].EntityKey = resolveEntityKey(&cs.Cards[i], date, cal, threads)
		if cs.Cards[i].EntityKey != "" {
			cs.Cards[i].ID = cs.Cards[i].EntityKey
		} else {
			cs.Cards[i].ID = stableCardID(&cs.Cards[i])
		}

		postProcessIntent(&cs.Cards[i])
		if dropped := dropUnwiredActions(&cs.Cards[i], wired); dropped > 0 && logger != nil {
			logger.WithFields(logrus.Fields{
				"card_id":   cs.Cards[i].ID,
				"dropped":   dropped,
				"remaining": len(cs.Cards[i].Actions),
			}).Warn("cards.action_dropped: removed actions whose intent isn't wired")
		}
	}
	normalizeCardSources(cs, cal, threads, logger)
	backfillMailTargets(cs, threads, logger)
}

// mailSourceIntents enumerates the action intents whose Executors look up
// a source mail thread via target.subject. Backfilled by
// backfillMailTargets when the LLM omits subject from target.
var mailSourceIntents = map[string]struct{}{
	"mark_read":   {},
	"move_mail":   {},
	"draft_reply": {},
	"send_reply":  {},
	"forward":     {},
	"flag_mail":   {},
}

// backfillMailTargets injects target.subject into mail-source actions
// whose target lacks one. The LLM frequently omits target.subject even
// though every mail Executor needs it to resolve the source thread —
// this safety net matches the card to a projection thread (best
// distinctive-token overlap, ties broken by most-recent LastReceived)
// and copies the thread's Subject into the action's target.
//
// Gated on action intent rather than Card.Source so cards whose source
// got flipped by normalizeCardSources (e.g. mail → tasks for deferred-
// work threads) still get their mail-action targets backfilled.
func backfillMailTargets(cs *CardSet, threads []projection.Thread, logger *logrus.Entry) {
	if cs == nil || len(threads) == 0 {
		return
	}
	for i := range cs.Cards {
		c := &cs.Cards[i]
		var matched *projection.Thread
		matchedOnce := false
		for j := range c.Actions {
			a := &c.Actions[j]
			if _, ok := mailSourceIntents[a.Intent]; !ok {
				continue
			}
			if stringFromTargetMap(a.Target, "subject") != "" {
				continue
			}
			if !matchedOnce {
				matched = bestMatchingThread(c, threads)
				matchedOnce = true
			}
			if matched == nil {
				continue
			}
			if a.Target == nil {
				a.Target = map[string]any{}
			}
			a.Target["subject"] = matched.Subject
			if logger != nil {
				logger.WithFields(logrus.Fields{
					"card_id": c.ID,
					"intent":  a.Intent,
					"subject": matched.Subject,
				}).Info("cards: backfilled mail target.subject")
			}
		}
	}
}

// bestMatchingThread returns the thread sharing the most distinctive
// tokens with the card's title+sub. Returns nil when no thread shares
// any distinctive token. Ties are broken by most-recent LastReceived
// — same recency preference the open_email_threads projection uses
// when picking the "current" subject for a thread bucket.
func bestMatchingThread(c *Card, threads []projection.Thread) *projection.Thread {
	cardLower := strings.ToLower(c.Title + " " + c.Sub)
	var best *projection.Thread
	bestHits := 0
	for i := range threads {
		t := &threads[i]
		hits := 0
		for _, tok := range distinctiveTokens(t.Subject) {
			if strings.Contains(cardLower, tok) {
				hits++
			}
		}
		if hits == 0 {
			continue
		}
		if hits > bestHits || (hits == bestHits && best != nil && t.LastReceived.After(best.LastReceived)) {
			best = t
			bestHits = hits
		}
	}
	return best
}

// stringFromTargetMap reads a string value from an action target map.
// Mirrors action.stringFromTarget but kept local to avoid a synth →
// action import cycle.
func stringFromTargetMap(target map[string]any, key string) string {
	if target == nil {
		return ""
	}
	v, ok := target[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// dropUnwiredActions removes actions whose final Intent is not in the
// live registry's wired set. Runs after postProcessIntent so the
// inference table has had its chance to backfill an empty Intent
// before we decide an action is unwired.
//
// When wired is empty (replay/eval path with no registry) this is a
// no-op — preserves the V2.8.0 behavior of "every canonical intent is
// valid". When wired is non-empty, any action whose intent is missing
// from the set is removed entirely; if a card ends with zero actions
// the schema's minitems=1 constraint is satisfied by appending a
// {"Dismiss", dismiss} fallback.
//
// Returns the number of actions dropped across the card so callers can
// log a single audit event per card rather than per action.
func dropUnwiredActions(c *Card, wired map[string]struct{}) int {
	if len(wired) == 0 {
		return 0
	}
	out := c.Actions[:0]
	dropped := 0
	for _, a := range c.Actions {
		if _, ok := wired[a.Intent]; ok {
			out = append(out, a)
			continue
		}
		dropped++
	}
	c.Actions = out
	if len(c.Actions) == 0 {
		// Backfill the minitems=1 floor with a safe default.
		// Only insert dismiss when it's itself wired — otherwise the
		// caller is running with a registry that doesn't even support
		// dismiss, in which case any default we pick is wrong; leave
		// the card empty and let the schema validator reject it.
		if _, ok := wired["dismiss"]; ok {
			c.Actions = []Action{{Label: "Dismiss", Intent: "dismiss"}}
		}
	}
	return dropped
}

// postProcessIntent backfills Action.Intent from Action.Label for any
// action where the model omitted the field. The pre-V2.8 prompt taught
// labels only; legacy stored cards and the entire eval corpus rely on
// this inference so they continue to dispatch correctly under the new
// intent registry.
//
// Lookup is conservative: an exact label match wins; otherwise a
// keyword scan picks the first matching intent in priority order; if
// nothing matches we fall back to "dismiss" (the safe no-op default
// every Executor implements).
func postProcessIntent(c *Card) {
	for i := range c.Actions {
		if c.Actions[i].Intent != "" {
			continue
		}
		c.Actions[i].Intent = inferIntent(c.Actions[i].Label)
	}
}

// InferIntent returns the structured action verb for a button label.
// Used both in this package (when the LLM omits the intent field) and
// in internal/action (handler.go, when a legacy UI sends the label as
// the action string). The order of substring checks matters — more
// specific phrases ("draft a reply") must run before generic ones
// ("reply"). Lowercased once at the top.
//
// Exported so internal/action can reuse the same inference table; the
// dispatch decision must not drift between the synth-side "where do
// stored cards' intents come from?" path and the handler-side "what
// does this legacy label dispatch to?" path.
func InferIntent(label string) string { return inferIntent(label) }

func inferIntent(label string) string {
	l := strings.ToLower(strings.TrimSpace(label))
	if l == "" {
		return "dismiss"
	}
	if intent, ok := exactLabelToIntent[l]; ok {
		return intent
	}
	for _, rule := range labelKeywordRules {
		for _, kw := range rule.keywords {
			if strings.Contains(l, kw) {
				return rule.intent
			}
		}
	}
	return "dismiss"
}

// V2.8.1 expansion verb labels merged into the inference table below.

// exactLabelToIntent matches the legacy-prompt and design-spec labels
// that we saw shipping in V2.0–V2.7 prompts and templates. Keys must
// be lowercased.
var exactLabelToIntent = map[string]string{
	"dismiss":          "dismiss",
	"snooze":           "snooze",
	"mute":             "dismiss",
	"mark read":        "mark_read",
	"mark as read":     "mark_read",
	"move":             "move_mail",
	"forward":          "forward",
	"draft a reply":    "draft_reply",
	"draft reply":      "draft_reply",
	"reply":            "draft_reply",
	"send":             "send_reply",
	"send reply":       "send_reply",
	"open":             "open_url",
	"open agenda":      "open_url",
	"open url":         "open_url",
	"decline":          "rsvp_no",
	"accept":           "rsvp_yes",
	"maybe":            "rsvp_maybe",
	"tentative":        "rsvp_maybe",
	"add to concerns":  "add_concern",
	"remember this":    "add_memory",
	"remember":         "add_memory",
	"ask zeno":         "ask_followup",
	"ask follow-up":    "ask_followup",
	"pick a slot":      "ask_followup",
	"show options":     "ask_followup",
	"suggest tomorrow": "ask_followup",

	// V2.8.1.
	"flag":              "flag_mail",
	"star":              "flag_mail",
	"mark important":    "flag_mail",
	"mark as important": "flag_mail",
	"unflag":            "flag_mail",
	"reschedule":        "reschedule_event",
	"move to next week": "reschedule_event",
	"push to next week": "reschedule_event",
	"cancel":            "cancel_event",
	"cancel meeting":    "cancel_event",
	"pin":               "pin_card",
	"pin to top":        "pin_card",
	"unpin":             "unpin_card",
	"remind me":         "set_reminder",
	"set a reminder":    "set_reminder",
	"add to tasks":      "add_task",
	"add task":          "add_task",
	"track as task":     "add_task",
	"mark done":         "complete_task",
	"mark complete":     "complete_task",
	"complete task":     "complete_task",
	"finish task":       "complete_task",
	"delete task":       "delete_task",
	"remove task":       "delete_task",
}

// labelKeywordRules runs in order; the first matching rule wins.
// Specific verbs ("block") come before broader words ("add").
var labelKeywordRules = []struct {
	keywords []string
	intent   string
}{
	{[]string{"draft "}, "draft_reply"},
	{[]string{"forward "}, "forward"},
	{[]string{"whatsapp", " via wa", "text "}, "send_whatsapp"},
	{[]string{"send "}, "send_reply"},
	{[]string{"reply"}, "draft_reply"},
	{[]string{"block "}, "block_calendar"},
	{[]string{"hold a window", "hold window"}, "block_calendar"},
	{[]string{"add to ", "schedule "}, "add_event"},
	{[]string{"tell ", " yes", "rsvp yes", "accept"}, "rsvp_yes"},
	{[]string{" no", "rsvp no", "decline"}, "rsvp_no"},
	{[]string{"maybe", "tentative"}, "rsvp_maybe"},
	{[]string{"move to ", "archive", "trash"}, "move_mail"},
	{[]string{"mark "}, "mark_read"},
	{[]string{"concern"}, "add_concern"},
	{[]string{"remember"}, "add_memory"},
	{[]string{"open "}, "open_url"},
	{[]string{"delegate", "ask ", "show "}, "ask_followup"},
	{[]string{"snooze", "later"}, "snooze"},
	{[]string{"dismiss", "mute", "ignore"}, "dismiss"},

	// V2.8.1 keyword rules.
	{[]string{"flag", "star", "important"}, "flag_mail"},
	{[]string{"reschedul", "push to ", "shift to "}, "reschedule_event"},
	{[]string{"cancel"}, "cancel_event"},
	{[]string{"pin"}, "pin_card"},
	{[]string{"remind"}, "set_reminder"},
	{[]string{"mark done", "mark complete", "complete task", "finish task"}, "complete_task"},
	{[]string{"delete task", "remove task"}, "delete_task"},
	{[]string{"task"}, "add_task"},
}

// normalizeCardSources fixes the two source-routing mistakes the model
// makes on every run regardless of how the prompt asks: tagged-personal
// calendar events emit as src=calendar (should be src=personal), and
// long-deferred / owed email threads emit as src=mail (should be
// src=tasks). Match by distinctive tokens shared between the card title
// (and sub) and the projection input. Conservative: only flip the source,
// never invent a new card or drop one. Empty/unmatched cards stay as-is.
func normalizeCardSources(cs *CardSet, cal []projection.CalendarEvent, threads []projection.Thread, logger *logrus.Entry) {
	for i := range cs.Cards {
		c := &cs.Cards[i]
		switch c.Source {
		case "calendar":
			for _, ev := range cal {
				if ev.Tag != "personal" {
					continue
				}
				if cardMatchesText(c, ev.Title) {
					if logger != nil {
						logger.WithField("card_title", c.Title).
							WithField("event_title", ev.Title).
							Info("cards: normalized src calendar→personal (event tag=personal)")
					}
					c.Source = "personal"
					break
				}
			}
		case "mail":
			for _, t := range threads {
				if !subjectIsDeferredWork(t.Subject) {
					continue
				}
				if cardMatchesText(c, t.Subject) {
					if logger != nil {
						logger.WithField("card_title", c.Title).
							WithField("subject", t.Subject).
							Info("cards: normalized src mail→tasks (deferred/owed subject)")
					}
					c.Source = "tasks"
					break
				}
			}
		}
	}
}

// subjectIsDeferredWork reports whether a thread subject reads as a work
// item the user must DO — parked memos, unfinished drafts, owed
// deliverables. The fixture corpus aligned to `tasks` for these in
// round 7, so the normalizer needs to flip mail→tasks deterministically
// rather than betting on the model picking the right source.
func subjectIsDeferredWork(subject string) bool {
	s := strings.ToLower(subject)
	return strings.Contains(s, "long-deferred") ||
		strings.Contains(s, "deferred") ||
		strings.Contains(s, "owed")
}

// cardMatchesText reports whether the card's title or sub shares at least
// one distinctive token (≥5 chars, non-stopword) with the source text.
// Conservative — false negatives are fine (the card stays at its model-
// chosen source), but false positives would mis-route an unrelated card.
func cardMatchesText(c *Card, source string) bool {
	cardLower := strings.ToLower(c.Title + " " + c.Sub)
	for _, tok := range distinctiveTokens(source) {
		if strings.Contains(cardLower, tok) {
			return true
		}
	}
	return false
}

var sourceMatchStopwords = map[string]struct{}{
	"about": {}, "after": {}, "before": {}, "every": {}, "from": {},
	"into": {}, "must": {}, "next": {}, "over": {}, "than": {},
	"that": {}, "this": {}, "those": {}, "under": {}, "until": {},
	"with": {}, "your": {}, "yours": {}, "their": {}, "them": {},
	"long-deferred": {}, "deferred": {}, "owed": {}, "draft": {}, // routing markers don't disambiguate which card
}

// distinctiveTokens returns lowercased tokens from s of length ≥5 that
// aren't in the stopword set. Punctuation splits tokens; digits stay.
func distinctiveTokens(s string) []string {
	s = strings.ToLower(s)
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return !isWordRune(r)
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if len(f) < 5 {
			continue
		}
		if _, skip := sourceMatchStopwords[f]; skip {
			continue
		}
		out = append(out, f)
	}
	return out
}

func isWordRune(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9')
}

var (
	boldRE = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	emRE   = regexp.MustCompile(`<em>([^<]+)</em>`)
)

// canonicalizeMarkdown rewrites **bold** and <em>x</em> to *italic* so the
// on-disk form is one shape. The frontend only has to handle `*...*`.
func canonicalizeMarkdown(s string) string {
	s = boldRE.ReplaceAllString(s, "*$1*")
	s = emRE.ReplaceAllString(s, "*$1*")
	return balanceMarkdown(s)
}

// balanceMarkdown drops a trailing unmatched `*` so we never ship odd-count
// asterisks. Models occasionally emit `... *terms` with no closer; the
// frontend would render the literal `*`. We strip from the right because a
// dangling opener is the common case.
func balanceMarkdown(s string) string {
	if strings.Count(s, "*")%2 == 0 {
		return s
	}
	if i := strings.LastIndex(s, "*"); i >= 0 {
		return s[:i] + s[i+1:]
	}
	return s
}

var stopwords = map[string]bool{
	"a": true, "an": true, "the": true, "and": true, "or": true, "but": true,
	"with": true, "your": true, "you": true, "is": true, "are": true,
	"to": true, "of": true, "in": true, "on": true, "for": true, "at": true,
	"re": true, "fwd": true, "fw": true,
}

var slugCleanRE = regexp.MustCompile(`[^a-z0-9]+`)

// stableCardID picks a deterministic ID for a card. For most cards this
// is `slugFromTitle`; the V2.13.0 exception is a `send_whatsapp` proposal
// card with `target.context_kind="event"` and a non-empty `context_id` —
// those use `propose-confirm-<event-uid-slug>` so re-emission across
// cards-loop ticks Upserts in place rather than producing duplicates.
func stableCardID(c *Card) string {
	if len(c.Actions) > 0 {
		a := c.Actions[0]
		if a.Intent == "send_whatsapp" && a.Target != nil {
			kind, _ := a.Target["context_kind"].(string)
			id, _ := a.Target["context_id"].(string)
			if kind == "event" && strings.TrimSpace(id) != "" {
				return "propose-confirm-" + slugifyUID(id)
			}
		}
	}
	return slugFromTitle(c.Title)
}

// slugifyUID produces a filesystem-safe slug from an event UID. UIDs from
// CalDAV are already short and printable, but they can contain `@`, `:`,
// `/`, etc. — strip to lowercase alphanumerics + dashes to keep the
// resulting card ID readable in the rail.
func slugifyUID(uid string) string {
	s := slugCleanRE.ReplaceAllString(strings.ToLower(uid), "-")
	s = strings.Trim(s, "-")
	if s == "" {
		h := sha1.Sum([]byte(uid))
		return hex.EncodeToString(h[:4])
	}
	if len(s) > 32 {
		s = s[:32]
	}
	return s
}

// slugFromTitle generates a short slug from the title: first non-stopword
// token, lowercased, plus a 4-char hash suffix to avoid collisions when two
// cards share a leading word.
func slugFromTitle(title string) string {
	clean := slugCleanRE.ReplaceAllString(strings.ToLower(title), "-")
	parts := strings.Split(clean, "-")
	for _, p := range parts {
		if p == "" || stopwords[p] {
			continue
		}
		h := sha1.Sum([]byte(title))
		return fmt.Sprintf("%s-%s", p, hex.EncodeToString(h[:2]))
	}
	// Fallback: pure hash.
	h := sha1.Sum([]byte(title))
	return "card-" + hex.EncodeToString(h[:4])
}

// stripCodeFences removes a leading ```json (or ```) fence and a trailing
// ``` from the model's content. Tolerates a missing language tag and
// surrounding whitespace.
func stripCodeFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// drop opening fence + optional language tag + newline
	if i := strings.Index(s, "\n"); i > 0 {
		s = s[i+1:]
	} else {
		s = strings.TrimPrefix(s, "```")
	}
	s = strings.TrimRight(s, " \t\n")
	if strings.HasSuffix(s, "```") {
		s = strings.TrimSuffix(s, "```")
	}
	return strings.TrimSpace(s)
}
