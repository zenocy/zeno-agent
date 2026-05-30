package api

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/zenocy/zeno-v2/internal/projection"
	"github.com/zenocy/zeno-v2/internal/store"
)

// V2.x serve-time live binding.
//
// Cards declare volatile values (weather, prices, a meeting countdown) as
// `live` entries instead of baking the number into prose at synth time. The
// model leaves a sentinel token (⟦weather⟧ / ⟦$AAPL⟧ / ⟦countdown⟧) where the
// value belongs; this layer resolves the latest projection on every GET and
// substitutes it. No LLM call — a card minted at 07:00 still shows current
// numbers. The strict post-parse schema validates the `live` shape upstream;
// here we only read it.

// liveFieldRow mirrors synth.LiveField on the read side. Kept local so the
// api package doesn't import synth.
type liveFieldRow struct {
	Slot string `json:"slot"`
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

// liveChipDTO is the resolved live value exposed to the UI alongside the
// in-place substitution, so the client can render a freshness affordance
// (and grey out stale values).
type liveChipDTO struct {
	Slot  string `json:"slot"`
	Kind  string `json:"kind"`
	Text  string `json:"text"`
	Stale bool   `json:"stale,omitempty"`
	AsOf  string `json:"as_of,omitempty"`
}

// liveViews bundles the projections that back live fields, computed once per
// request. Any may be nil (no data / stale / not configured).
type liveViews struct {
	weather *projection.WeatherView
	stock   *projection.StockView
	cal     []projection.CalendarEvent
	now     time.Time
	active  bool // true when at least one card declared a live field
}

var sentinelRE = regexp.MustCompile(`⟦[^⟧]*⟧`)
var multiSpaceRE = regexp.MustCompile(`[ \t]{2,}`)

// buildLiveViews computes the projections needed to resolve the live fields
// across the given rows. Returns an inactive zero value (no projection
// reads) when no row declares a live field — the common case, so the cards
// endpoint pays nothing extra. Each projection is best-effort: a failure
// leaves that view nil and its sentinels resolve to empty.
func buildLiveViews(ctx context.Context, h *CardsHandler, rows []store.Card, now time.Time) liveViews {
	lv := liveViews{now: now}
	for i := range rows {
		if len(rows[i].Live) > 0 {
			lv.active = true
			break
		}
	}
	if !lv.active || h.Reader == nil {
		return liveViews{now: now}
	}
	if w, err := (projection.Weather{Cfg: h.ProjCfg}).Compute(ctx, h.Reader); err == nil {
		lv.weather = w
	}
	if s, err := (projection.Stock{Cfg: h.ProjCfg, Tickers: h.Tickers}).Compute(ctx, h.Reader); err == nil {
		lv.stock = s
	}
	if cal, err := (projection.TodaysCalendar{Cfg: h.ProjCfg}).Compute(ctx, h.Reader); err == nil {
		lv.cal = cal
	}
	return lv
}

// sentinelFor returns the deterministic sentinel token the model is told to
// emit for a given live field, or "" for an unknown kind.
func sentinelFor(kind, ref string) string {
	switch kind {
	case "weather":
		return "⟦weather⟧"
	case "countdown":
		return "⟦countdown⟧"
	case "stock":
		return "⟦$" + strings.ToUpper(strings.TrimSpace(ref)) + "⟧"
	}
	return ""
}

// resolve returns the current display text for a live field plus whether it
// is stale and its as-of time. ok=false means the value couldn't be resolved
// (no data / unknown ref) and the sentinel should be stripped.
func (lv liveViews) resolve(kind, ref string) (text string, stale bool, asOf time.Time, ok bool) {
	switch kind {
	case "weather":
		// Weather.Compute returns nil past the staleness window.
		if lv.weather == nil {
			return "", false, time.Time{}, false
		}
		cur := lv.weather.Current
		label := strings.TrimSpace(cur.Label)
		if label != "" {
			text = fmt.Sprintf("%.0f° %s", cur.TempC, label)
		} else {
			text = fmt.Sprintf("%.0f°", cur.TempC)
		}
		return text, false, lv.weather.CapturedAt, true
	case "stock":
		if lv.stock == nil {
			return "", false, time.Time{}, false
		}
		want := strings.ToUpper(strings.TrimSpace(ref))
		for _, q := range lv.stock.Quotes {
			if strings.ToUpper(q.Ticker) == want {
				return fmt.Sprintf("$%.2f %+.2f%%", q.Price, q.ChangePct), q.Stale, q.AsOf, true
			}
		}
		return "", false, time.Time{}, false
	case "countdown":
		for _, ev := range lv.cal {
			if ev.UID == ref {
				return humanizeUntil(ev.Start.Sub(lv.now)), false, time.Time{}, true
			}
		}
		return "", false, time.Time{}, false
	}
	return "", false, time.Time{}, false
}

// humanizeUntil formats a positive duration as "in 2h13m" / "in 9m"; a
// non-positive duration (event already started) reads "now".
func humanizeUntil(d time.Duration) string {
	if d <= 0 {
		return "now"
	}
	d = d.Round(time.Minute)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("in %dh%02dm", h, m)
	}
	return fmt.Sprintf("in %dm", m)
}

// applyLiveFields resolves a card's live entries, substitutes the sentinels
// in its meta chips and sub in place, strips any leftover (unmatched)
// sentinels, and returns the resolved chips for the DTO. meta is returned
// with empty / separator-only chips dropped.
func applyLiveFields(raw []byte, meta []string, sub string, lv liveViews) ([]string, string, []liveChipDTO) {
	if len(raw) == 0 {
		return meta, sub, nil
	}
	var fields []liveFieldRow
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) == 0 {
		return stripSentinelsMeta(meta), stripSentinels(sub), nil
	}

	repl := make(map[string]string, len(fields))
	var chips []liveChipDTO
	for _, f := range fields {
		sent := sentinelFor(f.Kind, f.Ref)
		if sent == "" {
			continue
		}
		text, stale, asOf, ok := lv.resolve(f.Kind, f.Ref)
		if !ok {
			repl[sent] = "" // strip the sentinel; value unavailable
			continue
		}
		repl[sent] = text
		chip := liveChipDTO{Slot: f.Slot, Kind: f.Kind, Text: text, Stale: stale}
		if !asOf.IsZero() {
			chip.AsOf = asOf.UTC().Format(time.RFC3339)
		}
		chips = append(chips, chip)
	}

	outMeta := make([]string, 0, len(meta))
	for _, chip := range meta {
		outMeta = append(outMeta, substitute(chip, repl))
	}
	outMeta = tidyMeta(outMeta)
	return outMeta, substitute(sub, repl), chips
}

// substitute replaces every known sentinel with its resolved text, strips
// any leftover sentinel tokens, and collapses the whitespace left behind.
func substitute(s string, repl map[string]string) string {
	for sent, text := range repl {
		s = strings.ReplaceAll(s, sent, text)
	}
	return stripSentinels(s)
}

func stripSentinels(s string) string {
	s = sentinelRE.ReplaceAllString(s, "")
	s = multiSpaceRE.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func stripSentinelsMeta(meta []string) []string {
	out := make([]string, 0, len(meta))
	for _, c := range meta {
		out = append(out, stripSentinels(c))
	}
	return tidyMeta(out)
}

// tidyMeta drops empty chips and trims leading/trailing separator-only chips
// left behind when a sentinel chip resolved to empty.
func tidyMeta(meta []string) []string {
	cleaned := make([]string, 0, len(meta))
	for _, c := range meta {
		if strings.TrimSpace(c) != "" {
			cleaned = append(cleaned, c)
		}
	}
	isSep := func(s string) bool {
		switch strings.TrimSpace(s) {
		case "·", "•", "—", "-", "|":
			return true
		}
		return false
	}
	for len(cleaned) > 0 && isSep(cleaned[0]) {
		cleaned = cleaned[1:]
	}
	for len(cleaned) > 0 && isSep(cleaned[len(cleaned)-1]) {
		cleaned = cleaned[:len(cleaned)-1]
	}
	return cleaned
}
