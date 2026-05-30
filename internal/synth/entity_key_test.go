package synth

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/projection"
)

func TestResolveEntityKey(t *testing.T) {
	cal := []projection.CalendarEvent{
		{UID: "evt-acuity-123", Title: "Acuity Capital — Series B narrative review", Start: time.Now()},
		{UID: "evt-gym-456", Title: "Lia gymnastics practice", Tag: "personal", Start: time.Now()},
	}
	threads := []projection.Thread{
		{Subject: "Saru Patel · redline questions", LastReceived: time.Now()},
		{Subject: "Pricing v2 memo — long-deferred", LastReceived: time.Now()},
	}

	tests := []struct {
		name string
		card Card
		want string
	}{
		{
			name: "calendar event by srclabel abbreviation",
			card: Card{Source: "calendar", SrcLabel: "Calendar · Acuity", Title: "Series B narrative review at 11", Sub: "Walk Lin through the deck."},
			want: "cal:evt-acuity-123",
		},
		{
			name: "personal event by distinctive token in title",
			card: Card{Source: "personal", SrcLabel: "Family · Sam", Title: "Lia's gymnastics at 16:00", Sub: "Pickup right after."},
			want: "cal:evt-gym-456",
		},
		{
			name: "mail thread by distinctive token",
			card: Card{Source: "mail", SrcLabel: "Email · Saru", Title: "Saru · re: redline", Sub: "Two questions remain on the redline."},
			want: "thread:saru-patel-redline-questions",
		},
		{
			name: "tasks thread (deferred memo)",
			card: Card{Source: "tasks", SrcLabel: "Task", Title: "Finish the pricing memo", Sub: "The pricing memo has been deferred a week."},
			want: "thread:pricing-v2-memo-long-deferred",
		},
		{
			name: "markets ticker from srclabel",
			card: Card{Source: "markets", SrcLabel: "Markets · AAPL", Title: "AAPL up sharply", Sub: "Crossed your alert threshold."},
			want: "ticker:AAPL",
		},
		{
			name: "digest keyed on date",
			card: Card{Source: "mail", Kind: "digest", Title: "Five low-signal threads", Sub: "Skim later."},
			want: "digest:2026-04-25",
		},
		{
			name: "proposal confirmation card",
			card: Card{Source: "personal", Title: "Confirm dinner with Sam", Sub: "Text Sam to lock the slot.",
				Actions: []Action{{Intent: "send_whatsapp", Target: map[string]any{"context_kind": "event", "context_id": "evt-dinner-9"}}}},
			want: "propose-confirm-evt-dinner-9",
		},
		{
			name: "no confident match returns empty",
			card: Card{Source: "mail", SrcLabel: "Email · Newsletter", Title: "Weekly roundup", Sub: "Industry news you might have missed."},
			want: "",
		},
		{
			name: "ask card with no entity match stays unanchored",
			card: Card{Source: "ask", SrcLabel: "Generated", Title: "A fragile pause in the conflict", Sub: "Talks continue under pressure."},
			want: "",
		},
		{
			name: "ask card about a known thread anchors to it (cross-source dedup)",
			card: Card{Source: "ask", SrcLabel: "Generated", Title: "What's the latest on the redline?", Sub: "Saru raised redline questions."},
			want: "thread:saru-patel-redline-questions",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveEntityKey(&tc.card, "2026-04-25", cal, threads)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestResolveEntityKey_TitleDriftSameThread is the repetition-critical
// case: two differently-worded cards about the SAME thread must resolve to
// the same key (so the Upsert collapses them), while a card about a
// genuinely different subject gets a different key.
func TestResolveEntityKey_TitleDrift(t *testing.T) {
	threads := []projection.Thread{
		{Subject: "Saru Patel · redline questions", LastReceived: time.Now()},
	}
	a := Card{Source: "mail", Title: "Saru · re: redline", Sub: "Two redline questions remain."}
	b := Card{Source: "mail", Title: "Redline: Saru's two open items", Sub: "Saru still has redline questions."}

	ka := resolveEntityKey(&a, "2026-04-25", nil, threads)
	kb := resolveEntityKey(&b, "2026-04-25", nil, threads)
	require.NotEmpty(t, ka)
	require.Equal(t, ka, kb, "title drift on the same thread must yield the same entity key")

	// A card about an unrelated subject must not collide.
	c := Card{Source: "mail", Title: "Quarterly invoice from vendor", Sub: "Vendor invoice due Friday."}
	require.NotEqual(t, ka, resolveEntityKey(&c, "2026-04-25", nil, threads))
}

func TestNormalizeSubject(t *testing.T) {
	require.Equal(t, "redline", normalizeSubject("Re: redline"))
	require.Equal(t, "redline", normalizeSubject("RE: FWD: redline"))
	require.Equal(t, "redline", normalizeSubject("  fwd:redline"))
	require.Equal(t, "Series B", normalizeSubject("Series B"))
}

func TestTickerFromSrcLabel(t *testing.T) {
	require.Equal(t, "AAPL", tickerFromSrcLabel("Markets · AAPL"))
	require.Equal(t, "BRK.B", tickerFromSrcLabel("Markets · BRK.B"))
	require.Equal(t, "", tickerFromSrcLabel("Markets · Some Long Company Name"))
	require.Equal(t, "", tickerFromSrcLabel(""))
}
