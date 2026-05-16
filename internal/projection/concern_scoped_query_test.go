package projection

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/log/logtest"
	"github.com/zenocy/zeno-v2/internal/store"
)

type evidenceRig struct {
	DB       *gorm.DB
	Concerns *store.ConcernRepo
	Tags     *store.ConcernObservationRepo
	Reader   *logtest.MemReader
}

func newEvidenceRig(t *testing.T) *evidenceRig {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	c := &store.ConcernRepo{DB: db, Table: "concerns"}
	o := &store.ConcernObservationRepo{DB: db, Table: "concern_observations"}
	require.NoError(t, c.Migrate())
	require.NoError(t, o.Migrate())
	return &evidenceRig{DB: db, Concerns: c, Tags: o, Reader: logtest.NewMemReader()}
}

func (r *evidenceRig) seedConcern(t *testing.T, id, name, state string) {
	t.Helper()
	now := time.Now()
	require.NoError(t, r.Concerns.Insert(context.Background(), store.Concern{
		ID: id, Name: name, NormName: store.NormalizeConcernName(name),
		Description: name + " — long-running situation",
		State:       state, Source: store.ConcernSourceUser,
		LastActiveAt: now, CreatedAt: now, UpdatedAt: now, Confidence: 1,
	}))
}

func (r *evidenceRig) seedMail(t *testing.T, id string, ts time.Time, subject, from string) {
	t.Helper()
	r.Reader.AppendEvent(log.Event{
		ID: id, TS: ts.UTC(), Kind: log.KindMailReceived, Source: "imap",
		Payload: jsonMust(map[string]any{
			"subject": subject, "from": from, "body_preview": "preview text",
		}),
	})
}

func (r *evidenceRig) tag(t *testing.T, concernID, eventID string, taggedAt time.Time) {
	t.Helper()
	require.NoError(t, r.Tags.Tag(context.Background(), store.ConcernObservation{
		ConcernID: concernID, EventID: eventID,
		Source: store.ConcernTagSourceModel, TaggedAt: taggedAt,
	}))
}

func jsonMust(v any) []byte {
	logtest.MakeEvent("k", "src", time.Now(), v) // exercise the marshaler — panics on failure
	// Re-use logtest's JSON marshal path for symmetry; fall back to direct encode below.
	// (logtest.MakeEvent returns the event with the marshalled payload; we just want raw bytes.)
	return logtest.MakeEvent("k", "src", time.Now(), v).Payload
}

// TestQueryConcernEvidence_HappyPath returns the most-recent N tagged
// observations, oldest dropped, in descending date order. This is the
// shape the ReadConcernEvidenceTool's prose generator depends on.
func TestQueryConcernEvidence_HappyPath_ReturnsTaggedObservations(t *testing.T) {
	rig := newEvidenceRig(t)
	rig.seedConcern(t, "c-1", "Construction", store.ConcernStateActive)
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	for i, id := range []string{"o1", "o2", "o3", "o4", "o5", "o6", "o7", "o8"} {
		ts := now.Add(-time.Duration(i+1) * 24 * time.Hour)
		rig.seedMail(t, id, ts, "subject "+id, "sender")
		rig.tag(t, "c-1", id, ts)
	}

	got, err := QueryConcernEvidence(context.Background(), QueryConcernEvidenceDeps{
		Concerns: rig.Concerns, Tags: rig.Tags, Reader: rig.Reader,
	}, "c-1", QueryConcernEvidenceOpts{Now: now})
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "Construction", got.ConcernName)
	require.Len(t, got.Observations, 5, "default cap is 5")

	// Descending date order — newest first.
	for i := 1; i < len(got.Observations); i++ {
		require.False(t,
			got.Observations[i-1].Date.Before(got.Observations[i].Date),
			"observations must be in descending date order")
	}
}

// TestQueryConcernEvidence_FiltersTerminalConcerns guarantees a merged
// or ended concern returns nil cleanly so a stale tombstone never
// shows up in a card.
func TestQueryConcernEvidence_FiltersTerminalConcerns(t *testing.T) {
	rig := newEvidenceRig(t)
	rig.seedConcern(t, "c-1", "Old", store.ConcernStateActive)
	rig.tag(t, "c-1", "o1", time.Now())
	rig.seedMail(t, "o1", time.Now(), "x", "y")
	require.NoError(t, rig.Concerns.Transition(context.Background(), "c-1", store.ConcernStateEnded))

	got, err := QueryConcernEvidence(context.Background(), QueryConcernEvidenceDeps{
		Concerns: rig.Concerns, Tags: rig.Tags, Reader: rig.Reader,
	}, "c-1", QueryConcernEvidenceOpts{})
	require.NoError(t, err)
	require.Nil(t, got)
}

// TestQueryConcernEvidence_HonorsLookback drops events older than the
// configured window even if their tags exist. Cuts noise from
// retrospective-tagged ancient mail.
func TestQueryConcernEvidence_HonorsLookback(t *testing.T) {
	rig := newEvidenceRig(t)
	rig.seedConcern(t, "c-1", "X", store.ConcernStateActive)
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	// Recent (within 7 days)
	rig.seedMail(t, "recent", now.Add(-3*24*time.Hour), "recent", "x")
	rig.tag(t, "c-1", "recent", now.Add(-3*24*time.Hour))
	// Old (60 days)
	rig.seedMail(t, "old", now.Add(-60*24*time.Hour), "old", "x")
	rig.tag(t, "c-1", "old", now.Add(-60*24*time.Hour))

	got, err := QueryConcernEvidence(context.Background(), QueryConcernEvidenceDeps{
		Concerns: rig.Concerns, Tags: rig.Tags, Reader: rig.Reader,
	}, "c-1", QueryConcernEvidenceOpts{Now: now, Lookback: 7 * 24 * time.Hour})
	require.NoError(t, err)
	require.Len(t, got.Observations, 1)
	require.Equal(t, "recent", got.Observations[0].EventID)
}

// TestQueryConcernEvidence_EmptyConcernReturnsEmptyEvidence confirms a
// concern with zero tagged observations gets an empty (but non-nil)
// Evidence — the tool's prose generator can render "no recent activity"
// rather than crashing on a nil pointer.
func TestQueryConcernEvidence_EmptyConcernReturnsEmptyEvidence(t *testing.T) {
	rig := newEvidenceRig(t)
	rig.seedConcern(t, "c-empty", "Empty concern", store.ConcernStateActive)

	got, err := QueryConcernEvidence(context.Background(), QueryConcernEvidenceDeps{
		Concerns: rig.Concerns, Tags: rig.Tags, Reader: rig.Reader,
	}, "c-empty", QueryConcernEvidenceOpts{})
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got.Observations)
	require.Equal(t, "Empty concern", got.ConcernName)
}

// TestQueryConcernEvidence_MissingConcernReturnsNil pins the unknown-id
// path: returns nil, nil rather than an error so the tool surfaces a
// clean "no such concern" to the loop.
func TestQueryConcernEvidence_MissingConcernReturnsNil(t *testing.T) {
	rig := newEvidenceRig(t)
	got, err := QueryConcernEvidence(context.Background(), QueryConcernEvidenceDeps{
		Concerns: rig.Concerns, Tags: rig.Tags, Reader: rig.Reader,
	}, "does-not-exist", QueryConcernEvidenceOpts{})
	require.NoError(t, err)
	require.Nil(t, got)
}

// TestQueryConcernEvidence_NilDepsReturnsNilCleanly preserves the
// optional-projection contract: callers without concerns wiring
// (eval-only paths) can pass empty deps without panicking.
func TestQueryConcernEvidence_NilDepsReturnsNilCleanly(t *testing.T) {
	got, err := QueryConcernEvidence(context.Background(), QueryConcernEvidenceDeps{}, "c-1", QueryConcernEvidenceOpts{})
	require.NoError(t, err)
	require.Nil(t, got)
}

// TestQueryConcernEvidence_MaxObservationsCap pins the configurable
// upper bound. Phase 5 may surface more evidence on a dedicated
// concern-detail UI, but the prompt-bound default stays small.
func TestQueryConcernEvidence_MaxObservationsCap(t *testing.T) {
	rig := newEvidenceRig(t)
	rig.seedConcern(t, "c-1", "X", store.ConcernStateActive)
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	for i := range 6 {
		id := "o" + time.Duration(i).String()
		ts := now.Add(-time.Duration(i+1) * time.Hour)
		rig.seedMail(t, id, ts, "subject", "x")
		rig.tag(t, "c-1", id, ts)
	}

	got, err := QueryConcernEvidence(context.Background(), QueryConcernEvidenceDeps{
		Concerns: rig.Concerns, Tags: rig.Tags, Reader: rig.Reader,
	}, "c-1", QueryConcernEvidenceOpts{Now: now, MaxObservations: 2})
	require.NoError(t, err)
	require.Len(t, got.Observations, 2)
}

// TestQueryConcernEvidence_MailFieldsDecoded pins the prose-shaping
// payload decode. Subject + sender + body_preview must populate so
// the tool's prose looks like a real card sub.
func TestQueryConcernEvidence_MailFieldsDecoded(t *testing.T) {
	rig := newEvidenceRig(t)
	rig.seedConcern(t, "c-1", "X", store.ConcernStateActive)
	now := time.Now()
	rig.seedMail(t, "m1", now, "Drywall slipping", "Hector Reyes")
	rig.tag(t, "c-1", "m1", now)

	got, err := QueryConcernEvidence(context.Background(), QueryConcernEvidenceDeps{
		Concerns: rig.Concerns, Tags: rig.Tags, Reader: rig.Reader,
	}, "c-1", QueryConcernEvidenceOpts{})
	require.NoError(t, err)
	require.Len(t, got.Observations, 1)
	o := got.Observations[0]
	require.Equal(t, "Drywall slipping", o.Title)
	require.Equal(t, "Hector Reyes", o.Sender)
	require.Equal(t, "preview text", o.Preview)
}
