package synth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
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

// PinnedCard is the lean view of the source card injected into the
// converse prompt. Only the fields the model needs to anchor the
// conversation — the persisted store.Card has more (origin, run_id,
// dismissed, ...) that don't belong in the prompt.
//
// V2.13.1: EventUID is the calendar event the card is anchored on,
// derived server-side via title match against today's projection.
// When set, the converse prompt instructs the model to pass it as
// `context_id` on any send_whatsapp_message call so the V2.13
// assistant-mode reply correlation tracks the right event. Empty
// when the card isn't a calendar/personal card or no event match
// was found.
type PinnedCard struct {
	ID       string
	Title    string
	Sub      string
	SrcLabel string
	Meta     []string
	EventUID string
}

// PriorTurn is one previously-completed turn of the conversation,
// rendered into the prompt so the model has continuity.
type PriorTurn struct {
	Prompt string
	Reply  SubCard
}

// ConverseDeps bundles everything Converse needs. Mirrors ReactiveDeps
// minus the WhatsApp / concern-declaration / morning-bias fields that
// don't apply to a card-scoped follow-up.
type ConverseDeps struct {
	LLM           llm.Provider
	Reader        log.Reader
	Tasks         *store.TaskRepo // V2.11: backs read_tasks
	ProjCfg       projection.Config
	Memory        *store.MemoryRepo
	MemoryRanker  *MemoryRanker
	Prompts       *PromptSet
	Date          string
	Now           time.Time
	State         State
	Deadline        time.Duration
	ToolTimeout     time.Duration
	FinalCallBudget time.Duration // per-call deadline for the loop's final wrap-up LLM call; 0 → 15s (llm.LoopConfig default)
	MaxIterations   int
	Logger        *logrus.Entry

	Bus *eventbus.Bus

	// Optional Jina web tools — same pattern as ReactiveDeps.
	JinaClient JinaClient
	JinaCache  *jina.Store
	SearchTTL  time.Duration
	ReadTTL    time.Duration

	LoopObserver llm.LoopObserver
	OnSynthRun   func(stage, outcome string, dur time.Duration)

	WiredIntents []WiredIntent

	// V2.12: WhatsApp send tools. Same wiring as ReactiveDeps — both
	// closures must be set together. Eval/replay paths leave both nil
	// so historical fixture traces stay byte-equal.
	ResolveContact      ResolveContactFn
	SendWhatsAppMessage SendWhatsAppPreviewFn

	// V2.13.2: source for the "Recent WhatsApp activity" prompt block.
	// Same wiring as ReactiveDeps — when non-nil, the projection
	// surfaces recent assistant-mode sends so card-anchored asks like
	// "did Dana reply?" don't lose memory of prior outbound
	// activity.
	ExpectedReplies *store.ExpectedReplyRepo

	// Conversation context — pinned card + prior turns.
	Card       PinnedCard
	PriorTurns []PriorTurn
}

// Converse runs one turn of a card-scoped conversation. Returns the
// typed SubCard reply, the trace, and any memory candidates the model
// emitted. On timeout or hard failure returns a degraded answer-kind
// SubCard so the UI never sees a raw error.
func Converse(ctx context.Context, d ConverseDeps, query string) (retCard SubCard, retTrace llm.Trace, retMems []llm.MemoryCandidate, retErr error) {
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
		d.OnSynthRun("card_converse", outcome, time.Since(runStart))
	}()

	logger := d.Logger
	if logger == nil {
		logger = logrus.NewEntry(logrus.New())
	}
	logger = logger.WithFields(logrus.Fields{"query": query, "card_id": d.Card.ID})

	tz := d.ProjCfg.TZ
	if tz == nil {
		tz = time.UTC
	}

	var cal []projection.CalendarEvent
	if c, err := (projection.TodaysCalendar{Cfg: d.ProjCfg}).Compute(ctx, d.Reader); err != nil {
		logger.WithError(err).Warn("converse: calendar projection failed — continuing without it")
	} else {
		cal = c
	}
	// V2.13.1: derive the anchored event UID for calendar/personal cards
	// so the model can pass it as context_id without title-matching.
	pinned := d.Card
	if pinned.EventUID == "" {
		pinned.EventUID = AnchorEventUID(pinned, cal)
	}
	var threads []projection.Thread
	if t, err := (projection.OpenEmailThreads{Cfg: d.ProjCfg}).Compute(ctx, d.Reader); err != nil {
		logger.WithError(err).Warn("converse: threads projection failed — continuing without it")
	} else {
		threads = t
	}
	var window *projection.Window
	if w, err := (projection.RunWindow{Cfg: d.ProjCfg}).Compute(ctx, d.Reader); err != nil {
		logger.WithError(err).Warn("converse: run-window projection failed — continuing without it")
	} else {
		window = w
	}
	var memory []projection.MemoryFact
	poolLimit := ReactiveMemoryLimit
	if d.MemoryRanker != nil {
		poolLimit = ReactivePoolLimit
	}
	if m, err := (projection.MemoryFacts{Repo: d.Memory, Config: projection.MemoryFactsConfig{Limit: poolLimit}}).Compute(ctx, d.Reader); err != nil {
		logger.WithError(err).Warn("converse: memory projection failed — continuing without it")
	} else if d.MemoryRanker != nil {
		memory = d.MemoryRanker.Rank(ctx, query, m, ReactiveMemoryLimit)
	} else {
		memory = m
	}

	// V2.13.2: recent assistant-mode WhatsApp sends.
	// V2.13.3d: TZ for local-clock formatting in the prompt.
	var whatsappActivity []projection.WhatsAppActivity
	if d.ExpectedReplies != nil {
		nowFn := func() time.Time { return d.Now }
		if a, err := (projection.WhatsAppActivityProjection{
			Repo: d.ExpectedReplies,
			Now:  nowFn,
			TZ:   tz,
		}).Compute(ctx, cal, 5); err != nil {
			logger.WithError(err).Warn("converse: whatsapp activity projection failed — continuing without it")
		} else {
			whatsappActivity = a
		}
	}

	systemBuf := &bytes.Buffer{}
	if err := d.Prompts.Converse.Execute(systemBuf, map[string]any{
		"VoiceShort":       d.Prompts.VoiceShort,
		"State":            string(d.State),
		"Date":             d.Date,
		"TZ":               tz.String(),
		"Now":              d.Now.In(tz).Format("Mon 15:04"),
		"Query":            query,
		"Calendar":         cal,
		"Threads":          threads,
		"RunWindow":        window,
		"Memory":           memory,
		"Card":             pinned,
		"PriorTurns":       d.PriorTurns,
		"WiredIntents":     d.WiredIntents,
		"WhatsAppActivity": whatsappActivity,
	}); err != nil {
		return degradedSubCard(), llm.Trace{}, nil, fmt.Errorf("render converse prompt: %w", err)
	}

	reg := llm.NewRegistry(
		&ReadThreadTool{Reader: d.Reader, Now: func() time.Time { return d.Now }},
		&ReadEventTool{Reader: d.Reader, TZ: tz},
		&ReadWeatherWindowTool{Reader: d.Reader, TZ: tz},
		&ReadTasksTool{Tasks: d.Tasks, Reader: d.Reader, Now: func() time.Time { return d.Now }, TZ: tz},
	)
	// V2.12: WhatsApp send tools. Mirror the reactive loop: register
	// resolve_contact + send_whatsapp_message together so the model can
	// disambiguate the recipient and propose a Send card from a card-
	// scoped follow-up (e.g. "send these details to my wife on WhatsApp"
	// asked from the pinned card the details live on).
	if d.ResolveContact != nil {
		reg.Register(&ResolveContactTool{Resolve: d.ResolveContact})
	}
	if d.SendWhatsAppMessage != nil {
		reg.Register(&SendWhatsAppMessageTool{Preview: d.SendWhatsAppMessage})
	}
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
		maxIter = 6
	}
	toolTimeout := d.ToolTimeout
	if d.JinaClient != nil && (toolTimeout <= 0 || toolTimeout < 20*time.Second) {
		toolTimeout = 20 * time.Second
	}
	var loopChatOpts []llm.ChatOption
	if nativeSearch {
		loopChatOpts = append(loopChatOpts, llm.WithGoogleSearch())
	}

	result, loopErr := llm.RunLoop(ctx, d.LLM, systemBuf.String(), query, reg, llm.LoopConfig{
		MaxIterations:    maxIter,
		Deadline:         deadline,
		ToolTimeout:      toolTimeout,
		FinalCallBudget:  d.FinalCallBudget,
		ChatOptions:      loopChatOpts,
		Logger:           logger,
		Stage:            "card_converse",
		Observer:         d.LoopObserver,
	})

	if loopErr != nil {
		logger.WithError(loopErr).WithField("stopped", result.Stopped).Warn("converse: loop error — returning degraded sub-card")
		return degradedSubCard(), result.Trace, result.Memories, nil
	}

	sub, err := parseAndValidateSubCard(result.Content)
	if err == nil {
		postProcessSubCard(&sub, IntentSet(d.WiredIntents))
		return sub, result.Trace, result.Memories, nil
	}

	if t := llm.SummarizeText(result.Content); t != "" {
		result.Trace.Steps = append(result.Trace.Steps, llm.TraceStep{
			Kind: llm.KindThought,
			T:    t,
			MsAt: result.Trace.TotalMs,
		})
	}

	logger.WithError(err).
		WithField("stopped", result.Stopped).
		WithField("raw_preview", previewContent(result.Content, 500)).
		Warn("converse: validation failed — attempting repair")

	repaired, repairErr := repairSubCard(ctx, d, systemBuf.String(), query, result.Content, err)
	if repairErr != nil {
		logger.WithError(repairErr).
			WithField("raw_preview", previewContent(result.Content, 500)).
			Warn("converse: repair failed — returning degraded sub-card")
		return degradedSubCard(), result.Trace, result.Memories, nil
	}
	postProcessSubCard(&repaired, IntentSet(d.WiredIntents))
	return repaired, result.Trace, result.Memories, nil
}

// parseAndValidateSubCard strips fences, applies post-parse defaults
// (intent inference, ID fallback, kind-specific body sanity), and
// validates against the SubCard schema.
func parseAndValidateSubCard(raw string) (SubCard, error) {
	cleaned := stripCodeFences(raw)
	if cleaned == "" {
		return SubCard{}, fmt.Errorf("empty content")
	}
	var out SubCard
	if err := json.Unmarshal([]byte(cleaned), &out); err != nil {
		return SubCard{}, err
	}
	if out.ID == "" {
		out.ID = "sub-" + slugFromTitle(out.Title)
	}
	for i := range out.Actions {
		if out.Actions[i].Intent == "" {
			out.Actions[i].Intent = inferIntent(out.Actions[i].Label)
		}
	}
	rebuilt, err := json.Marshal(out)
	if err != nil {
		return SubCard{}, err
	}
	if err := ValidateJSON(SubCardSchema(), rebuilt); err != nil {
		return SubCard{}, err
	}
	return out, nil
}

// repairSubCard runs one repair pass after a validation failure.
func repairSubCard(ctx context.Context, d ConverseDeps, system, user, prevContent string, validationErr error) (SubCard, error) {
	msgs := []llm.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
		{Role: "assistant", Content: prevContent},
		{Role: "system", Content: subCardRepairMessage(validationErr)},
	}
	opts := []llm.ChatOption{}
	if d.LLM.JSONSchemaEnabled() {
		opts = append(opts, llm.WithJSONSchema("sub_card", SubCardSchemaMap()))
	}
	cr, err := d.LLM.ChatCompletion(ctx, msgs, nil, opts...)
	if err != nil {
		return SubCard{}, err
	}
	return parseAndValidateSubCard(cr.Content)
}

func subCardRepairMessage(validationErr error) string {
	errStr := validationErr.Error()
	base := fmt.Sprintf(
		"Your previous JSON failed validation: %s. Re-emit ONLY a valid JSON SubCard object — no prose, no code fences. `kind` must be exactly one of calendar, draft, research, answer.",
		errStr,
	)
	if strings.Contains(strings.ToLower(errStr), "minlength") {
		base += " A field is too short. Title must be ≥4 characters; eyebrow must be ≥1 character — pick a short label like \"answer\", \"draft · ready\", or \"research · 4 sources\"."
	}
	return base
}

// postProcessSubCard canonicalizes prose and drops actions whose intent
// isn't wired in this deployment.
func postProcessSubCard(s *SubCard, wired map[string]struct{}) {
	s.Title = canonicalizeMarkdown(s.Title)
	if s.Body != "" {
		s.Body = canonicalizeMarkdown(s.Body)
	}
	if s.Draft != "" {
		s.Draft = canonicalizeMarkdown(s.Draft)
	}
	if len(wired) > 0 && len(s.Actions) > 0 {
		kept := s.Actions[:0]
		for _, a := range s.Actions {
			if a.Intent == "" {
				kept = append(kept, a)
				continue
			}
			if _, ok := wired[a.Intent]; ok {
				kept = append(kept, a)
			}
		}
		s.Actions = kept
	}
	if s.Kind == "research" {
		sanitiseResearchSources(s)
	}
}

// sourceCountRE matches the "N sources" (or "N source") fragment in a
// research eyebrow so we can rewrite the count to match the actual
// emitted source list. The model frequently parrots the example label
// without counting what it actually produced.
var sourceCountRE = regexp.MustCompile(`\b\d+\s+sources?\b`)

// sanitiseResearchSources drops sources the model emitted as empty
// shells (`{i: 0, t: ""}` etc.), renumbers `i` 1-based by position so
// citations point somewhere stable, and reconciles the eyebrow's
// `N sources` count with the actual list length. If nothing of value
// survives, the card is downgraded to `kind: answer` so the UI never
// renders an empty "sources" subsection.
func sanitiseResearchSources(s *SubCard) {
	kept := s.Sources[:0]
	for _, src := range s.Sources {
		if strings.TrimSpace(src.T) == "" {
			continue
		}
		kept = append(kept, src)
	}
	for i := range kept {
		kept[i].I = i + 1
	}
	s.Sources = kept

	if len(s.Sources) == 0 {
		s.Sources = nil
		s.Kind = "answer"
		s.Eyebrow = "answer"
		return
	}

	count := fmt.Sprintf("%d sources", len(s.Sources))
	if len(s.Sources) == 1 {
		count = "1 source"
	}
	if sourceCountRE.MatchString(s.Eyebrow) {
		s.Eyebrow = sourceCountRE.ReplaceAllString(s.Eyebrow, count)
	}
}

// AnchorEventUID picks the today-calendar event whose title shares a
// distinctive token with the pinned card's title. Used by Converse so
// the model can pass `context_id: <UID>` to send_whatsapp_message
// without inferring the right event from the calendar list.
//
// Matching logic mirrors `cardMatchesText` in cards.go: at least one
// non-stopword token of length ≥5 must overlap. First match wins
// (calendar is sorted by start time, so the earliest matching event
// is preferred — usually the right one for "the dinner tonight").
//
// Returns "" when card.Title is empty, calendar is empty, or no
// match. The caller treats empty as "no anchor" and falls back to
// the V2.12 behavior (model picks event by title-matching itself).
func AnchorEventUID(card PinnedCard, cal []projection.CalendarEvent) string {
	title := strings.TrimSpace(card.Title)
	if title == "" || len(cal) == 0 {
		return ""
	}
	cardLower := strings.ToLower(title + " " + card.Sub)
	for _, ev := range cal {
		if strings.TrimSpace(ev.Title) == "" {
			continue
		}
		for _, tok := range distinctiveTokens(ev.Title) {
			if strings.Contains(cardLower, tok) {
				return ev.UID
			}
		}
	}
	return ""
}

// degradedSubCard surfaces a low-friction "couldn't reach an answer"
// reply when the loop times out or hard-fails.
func degradedSubCard() SubCard {
	return SubCard{
		ID:      "sub-degraded",
		Kind:    "answer",
		Eyebrow: "answer",
		Title:   "Couldn't reach an answer in time.",
		Body:    "Zeno ran out of time on this one. Try rephrasing or asking again.",
		Actions: []Action{{Label: "Done", Intent: "dismiss"}},
	}
}
