package sensor

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/projection"
	"github.com/zenocy/zeno-v2/internal/store"
	"github.com/zenocy/zeno-v2/internal/synth"
)

// Default detector horizons + thresholds. The brief pinned each one;
// they're exposed as exported constants so the soak test and any future
// config plumbing can reference them by name.
const (
	DefaultDebounceWindow              = 30 * time.Minute
	DefaultEmailHorizon                = 30 * time.Minute
	DefaultCalendarMoveHorizon         = 4 * time.Hour
	DefaultCalendarMoveModifiedHorizon = 30 * time.Minute
	DefaultCalendarMoveMinAttendees    = 2
	// DefaultStockBreachHorizon is the look-back window the detector
	// applies on top of any pre-filtering the projection did. A breach
	// older than this is past awareness — don't wake the reactive synth.
	DefaultStockBreachHorizon = 15 * time.Minute
	// DefaultCalendarSoonHorizon is how close an event's start must be to
	// fire the calendar_soon (update-in-place countdown refresh) path.
	DefaultCalendarSoonHorizon = 30 * time.Minute
)

// InjectDetectorDeps is the typed surface Detect reads. Built fresh per
// cron tick by the orchestrator (cmd/zeno/main.go) — pure deps, no I/O
// inside Detect itself, so the function is trivially testable.
type InjectDetectorDeps struct {
	Threads       []projection.Thread        // open email threads in the projection
	Calendar      []projection.CalendarEvent // today's calendar events
	Cards         []store.Card               // today's already-surfaced cards (the VIP-name source)
	Concerns      []projection.Concern       // V2.5.0: active concerns; powers the concern-boost path
	StockBreaches []projection.StockBreach   // recent stock.alert events; powers the stock-breach path
	LastFire      time.Time                  // most recent inject fire (zero = never)
	Now           func() time.Time
	Logger        *logrus.Entry
}

// InjectDetectorConfig is the tunable surface — horizons, attendee floors,
// the deny-subject regex set. DefaultInjectConfig returns the conservative
// defaults pinned in the V2.3.0 plan.
type InjectDetectorConfig struct {
	DebounceWindow              time.Duration
	EmailHorizon                time.Duration
	CalendarMoveHorizon         time.Duration
	CalendarMoveModifiedHorizon time.Duration
	CalendarMoveMinAttendees    int
	DenySubjectPatterns         []*regexp.Regexp
	// V2.5.0: minimum concern confidence for a thread admitted via the
	// concern-boost path (subject substring matches an active concern's
	// name). 0 → DefaultConcernConfidenceFloor (0.7).
	ConcernConfidenceFloor float64
	// StockBreachHorizon caps how recent a stock.alert event must be to
	// fire the stock-breach path. 0 → DefaultStockBreachHorizon.
	StockBreachHorizon time.Duration
	// CalendarSoonHorizon is the "starts within" window for the
	// calendar_soon path. 0 → DefaultCalendarSoonHorizon.
	CalendarSoonHorizon time.Duration
}

// DefaultConcernConfidenceFloor is the V2.5.0 floor below which the
// concern-boost path will not admit a thread. Conservative — only
// high-confidence concerns earn the additive admission.
const DefaultConcernConfidenceFloor = 0.7

// DefaultInjectConfig returns the V2.3.0 defaults: 30-min debounce, 30-min
// email recency, 4h calendar-move horizon, 30-min "recently moved"
// threshold, 2-attendee minimum on calendar moves, plus a small set of
// newsletter / auto-reply / unsubscribe regex denylist patterns. V2.5.0
// adds the concern-confidence floor (0.7).
func DefaultInjectConfig() InjectDetectorConfig {
	return InjectDetectorConfig{
		DebounceWindow:              DefaultDebounceWindow,
		EmailHorizon:                DefaultEmailHorizon,
		CalendarMoveHorizon:         DefaultCalendarMoveHorizon,
		CalendarMoveModifiedHorizon: DefaultCalendarMoveModifiedHorizon,
		CalendarMoveMinAttendees:    DefaultCalendarMoveMinAttendees,
		DenySubjectPatterns: []*regexp.Regexp{
			regexp.MustCompile(`(?i)^(re:\s*)?(newsletter|update|digest|weekly|daily)\b`),
			regexp.MustCompile(`(?i)\bunsubscribe\b`),
			regexp.MustCompile(`(?i)^auto[\- ]reply\b`),
		},
		ConcernConfidenceFloor: DefaultConcernConfidenceFloor,
		StockBreachHorizon:     DefaultStockBreachHorizon,
		CalendarSoonHorizon:    DefaultCalendarSoonHorizon,
	}
}

// Detect inspects the deps and returns at most one InjectSignal.
//
// Conservative deny-by-default. Four paths run in priority order, with
// a debounce gate ahead of all four:
//
//  1. Debounce — if LastFire is within DebounceWindow, return nil.
//  2. Calendar-move — any event in [now, now+CalendarMoveHorizon] with
//     >= CalendarMoveMinAttendees attendees AND LastModified within
//     CalendarMoveModifiedHorizon. Soonest start wins.
//  3. Stock-breach — any stock.alert payload from RecentStockBreaches
//     within StockBreachHorizon. Most-recent breach wins.
//  4. Email (VIP) — any unread thread with LastReceived within
//     EmailHorizon whose sender's name appears in today's Cards (Title
//     or Sub) and whose Subject doesn't match any DenySubjectPattern.
//     Most-recent thread wins.
//  5. Email (concern boost, V2.5.0) — any unread thread within
//     EmailHorizon whose Subject substring-matches an active concern
//     with Confidence >= ConcernConfidenceFloor. Same deny patterns
//     apply. Additive: only fires when the VIP path returned nil.
//
// Tie-break rationale: calendar moves are the most time-sensitive (you
// physically can't be in two places). Stock breaches sit second —
// market hours are short and the signal is unambiguous (a watched
// ticker crossed an absolute threshold the user pre-declared). VIP
// email beats concern-boost — a known sender on a known thread is a
// stronger signal than a name match on a long-running situation.
func Detect(deps InjectDetectorDeps, cfg InjectDetectorConfig) *synth.InjectSignal {
	now := deps.Now()

	if !deps.LastFire.IsZero() && now.Sub(deps.LastFire) < cfg.DebounceWindow {
		if deps.Logger != nil {
			deps.Logger.WithField("since_last_fire", now.Sub(deps.LastFire)).
				Debug("inject detector: debounced")
		}
		return nil
	}

	// V2.x: the set of entity keys already carried by today's cards. A
	// signal whose entity is in this set is an UPDATE (refresh the card in
	// place); otherwise it's an APPEND (a new inject card). Computed once.
	present := buildEntityKeySet(deps.Cards)

	// Priority order: most time-sensitive first. Calendar moves and
	// imminent starts trump markets; markets trump email. thread_reply
	// (an already-carded thread got a reply) runs ahead of the VIP/concern
	// email paths because "the user already cares — there's a card" is a
	// stronger, broader signal than a name match.
	if sig := detectCalendarMove(deps, cfg, now, present); sig != nil {
		return sig
	}
	if sig := detectCalendarSoon(deps, cfg, now, present); sig != nil {
		return sig
	}
	if sig := detectStockBreach(deps, cfg, now, present); sig != nil {
		return sig
	}
	if sig := detectThreadReply(deps, cfg, now, present); sig != nil {
		return sig
	}
	if sig := detectEmail(deps, cfg, now, present); sig != nil {
		return sig
	}
	return detectConcernEmail(deps, cfg, now, present)
}

// buildEntityKeySet collects the non-empty entity keys carried by today's
// cards — the basis for the update-vs-append decision.
func buildEntityKeySet(cards []store.Card) map[string]bool {
	out := make(map[string]bool, len(cards))
	for _, c := range cards {
		if c.EntityKey != "" {
			out[c.EntityKey] = true
		}
	}
	return out
}

// modeFor returns "update" when the entity already has a card today,
// "append" otherwise. Empty entityKey is always append.
func modeFor(entityKey string, present map[string]bool) string {
	if entityKey != "" && present[entityKey] {
		return synth.InjectModeUpdate
	}
	return synth.InjectModeAppend
}

// detectCalendarMove walks the calendar for upcoming events that were
// recently changed (a board call moved up, etc).
func detectCalendarMove(deps InjectDetectorDeps, cfg InjectDetectorConfig, now time.Time, present map[string]bool) *synth.InjectSignal {
	horizon := now.Add(cfg.CalendarMoveHorizon)
	modCutoff := now.Add(-cfg.CalendarMoveModifiedHorizon)

	var pick *projection.CalendarEvent
	for i := range deps.Calendar {
		ev := deps.Calendar[i]
		if !ev.Start.After(now) || ev.Start.After(horizon) {
			continue
		}
		if len(ev.Attendees) < cfg.CalendarMoveMinAttendees {
			continue
		}
		if ev.LastModified.IsZero() || ev.LastModified.Before(modCutoff) {
			continue
		}
		// Soonest-start wins — most time-sensitive.
		if pick == nil || ev.Start.Before(pick.Start) {
			ev := ev
			pick = &ev
		}
	}
	if pick == nil {
		return nil
	}
	ek := synth.EventEntityKey(pick.UID)
	return &synth.InjectSignal{
		Kind:       "calendar_move",
		Subject:    pick.Title,
		EvidenceID: pick.UID,
		At:         now,
		EntityKey:  ek,
		Mode:       modeFor(ek, present),
	}
}

// detectCalendarSoon fires when an attendee event is about to start
// (within CalendarSoonHorizon) AND already has a card today — an
// update-only path that refreshes the card's countdown/imminence. It never
// appends: a soon-starting event the user has never seen a card for is left
// to the morning grid, keeping this conservative. Soonest start wins.
func detectCalendarSoon(deps InjectDetectorDeps, cfg InjectDetectorConfig, now time.Time, present map[string]bool) *synth.InjectSignal {
	if len(present) == 0 {
		return nil
	}
	horizonDur := cfg.CalendarSoonHorizon
	if horizonDur <= 0 {
		horizonDur = DefaultCalendarSoonHorizon
	}
	horizon := now.Add(horizonDur)

	var pick *projection.CalendarEvent
	var pickKey string
	for i := range deps.Calendar {
		ev := deps.Calendar[i]
		if !ev.Start.After(now) || ev.Start.After(horizon) {
			continue
		}
		if len(ev.Attendees) < cfg.CalendarMoveMinAttendees {
			continue
		}
		ek := synth.EventEntityKey(ev.UID)
		if !present[ek] {
			continue
		}
		if pick == nil || ev.Start.Before(pick.Start) {
			ev := ev
			pick = &ev
			pickKey = ek
		}
	}
	if pick == nil {
		return nil
	}
	return &synth.InjectSignal{
		Kind:       "calendar_soon",
		Subject:    pick.Title,
		EvidenceID: pick.UID,
		At:         now,
		EntityKey:  pickKey,
		Mode:       synth.InjectModeUpdate,
	}
}

// detectThreadReply fires when an open thread that ALREADY has a card today
// receives a fresh unread message — regardless of whether the sender is a
// VIP. The existing card is the signal that the user cares; a reply on it
// is high-value and should refresh the card in place rather than spawn a
// duplicate. Deny patterns + the email recency horizon still apply.
func detectThreadReply(deps InjectDetectorDeps, cfg InjectDetectorConfig, now time.Time, present map[string]bool) *synth.InjectSignal {
	if len(deps.Threads) == 0 || len(present) == 0 {
		return nil
	}
	emailCutoff := now.Add(-cfg.EmailHorizon)

	var pick *projection.Thread
	var pickKey string
	for i := range deps.Threads {
		t := deps.Threads[i]
		if t.UnreadCount < 1 {
			continue
		}
		if t.LastReceived.Before(emailCutoff) {
			continue
		}
		if matchesAny(cfg.DenySubjectPatterns, t.Subject) {
			continue
		}
		ek := synth.ThreadEntityKey(t.Subject)
		if !present[ek] {
			continue
		}
		if pick == nil || t.LastReceived.After(pick.LastReceived) {
			t := t
			pick = &t
			pickKey = ek
		}
	}
	if pick == nil {
		return nil
	}
	return &synth.InjectSignal{
		Kind:       "thread_reply",
		Subject:    pick.Subject,
		EvidenceID: pick.Subject,
		At:         now,
		EntityKey:  pickKey,
		Mode:       synth.InjectModeUpdate,
	}
}

// detectStockBreach scans the recent stock.alert events and returns
// the most-recent breach inside StockBreachHorizon. Tickers, prices
// and thresholds are pre-validated by the sensor — this path only
// applies the recency cap and picks the freshest signal.
func detectStockBreach(deps InjectDetectorDeps, cfg InjectDetectorConfig, now time.Time, present map[string]bool) *synth.InjectSignal {
	if len(deps.StockBreaches) == 0 {
		return nil
	}
	horizon := cfg.StockBreachHorizon
	if horizon <= 0 {
		horizon = DefaultStockBreachHorizon
	}
	cutoff := now.Add(-horizon)

	var pick *projection.StockBreach
	for i := range deps.StockBreaches {
		b := deps.StockBreaches[i]
		if b.AsOf.Before(cutoff) {
			continue
		}
		// Most-recent breach wins — newer prices outvote older ones.
		if pick == nil || b.AsOf.After(pick.AsOf) {
			b := b
			pick = &b
		}
	}
	if pick == nil {
		return nil
	}
	subject := stockBreachSubject(*pick)
	ek := synth.TickerEntityKey(pick.Ticker)
	return &synth.InjectSignal{
		Kind:       "stock_breach",
		Subject:    subject,
		EvidenceID: pick.EvidenceID,
		At:         now,
		EntityKey:  ek,
		Mode:       modeFor(ek, present),
	}
}

// stockBreachSubject formats a one-line human subject for the inject
// signal: "AAPL +5.2%" or "TSLA −3.7%". Used by the synth prompt and
// the degraded-card fallback alike.
func stockBreachSubject(b projection.StockBreach) string {
	sign := "+"
	if b.ChangePct < 0 {
		sign = "−"
	}
	return fmt.Sprintf("%s %s%.2f%%", b.Ticker, sign, math.Abs(b.ChangePct))
}

// detectEmail walks open threads for an unread message from someone the
// morning grid already cares about.
func detectEmail(deps InjectDetectorDeps, cfg InjectDetectorConfig, now time.Time, present map[string]bool) *synth.InjectSignal {
	if len(deps.Threads) == 0 {
		return nil
	}
	names := buildVIPNameSet(deps.Cards)

	emailCutoff := now.Add(-cfg.EmailHorizon)

	var pick *projection.Thread
	for i := range deps.Threads {
		t := deps.Threads[i]
		if t.UnreadCount < 1 {
			continue
		}
		if t.LastReceived.Before(emailCutoff) {
			continue
		}
		if matchesAny(cfg.DenySubjectPatterns, t.Subject) {
			continue
		}
		if !senderMatchesVIP(t.LastSender, names) {
			continue
		}
		// Most-recent wins — newer messages are higher signal.
		if pick == nil || t.LastReceived.After(pick.LastReceived) {
			t := t
			pick = &t
		}
	}
	if pick == nil {
		return nil
	}
	ek := synth.ThreadEntityKey(pick.Subject)
	return &synth.InjectSignal{
		Kind:       "email",
		Subject:    pick.Subject,
		EvidenceID: pick.Subject, // V2.3 has no thread IDs; subject is the stable handle
		At:         now,
		EntityKey:  ek,
		Mode:       modeFor(ek, present),
	}
}

// detectConcernEmail walks open threads for an unread message whose
// subject substring-matches an active high-confidence concern. The
// match is forward-looking — a brand-new on-topic email fires before
// recognition's post-tag pass catches up. Subject substring is the
// same primitive `lookup_concern` uses, so the detector stays
// decoupled from the tag store. Confidence floor (default 0.7) keeps
// the path conservative.
func detectConcernEmail(deps InjectDetectorDeps, cfg InjectDetectorConfig, now time.Time, present map[string]bool) *synth.InjectSignal {
	if len(deps.Threads) == 0 || len(deps.Concerns) == 0 {
		return nil
	}
	floor := cfg.ConcernConfidenceFloor
	if floor <= 0 {
		floor = DefaultConcernConfidenceFloor
	}
	emailCutoff := now.Add(-cfg.EmailHorizon)

	// Build the candidate concern-name set once: lowercased, trimmed,
	// confidence-gated, non-empty. Bail if no concern qualifies.
	type namedConcern struct {
		nameLower string
		original  string
	}
	candidates := make([]namedConcern, 0, len(deps.Concerns))
	for _, c := range deps.Concerns {
		if c.Confidence < floor {
			continue
		}
		n := strings.ToLower(strings.TrimSpace(c.Name))
		if n == "" {
			continue
		}
		candidates = append(candidates, namedConcern{nameLower: n, original: c.Name})
	}
	if len(candidates) == 0 {
		return nil
	}

	var pick *projection.Thread
	for i := range deps.Threads {
		t := deps.Threads[i]
		if t.UnreadCount < 1 {
			continue
		}
		if t.LastReceived.Before(emailCutoff) {
			continue
		}
		if matchesAny(cfg.DenySubjectPatterns, t.Subject) {
			continue
		}
		subjLower := strings.ToLower(t.Subject)
		matched := false
		for _, cand := range candidates {
			if strings.Contains(subjLower, cand.nameLower) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		// Most-recent wins — newer messages are higher signal.
		if pick == nil || t.LastReceived.After(pick.LastReceived) {
			t := t
			pick = &t
		}
	}
	if pick == nil {
		return nil
	}
	ek := synth.ThreadEntityKey(pick.Subject)
	return &synth.InjectSignal{
		Kind:       "email",
		Subject:    pick.Subject,
		EvidenceID: pick.Subject,
		At:         now,
		EntityKey:  ek,
		Mode:       modeFor(ek, present),
	}
}

// buildVIPNameSet extracts a lowercased set of names from the morning's
// already-surfaced cards. Only tokens of length >= 3 count; that filters
// articles/conjunctions and keeps the set focused on proper nouns. Called
// once per Detect invocation.
func buildVIPNameSet(cards []store.Card) map[string]bool {
	names := make(map[string]bool, len(cards)*4)
	add := func(s string) {
		for _, tok := range tokenize(s) {
			if len(tok) >= 3 {
				names[tok] = true
			}
		}
	}
	for _, c := range cards {
		add(c.Title)
		add(c.Sub)
	}
	return names
}

// senderMatchesVIP returns true if any of the sender's tokens appears in
// the VIP set. "Saru Patel" matches if either "saru" or "patel" is in
// the set; "patel@example.com" matches if "patel" is in the set.
func senderMatchesVIP(sender string, names map[string]bool) bool {
	for _, tok := range tokenize(sender) {
		if len(tok) < 3 {
			continue
		}
		if names[tok] {
			return true
		}
	}
	return false
}

// tokenize lowercases s and splits on any non-letter/digit, dropping
// empty tokens. Used by both name-set construction and sender matching
// so the comparison is over normalized tokens.
func tokenize(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.ToLower(s)
	out := strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	return out
}

func matchesAny(patterns []*regexp.Regexp, s string) bool {
	for _, p := range patterns {
		if p.MatchString(s) {
			return true
		}
	}
	return false
}
