package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/llm"
	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/projection"
	"github.com/zenocy/zeno-v2/internal/synth"
)

// degradedReactiveTitle is the exact title set by synth.degradedCard when
// the reactive loop times out or hard-fails. The harness uses an exact
// title match (not just `rel == "low"`) so a fixture can legitimately
// return a low-rel "no data found" card without being flagged as degraded.
const degradedReactiveTitle = "Couldn't reach an answer in time."

// ReactiveResult is one reactive fixture's evaluated output. Mirrors
// RunResult but holds a single Card and a per-criterion expect-hits map
// rather than a parallel must-cards slice.
type ReactiveResult struct {
	FixtureName  string        `json:"fixture_name"`
	FixturePath  string        `json:"fixture_path"`
	Query        string        `json:"query"`
	Card         synth.Card    `json:"card"`
	Trace        llm.Trace     `json:"trace"`
	AskErr       string        `json:"ask_err,omitempty"`
	TotalLatency time.Duration `json:"total_latency_ms"`
	Degraded     bool          `json:"degraded"`
	Scoreboard   Scoreboard    `json:"scoreboard"`
	ExpectHits   ReactiveHits  `json:"expect_hits"`
}

// ReactiveHits records per-criterion pass/fail for a ReactiveExpect.
type ReactiveHits struct {
	Title       bool `json:"title"`         // empty TitleContains → true (no constraint)
	Sub         bool `json:"sub"`           // empty SubContains → true
	Src         bool `json:"src"`           // empty SrcIn → true
	Rel         bool `json:"rel"`           // empty RelIn → true
	NotDeg      bool `json:"not_degraded"`  // true when MustNotBeDegraded passes (or wasn't required)
	Speech      bool `json:"speech"`        // V2.7: empty SpeechContains → true
	SpeechClean bool `json:"speech_clean"`  // V2.7: empty SpeechMustNotContain → true
}

// AllPass reports whether every active criterion passed.
func (h ReactiveHits) AllPass() bool {
	return h.Title && h.Sub && h.Src && h.Rel && h.NotDeg && h.Speech && h.SpeechClean
}

// CheckReactiveExpect evaluates a single Card against a ReactiveExpect.
// Reuses substringHit for title/sub matching. Empty constraint lists are
// treated as "no constraint" — the corresponding hit is always true.
func CheckReactiveExpect(card synth.Card, e *ReactiveExpect, degraded bool) ReactiveHits {
	if e == nil {
		// No expectations declared — everything trivially passes.
		return ReactiveHits{
			Title: true, Sub: true, Src: true, Rel: true, NotDeg: true,
			Speech: true, SpeechClean: true,
		}
	}
	subClean := !containsAddressLeak(card.Sub)
	subFromList := substringHit(card.Sub, e.SubContains)
	out := ReactiveHits{
		Title:       substringHit(card.Title, e.TitleContains) && !containsAddressLeak(card.Title),
		Sub:         subFromList && subClean,
		Src:         len(e.SrcIn) == 0 || slices.Contains(e.SrcIn, card.Source),
		Rel:         len(e.RelIn) == 0 || slices.Contains(e.RelIn, card.Rel),
		NotDeg:      !e.MustNotBeDegraded || !degraded,
		Speech:      substringHit(card.Speech, e.SpeechContains),
		SpeechClean: noSubstringHit(card.Speech, e.SpeechMustNotContain),
	}
	return out
}

// noSubstringHit reports true when haystack contains NONE of needles
// (case-insensitive). Empty needles → trivially true. Used for the
// speech_must_not_contain veto list — any single hit fails.
//
// V2.12 also runs the always-on address-leak scan: a JID or phone-shaped
// digit run anywhere in the haystack fails the criterion regardless of
// what the fixture's needle list says. This is the regression bar for
// the WhatsApp send action surface — operational identifiers must never
// reach user-facing copy.
func noSubstringHit(haystack string, needles []string) bool {
	if containsAddressLeak(haystack) {
		return false
	}
	if len(needles) == 0 {
		return true
	}
	hay := strings.ToLower(haystack)
	for _, n := range needles {
		if strings.Contains(hay, strings.ToLower(n)) {
			return false
		}
	}
	return true
}

// addressLeakPatterns matches operational identifiers that should never
// appear in Card.Speech or Card.Sub. Compiled once at package init for
// the per-card scan.
//
//   - WhatsApp DM JIDs: <digits>@s.whatsapp.net
//   - WhatsApp group JIDs: <digits>@g.us
//   - Long contiguous digit runs (likely phone numbers in any of the
//     common formattings — "+447700900111", "447700900111",
//     "00447700900111").
var addressLeakPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\d{6,}@s\.whatsapp\.net`),
	regexp.MustCompile(`\d{6,}@g\.us`),
	regexp.MustCompile(`\+\d{8,}`),
	regexp.MustCompile(`\b\d{10,}\b`),
}

// containsAddressLeak reports whether s contains any operational
// identifier (JID / phone-shaped digit run). Always-on regression for
// the V2.12 WhatsApp send surface.
func containsAddressLeak(s string) bool {
	for _, re := range addressLeakPatterns {
		if re.FindStringIndex(s) != nil {
			return true
		}
	}
	return false
}

// disablingReader wraps a log.Reader and drops the responses for tools the
// fixture flagged in DisableTools. Implementation detail: we don't actually
// have hooks to intercept tool calls at this layer, so the harness today
// does NOT respect DisableTools. The field is reserved for future use; the
// `no_data` reactive fixture works by seeding zero matching events instead.
//
// (Reserved-for-future stub kept for shape. CheckReactiveExpect doesn't
// depend on it; the field is documented as "reserved" in the fixture doc.)

// RunReactiveFixture loads a reactive fixture, seeds an ephemeral store with
// its event log, invokes synth.Ask with the fixture's Query, and scores the
// returned single Card. Mirrors RunFixture's structure for the morning path.
func RunReactiveFixture(ctx context.Context, fixturePath string, opts RunOpts) (*ReactiveResult, error) {
	f, err := LoadFixture(fixturePath)
	if err != nil {
		return nil, err
	}
	if f.Kind != "reactive" {
		return nil, fmt.Errorf("fixture %s: kind=%q is not reactive", fixturePath, f.Kind)
	}
	if strings.TrimSpace(f.Query) == "" {
		return nil, fmt.Errorf("fixture %s: reactive fixtures require a non-empty query", fixturePath)
	}

	estore, err := NewEphemeralStore("")
	if err != nil {
		return nil, err
	}
	defer estore.Close()

	if err := estore.Seed(ctx, f); err != nil {
		return nil, fmt.Errorf("seed: %w", err)
	}

	now, err := reactiveQueryTime(f)
	if err != nil {
		return nil, err
	}

	logger := opts.Logger
	if logger == nil {
		logger = logrus.NewEntry(logrus.New())
	}

	loc, err := time.LoadLocation(f.User.TZ)
	if err != nil {
		loc = time.UTC
	}
	projCfg := projection.Config{
		TZ:                    loc,
		LookbackDays:          14,
		RunWindowMinMinutes:   45,
		RunWindowMaxWindKmh:   25,
		RunWindowEarliestHour: 6,
		RunWindowLatestHour:   20,
		OpenThreadsMax:        20,
		Now:                   func() time.Time { return now },
	}

	// Reactive needs its own deadline knob — cards-timeout (which RunOpts
	// uses for the morning cards loop) is the wrong axis. LM Studio +
	// Qwen3 routinely needs 60–120s for a single reactive answer; default
	// generous at 180s.
	deadline := opts.ReactiveTimeout
	if deadline == 0 {
		deadline = 180 * time.Second
	}

	// V2.3.0 P2: pass the fixture's expected_state through to the reactive
	// prompt so the one-line `Today's register: <state>` hint renders. The
	// full state-aware fork is V2.3.1; for now the hint exercises the wiring
	// and lets us catch regressions on reactive prompts.
	deps := synth.ReactiveDeps{
		LLM:         opts.LLM,
		Reader:      estore.Store.(log.Reader),
		ProjCfg:     projCfg,
		Prompts:     opts.Prompts,
		Date:        f.Today,
		Now:         now,
		State:       synth.State(f.ExpectedState),
		Deadline:    deadline,
		ToolTimeout: opts.ToolTimeout,
		Logger:      logger.WithField("c", "eval-reactive"),
	}
	// V2.7: WhatsApp register activation. When the fixture declares a
	// `whatsapp` block, populate the Conversation context so the
	// reactive prompt's WhatsApp instructions render and the model
	// fills Card.Speech with a chat-shaped reply we can then assert
	// against speech_contains / speech_must_not_contain.
	if f.WhatsApp != nil {
		deps.Conversation = &synth.ConversationContext{
			SenderName: f.WhatsApp.SenderName,
			GroupName:  f.WhatsApp.GroupName,
			IsDM:       f.WhatsApp.IsDM,
			IsMention:  f.WhatsApp.IsMention,
		}
	}

	start := time.Now()
	card, trace, _, askErr := synth.Ask(ctx, deps, f.Query)
	totalLat := time.Since(start)

	degraded := card.Title == degradedReactiveTitle && card.SrcLabel == "Generated"

	// Score the single card by wrapping in a CardSet and reusing ScoreAll
	// with an empty Briefing for the prose-text checks (calmness,
	// decisiveness, concreteness). The empty Briefing contributes empty
	// strings to CombinedProse, which is inert for every regex pattern.
	//
	// Schema checks need overriding: ScoreAll validates the wrapped CardSet
	// against synth.CardSetSchema() which requires minItems=2 — a single-
	// card wrap always fails. Reactive uses synth.CardSchema() (single
	// card). And the empty Briefing always fails BriefingSchema (minLength
	// on Title), but there is no briefing for reactive — set true.
	cs := synth.CardSet{Cards: []synth.Card{card}}
	// Reactive has no morning briefing → no persisted state; pass empty
	// actualState so the state_match stub falls back to morning_calm.
	// injectMode=true so the initial cards-schema check uses the
	// 1-card-min InjectCardSetSchema; the line below then overrides
	// CardsSchema with the single-Card validator that's correct for
	// the reactive surface anyway.
	sb := ScoreAll(synth.Briefing{}, cs, nil, f.Memory, "", f.ExpectedState, true)
	sb.BriefingSchema = true
	sb.BriefingSchema_ = ""
	cardJSON, _ := json.Marshal(card)
	if err := synth.ValidateJSON(synth.CardSchema(), cardJSON); err != nil {
		sb.CardsSchema = false
		sb.CardsSchema_ = err.Error()
	} else {
		sb.CardsSchema = true
		sb.CardsSchema_ = ""
	}
	sb.ItalicBalanced = ItalicBalanced(card.Title) && ItalicBalanced(card.Sub)

	res := &ReactiveResult{
		FixtureName:  fixtureNameFromPath(fixturePath),
		FixturePath:  fixturePath,
		Query:        f.Query,
		Card:         card,
		Trace:        trace,
		TotalLatency: totalLat,
		Degraded:     degraded,
		Scoreboard:   sb,
		ExpectHits:   CheckReactiveExpect(card, f.ReactiveExpect, degraded),
	}
	if askErr != nil {
		res.AskErr = askErr.Error()
	}
	return res, nil
}

// reactiveQueryTime resolves the synthesizer's "now" for a reactive fixture.
// Defaults to 10:00 local on the fixture's Today date when QueryTime is
// absent — matches the day-shape of "user types a query mid-morning".
func reactiveQueryTime(f *Fixture) (time.Time, error) {
	loc, err := time.LoadLocation(f.User.TZ)
	if err != nil {
		return time.Time{}, err
	}
	day, err := time.ParseInLocation("2006-01-02", f.Today, loc)
	if err != nil {
		return time.Time{}, err
	}
	hh, mm := 10, 0
	if s := strings.TrimSpace(f.QueryTime); s != "" {
		var h, m int
		if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
			return time.Time{}, fmt.Errorf("parse query_time %q: %w", s, err)
		}
		hh, mm = h, m
	}
	return day.Add(time.Duration(hh)*time.Hour + time.Duration(mm)*time.Minute), nil
}

// SummaryLineReactive is a one-line CLI summary per reactive fixture.
func SummaryLineReactive(r *ReactiveResult) string {
	flags := []string{}
	if r.Degraded {
		flags = append(flags, "DEGRADED")
	}
	if !r.Scoreboard.CardsSchema {
		flags = append(flags, "CARD-SCHEMA-FAIL")
	}
	if !r.ExpectHits.AllPass() {
		miss := []string{}
		if !r.ExpectHits.Title {
			miss = append(miss, "title")
		}
		if !r.ExpectHits.Sub {
			miss = append(miss, "sub")
		}
		if !r.ExpectHits.Src {
			miss = append(miss, "src")
		}
		if !r.ExpectHits.Rel {
			miss = append(miss, "rel")
		}
		if !r.ExpectHits.NotDeg {
			miss = append(miss, "not_degraded")
		}
		if !r.ExpectHits.Speech {
			miss = append(miss, "speech")
		}
		if !r.ExpectHits.SpeechClean {
			miss = append(miss, "speech_clean")
		}
		flags = append(flags, "EXPECT-MISS:"+strings.Join(miss, ","))
	}
	flagStr := ""
	if len(flags) > 0 {
		flagStr = " · " + strings.Join(flags, " ")
	}
	return fmt.Sprintf(
		"[%s] reactive · calm=%d decisive=%d concrete=%d total=%d/9 latency=%dms%s",
		r.FixtureName,
		r.Scoreboard.Calmness.Value,
		r.Scoreboard.Decisiveness.Value,
		r.Scoreboard.Concreteness.Value,
		r.Scoreboard.Total(),
		r.TotalLatency.Milliseconds(),
		flagStr,
	)
}
