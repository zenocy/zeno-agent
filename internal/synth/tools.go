package synth

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/zenocy/zeno-v2/internal/llm"
	"github.com/zenocy/zeno-v2/internal/log"
)

// argString picks a string argument by key from the LLM's tool args, or
// returns "" if missing/wrong type.
func argString(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	v, ok := args[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// ---------------------------------------------------------------------------
// read_thread
// ---------------------------------------------------------------------------

// ReadThreadTool finds an email thread whose subject contains the given hint
// (case-insensitive substring) and returns a compact prose summary the LLM
// can drop into a card's `sub` field.
type ReadThreadTool struct {
	Reader   log.Reader
	Lookback time.Duration // 0 → 14 days
	Now      func() time.Time
}

func (t *ReadThreadTool) Name() string { return "read_thread" }

func (t *ReadThreadTool) Description() string {
	return "Find one email thread by subject substring. Returns sender, last reply time, message count, and a body preview."
}

func (t *ReadThreadTool) Parameters() []llm.ToolParamSpec {
	return []llm.ToolParamSpec{{
		Name:        "subject_hint",
		Type:        "string",
		Description: "Case-insensitive substring of the email subject (e.g., 'redline').",
		Required:    true,
	}}
}

func (t *ReadThreadTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	hint := argString(args, "subject_hint")
	if hint == "" {
		return "", fmt.Errorf("subject_hint is required")
	}
	now := t.now()
	lookback := t.Lookback
	if lookback <= 0 {
		lookback = 14 * 24 * time.Hour
	}
	since := now.Add(-lookback)

	events, err := t.Reader.ByKind(ctx, log.KindMailReceived)
	if err != nil {
		return "", err
	}

	type msg struct {
		Subject     string
		From        string
		Date        time.Time
		BodyPreview string
	}
	var matches []msg
	hintLower := strings.ToLower(hint)
	for _, e := range events {
		if e.TS.Before(since) {
			continue
		}
		var p struct {
			Subject     string    `json:"subject"`
			From        string    `json:"from"`
			Date        time.Time `json:"date"`
			BodyPreview string    `json:"body_preview"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		if !strings.Contains(strings.ToLower(p.Subject), hintLower) {
			continue
		}
		when := p.Date
		if when.IsZero() {
			when = e.TS
		}
		matches = append(matches, msg{
			Subject:     p.Subject,
			From:        p.From,
			Date:        when,
			BodyPreview: p.BodyPreview,
		})
	}

	if len(matches) == 0 {
		return fmt.Sprintf("No thread found whose subject contains %q.", hint), nil
	}

	sort.SliceStable(matches, func(i, j int) bool { return matches[i].Date.Before(matches[j].Date) })
	last := matches[len(matches)-1]

	preview := truncate(last.BodyPreview, 1500)
	out := fmt.Sprintf(
		"Thread: %s\nLast sender: %s\nLast received: %s\nMessages in window: %d\nLast body preview:\n%s",
		last.Subject, last.From, last.Date.Format(time.RFC3339), len(matches), preview,
	)
	return capOutput(out, 4096), nil
}

func (t *ReadThreadTool) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// ---------------------------------------------------------------------------
// read_event
// ---------------------------------------------------------------------------

// ReadEventTool fetches one calendar event by UID. The latest cal.event_*
// payload for that UID wins.
type ReadEventTool struct {
	Reader log.Reader
	TZ     *time.Location
}

func (t *ReadEventTool) Name() string { return "read_event" }

func (t *ReadEventTool) Description() string {
	return "Fetch one calendar event by UID. Returns time, title, location, and tag."
}

func (t *ReadEventTool) Parameters() []llm.ToolParamSpec {
	return []llm.ToolParamSpec{{
		Name:        "uid",
		Type:        "string",
		Description: "The calendar event UID.",
		Required:    true,
	}}
}

func (t *ReadEventTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	uid := argString(args, "uid")
	if uid == "" {
		return "", fmt.Errorf("uid is required")
	}

	events, err := t.Reader.ByKind(ctx, log.KindCalEventSeen, log.KindCalEventChanged)
	if err != nil {
		return "", err
	}

	type calRow struct {
		UID      string    `json:"uid"`
		Title    string    `json:"title"`
		Location string    `json:"location"`
		Tag      string    `json:"tag"`
		Start    time.Time `json:"start"`
		End      time.Time `json:"end"`
	}
	var latest *calRow
	for _, e := range events {
		var raw calRow
		if err := json.Unmarshal(e.Payload, &raw); err != nil {
			continue
		}
		if raw.UID != uid {
			continue
		}
		row := raw
		latest = &row // ByKind returns oldest-first, so the last assignment wins
	}
	if latest == nil {
		return fmt.Sprintf("No event found with uid %q.", uid), nil
	}

	tz := t.TZ
	if tz == nil {
		tz = time.UTC
	}
	out := fmt.Sprintf(
		"Event: %s\nUID: %s\nStart: %s\nEnd: %s\nLocation: %s\nTag: %s",
		fallback(latest.Title, "(untitled)"),
		latest.UID,
		latest.Start.In(tz).Format(time.RFC3339),
		latest.End.In(tz).Format(time.RFC3339),
		fallback(latest.Location, "(none)"),
		fallback(latest.Tag, "(none)"),
	)
	return capOutput(out, 4096), nil
}

// ---------------------------------------------------------------------------
// read_stock_alert
// ---------------------------------------------------------------------------

// ReadStockAlertTool fetches one stock.alert payload by its event-log
// ID — the value the inject pipeline carries on InjectSignal.EvidenceID
// for kind="stock_breach". Returns the ticker, current price, prior
// close, percent move and the threshold that was breached so the
// model can ground card prose in the actual numbers rather than just
// the percent in the subject line.
type ReadStockAlertTool struct {
	Reader log.Reader
}

func (t *ReadStockAlertTool) Name() string { return "read_stock_alert" }

func (t *ReadStockAlertTool) Description() string {
	return "Fetch one stock.alert payload by evidence_id. Returns ticker, current price, previous close, percent move, and the threshold that was breached."
}

func (t *ReadStockAlertTool) Parameters() []llm.ToolParamSpec {
	return []llm.ToolParamSpec{{
		Name:        "evidence_id",
		Type:        "string",
		Description: "The stock.alert event ID, surfaced as InjectSignal.EvidenceID for kind=stock_breach.",
		Required:    true,
	}}
}

func (t *ReadStockAlertTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	evidenceID := argString(args, "evidence_id")
	if evidenceID == "" {
		return "", fmt.Errorf("evidence_id is required")
	}

	events, err := t.Reader.ByKind(ctx, log.KindStockAlert)
	if err != nil {
		return "", err
	}

	type alertRow struct {
		Ticker       string    `json:"ticker"`
		Price        float64   `json:"price"`
		PrevClose    float64   `json:"prev_close"`
		ChangePct    float64   `json:"change_pct"`
		ThresholdPct float64   `json:"threshold_pct"`
		Currency     string    `json:"currency"`
		AsOf         time.Time `json:"as_of"`
	}
	for _, e := range events {
		if e.ID != evidenceID {
			continue
		}
		var raw alertRow
		if err := json.Unmarshal(e.Payload, &raw); err != nil {
			return "", fmt.Errorf("decode stock.alert payload: %w", err)
		}
		direction := "+"
		if raw.ChangePct < 0 {
			direction = "−"
		}
		out := fmt.Sprintf(
			"Stock alert: %s\nPrice: %.2f %s\nPrior close: %.2f\nMove: %s%.2f%% (threshold %.2f%%)\nObserved at: %s",
			raw.Ticker,
			raw.Price, fallback(raw.Currency, "(unknown currency)"),
			raw.PrevClose,
			direction, abs(raw.ChangePct), raw.ThresholdPct,
			raw.AsOf.UTC().Format(time.RFC3339),
		)
		return capOutput(out, 4096), nil
	}
	return fmt.Sprintf("No stock.alert found with evidence_id %q.", evidenceID), nil
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// ---------------------------------------------------------------------------
// read_weather_window
// ---------------------------------------------------------------------------

// ReadWeatherWindowTool reads the latest weather snapshot and summarizes the
// hourly conditions inside [start_iso, end_iso].
type ReadWeatherWindowTool struct {
	Reader log.Reader
	TZ     *time.Location
}

func (t *ReadWeatherWindowTool) Name() string { return "read_weather_window" }

func (t *ReadWeatherWindowTool) Description() string {
	return "Summarize weather conditions in a time window. Returns dominant condition, max wind, and total precipitation."
}

func (t *ReadWeatherWindowTool) Parameters() []llm.ToolParamSpec {
	return []llm.ToolParamSpec{
		{
			Name:        "start_iso",
			Type:        "string",
			Description: "Window start in RFC3339 (e.g., 2026-04-25T12:30:00Z).",
			Required:    true,
		},
		{
			Name:        "end_iso",
			Type:        "string",
			Description: "Window end in RFC3339.",
			Required:    true,
		},
	}
}

func (t *ReadWeatherWindowTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	startStr := argString(args, "start_iso")
	endStr := argString(args, "end_iso")
	if startStr == "" || endStr == "" {
		return "", fmt.Errorf("start_iso and end_iso are required")
	}
	start, err := time.Parse(time.RFC3339, startStr)
	if err != nil {
		return "", fmt.Errorf("parse start_iso: %w", err)
	}
	end, err := time.Parse(time.RFC3339, endStr)
	if err != nil {
		return "", fmt.Errorf("parse end_iso: %w", err)
	}
	if !end.After(start) {
		return "", fmt.Errorf("end_iso must be after start_iso")
	}

	ev, err := t.Reader.Latest(ctx, log.KindWeatherSnapshot)
	if err != nil {
		return "", err
	}
	if ev == nil {
		return "No weather snapshot available.", nil
	}

	type hour struct {
		Time     time.Time `json:"time"`
		Code     int       `json:"code"`
		WindKmh  float64   `json:"wind_kmh"`
		PrecipMM float64   `json:"precip_mm"`
	}
	var snap struct {
		CapturedAt time.Time `json:"captured_at"`
		Hourly     []hour    `json:"hourly"`
	}
	if err := json.Unmarshal(ev.Payload, &snap); err != nil {
		return "", fmt.Errorf("decode weather snapshot: %w", err)
	}

	var (
		count       int
		maxWind     float64
		totalPrecip float64
		codes       = map[int]int{}
	)
	for _, h := range snap.Hourly {
		ht := h.Time
		if !ht.Before(start) && ht.Before(end) {
			count++
			if h.WindKmh > maxWind {
				maxWind = h.WindKmh
			}
			totalPrecip += h.PrecipMM
			codes[h.Code]++
		}
	}
	if count == 0 {
		return fmt.Sprintf("No hourly data in window %s–%s.", startStr, endStr), nil
	}

	dominantCode := -1
	dominantHits := 0
	for code, n := range codes {
		if n > dominantHits {
			dominantHits = n
			dominantCode = code
		}
	}

	tz := t.TZ
	if tz == nil {
		tz = time.UTC
	}
	out := fmt.Sprintf(
		"Window: %s–%s (%d hourly points)\nCondition: %s\nMax wind: %.0f km/h\nTotal precip: %.1f mm\nSnapshot captured: %s",
		start.In(tz).Format(time.RFC3339),
		end.In(tz).Format(time.RFC3339),
		count,
		weatherCodeLabel(dominantCode),
		maxWind,
		totalPrecip,
		snap.CapturedAt.In(tz).Format(time.RFC3339),
	)
	return capOutput(out, 4096), nil
}

// weatherCodeLabel maps Open-Meteo WMO weather codes to a short label. Kept
// independent from the projection helper so prompt iteration can tune copy
// without breaking the projection's logic.
func weatherCodeLabel(code int) string {
	switch {
	case code == 0:
		return "clear"
	case code == 1:
		return "mainly clear"
	case code == 2:
		return "partly cloudy"
	case code == 3:
		return "overcast"
	case code >= 45 && code <= 48:
		return "fog"
	case code >= 51 && code <= 67:
		return "rain"
	case code >= 71 && code <= 77:
		return "snow"
	case code >= 80 && code <= 82:
		return "showers"
	case code >= 95:
		return "thunderstorm"
	default:
		return "mixed"
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func fallback(s, alt string) string {
	if strings.TrimSpace(s) == "" {
		return alt
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func capOutput(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
