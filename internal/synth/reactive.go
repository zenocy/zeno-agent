package synth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/eventbus"
	"github.com/zenocy/zeno-v2/internal/jina"
	"github.com/zenocy/zeno-v2/internal/llm"
	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/projection"
	"github.com/zenocy/zeno-v2/internal/store"
)

// ReactiveMemoryLimit caps memory facts injected into the reactive Ask
// prompt. Smaller than the cards cap because reactive queries are short and
// small models choke on long context for ad-hoc lookups.
const ReactiveMemoryLimit = 8

// ReactivePoolLimit is the candidate pool size pulled from the memory store
// before re-ranking by query relevance. Larger than the prompt cap so the
// embedding ranker sees a real choice; small enough that the per-call work
// (one embed for the query, one cosine-vs-30 dot product) stays inside the
// reactive deadline.
const ReactivePoolLimit = 30

// ReactiveDeps bundles everything Ask needs. Constructed per request.
type ReactiveDeps struct {
	LLM           llm.Provider
	Reader        log.Reader
	Tasks         *store.TaskRepo // V2.11: backs read_tasks; nil → tool returns empty
	ProjCfg       projection.Config
	Memory        *store.MemoryRepo // optional; nil → memory section renders empty
	MemoryRanker  *MemoryRanker     // optional; nil → ListTop ordering (V2.2.0 baseline)
	Prompts       *PromptSet
	Date          string // YYYY-MM-DD in local TZ
	Now           time.Time
	State         State         // V2.3.0 P2: one-line register hint inlined into reactive.tmpl; full fork is V2.3.1
	Deadline        time.Duration // loop deadline; 0 → 45s default
	ToolTimeout     time.Duration // per-tool execution timeout; 0 → 5s
	FinalCallBudget time.Duration // per-call deadline for the loop's final wrap-up LLM call; 0 → 15s (llm.LoopConfig default)
	MaxIterations   int           // max reactive-loop LLM calls; 0 → 4
	Logger        *logrus.Entry

	// V2.5.0 Phase 2: optional declare_concern wiring. All four must be
	// non-nil for the tool to be registered into the loop; eval/replay
	// paths leave these zero so historical fixture runs don't accidentally
	// create concern rows.
	Concerns                *store.ConcernRepo
	ConcernObservations     *store.ConcernObservationRepo
	Bus                     *eventbus.Bus
	EventLogWriter          log.Writer
	RetrospectiveDispatcher RetrospectiveDispatcher

	// V2.5.0 Phase 3: optional projected-concerns context. Top-N (≤3)
	// active+paused concerns rendered into the reactive prompt's
	// "Concerns the user is tracking" block — gives the model the
	// orientation it needs to decide whether to call lookup_concern.
	// Nil/empty → block renders nothing (template-guarded).
	ConcernsProjected []projection.Concern

	// V2.6: optional Jina web tools. When JinaClient is non-nil the
	// search_web + read_url tools are registered into the reactive
	// loop. JinaCache may be nil (cache-bypass mode) but is normally
	// wired so repeat queries don't burn the per-minute Jina budget.
	// SearchTTL/ReadTTL fall back to 6h/24h when zero.
	JinaClient JinaClient
	JinaCache  *jina.Store
	SearchTTL  time.Duration
	ReadTTL    time.Duration

	// LoopObserver receives per-LLM-call / tool / repair / loop-iteration
	// events from the underlying tool loop. Nil disables.
	LoopObserver llm.LoopObserver
	// OnSynthRun is invoked once with stage="reactive_ask" and outcome.
	OnSynthRun func(stage, outcome string, dur time.Duration)

	// V2.7: optional WhatsApp conversation context. When non-nil the
	// reactive prompt activates the WhatsApp register, asking the
	// model to populate Card.Speech with a 1–3 sentence plain-text
	// reply suitable for sending verbatim. Single-shot for v0; the
	// struct intentionally has no turn buffer so future multi-turn
	// support can extend without retrofitting every caller.
	Conversation *ConversationContext

	// V2.8.1: action vocabulary the runner advertises to the LLM.
	// See cards.go for the WiredIntent type. Reactive prompt
	// renders the same "Action verbs" section as the morning prompt
	// when this is non-empty.
	WiredIntents []WiredIntent

	// V2.10: optional add_task tool. When non-nil the reactive loop
	// registers AddTaskTool so the model can commit a task during
	// synthesis (parallel to declare_concern). Nil → tool is not
	// registered and the model must fall back to proposing an
	// `add_task` action button on the response card. Eval/replay
	// paths leave this nil so historical fixture runs do not mutate
	// the user's tasks file.
	AddTask AddTaskFn

	// V2.12: WhatsApp send tools. Both must be wired together — the
	// reactive prompt steers the model to call ResolveContact before
	// SendWhatsAppMessage, and registering only one half makes that
	// instruction unsatisfiable.
	//
	// Eval/replay paths leave both nil so historical traces don't drift
	// when WhatsApp config changes.
	ResolveContact      ResolveContactFn
	SendWhatsAppMessage SendWhatsAppPreviewFn

	// V2.13.2: source for the "Recent WhatsApp activity" prompt block.
	// When non-nil, the projection reads recent assistant-mode sends
	// (last 24h) so Zeno can answer "has X replied yet?" without losing
	// track of what it sent on the user's behalf. Nil → block renders
	// the empty-state line, eval/replay paths unaffected.
	ExpectedReplies *store.ExpectedReplyRepo
}

// ConversationContext describes the chat the user's query arrived
// from. Populated by internal/whatsapp.Service when bridging an
// inbound WhatsApp message into Ask; nil for the in-app reactive
// surface.
type ConversationContext struct {
	SenderName string // pushName or contact display name; may be empty
	GroupName  string // empty for DMs
	IsDM       bool
	IsMention  bool // group-only: was Zeno explicitly @-mentioned?
}

// Ask runs a single reactive synthesis for the given user query. It uses the
// same three read tools as the morning cards loop, capped at 4 iterations and
// a default 45s deadline (configurable via llm.reactive_deadline_sec). The
// budget targets the "ambient" UX feel: long enough for a 35B local model to
// reach an answer, short enough to never look like the UI hung. On timeout or
// hard failure the caller always gets a degraded low-rel card so the UI never
// surfaces a raw error.
//
// Returns any `remember:` memory candidates the main answer loop emitted (rare
// — the multiplexed contract is unreliable on local 35B models, which is why
// the dedicated ExtractFacts path exists). The handler runs ExtractFacts
// detached from this call so extraction never blocks the user-perceived
// answer latency. Degraded / repair / parse paths still surface candidates
// captured before the failure.
func Ask(ctx context.Context, d ReactiveDeps, query string) (retCard Card, retTrace llm.Trace, retMems []llm.MemoryCandidate, retErr error) {
	runStart := time.Now()
	defer func() {
		if d.OnSynthRun == nil {
			return
		}
		outcome := "ok"
		if retErr != nil {
			outcome = "failed"
		} else if retTrace.Stopped == "degraded" {
			outcome = "degraded"
		}
		d.OnSynthRun("reactive_ask", outcome, time.Since(runStart))
	}()

	log := d.Logger
	if log == nil {
		log = logrus.NewEntry(logrus.New())
	}
	log = log.WithField("query", query)

	tz := d.ProjCfg.TZ
	if tz == nil {
		tz = time.UTC
	}

	// Reactive needs the same "today's signals" surface that the morning
	// cards loop sees — otherwise the model can't *discover* what events,
	// threads, or run windows exist (the read tools require known
	// uids/subjects). Cheap because projections are SQL queries on the
	// already-open SQLite store.
	var cal []projection.CalendarEvent
	if c, err := (projection.TodaysCalendar{Cfg: d.ProjCfg}).Compute(ctx, d.Reader); err != nil {
		log.WithError(err).Warn("reactive: calendar projection failed — continuing without it")
	} else {
		cal = c
	}
	var calTomorrow []projection.CalendarEvent
	if c, err := (projection.TomorrowsCalendar{Cfg: d.ProjCfg}).Compute(ctx, d.Reader); err != nil {
		log.WithError(err).Warn("reactive: tomorrow calendar projection failed — continuing without it")
	} else {
		calTomorrow = c
	}
	var calWeek []projection.CalendarEvent
	if c, err := (projection.WeekCalendar{Cfg: d.ProjCfg}).Compute(ctx, d.Reader); err != nil {
		log.WithError(err).Warn("reactive: week calendar projection failed — continuing without it")
	} else {
		calWeek = c
	}
	var threads []projection.Thread
	if t, err := (projection.OpenEmailThreads{Cfg: d.ProjCfg}).Compute(ctx, d.Reader); err != nil {
		log.WithError(err).Warn("reactive: threads projection failed — continuing without it")
	} else {
		threads = t
	}
	var window *projection.Window
	if w, err := (projection.RunWindow{Cfg: d.ProjCfg}).Compute(ctx, d.Reader); err != nil {
		log.WithError(err).Warn("reactive: run-window projection failed — continuing without it")
	} else {
		window = w
	}
	var memory []projection.MemoryFact
	poolLimit := ReactiveMemoryLimit
	if d.MemoryRanker != nil {
		poolLimit = ReactivePoolLimit
	}
	if m, err := (projection.MemoryFacts{Repo: d.Memory, Config: projection.MemoryFactsConfig{Limit: poolLimit}}).Compute(ctx, d.Reader); err != nil {
		log.WithError(err).Warn("reactive: memory projection failed — continuing without it")
	} else if d.MemoryRanker != nil {
		memory = d.MemoryRanker.Rank(ctx, query, m, ReactiveMemoryLimit)
	} else {
		memory = m
	}

	// V2.13.2: recent assistant-mode WhatsApp sends so the model can
	// answer "has X replied yet?" — best-effort, empty on any failure.
	// V2.13.3d: TZ passed so SentAt/ResolvedAt render in the user's
	// local clock when the prompt template formats them.
	var whatsappActivity []projection.WhatsAppActivity
	if d.ExpectedReplies != nil {
		nowFn := func() time.Time { return d.Now }
		if a, err := (projection.WhatsAppActivityProjection{
			Repo: d.ExpectedReplies,
			Now:  nowFn,
			TZ:   tz,
		}).Compute(ctx, cal, 5); err != nil {
			log.WithError(err).Warn("reactive: whatsapp activity projection failed — continuing without it")
		} else {
			whatsappActivity = a
		}
	}

	systemBuf := &bytes.Buffer{}
	if err := d.Prompts.Reactive.Execute(systemBuf, map[string]any{
		"VoiceShort":       d.Prompts.VoiceShort,
		"State":            string(d.State),
		"Date":             d.Date,
		"TZ":               tz.String(),
		"Now":              d.Now.In(tz).Format("Mon 15:04"),
		"Query":            query,
		"Calendar":         cal,
		"CalendarTomorrow": calTomorrow,
		"CalendarWeek":     calWeek,
		"Threads":          threads,
		"RunWindow":        window,
		"Memory":           memory,
		"Concerns":         d.ConcernsProjected,
		"Conversation":     d.Conversation,
		"WiredIntents":     d.WiredIntents,
		"WhatsAppActivity": whatsappActivity,
	}); err != nil {
		return degradedCard(d.Date), llm.Trace{}, nil, fmt.Errorf("render reactive prompt: %w", err)
	}

	reg := llm.NewRegistry(
		&ReadThreadTool{Reader: d.Reader, Now: func() time.Time { return d.Now }},
		&ReadEventTool{Reader: d.Reader, TZ: tz},
		&ReadWeatherWindowTool{Reader: d.Reader, TZ: tz},
		&ReadTasksTool{Tasks: d.Tasks, Reader: d.Reader, Now: func() time.Time { return d.Now }, TZ: tz},
	)
	// V2.5.0: register declare_concern when wiring is present. Eval +
	// replay paths leave the deps nil so they keep their byte-equal
	// behavior across V2.4 fixtures.
	if d.Concerns != nil && d.ConcernObservations != nil && d.RetrospectiveDispatcher != nil {
		reg.Register(&DeclareConcernTool{
			Concerns:     d.Concerns,
			Observations: d.ConcernObservations,
			Bus:          d.Bus,
			EventLog:     d.EventLogWriter,
			Dispatcher:   d.RetrospectiveDispatcher,
			Now:          func() time.Time { return d.Now },
		})
	}
	// V2.5.0 Phase 3: lookup_concern + read_concern_evidence. These
	// are the two-step concern-scoped query path. Lookup needs only
	// the concerns repo; evidence needs both repos + the log reader.
	// Each is gated on its specific dep set so a partial wiring
	// degrades cleanly (e.g. lookup-only when there's no log reader).
	if d.Concerns != nil {
		reg.Register(&LookupConcernTool{Concerns: d.Concerns})
	}
	if d.Concerns != nil && d.ConcernObservations != nil && d.Reader != nil {
		reg.Register(&ReadConcernEvidenceTool{
			Concerns:     d.Concerns,
			Observations: d.ConcernObservations,
			Reader:       d.Reader,
			Now:          func() time.Time { return d.Now },
		})
	}
	// V2.10: add_task. Registered when the wiring layer provides a
	// dispatch closure. Eval/replay paths leave d.AddTask nil so
	// fixture traces stay byte-equal across V2.4/V2.5 corpora.
	if d.AddTask != nil {
		reg.Register(&AddTaskTool{Dispatch: d.AddTask})
	}
	// V2.12: WhatsApp send tools. Both registered together when the
	// wiring layer provides closures (i.e. WhatsApp + a Resolver are
	// configured); otherwise the reactive prompt's "Messaging" rules
	// are unreachable and silently inert.
	if d.ResolveContact != nil {
		reg.Register(&ResolveContactTool{Resolve: d.ResolveContact})
	}
	if d.SendWhatsAppMessage != nil {
		reg.Register(&SendWhatsAppMessageTool{Preview: d.SendWhatsAppMessage})
	}
	// V2.6: web tools. Registered together — the LLM is told to call
	// search_web first, then read_url on a result if needed.
	//
	// V2.x: when the provider has native search grounding (Gemini's
	// google_search) and it's enabled in config, skip search_web — the
	// model gets a more capable grounding path via WithGoogleSearch()
	// on every loop call, and citations come back via
	// ChatResult.Citations. read_url stays registered whenever
	// JinaClient is wired (Gemini has no native fetch for arbitrary
	// URLs).
	nativeSearch := d.LLM != nil && d.LLM.NativeSearchEnabled()
	if d.JinaClient != nil {
		ttls := jinaTTLs{Search: d.SearchTTL, Read: d.ReadTTL}
		if !nativeSearch {
			reg.Register(&SearchWebTool{Client: d.JinaClient, Cache: d.JinaCache, TTLs: ttls})
		}
		reg.Register(&ReadURLTool{Client: d.JinaClient, Cache: d.JinaCache, TTLs: ttls})
	}

	deadline := d.Deadline
	if deadline <= 0 {
		deadline = 45 * time.Second
	}

	maxIter := d.MaxIterations
	if maxIter <= 0 {
		// Default 6 (vs the V2.5 default of 4) leaves room for
		// search_web → read_url → synthesize when web tools are
		// registered. Reactive deadline still caps wall time.
		maxIter = 6
	}
	// When Jina is wired, ensure the per-tool timeout is generous
	// enough for cold reads (typical ~3-15s, occasionally 30s). The
	// global default is 5s which is right for DB-backed read tools but
	// hostile to network fetches.
	toolTimeout := d.ToolTimeout
	if d.JinaClient != nil && (toolTimeout <= 0 || toolTimeout < 20*time.Second) {
		toolTimeout = 20 * time.Second
	}
	var loopChatOpts []llm.ChatOption
	if nativeSearch {
		loopChatOpts = append(loopChatOpts, llm.WithGoogleSearch())
	}

	// Memory extraction is no longer co-launched here. It runs detached from
	// the request in the HTTP handler so the answer loop's latency is never
	// extended by a parallel-but-joined LLM call, and a fast answer no longer
	// kills a near-finished extraction. The handler consolidates extracted
	// candidates into the memory store directly when the goroutine completes.
	// See AskHandler.runDetachedExtractor.

	result, loopErr := llm.RunLoop(ctx, d.LLM, systemBuf.String(), query, reg, llm.LoopConfig{
		MaxIterations:    maxIter,
		Deadline:         deadline,
		ToolTimeout:      toolTimeout,
		FinalCallBudget:  d.FinalCallBudget,
		ChatOptions:      loopChatOpts,
		Logger:           log,
		Stage:            "reactive_ask",
		Observer:         d.LoopObserver,
	})

	if loopErr != nil {
		log.WithError(loopErr).WithField("stopped", result.Stopped).Warn("reactive: loop error — returning degraded card")
		return degradedCard(d.Date), result.Trace, result.Memories, nil
	}

	card, err := parseAndValidateCard(result.Content)
	if err == nil {
		postProcessCard(&card, d.Date, IntentSet(d.WiredIntents), d.Logger)
		return card, result.Trace, result.Memories, nil
	}

	// Validation failed — the model's freeform output is typically its
	// chain-of-thought reasoning (especially on reasoning-mode models that
	// dump prose to content rather than wrapping it in <think> tags or
	// putting it in reasoning_content). Surface it as a thought step so
	// the Trace UI reflects what the model actually did before the repair
	// pass cleaned things up.
	if t := llm.SummarizeText(result.Content); t != "" {
		result.Trace.Steps = append(result.Trace.Steps, llm.TraceStep{
			Kind: llm.KindThought,
			T:    t,
			MsAt: result.Trace.TotalMs,
		})
	}

	log.WithError(err).
		WithField("stopped", result.Stopped).
		WithField("raw_preview", previewContent(result.Content, 500)).
		Warn("reactive: validation failed — attempting repair")

	// Hold on to the parsed-but-invalid initial card. If the repair
	// pass fails too (truncated, empty, schema-rejected), prefer
	// shipping this slightly off-spec card over a degraded "couldn't
	// reach an answer" placeholder. The user typed a real query and
	// the model gave a real answer; a minLength miss on `sub` is
	// strictly less useful to surface than the actual content.
	initialParsed, initialParseErr := parseCardPermissive(result.Content)

	// One repair pass — feed the validation error back and ask for clean JSON.
	repaired, repairErr := repairCard(ctx, d, systemBuf.String(), query, result.Content, err)
	if repairErr == nil {
		postProcessCard(&repaired, d.Date, IntentSet(d.WiredIntents), d.Logger)
		return repaired, result.Trace, result.Memories, nil
	}

	if initialParseErr == nil && initialParsed.Title != "" {
		log.WithError(repairErr).
			WithField("raw_preview", previewContent(result.Content, 500)).
			Warn("reactive: repair failed — falling back to initial parsed card")
		postProcessCard(&initialParsed, d.Date, IntentSet(d.WiredIntents), d.Logger)
		return initialParsed, result.Trace, result.Memories, nil
	}

	log.WithError(repairErr).
		WithField("raw_preview", previewContent(result.Content, 500)).
		Warn("reactive: repair failed and no usable initial parse — returning degraded card")
	return degradedCard(d.Date), result.Trace, result.Memories, nil
}

// parseCardPermissive is the parse half of parseAndValidateCard,
// without the strict schema check. Used by Ask to fall back to the
// model's initial output when the repair pass also fails.
func parseCardPermissive(raw string) (Card, error) {
	cleaned := stripCodeFences(raw)
	if cleaned == "" {
		return Card{}, fmt.Errorf("empty content")
	}
	var out Card
	if err := json.Unmarshal([]byte(cleaned), &out); err != nil {
		return Card{}, err
	}
	if len(out.Actions) == 0 {
		out.Actions = []Action{{Label: "Dismiss", Intent: "dismiss"}}
	}
	for i := range out.Actions {
		if out.Actions[i].Intent == "" {
			out.Actions[i].Intent = inferIntent(out.Actions[i].Label)
		}
	}
	if out.Meta == nil {
		out.Meta = []string{}
	}
	return out, nil
}

// parseAndValidateCard strips code fences, parses, applies post-parse
// defaults (empty actions → [Dismiss], nil meta → []), and validates a
// single Card.
func parseAndValidateCard(raw string) (Card, error) {
	cleaned := stripCodeFences(raw)
	if cleaned == "" {
		return Card{}, fmt.Errorf("empty content")
	}
	var out Card
	if err := json.Unmarshal([]byte(cleaned), &out); err != nil {
		return Card{}, err
	}
	if len(out.Actions) == 0 {
		out.Actions = []Action{{Label: "Dismiss", Intent: "dismiss"}}
	}
	for i := range out.Actions {
		if out.Actions[i].Intent == "" {
			out.Actions[i].Intent = inferIntent(out.Actions[i].Label)
		}
	}
	if out.Meta == nil {
		out.Meta = []string{}
	}
	rebuilt, err := json.Marshal(out)
	if err != nil {
		return Card{}, err
	}
	if err := ValidateJSON(CardSchema(), rebuilt); err != nil {
		return Card{}, err
	}
	return out, nil
}

// reactiveRepairMessage builds the system message for the one-shot
// repair pass. Mirrors cardsRepairMessage in cards.go: detect specific
// validation-error classes and append targeted coaching instead of
// dumping the raw jsonschema string and hoping. maxLength on `sub` is
// no longer enforced post-parse (a slightly long answer is preferable
// to a degraded card), so this only special-cases minLength failures.
func reactiveRepairMessage(validationErr error) string {
	errStr := validationErr.Error()
	base := fmt.Sprintf(
		"Your previous JSON failed validation: %s. Re-emit ONLY a valid JSON Card object — no prose, no code fences.",
		errStr,
	)
	if strings.Contains(strings.ToLower(errStr), "minlength") {
		base += " A field is too short. Title must be ≥4 characters; sub must be ≥20 characters — write one concrete sentence (a time, a name, a count, a next step). If you produced a one-word or two-word sub, expand it with context drawn from the card's title."
	}
	return base
}

// repairCard asks the model to re-emit valid JSON after a validation failure.
// When json_schema mode is enabled the single-Card schema is sent along to
// constrain the output by construction.
func repairCard(ctx context.Context, d ReactiveDeps, system, user, prevContent string, validationErr error) (Card, error) {
	msgs := []llm.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
		{Role: "assistant", Content: prevContent},
		{Role: "system", Content: reactiveRepairMessage(validationErr)},
	}
	opts := []llm.ChatOption{}
	if d.LLM.JSONSchemaEnabled() {
		opts = append(opts, llm.WithJSONSchema("card", CardSchemaMap()))
	}
	cr, err := d.LLM.ChatCompletion(ctx, msgs, nil, opts...)
	if err != nil {
		return Card{}, err
	}
	return parseAndValidateCard(cr.Content)
}

// postProcessCard applies canonicalization and ID generation to a
// single card. V2.8.1 added the wired-set filter so reactive answers
// don't ship buttons that won't fire.
func postProcessCard(c *Card, date string, wired map[string]struct{}, logger *logrus.Entry) {
	c.Title = canonicalizeMarkdown(c.Title)
	c.Sub = canonicalizeMarkdown(c.Sub)
	c.Date = date
	c.ID = slugFromTitle(c.Title)
	if dropped := dropUnwiredActions(c, wired); dropped > 0 && logger != nil {
		logger.WithFields(logrus.Fields{
			"card_id": c.ID, "dropped": dropped, "remaining": len(c.Actions),
		}).Warn("reactive.action_dropped: removed actions whose intent isn't wired")
	}
}

// previewContent returns up to maxLen characters of s with a "(empty)"
// sentinel when s is empty/whitespace. Used to surface what the model
// actually emitted on validation failure — debugging "empty content"
// errors blind is rough, and this is cheap.
func previewContent(s string, maxLen int) string {
	if s == "" {
		return "(empty)"
	}
	if len(s) > maxLen {
		return s[:maxLen] + "…"
	}
	return s
}

// degradedCard is returned when the reactive loop times out or hard-fails.
// It surfaces in the UI as a low-rel grey card rather than a raw error.
func degradedCard(date string) Card {
	title := "Couldn't reach an answer in time."
	return Card{
		ID:       slugFromTitle(title),
		Date:     date,
		Source:   "ask",
		SrcLabel: "Generated",
		Rel:      "low",
		Title:    title,
		Sub:      "Zeno ran out of time on this one. Try rephrasing.",
		Meta:     []string{},
		Actions:  []Action{{Label: "Dismiss", Intent: "dismiss"}},
	}
}
