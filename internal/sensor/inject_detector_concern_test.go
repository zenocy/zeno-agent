package sensor

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/projection"
	"github.com/zenocy/zeno-v2/internal/store"
)

// V2.5.0 Phase 5 — the concern-boost path admits unread threads whose
// subject substring-matches an active concern with confidence >=
// floor. Conservative + additive: it runs only when calendar-move and
// VIP email paths returned nil.

// 1. Concern boost admits a thread with subject substring matching a
// high-confidence concern, no VIP match needed.
func TestDetect_ConcernBoostAdmitsOnSubjectMatch(t *testing.T) {
	deps := InjectDetectorDeps{
		// No VIP cards — VIP email path returns nil so the boost runs.
		Cards: nil,
		Threads: []projection.Thread{{
			Subject:      "Re: Frankfurt agenda — flights confirmed",
			LastSender:   "lukas.heim@example.com",
			LastReceived: detectorTime.Add(-5 * time.Minute),
			UnreadCount:  1,
		}},
		Concerns: []projection.Concern{{
			ID:         "c-frankfurt",
			Name:       "Frankfurt",
			State:      store.ConcernStateActive,
			Confidence: 0.85,
		}},
		Now: fixedNow(detectorTime),
	}
	sig := Detect(deps, DefaultInjectConfig())
	require.NotNil(t, sig, "concern boost must admit on subject substring match")
	require.Equal(t, "email", sig.Kind)
	require.Contains(t, sig.Subject, "Frankfurt")
}

// 2. Concern below confidence floor does NOT admit.
func TestDetect_ConcernBoostRejectsBelowFloor(t *testing.T) {
	deps := InjectDetectorDeps{
		Threads: []projection.Thread{{
			Subject:      "Frankfurt — quick note",
			LastSender:   "stranger@example.com",
			LastReceived: detectorTime.Add(-5 * time.Minute),
			UnreadCount:  1,
		}},
		Concerns: []projection.Concern{{
			ID:         "c-frankfurt",
			Name:       "Frankfurt",
			State:      store.ConcernStateActive,
			Confidence: 0.5, // below default floor 0.7
		}},
		Now: fixedNow(detectorTime),
	}
	require.Nil(t, Detect(deps, DefaultInjectConfig()),
		"concern below confidence floor must not admit")
}

// 3. No concern subject substring match → boost does not fire.
func TestDetect_ConcernBoostRejectsNoMatch(t *testing.T) {
	deps := InjectDetectorDeps{
		Threads: []projection.Thread{{
			Subject:      "Quarterly review — pricing slide",
			LastSender:   "stranger@example.com",
			LastReceived: detectorTime.Add(-5 * time.Minute),
			UnreadCount:  1,
		}},
		Concerns: []projection.Concern{{
			ID:         "c-frankfurt",
			Name:       "Frankfurt",
			State:      store.ConcernStateActive,
			Confidence: 0.85,
		}},
		Now: fixedNow(detectorTime),
	}
	require.Nil(t, Detect(deps, DefaultInjectConfig()),
		"no subject substring match must not admit")
}

// 4. VIP path wins when both paths would admit.
func TestDetect_VIPPathTrumpsConcernBoost(t *testing.T) {
	deps := InjectDetectorDeps{
		Cards: []store.Card{vipCard("Frankfurt prep with Lukas Heim", "")},
		Threads: []projection.Thread{
			{
				Subject:      "Re: Frankfurt agenda — Lukas",
				LastSender:   "Lukas Heim",
				LastReceived: detectorTime.Add(-5 * time.Minute),
				UnreadCount:  1,
			},
		},
		Concerns: []projection.Concern{{
			ID:         "c-frankfurt",
			Name:       "Frankfurt",
			State:      store.ConcernStateActive,
			Confidence: 0.85,
		}},
		Now: fixedNow(detectorTime),
	}
	sig := Detect(deps, DefaultInjectConfig())
	require.NotNil(t, sig)
	// The VIP path also returns Kind: "email"; the assertion here is
	// that we get a signal at all and don't double-fire. The boost
	// path is a fallback, not a parallel.
	require.Equal(t, "email", sig.Kind)
}

// 5. Empty Concerns list → boost is a no-op.
func TestDetect_ConcernBoostNoOpWhenConcernsEmpty(t *testing.T) {
	deps := InjectDetectorDeps{
		Threads: []projection.Thread{{
			Subject:      "Re: Frankfurt agenda",
			LastSender:   "stranger@example.com",
			LastReceived: detectorTime.Add(-5 * time.Minute),
			UnreadCount:  1,
		}},
		Concerns: nil,
		Now:      fixedNow(detectorTime),
	}
	require.Nil(t, Detect(deps, DefaultInjectConfig()))
}

// 6. Debounce gate covers the boost path too.
func TestDetect_DebounceGateBlocksConcernBoost(t *testing.T) {
	deps := InjectDetectorDeps{
		Threads: []projection.Thread{{
			Subject:      "Re: Frankfurt agenda",
			LastSender:   "stranger@example.com",
			LastReceived: detectorTime.Add(-5 * time.Minute),
			UnreadCount:  1,
		}},
		Concerns: []projection.Concern{{
			ID:         "c-frankfurt",
			Name:       "Frankfurt",
			State:      store.ConcernStateActive,
			Confidence: 0.85,
		}},
		LastFire: detectorTime.Add(-5 * time.Minute), // within 30-min debounce
		Now:      fixedNow(detectorTime),
	}
	require.Nil(t, Detect(deps, DefaultInjectConfig()),
		"debounce must gate the boost path")
}

// 7. Match is case-insensitive.
func TestDetect_ConcernBoostCaseInsensitive(t *testing.T) {
	deps := InjectDetectorDeps{
		Threads: []projection.Thread{{
			Subject:      "RE: FRANKFURT AGENDA",
			LastSender:   "stranger@example.com",
			LastReceived: detectorTime.Add(-5 * time.Minute),
			UnreadCount:  1,
		}},
		Concerns: []projection.Concern{{
			ID:         "c-frankfurt",
			Name:       "Frankfurt",
			State:      store.ConcernStateActive,
			Confidence: 0.85,
		}},
		Now: fixedNow(detectorTime),
	}
	require.NotNil(t, Detect(deps, DefaultInjectConfig()))
}
