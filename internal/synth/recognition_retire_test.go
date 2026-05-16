package synth

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/eventbus"
	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/store"
)

// V2.5.0 Phase 5 — retirement survey runs after recognition completes
// and emits one audit + bus event per active concern crossing the
// auto_retire_days threshold. Idempotency is enforced via the audit
// log: a concern with a prior retirement-proposed event within the
// window is skipped.

func seedActiveConcernWithLastActive(t *testing.T, repo *store.ConcernRepo, name string, lastActive time.Time) string {
	t.Helper()
	id := uuid.New().String()
	now := time.Now()
	c := store.Concern{
		ID:           id,
		Name:         name,
		NormName:     store.NormalizeConcernName(name),
		Description:  "test",
		State:        store.ConcernStateActive,
		Source:       store.ConcernSourceUser,
		Confidence:   1.0,
		LastActiveAt: lastActive,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	require.NoError(t, repo.Insert(context.Background(), c))
	return id
}

func TestSurveyForRetirement_EmitsAuditAndBusEvent(t *testing.T) {
	rig := newRecognitionRig(t, proposalsJSON())
	ctx := context.Background()
	now := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)

	// Concern inactive 95 days — past the 90d threshold.
	id := seedActiveConcernWithLastActive(t, rig.Concerns, "Construction at the house",
		now.Add(-95*24*time.Hour))

	sub := rig.Bus.Subscribe()
	defer rig.Bus.Unsubscribe(sub)

	surveyForRetirement(ctx, RecognizeDeps{
		Concerns: rig.Concerns,
		Reader:   rig.Reader,
		Bus:      rig.Bus,
		EventLog: rig.Reader,
		Logger:   rig.Logger,
	}, now, 90, rig.Logger)

	// Bus delivered the event.
	deadline := time.After(time.Second)
	got := false
	for !got {
		select {
		case ev := <-sub:
			if e, ok := ev.(eventbus.ConcernRetirementProposedEvent); ok {
				require.Equal(t, id, e.ConcernID)
				require.GreaterOrEqual(t, e.DaysInactive, 90)
				got = true
			}
		case <-deadline:
			t.Fatalf("timed out waiting for retirement bus event")
		}
	}

	// Audit landed.
	count := 0
	for _, e := range rig.Reader.Events() {
		if e.Kind == log.KindConcernRetirementProposed {
			count++
		}
	}
	require.Equal(t, 1, count, "exactly one retirement audit event for one concern")
}

func TestSurveyForRetirement_IdempotentOnReRun(t *testing.T) {
	rig := newRecognitionRig(t, proposalsJSON())
	ctx := context.Background()
	now := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)

	seedActiveConcernWithLastActive(t, rig.Concerns, "Quiet thread",
		now.Add(-100*24*time.Hour))

	deps := RecognizeDeps{
		Concerns: rig.Concerns,
		Reader:   rig.Reader,
		Bus:      rig.Bus,
		EventLog: rig.Reader,
		Logger:   rig.Logger,
	}

	// First pass emits.
	surveyForRetirement(ctx, deps, now, 90, rig.Logger)
	// Second pass on the same day should be a no-op (already proposed).
	surveyForRetirement(ctx, deps, now, 90, rig.Logger)

	count := 0
	for _, e := range rig.Reader.Events() {
		if e.Kind == log.KindConcernRetirementProposed {
			count++
		}
	}
	require.Equal(t, 1, count, "idempotent: second pass must not re-emit")
}

func TestSurveyForRetirement_NoActiveInactive_NoOp(t *testing.T) {
	rig := newRecognitionRig(t, proposalsJSON())
	ctx := context.Background()
	now := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)

	// Concern is recent — well within threshold.
	seedActiveConcernWithLastActive(t, rig.Concerns, "Recently active",
		now.Add(-10*24*time.Hour))

	surveyForRetirement(ctx, RecognizeDeps{
		Concerns: rig.Concerns,
		Reader:   rig.Reader,
		Bus:      rig.Bus,
		EventLog: rig.Reader,
		Logger:   rig.Logger,
	}, now, 90, rig.Logger)

	for _, e := range rig.Reader.Events() {
		require.NotEqual(t, log.KindConcernRetirementProposed, e.Kind,
			"recently active concern must not be proposed")
	}
}

func TestSurveyForRetirement_PausedSkipped(t *testing.T) {
	rig := newRecognitionRig(t, proposalsJSON())
	ctx := context.Background()
	now := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)

	// A paused concern is not "active" — ListInactiveSince filters by
	// state=active, so paused never enters the survey set even when
	// last_active_at is ancient.
	id := seedActiveConcernWithLastActive(t, rig.Concerns, "Once active, now paused",
		now.Add(-200*24*time.Hour))
	require.NoError(t, rig.Concerns.Transition(ctx, id, store.ConcernStatePaused))

	surveyForRetirement(ctx, RecognizeDeps{
		Concerns: rig.Concerns,
		Reader:   rig.Reader,
		Bus:      rig.Bus,
		EventLog: rig.Reader,
		Logger:   rig.Logger,
	}, now, 90, rig.Logger)

	for _, e := range rig.Reader.Events() {
		require.NotEqual(t, log.KindConcernRetirementProposed, e.Kind)
	}
}
