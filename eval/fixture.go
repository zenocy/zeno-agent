// Package eval is the V2.1 evals harness for Zeno's synthesizer.
//
// The harness loads a fixture (a JSON file describing a "day shape"),
// seeds an ephemeral observation log with events, runs the real synth
// pipeline (Runner.Run) against it, and scores the resulting Briefing
// + Cards using deterministic checks for calmness, decisiveness,
// concreteness, schema validity, and latency. Results land in eval.db
// and an HTML report.
//
// Fixture format mirrors the existing benches/fixtures shape so authors
// can iterate in human-friendly JSON without thinking about event-log
// internals; the load step expands the shape into log.Event rows.
package eval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/zenocy/zeno-v2/internal/synth"
)

// CardSources accepts either a single source string ("mail") or an array
// (["mail","tasks"]) in JSON. Internal representation is always a slice;
// callers use Accepts to test membership. The custom UnmarshalJSON keeps
// the existing single-string fixture corpus working while letting newer
// fixtures express genuinely ambiguous routing (e.g. an email thread
// asking for an owed deliverable, which the model can defensibly emit as
// either `mail` or `tasks`).
type CardSources []string

// UnmarshalJSON accepts both forms: `"src": "mail"` and `"src": ["mail","tasks"]`.
func (s *CardSources) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '[' {
		var arr []string
		if err := json.Unmarshal(b, &arr); err != nil {
			return err
		}
		*s = arr
		return nil
	}
	var single string
	if err := json.Unmarshal(b, &single); err != nil {
		return err
	}
	*s = []string{single}
	return nil
}

// Accepts reports whether src is one of the permitted sources. An empty
// CardSources accepts nothing (callers should treat that as a programming
// error — every must-card needs at least one source).
func (s CardSources) Accepts(src string) bool {
	for _, v := range s {
		if v == src {
			return true
		}
	}
	return false
}

// Fixture is the human-authored JSON shape for one eval scenario. The
// Today + User block fixes the synthesizer's "now"; calendar/email/weather
// expand into observation-log events.
//
// Kind discriminates morning fixtures (default, kind="" or "morning") from
// reactive fixtures (kind="reactive"), which carry a Query + ReactiveExpect
// instead of MustCards.
type Fixture struct {
	Kind                 string               `json:"kind,omitempty"` // "" or "morning" (default) | "reactive"
	Today                string               `json:"today"`          // YYYY-MM-DD
	User                 FixtureUser          `json:"user"`
	Calendar             []FixtureEvent       `json:"calendar"`
	EmailThreads         []FixtureThread      `json:"email_threads"`
	Weather              *FixtureWeather      `json:"weather,omitempty"`
	RunWindow            *FixtureRunWindow    `json:"run_window,omitempty"`
	MustCards            []MustCard           `json:"must_cards,omitempty"`                // morning: deterministic match expectations (always enforced)
	MustCardsMorningCalm []MustCard           `json:"must_cards_morning_calm,omitempty"`   // V2.3.0 P2: enforced only when expected_state=morning_calm
	MustCardsPreMeeting  []MustCard           `json:"must_cards_pre_meeting,omitempty"`    // V2.3.0 P2: enforced only when expected_state=pre_meeting
	MustCardsDeepWork    []MustCard           `json:"must_cards_deep_work,omitempty"`      // V2.3.0 P2: enforced only when expected_state=deep_work
	MustCardsEndOfDay    []MustCard           `json:"must_cards_end_of_day,omitempty"`     // V2.3.0 P2: enforced only when expected_state=end_of_day
	MustCardsInject      []MustCard           `json:"must_cards_message_inject,omitempty"` // V2.3.0 P2: enforced only when expected_state=message_inject
	Memory               []FixtureMemoryFact  `json:"memory,omitempty"`                    // V2.2.0: derived-memory facts to seed before the run
	ExpectedState        string               `json:"expected_state,omitempty"`            // V2.3.0: adaptive-state register the fixture should land in (empty = morning_calm)
	InjectSignal         *FixtureInjectSignal `json:"inject_signal,omitempty"`             // V2.3.0 P1: test-only synthetic inject signal (Phase 3 ships the real producer)
	SynthTime            string               `json:"synth_time,omitempty"`                // V2.3.0 P1: HH:MM local time the morning synth runs at (default "06:30"); state fixtures override to land in their target window
	Query                string               `json:"query,omitempty"`                     // reactive: user-typed text fed to synth.Ask
	ReactiveExpect       *ReactiveExpect      `json:"reactive_expect,omitempty"`           // reactive: single-card expectations
	DisableTools         []string             `json:"disable_tools,omitempty"`             // reactive: tool names to stub-out (read_thread / read_event / read_weather_window)
	QueryTime            string               `json:"query_time,omitempty"`                // reactive: HH:MM local time the query is "asked at"; defaults to 10:00

	// V2.5.0 Phase 2 — concern recognition + retrospective fixtures.
	//
	// `concern_recognition` fixtures seed a log of email_threads (and optional
	// calendar events) covering `recognition_lookback_days`, then assert the
	// daily recognition pass produces concerns whose names match
	// `expected_concerns`. A negative fixture sets `expected_concerns: []`.
	//
	// `retrospective` fixtures pre-create one concern (via SeedConcerns) and
	// assert the retrospective tagger tags exactly the events listed in
	// `expected_concern_tags[concern_id]` from the historical log.
	ExpectedConcerns          []FixtureExpectedConcern `json:"expected_concerns,omitempty"`           // recognition: ground-truth set
	ExpectedConcernTags       map[string][]string      `json:"expected_concern_tags,omitempty"`       // retrospective: concern_id → expected event_ids
	RecognitionLookbackDays   int                      `json:"recognition_lookback_days,omitempty"`   // recognition: 0 → 14
	RetrospectiveLookbackDays int                      `json:"retrospective_lookback_days,omitempty"` // retrospective: 0 → 180
	RecognitionDailyCap       int                      `json:"recognition_daily_cap,omitempty"`       // recognition: 0 → 2
	SeedConcerns              []FixtureSeedConcern     `json:"seed_concerns,omitempty"`               // pre-create concerns for retrospective fixtures

	// V2.5.0 Phase 3 — concern-scoped query tests for reactive fixtures.
	// Each test runs synth.Ask once with the given Query and asserts
	// against the resulting concern_id (via the lookup_concern tool path)
	// and the set of observation IDs the trace touched (via the
	// TraceStep.Refs collector). MustNotMatchConcern flags the negative
	// case — the lookup must NOT fire on unrelated queries.
	ConcernQueryTests []FixtureConcernQueryTest `json:"concern_query_tests,omitempty"`

	// V2.6.0 — open-tasks seed. One task.snapshot row per entry is
	// appended before the synth run so the OpenTasks projection (and the
	// read_tasks LLM tool) sees them. Anchored to the fixture's Today
	// timestamp; UID is derived from title|due_date when not provided.
	OpenTasks []FixtureTask `json:"open_tasks,omitempty"`

	// V2.7 — stock sensor seed. Both kinds get appended to the
	// ephemeral log so the Stock + MarketsContext projections (morning
	// markets card) and RecentStockBreaches (inject path) pick them up.
	// MinutesAgo is relative to the fixture's `today` clock.
	StockSnapshots []FixtureStockSnapshot `json:"stock_snapshots,omitempty"`
	StockAlerts    []FixtureStockAlert    `json:"stock_alerts,omitempty"`

	// V2.7 — WhatsApp register activation for reactive fixtures.
	// Non-nil seeds the synth.ConversationContext that the reactive
	// prompt's WhatsApp block reads; the fixture's reactive_expect can
	// then assert speech_contains / speech_must_not_contain against
	// the resulting Card.Speech field.
	WhatsApp *FixtureWhatsApp `json:"whatsapp,omitempty"`
}

// FixtureStockSnapshot mirrors the runtime stock.snapshot payload.
// Empty Currency defaults to "USD". MinutesAgo of 0 anchors at
// fixture today + synth time.
type FixtureStockSnapshot struct {
	Ticker      string  `json:"ticker"`
	Price       float64 `json:"price"`
	PrevClose   float64 `json:"prev_close"`
	Open        float64 `json:"open,omitempty"`
	DayHigh     float64 `json:"day_high,omitempty"`
	DayLow      float64 `json:"day_low,omitempty"`
	Volume      int64   `json:"volume,omitempty"`
	PostPrice   float64 `json:"post_price,omitempty"`
	PostChange  float64 `json:"post_change_pct,omitempty"`
	MarketState string  `json:"market_state,omitempty"`
	Currency    string  `json:"currency,omitempty"`
	ChangePct   float64 `json:"change_pct,omitempty"` // computed from Price+PrevClose when 0
	MinutesAgo  int     `json:"minutes_ago,omitempty"`
}

// FixtureStockAlert mirrors the runtime stock.alert payload. The
// EvidenceID, when non-empty, becomes the event-log row ID — the
// inject signal's EvidenceID must match this for read_stock_alert
// resolution.
type FixtureStockAlert struct {
	EvidenceID   string  `json:"evidence_id,omitempty"`
	Ticker       string  `json:"ticker"`
	Price        float64 `json:"price"`
	PrevClose    float64 `json:"prev_close"`
	ChangePct    float64 `json:"change_pct"`
	ThresholdPct float64 `json:"threshold_pct"`
	Currency     string  `json:"currency,omitempty"`
	MinutesAgo   int     `json:"minutes_ago,omitempty"`
}

// FixtureTask is one seeded task. Mirrors the runtime task.snapshot payload
// shape; missing fields default sensibly (priority=med, completed=false).
type FixtureTask struct {
	UID       string   `json:"uid,omitempty"`
	Title     string   `json:"title"`
	Completed bool     `json:"completed,omitempty"`
	DueDate   string   `json:"due_date,omitempty"`
	DoneDate  string   `json:"done_date,omitempty"`
	Priority  string   `json:"priority,omitempty"`
	Tags      []string `json:"tags,omitempty"`
}

// FixtureConcernQueryTest is one query × expected-concern × expected-evidence
// triple. Used by the reactive fixture runner to score the concern-scoped
// query path end-to-end.
type FixtureConcernQueryTest struct {
	Query               string   `json:"query"`
	ExpectedConcernID   string   `json:"expected_concern_id,omitempty"`
	ExpectedEvidenceIDs []string `json:"expected_evidence_ids,omitempty"`
	MustNotMatchConcern bool     `json:"must_not_match_concern,omitempty"`
}

// FixtureExpectedConcern is one ground-truth concern the recognition pass is
// expected to surface. NameContains is a slice of substrings any-of which the
// proposal's name should contain (case-insensitive); the eval scorer treats
// this as loose match. ObservationIDs is the set of seeded event UIDs the
// proposal should pre-tag (covered by the precision/recall rubric).
type FixtureExpectedConcern struct {
	Name           string   `json:"name"`
	NameContains   []string `json:"name_contains,omitempty"`
	ObservationIDs []string `json:"observation_ids,omitempty"`
}

// FixtureSeedConcern pre-creates a concern row before the retrospective
// fixture runs. ID is the stable handle the test uses to correlate
// expected_concern_tags entries.
type FixtureSeedConcern struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	State       string `json:"state,omitempty"`  // default "active"
	Source      string `json:"source,omitempty"` // default "user"
}

// FixtureMemoryFact is one seeded MemoryFact row. Mirrors the JSON shape of
// eval/seed/memory.json so a fixture can either inline its memory state or
// reference a shared seed externally. Provenance fields default to
// now / now / 1 at seed time so authors don't need to think about them.
type FixtureMemoryFact struct {
	Subject       string `json:"subject"`
	Fact          string `json:"fact"`
	Category      string `json:"category,omitempty"`
	Confidence    string `json:"confidence,omitempty"`
	Source        string `json:"source,omitempty"`
	EvidenceCount int    `json:"evidence_count,omitempty"`
}

// FixtureInjectSignal is the V2.3 test-only synthetic inject signal a
// fixture can declare. The eval harness constructs a synth.InjectSignal
// from this block and sets Runner.InjectSignal so the detector picks
// StateMessageInject. Phase 3 owns the real signal pipeline; this block
// exists so the inject fixture can detect deterministically before P3.
//
// EvidenceID is V2.7 — populated for stock_breach signals so the
// read_stock_alert tool can resolve the underlying alert payload.
type FixtureInjectSignal struct {
	Kind       string `json:"kind"`              // "email" | "calendar_move" | "stock_breach"
	Subject    string `json:"subject,omitempty"` // human-readable subject
	EvidenceID string `json:"evidence_id,omitempty"`
}

// ReactiveExpect is the deterministic expectation for the single Card a
// reactive fixture asserts against. All non-empty fields AND together; each
// substring list is OR (any one needle hits).
type ReactiveExpect struct {
	TitleContains     []string `json:"title_contains,omitempty"`
	SubContains       []string `json:"sub_contains,omitempty"`
	SrcIn             []string `json:"src_in,omitempty"` // permitted Source values; empty = unconstrained
	RelIn             []string `json:"rel_in,omitempty"` // permitted Rel values; empty = unconstrained
	MustNotBeDegraded bool     `json:"must_not_be_degraded,omitempty"`

	// V2.7: WhatsApp-register expectations. Non-empty SpeechContains
	// requires AT LEAST ONE substring to appear in Card.Speech.
	// Non-empty SpeechMustNotContain treats EVERY substring as a hard
	// veto — an instance of any one fails the criterion. Together they
	// catch the most likely regression: the model leaks markdown
	// headers, multi-paragraph prose, or "## Synthesis"-style
	// boilerplate into the WhatsApp wire instead of writing a tight
	// chat reply.
	SpeechContains       []string `json:"speech_contains,omitempty"`
	SpeechMustNotContain []string `json:"speech_must_not_contain,omitempty"`
}

// FixtureWhatsApp activates the WhatsApp register for a reactive
// fixture. When non-nil the eval harness threads
// synth.ConversationContext through ReactiveDeps so reactive.tmpl
// renders the WhatsApp instructions block; the fixture's
// reactive_expect.speech_* fields then assert against Card.Speech.
type FixtureWhatsApp struct {
	SenderName string `json:"sender_name,omitempty"`
	GroupName  string `json:"group_name,omitempty"`
	IsDM       bool   `json:"is_dm,omitempty"`
	IsMention  bool   `json:"is_mention,omitempty"`
}

// FixtureUser carries the synthesizer's identity context.
type FixtureUser struct {
	Name string `json:"name"`
	TZ   string `json:"tz"` // IANA name, e.g. "America/Los_Angeles"
}

// FixtureEvent is one calendar event.
//
// LastModified (V2.3.0 P3) lets inject fixtures declare a recent calendar
// move — the inject detector's calendar-move path keys off it. Empty/zero
// in regular morning fixtures means "not recently moved".
type FixtureEvent struct {
	UID          string    `json:"uid"`
	Start        time.Time `json:"start"`
	End          time.Time `json:"end"`
	Title        string    `json:"title"`
	Tag          string    `json:"tag,omitempty"`
	Location     string    `json:"location,omitempty"`
	Attendees    []string  `json:"attendees,omitempty"`
	LastModified time.Time `json:"last_modified,omitzero"`
}

// FixtureThread is one email thread, summarized.
type FixtureThread struct {
	Subject      string    `json:"subject"`
	LastSender   string    `json:"last_sender"`
	LastReceived time.Time `json:"last_received"`
	MessageCount int       `json:"message_count"`
	UnreadCount  int       `json:"unread_count"`
	Preview      string    `json:"preview"`
}

// FixtureWeather mirrors the bench fixture's weather block but is
// expanded into a single weather.snapshot event by the loader.
type FixtureWeather struct {
	Summary  string             `json:"summary"`
	TempNow  int                `json:"temp_now"`
	TempHigh int                `json:"temp_high"`
	Hours    []FixtureWeatherHr `json:"hours"`
}

// FixtureWeatherHr is one hourly forecast row.
type FixtureWeatherHr struct {
	H    string `json:"h"`    // hour-of-day, "9", "11", etc.
	T    int    `json:"t"`    // temperature
	Icon string `json:"icon"` // sun | cloud | rain
}

// FixtureRunWindow optionally seeds a pre-computed run window. The
// projection layer can also derive one from the weather snapshot; this
// block is a hint for the harness's must-card matching.
type FixtureRunWindow struct {
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	Condition string    `json:"condition"`
}

// MustCard is one deterministic expectation: a card with one of the
// permitted Sources must appear in the run output, and one of its
// title/sub fields must contain at least one TitleContains substring
// (case-insensitive). Voice variation is allowed within those constraints.
//
// Sources is a slice (CardSources); fixtures may write either `"src": "mail"`
// or `"src": ["mail","tasks"]` and both round-trip through the custom
// UnmarshalJSON. Multi-source is appropriate when the model can defensibly
// emit a card under more than one routing (e.g. an email thread asking for
// an owed deliverable maps cleanly to either `mail` or `tasks`).
type MustCard struct {
	Name          string      `json:"name"`           // human label, used in report
	Sources       CardSources `json:"src"`            // one or more of mail | calendar | personal | tasks
	TitleContains []string    `json:"title_contains"` // any one substring is enough
	SubContains   []string    `json:"sub_contains"`   // any one substring is enough
}

// PerStateMustCards returns the fixture's MustCards declared under the
// per-state list for the given state, without the flat MustCards. Returns
// nil when the fixture declares no rules for that state. Used by the
// golden-corpus sidecar so a frozen directory exposes the per-state rule
// definitions alongside the merged scores.json results.
func (f *Fixture) PerStateMustCards(state synth.State) []MustCard {
	switch state {
	case synth.StateMorningCalm:
		return f.MustCardsMorningCalm
	case synth.StatePreMeeting:
		return f.MustCardsPreMeeting
	case synth.StateDeepWork:
		return f.MustCardsDeepWork
	case synth.StateEndOfDay:
		return f.MustCardsEndOfDay
	case synth.StateMessageInject:
		return f.MustCardsInject
	}
	return nil
}

// MustCardsForState returns the union of the fixture's flat MustCards plus
// any state-conditional MustCards matching the given state. The flat
// MustCards always run; per-state lists run only when the fixture's
// expected_state matches. Per-state cards get their Name suffixed with
// `[<state>]` so the report can show which list a result came from.
//
// Per spec: rules are additive — both lists must match.
func (f *Fixture) MustCardsForState(state synth.State) []MustCard {
	out := make([]MustCard, 0, len(f.MustCards))
	out = append(out, f.MustCards...)
	for _, mc := range f.PerStateMustCards(state) {
		tagged := mc
		tagged.Name = mc.Name + " [" + string(state) + "]"
		out = append(out, tagged)
	}
	return out
}

// LoadFixture reads and parses a fixture JSON file.
func LoadFixture(path string) (*Fixture, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read fixture %s: %w", path, err)
	}
	var f Fixture
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse fixture %s: %w", path, err)
	}
	if f.Today == "" {
		return nil, fmt.Errorf("fixture %s: today is required", path)
	}
	if f.User.TZ == "" {
		f.User.TZ = "UTC"
	}
	return &f, nil
}

// TodayTime parses Today as a midnight time.Time in User.TZ, plus
// SynthTime hours/minutes if set (defaulting to 06:30 — the canonical
// "morning briefing fires before the user wakes" target). State fixtures
// override SynthTime to a working-hours time so the V2.3 detector lands
// in their target register (e.g. "10:00" for deep_work, "14:00" for the
// Friday end_of_day shift).
func (f *Fixture) TodayTime() (time.Time, error) {
	loc, err := time.LoadLocation(f.User.TZ)
	if err != nil {
		return time.Time{}, fmt.Errorf("load tz %q: %w", f.User.TZ, err)
	}
	t, err := time.ParseInLocation("2006-01-02", f.Today, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse today %q: %w", f.Today, err)
	}
	hh, mm := 6, 30
	if f.SynthTime != "" {
		parsed, err := time.Parse("15:04", f.SynthTime)
		if err != nil {
			return time.Time{}, fmt.Errorf("parse synth_time %q: %w", f.SynthTime, err)
		}
		hh, mm = parsed.Hour(), parsed.Minute()
	}
	return t.Add(time.Duration(hh)*time.Hour + time.Duration(mm)*time.Minute), nil
}
