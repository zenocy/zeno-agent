package api

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	"github.com/zenocy/zeno-v2/internal/projection"
	"github.com/zenocy/zeno-v2/internal/store"
)

func sampleViews(now time.Time) liveViews {
	return liveViews{
		now:    now,
		active: true,
		weather: &projection.WeatherView{
			CapturedAt: now.Add(-20 * time.Minute),
			Current:    projection.WeatherCurrent{TempC: 15, Label: "clear"},
		},
		stock: &projection.StockView{
			Quotes: []projection.StockQuote{
				{Ticker: "AAPL", Price: 210.50, ChangePct: 1.25, AsOf: now.Add(-5 * time.Minute)},
				{Ticker: "TSLA", Price: 180.00, ChangePct: -3.40, AsOf: now.Add(-30 * time.Hour), Stale: true},
			},
		},
		cal: []projection.CalendarEvent{
			{UID: "evt-1", Title: "Board sync", Start: now.Add(2*time.Hour + 13*time.Minute)},
		},
	}
}

func TestApplyLiveFields_Weather(t *testing.T) {
	now := time.Date(2026, 4, 25, 9, 0, 0, 0, time.UTC)
	lv := sampleViews(now)
	meta := []string{"⟦weather⟧", "·", "noon window"}
	live := datatypes.JSON(`[{"slot":"meta","kind":"weather","ref":"current"}]`)

	outMeta, _, chips := applyLiveFields(live, meta, "Run window holds.", lv)
	require.Equal(t, []string{"15° clear", "·", "noon window"}, outMeta)
	require.Len(t, chips, 1)
	require.Equal(t, "15° clear", chips[0].Text)
	require.False(t, chips[0].Stale)

	// Stale/missing weather (Compute returned nil) → sentinel stripped, chip
	// dropped, surrounding meta preserved.
	lv.weather = nil
	outMeta, _, chips = applyLiveFields(live, meta, "Run window holds.", lv)
	require.Equal(t, []string{"noon window"}, outMeta, "unresolved sentinel chip dropped, trailing separator trimmed")
	require.Empty(t, chips)
}

func TestApplyLiveFields_Stock(t *testing.T) {
	now := time.Date(2026, 4, 25, 9, 0, 0, 0, time.UTC)
	lv := sampleViews(now)

	meta := []string{"⟦$AAPL⟧", "·", "alert $210"}
	live := datatypes.JSON(`[{"slot":"meta","kind":"stock","ref":"AAPL"}]`)
	outMeta, _, chips := applyLiveFields(live, meta, "", lv)
	require.Equal(t, "$210.50 +1.25%", outMeta[0])
	require.Len(t, chips, 1)
	require.False(t, chips[0].Stale)

	// Stale quote → resolved value still shown, but stale flag set.
	staleLive := datatypes.JSON(`[{"slot":"meta","kind":"stock","ref":"TSLA"}]`)
	_, _, chips = applyLiveFields(staleLive, []string{"⟦$TSLA⟧"}, "", lv)
	require.Len(t, chips, 1)
	require.True(t, chips[0].Stale)

	// Unknown ticker → stripped, no chip.
	unknown := datatypes.JSON(`[{"slot":"meta","kind":"stock","ref":"NVDA"}]`)
	outMeta, _, chips = applyLiveFields(unknown, []string{"⟦$NVDA⟧", "·", "x"}, "", lv)
	require.Equal(t, []string{"x"}, outMeta)
	require.Empty(t, chips)
}

func TestApplyLiveFields_Countdown_Freshness(t *testing.T) {
	now := time.Date(2026, 4, 25, 9, 0, 0, 0, time.UTC)
	lv := sampleViews(now)
	live := datatypes.JSON(`[{"slot":"sub_suffix","kind":"countdown","ref":"evt-1"}]`)
	sub := "Board sync starts ⟦countdown⟧."

	_, outSub, chips := applyLiveFields(live, nil, sub, lv)
	require.Equal(t, "Board sync starts in 2h13m.", outSub)
	require.Len(t, chips, 1)

	// Advance the clock WITHOUT re-synth — the same card text resolves to a
	// new countdown. This is the staleness fix.
	lv.now = now.Add(time.Hour)
	_, outSub, _ = applyLiveFields(live, nil, sub, lv)
	require.Equal(t, "Board sync starts in 1h13m.", outSub)

	// Past start → "now".
	lv.now = now.Add(3 * time.Hour)
	_, outSub, _ = applyLiveFields(live, nil, sub, lv)
	require.Equal(t, "Board sync starts now.", outSub)
}

func TestApplyLiveFields_StripsLeftoverSentinels(t *testing.T) {
	now := time.Date(2026, 4, 25, 9, 0, 0, 0, time.UTC)
	lv := sampleViews(now)

	// A sentinel in the text with NO matching live entry must be stripped
	// (never leaked raw to the UI).
	_, outSub, chips := applyLiveFields(datatypes.JSON(`[]`), nil, "Temp now ⟦weather⟧ today.", lv)
	require.Equal(t, "Temp now today.", outSub)
	require.Empty(t, chips)

	// A live entry whose sentinel isn't present in the text is a no-op for
	// the text (the value still resolves into a chip).
	live := datatypes.JSON(`[{"slot":"meta","kind":"weather","ref":"current"}]`)
	outMeta, outSub, chips := applyLiveFields(live, []string{"12:00"}, "no sentinel here", lv)
	require.Equal(t, []string{"12:00"}, outMeta)
	require.Equal(t, "no sentinel here", outSub)
	require.Len(t, chips, 1)
}

func TestApplyLiveFields_NoLiveField(t *testing.T) {
	meta := []string{"06:14", "·", "thread of 7"}
	outMeta, outSub, chips := applyLiveFields(nil, meta, "static sub", liveViews{})
	require.Equal(t, meta, outMeta)
	require.Equal(t, "static sub", outSub)
	require.Nil(t, chips)
}

func TestToCardDTO_LiveSubstitution(t *testing.T) {
	now := time.Date(2026, 4, 25, 9, 0, 0, 0, time.UTC)
	lv := sampleViews(now)
	r := store.Card{
		ID: "aapl", Date: "2026-04-25", Source: "markets", SrcLabel: "Markets · AAPL", Rel: "med",
		Title: "AAPL crossed your alert",
		Sub:   "Trading at ⟦$AAPL⟧ now.",
		Meta:  datatypes.JSON(`["⟦$AAPL⟧","·","alert $210"]`),
		Live:  datatypes.JSON(`[{"slot":"sub_suffix","kind":"stock","ref":"AAPL"}]`),
	}
	d := toCardDTO(r, lv)
	require.Equal(t, "Trading at $210.50 +1.25% now.", d.Sub)
	require.Equal(t, "$210.50 +1.25%", d.Meta[0])
	require.Len(t, d.Live, 1)
}

func TestToCardDTO_DigestItems(t *testing.T) {
	r := store.Card{
		ID: "digest:2026-04-25", Date: "2026-04-25", Source: "mail", Kind: "digest", Rel: "low",
		Title: "5 newsletters", Sub: "Skim later.",
		Items: datatypes.JSON(`[{"title":"Stratechery","src":"mail"},{"title":"GitHub digest","sub":"3 repos"}]`),
	}
	d := toCardDTO(r, liveViews{})
	require.Len(t, d.Items, 2)
	require.Equal(t, "Stratechery", d.Items[0].Title)
	require.Equal(t, "3 repos", d.Items[1].Sub)
}

func TestHumanizeUntil(t *testing.T) {
	require.Equal(t, "now", humanizeUntil(0))
	require.Equal(t, "now", humanizeUntil(-5*time.Minute))
	require.Equal(t, "in 9m", humanizeUntil(9*time.Minute))
	require.Equal(t, "in 2h13m", humanizeUntil(2*time.Hour+13*time.Minute))
	require.Equal(t, "in 1h00m", humanizeUntil(time.Hour))
}
