package synth

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/log"
)

func openToolsLog(t *testing.T) log.Store {
	t.Helper()
	_, store, err := log.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	return store
}

func TestReadThreadTool_FindsBySubjectSubstring(t *testing.T) {
	store := openToolsLog(t)
	ctx := context.Background()
	now := time.Date(2026, 4, 25, 8, 0, 0, 0, time.UTC)

	_, err := store.Append(ctx, log.KindMailReceived, "imap", map[string]any{
		"subject":      "re: redline draft",
		"from":         "Saru <saru@acuity.test>",
		"date":         now.Add(-2 * time.Hour),
		"body_preview": "Walked the redline with Lin.",
	})
	require.NoError(t, err)
	_, err = store.Append(ctx, log.KindMailReceived, "imap", map[string]any{
		"subject":      "lunch tomorrow",
		"from":         "Aria <aria@halsen.test>",
		"date":         now.Add(-30 * time.Minute),
		"body_preview": "Moving to 12:30.",
	})
	require.NoError(t, err)

	tool := &ReadThreadTool{Reader: store, Now: func() time.Time { return now }}
	out, err := tool.Execute(ctx, map[string]any{"subject_hint": "REDLINE"})
	require.NoError(t, err)
	require.Contains(t, out, "redline draft")
	require.Contains(t, out, "Walked the redline")
	require.NotContains(t, out, "lunch tomorrow")
}

// TestReadThreadTool_ReturnsFullBodyUpTo6KB covers the V2.x bump: the
// body preview returned by the tool is now ~6KB (vs. 1500 chars
// previously). The `document` SubCard kind depends on this — a 2KB
// homework email must reach the model in full so it can reproduce
// every day's schedule.
func TestReadThreadTool_ReturnsFullBodyUpTo6KB(t *testing.T) {
	store := openToolsLog(t)
	ctx := context.Background()
	now := time.Date(2026, 5, 18, 8, 0, 0, 0, time.UTC)

	// 3KB of body — well above the prior 1500-char truncation point.
	body := ""
	for i := 0; i < 60; i++ {
		body += "Monday Greek: page 57. Tuesday Maths: halves. Wednesday Greek: page 58.\n"
	}
	require.Greater(t, len(body), 1500, "test fixture must exceed the prior truncation")

	_, err := store.Append(ctx, log.KindMailReceived, "imap", map[string]any{
		"subject":      "Homework Week of 18 May",
		"from":         "Miss Despoina",
		"date":         now.Add(-1 * time.Hour),
		"body_preview": body,
	})
	require.NoError(t, err)

	tool := &ReadThreadTool{Reader: store, Now: func() time.Time { return now }}
	out, err := tool.Execute(ctx, map[string]any{"subject_hint": "homework"})
	require.NoError(t, err)
	// Body must reach the model in full (or up to 6000 chars) — the
	// "Wednesday Greek" substring sits well past the old 1500 cap.
	require.Contains(t, out, "Wednesday Greek: page 58", "body preview must extend beyond 1500 chars")
}

// TestFindLatestThread covers the helper shared between ReadThreadTool
// and the /api/threads/preview endpoint: substring match by subject,
// most-recent wins, 14-day default lookback, ok=false on no match.
func TestFindLatestThread(t *testing.T) {
	store := openToolsLog(t)
	ctx := context.Background()
	now := time.Date(2026, 5, 18, 8, 0, 0, 0, time.UTC)

	_, err := store.Append(ctx, log.KindMailReceived, "imap", map[string]any{
		"subject":      "Homework Week of 11 May",
		"from":         "Miss Despoina",
		"date":         now.Add(-7 * 24 * time.Hour),
		"body_preview": "older content",
	})
	require.NoError(t, err)
	_, err = store.Append(ctx, log.KindMailReceived, "imap", map[string]any{
		"subject":      "Homework Week of 18 May",
		"from":         "Miss Despoina",
		"date":         now.Add(-1 * time.Hour),
		"body_preview": "newer content",
	})
	require.NoError(t, err)

	hit, ok, err := FindLatestThread(ctx, store, "HOMEWORK", 0, now)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "Homework Week of 18 May", hit.Subject)
	require.Equal(t, "newer content", hit.BodyPreview)
	require.Equal(t, 2, hit.MessageCount, "should count both matching messages")

	// Unknown hint → ok=false (the endpoint maps this to 404).
	_, ok, err = FindLatestThread(ctx, store, "vacation", 0, now)
	require.NoError(t, err)
	require.False(t, ok)

	// Empty hint → ok=false (the endpoint maps this to 400).
	_, ok, err = FindLatestThread(ctx, store, "  ", 0, now)
	require.NoError(t, err)
	require.False(t, ok)
}

func TestReadThreadTool_EmptyHintRejected(t *testing.T) {
	tool := &ReadThreadTool{Reader: openToolsLog(t)}
	_, err := tool.Execute(context.Background(), map[string]any{"subject_hint": ""})
	require.Error(t, err)
}

func TestReadThreadTool_NoMatch(t *testing.T) {
	tool := &ReadThreadTool{Reader: openToolsLog(t), Now: time.Now}
	out, err := tool.Execute(context.Background(), map[string]any{"subject_hint": "ghost"})
	require.NoError(t, err)
	require.Contains(t, out, "No thread found")
}

func TestReadEventTool_LatestPayloadWins(t *testing.T) {
	store := openToolsLog(t)
	ctx := context.Background()

	// Earlier seen, then later changed — change should win.
	_, err := store.Append(ctx, log.KindCalEventSeen, "caldav", map[string]any{
		"uid":   "evt-1",
		"title": "draft title",
		"start": time.Date(2026, 4, 25, 11, 0, 0, 0, time.UTC),
		"end":   time.Date(2026, 4, 25, 11, 30, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	time.Sleep(2 * time.Millisecond)
	_, err = store.Append(ctx, log.KindCalEventChanged, "caldav", map[string]any{
		"uid":      "evt-1",
		"title":    "Acuity — Series B review",
		"location": "Zoom",
		"tag":      "work",
		"start":    time.Date(2026, 4, 25, 11, 0, 0, 0, time.UTC),
		"end":      time.Date(2026, 4, 25, 11, 45, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	tool := &ReadEventTool{Reader: store, TZ: time.UTC}
	out, err := tool.Execute(ctx, map[string]any{"uid": "evt-1"})
	require.NoError(t, err)
	require.Contains(t, out, "Acuity — Series B review")
	require.Contains(t, out, "Zoom")
	require.NotContains(t, out, "draft title")
}

func TestReadEventTool_NotFound(t *testing.T) {
	tool := &ReadEventTool{Reader: openToolsLog(t)}
	out, err := tool.Execute(context.Background(), map[string]any{"uid": "missing"})
	require.NoError(t, err)
	require.Contains(t, out, "No event found")
}

func TestReadWeatherWindowTool_Summarizes(t *testing.T) {
	store := openToolsLog(t)
	ctx := context.Background()

	_, err := store.Append(ctx, log.KindWeatherSnapshot, "weather", map[string]any{
		"captured_at": time.Date(2026, 4, 25, 7, 0, 0, 0, time.UTC),
		"timezone":    "UTC",
		"hourly": []map[string]any{
			{"time": time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC), "code": 1, "wind_kmh": 8.0, "precip_mm": 0.0},
			{"time": time.Date(2026, 4, 25, 13, 0, 0, 0, time.UTC), "code": 2, "wind_kmh": 12.0, "precip_mm": 0.0},
			{"time": time.Date(2026, 4, 25, 14, 0, 0, 0, time.UTC), "code": 95, "wind_kmh": 45.0, "precip_mm": 5.0},
		},
	})
	require.NoError(t, err)

	tool := &ReadWeatherWindowTool{Reader: store, TZ: time.UTC}
	out, err := tool.Execute(ctx, map[string]any{
		"start_iso": "2026-04-25T12:00:00Z",
		"end_iso":   "2026-04-25T14:00:00Z",
	})
	require.NoError(t, err)
	require.Contains(t, out, "2 hourly points") // 12:00 + 13:00; 14:00 excluded by end exclusive
	require.Contains(t, out, "12 km/h")         // max wind in window
	require.Contains(t, out, "0.0 mm")          // no precip in window
}

func TestReadWeatherWindowTool_NoSnapshot(t *testing.T) {
	tool := &ReadWeatherWindowTool{Reader: openToolsLog(t), TZ: time.UTC}
	out, err := tool.Execute(context.Background(), map[string]any{
		"start_iso": "2026-04-25T12:00:00Z",
		"end_iso":   "2026-04-25T14:00:00Z",
	})
	require.NoError(t, err)
	require.Contains(t, out, "No weather snapshot")
}

func TestReadWeatherWindowTool_BadTimes(t *testing.T) {
	tool := &ReadWeatherWindowTool{Reader: openToolsLog(t), TZ: time.UTC}
	_, err := tool.Execute(context.Background(), map[string]any{
		"start_iso": "not-a-time",
		"end_iso":   "2026-04-25T14:00:00Z",
	})
	require.Error(t, err)

	_, err = tool.Execute(context.Background(), map[string]any{
		"start_iso": "2026-04-25T14:00:00Z",
		"end_iso":   "2026-04-25T12:00:00Z",
	})
	require.Error(t, err)
}

func TestReadStockAlertTool_HappyPath(t *testing.T) {
	store := openToolsLog(t)
	ctx := context.Background()
	asOf := time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC)

	ev, err := store.Append(ctx, log.KindStockAlert, "stock", map[string]any{
		"ticker":        "AAPL",
		"price":         210.5,
		"prev_close":    200.0,
		"change_pct":    5.25,
		"threshold_pct": 3.0,
		"currency":      "USD",
		"as_of":         asOf,
	})
	require.NoError(t, err)

	// A second, non-matching alert ensures the lookup is by ID, not "latest".
	_, err = store.Append(ctx, log.KindStockAlert, "stock", map[string]any{
		"ticker": "TSLA", "price": 100, "prev_close": 105, "change_pct": -4.76,
		"threshold_pct": 3.0, "currency": "USD", "as_of": asOf,
	})
	require.NoError(t, err)

	tool := &ReadStockAlertTool{Reader: store}
	out, err := tool.Execute(ctx, map[string]any{"evidence_id": ev.ID})
	require.NoError(t, err)
	require.Contains(t, out, "AAPL")
	require.Contains(t, out, "210.50")
	require.Contains(t, out, "200.00")
	require.Contains(t, out, "+5.25%")
	require.Contains(t, out, "threshold 3.00%")
	require.NotContains(t, out, "TSLA")
}

func TestReadStockAlertTool_NegativeMoveFormatsWithUnicodeMinus(t *testing.T) {
	store := openToolsLog(t)
	ctx := context.Background()
	asOf := time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC)

	ev, err := store.Append(ctx, log.KindStockAlert, "stock", map[string]any{
		"ticker":        "TSLA",
		"price":         100.0,
		"prev_close":    105.0,
		"change_pct":    -4.76,
		"threshold_pct": 3.0,
		"currency":      "USD",
		"as_of":         asOf,
	})
	require.NoError(t, err)

	tool := &ReadStockAlertTool{Reader: store}
	out, err := tool.Execute(ctx, map[string]any{"evidence_id": ev.ID})
	require.NoError(t, err)
	require.Contains(t, out, "−4.76%") // unicode minus
}

func TestReadStockAlertTool_EmptyEvidenceID_Errors(t *testing.T) {
	tool := &ReadStockAlertTool{Reader: openToolsLog(t)}
	_, err := tool.Execute(context.Background(), map[string]any{"evidence_id": ""})
	require.Error(t, err)
	require.Contains(t, err.Error(), "evidence_id")
}

func TestReadStockAlertTool_NotFound(t *testing.T) {
	tool := &ReadStockAlertTool{Reader: openToolsLog(t)}
	out, err := tool.Execute(context.Background(), map[string]any{"evidence_id": "no-such-id"})
	require.NoError(t, err, "missing evidence is a soft signal, not a tool error")
	require.Contains(t, out, "No stock.alert found")
}
