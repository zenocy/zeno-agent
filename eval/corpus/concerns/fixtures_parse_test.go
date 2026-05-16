package concerns_corpus

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/eval"
)

// TestPhase2Fixtures_Parse_AndCarryNewFields confirms each Phase 2 fixture
// loads via eval.LoadFixture, has Kind set to one of the Phase 2 kinds, and
// — for recognition fixtures — its expected_concerns block round-trips.
//
// This is the gate that catches a fixture whose JSON drifted from the struct
// (a refactor of FixtureExpectedConcern, a missing comma in a hand-written
// fixture). The recognition runner takes a *Fixture so the parse contract
// is part of the runner's public input shape.
func TestPhase2Fixtures_Parse_AndCarryNewFields(t *testing.T) {
	recognitionCases := []struct {
		path   string
		kind   string
		expect int // count of expected_concerns
	}{
		{"recognition_construction.json", "concern_recognition", 1},
		{"recognition_travel.json", "concern_recognition", 1},
		{"recognition_hiring.json", "concern_recognition", 1},
		{"recognition_negative_newsletters.json", "concern_recognition", 0},
		{"recognition_negative_mixed.json", "concern_recognition", 0},
		{"recognition_negative_oneoff.json", "concern_recognition", 0},
	}
	for _, c := range recognitionCases {
		t.Run(c.path, func(t *testing.T) {
			f, err := eval.LoadFixture(filepath.Join(".", c.path))
			require.NoError(t, err)
			require.Equal(t, c.kind, f.Kind)
			require.Len(t, f.ExpectedConcerns, c.expect)
		})
	}

	retrospectiveCases := []struct {
		path          string
		kind          string
		seedID        string
		expectMinTags int
	}{
		{"retrospective_construction.json", "retrospective", "c-construction", 8},
		{"retrospective_travel.json", "retrospective", "c-frankfurt", 4},
		{"retrospective_partial.json", "retrospective", "c-hiring", 4},
	}
	for _, c := range retrospectiveCases {
		t.Run(c.path, func(t *testing.T) {
			f, err := eval.LoadFixture(filepath.Join(".", c.path))
			require.NoError(t, err)
			require.Equal(t, c.kind, f.Kind)
			require.NotEmpty(t, f.SeedConcerns)
			require.Equal(t, c.seedID, f.SeedConcerns[0].ID)
			require.NotNil(t, f.ExpectedConcernTags)
			require.GreaterOrEqual(t, len(f.ExpectedConcernTags[c.seedID]), c.expectMinTags)
		})
	}

	// V2.5.0 Phase 3: surfacing fixtures.
	briefingCases := []struct {
		path      string
		seedCount int
		musts     int
	}{
		{"briefing_with_one_concern.json", 1, 1},
		{"briefing_with_two_concerns.json", 2, 1},
		{"briefing_zero_concerns.json", 0, 1},
		// V2.5.0 Phase 5: state-tagged variants for adaptive integration.
		{"pre_meeting_with_concerns.json", 1, 0},
		{"end_of_day_with_concerns.json", 1, 0},
	}
	for _, c := range briefingCases {
		t.Run(c.path, func(t *testing.T) {
			f, err := eval.LoadFixture(filepath.Join(".", c.path))
			require.NoError(t, err)
			require.Equal(t, "", f.Kind, "morning kind defaults to empty string")
			require.Len(t, f.SeedConcerns, c.seedCount)
			require.Len(t, f.MustCards, c.musts)
		})
	}

	reactiveQueryCases := []struct {
		path            string
		expectedConcern string
		mustNotMatch    bool
	}{
		{"reactive_construction_query.json", "c-construction", false},
		{"reactive_unrelated_query.json", "", true},
	}
	for _, c := range reactiveQueryCases {
		t.Run(c.path, func(t *testing.T) {
			f, err := eval.LoadFixture(filepath.Join(".", c.path))
			require.NoError(t, err)
			require.Equal(t, "reactive", f.Kind)
			require.Len(t, f.ConcernQueryTests, 1)
			test := f.ConcernQueryTests[0]
			require.Equal(t, c.expectedConcern, test.ExpectedConcernID)
			require.Equal(t, c.mustNotMatch, test.MustNotMatchConcern)
		})
	}
}
