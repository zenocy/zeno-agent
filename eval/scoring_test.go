package eval

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/synth"
)

func TestCalmness(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"clean", "A calm start. *One* thing wants you before noon.", 3},
		{"single_exclamation", "Hello world!", 2},
		{"important_marker", "Important: read this.", 2},
		{"many_violations", "URGENT! IMPORTANT: Note: TL;DR: !!", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := ScoreCalmness(tc.in)
			require.Equal(t, tc.want, s.Value, "hits=%v", s.Hits)
		})
	}
}

func TestDecisiveness(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"clean", "Series B review at 11. The day breathes.", 3},
		{"one_hedge", "Maybe block 13:30.", 2},
		{"hope_helps", "Hope this helps! Let me know.", 1},
		{"all_hedges", "Maybe perhaps you might want to feel free to let me know if you'd like.", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := ScoreDecisiveness(tc.in)
			require.Equal(t, tc.want, s.Value, "hits=%v", s.Hits)
		})
	}
}

func TestConcreteness(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int // exact, since the ratio bands are clearly delineated
		min  int // alternative: lower bound when prose is concrete-leaning
	}{
		{"empty", "", 0, 0},
		{"vague", "Today is busy. Soon you'll relax. Recently a thing happened.", 0, 0},
		{"concrete", "Series B at 11:00 with Saru Patel and Lin Vega. 16°. 45m run.", 0, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := ScoreConcreteness(tc.in)
			if tc.min > 0 {
				require.GreaterOrEqual(t, s.Value, tc.min, "hits=%v", s.Hits)
			} else {
				require.Equal(t, tc.want, s.Value, "hits=%v", s.Hits)
			}
		})
	}
}

func TestItalicBalanced(t *testing.T) {
	require.True(t, ItalicBalanced(""))
	require.True(t, ItalicBalanced("plain prose"))
	require.True(t, ItalicBalanced("A *calm* start."))
	require.True(t, ItalicBalanced("*One* thing and *another*."))
	require.False(t, ItalicBalanced("*One thing"))
	require.False(t, ItalicBalanced("*A* *B* *C"))
}

func TestCheckMustCards(t *testing.T) {
	cs := synth.CardSet{Cards: []synth.Card{
		{Source: "mail", Title: "Saru Patel — re: redline", Sub: "Walked the redline with Lin."},
		{Source: "calendar", Title: "Acuity — Series B", Sub: "11:00 with Saru, Lin, Park."},
	}}
	must := []MustCard{
		{Name: "saru", Sources: CardSources{"mail"}, TitleContains: []string{"saru"}, SubContains: nil},
		{Name: "acuity", Sources: CardSources{"calendar"}, TitleContains: []string{"acuity"}, SubContains: nil},
		{Name: "missing", Sources: CardSources{"personal"}, TitleContains: []string{"lia"}, SubContains: nil},
	}
	got := CheckMustCards(cs, must)
	require.Equal(t, []bool{true, true, false}, got)
}

func TestScoreboard_TotalAndAllPresent(t *testing.T) {
	sb := Scoreboard{
		Calmness:     Score{Value: 3},
		Decisiveness: Score{Value: 2},
		Concreteness: Score{Value: 3},
		MustCards:    []bool{true, true},
	}
	require.Equal(t, 8, sb.Total())
	require.True(t, sb.AllMustCardsPresent())

	sb.MustCards = []bool{true, false}
	require.False(t, sb.AllMustCardsPresent())
}
