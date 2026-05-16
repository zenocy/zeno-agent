package eval

import (
	"testing"

	"github.com/stretchr/testify/require"
)

var memoryFixtureFacts = []FixtureMemoryFact{
	{Subject: "partner", Fact: "Partner is Sam.", Confidence: "high", Source: "user"},
	{Subject: "child", Fact: "Daughter Lia, school pickup is on Sam.", Confidence: "high", Source: "user"},
	{Subject: "runs", Fact: "Usually runs Tue/Thu mornings; aims for 3x/week.", Confidence: "med", Source: "synth"},
	{Subject: "anniversary", Fact: "Anniversary is May 7.", Confidence: "high", Source: "user"},
	{Subject: "dinner", Fact: "Otto's is the special-occasion dinner spot.", Confidence: "low", Source: "synth"},
}

func TestScoreMemoryOpenerTells(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"clean", "Sam's making dinner; the day breathes.", 3},
		{"i_remember", "I remember you usually run on Tuesdays.", 0},
		{"as_you_mentioned", "As you mentioned, Lia's pickup is on Sam.", 0},
		{"based_on", "Based on what I know, the anniversary is in May.", 0},
		{"you_told_me", "You told me Otto's was the spot.", 0},
		{"you_said_me", "You said me to pick a slot — clipped grammar but a tell.", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := ScoreMemoryOpenerTells(tc.in)
			require.Equal(t, tc.want, s.Value, "hits=%v", s.Hits)
		})
	}
}

func TestScoreMemoryFactDensity(t *testing.T) {
	cases := []struct {
		name  string
		prose string
		want  int
	}{
		{"none", "A calm morning with the redline ahead.", 3},
		{"one_subject", "Partner makes dinner.", 3},
		{"three_subjects", "Partner is steady. The child is asleep. The runs window is open.", 3},
		{"four_subjects", "Partner. Child. Runs. Anniversary.", 2},
		{"five_subjects", "Partner. Child. Runs. Anniversary. Dinner.", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := ScoreMemoryFactDensity(tc.prose, memoryFixtureFacts)
			require.Equal(t, tc.want, s.Value, "hits=%v", s.Hits)
		})
	}
}

func TestScoreMemoryFactDensity_NoFactsAlwaysFull(t *testing.T) {
	s := ScoreMemoryFactDensity("partner child runs anniversary dinner", nil)
	require.Equal(t, 3, s.Value, "no seeded facts → no density penalty")
}

func TestScoreMemoryFactDensity_WordBoundary(t *testing.T) {
	// "runs" must not match inside "running" or "samurai".
	s := ScoreMemoryFactDensity("Tonight is yours and the running list is short.", memoryFixtureFacts)
	require.Equal(t, 3, s.Value, "running ≠ runs; expected no match. hits=%v", s.Hits)
}

func TestScoreMemoryVerbatimLeak(t *testing.T) {
	cases := []struct {
		name  string
		prose string
		want  int
	}{
		{"clean_paraphrase", "Sam handles pickup. The anniversary is on the way.", 3},
		{"verbatim_multi_word", "Daughter Lia, school pickup is on Sam.", 0},
		{"verbatim_lowercase", "usually runs tue/thu mornings; aims for 3x/week. ok?", 0},
		{"single_word_excluded", "Sam.", 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := ScoreMemoryVerbatimLeak(tc.prose, memoryFixtureFacts)
			require.Equal(t, tc.want, s.Value, "hits=%v", s.Hits)
		})
	}
}

func TestScoreMemoryVerbatimLeak_NoFactsAlwaysFull(t *testing.T) {
	s := ScoreMemoryVerbatimLeak("Anything goes here.", nil)
	require.Equal(t, 3, s.Value)
}

func TestScoreMemoryGrounding_FullPipeline(t *testing.T) {
	clean := "A calm start. Sam handles pickup; the anniversary is on the way and the morning window is wide."
	mg := ScoreMemoryGrounding(clean, memoryFixtureFacts)
	require.Equal(t, 3, mg.OpenerTells.Value)
	require.Equal(t, 3, mg.FactDensity.Value)
	require.Equal(t, 3, mg.VerbatimLeak.Value)
	require.Equal(t, 5, mg.FactsInjected)
	require.Equal(t, -1, mg.WithVsEmpty.Value, "LLM-judge slot is not-run by default")
	require.Equal(t, 9, mg.Total())

	dirty := "I remember you said me — Daughter Lia, school pickup is on Sam. Anniversary is May 7."
	mg = ScoreMemoryGrounding(dirty, memoryFixtureFacts)
	require.Equal(t, 0, mg.OpenerTells.Value, "opener tell must trip")
	require.Equal(t, 0, mg.VerbatimLeak.Value, "verbatim leak must trip")
	// "child" subject not literally referenced (needle is "child" not "daughter"),
	// "anniversary" is referenced, "runs" not. Density should be at full marks.
	require.Equal(t, 3, mg.FactDensity.Value)
	require.NotEmpty(t, mg.VerbatimHits)
}
