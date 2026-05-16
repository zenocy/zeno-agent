package eval

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestScoreConcernRecognitionExact_PerfectMatchScores3 pins the 3-band:
// every expected name is matched by at least one proposal.
func TestScoreConcernRecognitionExact_PerfectMatchScores3(t *testing.T) {
	got := ScoreConcernRecognitionExact(
		[]string{
			"Construction at the house",
			"Frankfurt trip",
			"Engineering lead hire",
		},
		[]FixtureExpectedConcern{
			{Name: "Construction at the house"},
			{Name: "Frankfurt trip"},
			{Name: "Engineering lead hire"},
		},
	)
	require.Equal(t, 3, got.Value)
}

func TestScoreConcernRecognitionExact_LooseMatchViaNameContains(t *testing.T) {
	got := ScoreConcernRecognitionExact(
		[]string{"Frankfurt — Heim review"},
		[]FixtureExpectedConcern{
			{Name: "Frankfurt trip", NameContains: []string{"frankfurt", "heim"}},
		},
	)
	require.Equal(t, 3, got.Value)
}

func TestScoreConcernRecognitionExact_PartialMatchScores2(t *testing.T) {
	got := ScoreConcernRecognitionExact(
		[]string{"Construction at the house", "Frankfurt trip"},
		[]FixtureExpectedConcern{
			{Name: "Construction at the house"},
			{Name: "Frankfurt trip"},
			{Name: "Engineering lead hire"},
		},
	)
	require.Equal(t, 2, got.Value, "2/3 match → band 2")
}

func TestScoreConcernRecognitionExact_OneOfThreeScores1(t *testing.T) {
	got := ScoreConcernRecognitionExact(
		[]string{"Construction at the house"},
		[]FixtureExpectedConcern{
			{Name: "Construction at the house"},
			{Name: "Frankfurt trip"},
			{Name: "Engineering lead hire"},
		},
	)
	require.Equal(t, 1, got.Value)
}

func TestScoreConcernRecognitionExact_NoMatchScores0(t *testing.T) {
	got := ScoreConcernRecognitionExact(
		[]string{"Random other thing"},
		[]FixtureExpectedConcern{{Name: "Construction"}},
	)
	require.Equal(t, 0, got.Value)
}

// TestScoreConcernRecognitionExact_NegativeFixtureInverts pins the
// inversion rule for negative fixtures: empty expected + empty actual
// = perfect; empty expected + any actual = total fail.
func TestScoreConcernRecognitionExact_NegativeFixtureInverts(t *testing.T) {
	got := ScoreConcernRecognitionExact(nil, nil)
	require.Equal(t, 3, got.Value)

	got = ScoreConcernRecognitionExact(
		[]string{"Newsletter follow-up"}, nil,
	)
	require.Equal(t, 0, got.Value)
}

func TestScoreRetrospectiveTagPrecisionRecall_PerfectMatch(t *testing.T) {
	p, r := ScoreRetrospectiveTagPrecisionRecall(
		[]string{"a", "b", "c"},
		[]string{"a", "b", "c"},
	)
	require.Equal(t, 3, p.Value)
	require.Equal(t, 3, r.Value)
}

func TestScoreRetrospectiveTagPrecisionRecall_Partial(t *testing.T) {
	// actual = {a,b,d}, expected = {a,b,c}
	// tp=2, fp=1, fn=1 → precision=2/3≈0.67 (band 1), recall=2/3≈0.67 (band 2)
	p, r := ScoreRetrospectiveTagPrecisionRecall(
		[]string{"a", "b", "d"},
		[]string{"a", "b", "c"},
	)
	require.Equal(t, 1, p.Value)
	require.Equal(t, 2, r.Value)
}

func TestScoreRetrospectiveTagPrecisionRecall_HighRecallLowPrecision(t *testing.T) {
	// actual = {a,b,c,d,e}, expected = {a,b,c}
	// tp=3, fp=2, fn=0 → precision=3/5=0.6 (band 1), recall=1.0 (band 3)
	p, r := ScoreRetrospectiveTagPrecisionRecall(
		[]string{"a", "b", "c", "d", "e"},
		[]string{"a", "b", "c"},
	)
	require.Equal(t, 1, p.Value)
	require.Equal(t, 3, r.Value)
}

func TestScoreRetrospectiveTagPrecisionRecall_EmptyActual(t *testing.T) {
	p, r := ScoreRetrospectiveTagPrecisionRecall(nil, []string{"a", "b"})
	// actual empty → precision is 0/0 → 0 by convention; recall = 0/2 = 0
	require.Equal(t, 0, p.Value)
	require.Equal(t, 0, r.Value)
}

func TestScoreRetrospectiveTagPrecisionRecall_EmptyExpected(t *testing.T) {
	// Expected empty → recall is 0/0 → 0 (no positive set to recall);
	// precision = 0/anything-actual = 0 because tp=0.
	p, r := ScoreRetrospectiveTagPrecisionRecall([]string{"a"}, nil)
	require.Equal(t, 0, p.Value)
	require.Equal(t, 0, r.Value)
}

// TestScoreRecognitionDailyCap_PassesAtOrUnderCap pins the boundary.
// At-cap is a pass; one over is a fail. Important because the contract
// is `proposals ≤ cap`, not `< cap`.
func TestScoreRecognitionDailyCap_PassesAtOrUnderCap(t *testing.T) {
	require.Equal(t, 3, ScoreRecognitionDailyCap(0, 2).Value)
	require.Equal(t, 3, ScoreRecognitionDailyCap(1, 2).Value)
	require.Equal(t, 3, ScoreRecognitionDailyCap(2, 2).Value)
}

func TestScoreRecognitionDailyCap_FailsOverCap(t *testing.T) {
	got := ScoreRecognitionDailyCap(3, 2)
	require.Equal(t, 0, got.Value)
	require.NotEmpty(t, got.Hits)
}

func TestScoreRecognitionDailyCap_DefaultCap(t *testing.T) {
	// cap <= 0 falls back to 2.
	require.Equal(t, 3, ScoreRecognitionDailyCap(2, 0).Value)
	require.Equal(t, 0, ScoreRecognitionDailyCap(3, 0).Value)
}

// V2.5.0 Phase 3 rubrics ------------------------------------------------

// TestScoreConcernQueryMatch_PerfectMatchScores3 covers the canonical
// happy path.
func TestScoreConcernQueryMatch_PerfectMatchScores3(t *testing.T) {
	s := ScoreConcernQueryMatch("c-construction", FixtureConcernQueryTest{
		Query: "what's happening with construction?", ExpectedConcernID: "c-construction",
	})
	require.Equal(t, 3, s.Value)
}

// TestScoreConcernQueryMatch_WrongConcernScores0 pins the shape: a
// wrong concern is total fail, with the actual+expected in Hits for
// the report.
func TestScoreConcernQueryMatch_WrongConcernScores0(t *testing.T) {
	s := ScoreConcernQueryMatch("c-other", FixtureConcernQueryTest{
		ExpectedConcernID: "c-construction",
	})
	require.Equal(t, 0, s.Value)
	require.NotEmpty(t, s.Hits)
}

// TestScoreConcernQueryMatch_NegativeCase pins the inversion: when the
// fixture says "must NOT match", an empty actual is correct (3); a
// non-empty actual is a regression (0).
func TestScoreConcernQueryMatch_NegativeCase(t *testing.T) {
	pos := ScoreConcernQueryMatch("", FixtureConcernQueryTest{MustNotMatchConcern: true})
	require.Equal(t, 3, pos.Value)
	neg := ScoreConcernQueryMatch("c-anything", FixtureConcernQueryTest{MustNotMatchConcern: true})
	require.Equal(t, 0, neg.Value)
}

// TestScoreConcernRetrievalQuality_PrecisionBands pins the four bands
// of the precision-only rubric. We use precision (not recall)
// because surfacing extra evidence is acceptable; surfacing irrelevant
// evidence is not.
func TestScoreConcernRetrievalQuality_PrecisionBands(t *testing.T) {
	expected := []string{"e1", "e2", "e3", "e4"}

	// 4/4 actual all in expected = 1.0 → 3.
	require.Equal(t, 3, ScoreConcernRetrievalQuality([]string{"e1", "e2", "e3", "e4"}, expected).Value)

	// 3/4 hits = 0.75 → 3.
	require.Equal(t, 3, ScoreConcernRetrievalQuality([]string{"e1", "e2", "e3", "x"}, expected).Value)

	// 2/4 hits = 0.5 → 2.
	require.Equal(t, 2, ScoreConcernRetrievalQuality([]string{"e1", "e2", "x", "y"}, expected).Value)

	// 1/3 hits ≈ 0.33 → 1.
	require.Equal(t, 1, ScoreConcernRetrievalQuality([]string{"e1", "x", "y"}, expected).Value)

	// 0 hits = 0.
	require.Equal(t, 0, ScoreConcernRetrievalQuality([]string{"x", "y"}, expected).Value)
}

// TestScoreConcernRetrievalQuality_EmptyActualWithExpected captures
// the no-retrieval case correctly: when the model SHOULD have pulled
// evidence and didn't, score 0.
func TestScoreConcernRetrievalQuality_EmptyActualWithExpected(t *testing.T) {
	require.Equal(t, 0, ScoreConcernRetrievalQuality(nil, []string{"e1"}).Value)
}

// TestScoreConcernRetrievalQuality_EmptyActualEmptyExpected returns 3:
// the negative path (correctly didn't retrieve anything because nothing
// was relevant).
func TestScoreConcernRetrievalQuality_EmptyActualEmptyExpected(t *testing.T) {
	require.Equal(t, 3, ScoreConcernRetrievalQuality(nil, nil).Value)
}

// TestScoreConcernsBlockBytePresence_HasConcerns_Mentions pins the
// success: with seeded concerns, the briefing prose must mention at
// least one. The voice-canon rule says "weave it in" — a briefing
// that ignores seeded concerns entirely fails.
func TestScoreConcernsBlockBytePresence_HasConcerns_Mentions(t *testing.T) {
	s := ScoreConcernsBlockBytePresence(
		"Today the *Frankfurt trip* is the open thread; Heim's review is mid-week.",
		[]string{"Frankfurt trip"},
		true,
	)
	require.Equal(t, 3, s.Value)
}

// TestScoreConcernsBlockBytePresence_HasConcerns_DoesNotMention is
// the regression on the surfacing path: seeded concerns that the
// model omitted entirely score 0 with a diagnostic Hit.
func TestScoreConcernsBlockBytePresence_HasConcerns_DoesNotMention(t *testing.T) {
	s := ScoreConcernsBlockBytePresence(
		"A calm morning. The board call sets the day.",
		[]string{"Frankfurt trip"},
		true,
	)
	require.Equal(t, 0, s.Value)
	require.NotEmpty(t, s.Hits)
}

// TestScoreConcernsBlockBytePresence_NegativeFixture_NoLeakage is the
// voice-canon regression on the OTHER side: a fixture with zero
// concerns must NOT see "Frankfurt trip" in prose. Without this gate,
// any voice-canon update could silently leak concern names into
// briefings that have no concerns.
func TestScoreConcernsBlockBytePresence_NegativeFixture_NoLeakage(t *testing.T) {
	s := ScoreConcernsBlockBytePresence(
		"A calm morning. Three things matter.",
		[]string{"Frankfurt trip"},
		false,
	)
	require.Equal(t, 3, s.Value)
}

func TestScoreConcernsBlockBytePresence_NegativeFixture_LeakageFails(t *testing.T) {
	s := ScoreConcernsBlockBytePresence(
		"Mention of the Frankfurt trip slipped in here.",
		[]string{"Frankfurt trip"},
		false,
	)
	require.Equal(t, 0, s.Value)
	require.Contains(t, s.Hits, "Frankfurt trip")
}
