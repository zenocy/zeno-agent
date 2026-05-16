package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// makeConcern inserts a placeholder concern row and returns its ID. Tags
// need a real concern_id; FK constraints aren't enforced (gorm + sqlite
// doesn't add FKs by default), but the convention helps tests stay honest.
func makeConcern(t *testing.T, ctx context.Context, repo *ConcernRepo, name string) string {
	t.Helper()
	c := newConcern(t, name, ConcernStateActive, ConcernSourceUser)
	require.NoError(t, repo.Insert(ctx, c))
	return c.ID
}

func TestConcernObservationRepo_TagIdempotent(t *testing.T) {
	db := openTestDB(t)
	cRepo := &ConcernRepo{DB: db}
	repo := &ConcernObservationRepo{DB: db}
	ctx := context.Background()

	cID := makeConcern(t, ctx, cRepo, "Construction")
	evID := uuid.New().String()
	now := time.Now()

	tag := ConcernObservation{
		ConcernID: cID, EventID: evID,
		Source: ConcernTagSourceModel, Confidence: 0.8, TaggedAt: now,
	}
	require.NoError(t, repo.Tag(ctx, tag))

	// Second tag with same (concern_id, event_id) is a no-op via OnConflict
	// DoNothing.
	require.NoError(t, repo.Tag(ctx, tag))

	count, err := repo.CountByConcern(ctx, cID)
	require.NoError(t, err)
	require.EqualValues(t, 1, count, "re-tag must not multiply rows")
}

func TestConcernObservationRepo_TagBatch(t *testing.T) {
	db := openTestDB(t)
	cRepo := &ConcernRepo{DB: db}
	repo := &ConcernObservationRepo{DB: db}
	ctx := context.Background()

	cID := makeConcern(t, ctx, cRepo, "Travel")
	evIDs := []string{uuid.New().String(), uuid.New().String(), uuid.New().String()}
	tags := []ConcernObservation{
		{ConcernID: cID, EventID: evIDs[0], Source: ConcernTagSourceModel, Confidence: 0.7},
		{ConcernID: cID, EventID: evIDs[1], Source: ConcernTagSourceModel, Confidence: 0.7},
		{ConcernID: cID, EventID: evIDs[2], Source: ConcernTagSourceModel, Confidence: 0.7},
	}
	require.NoError(t, repo.TagBatch(ctx, tags))

	count, _ := repo.CountByConcern(ctx, cID)
	require.EqualValues(t, 3, count)

	// Re-batch the same set: idempotent.
	require.NoError(t, repo.TagBatch(ctx, tags))
	count, _ = repo.CountByConcern(ctx, cID)
	require.EqualValues(t, 3, count)
}

func TestConcernObservationRepo_UntagPreservesAuditDenylist(t *testing.T) {
	db := openTestDB(t)
	cRepo := &ConcernRepo{DB: db}
	repo := &ConcernObservationRepo{DB: db}
	ctx := context.Background()

	cID := makeConcern(t, ctx, cRepo, "Hiring")
	evID := uuid.New().String()
	require.NoError(t, repo.Tag(ctx, ConcernObservation{
		ConcernID: cID, EventID: evID, Source: ConcernTagSourceModel,
	}))

	require.NoError(t, repo.Untag(ctx, cID, evID))

	visible, err := repo.IsTagged(ctx, cID, evID)
	require.NoError(t, err)
	require.False(t, visible)

	// Audit lookup sees the soft-deleted row.
	allTime, err := repo.IsTaggedIncludingDeleted(ctx, cID, evID)
	require.NoError(t, err)
	require.True(t, allTime, "untag must preserve audit row for denylist semantics")
}

func TestConcernObservationRepo_ListByConcern_OrderAndLimit(t *testing.T) {
	db := openTestDB(t)
	cRepo := &ConcernRepo{DB: db}
	repo := &ConcernObservationRepo{DB: db}
	ctx := context.Background()

	cID := makeConcern(t, ctx, cRepo, "Construction")
	now := time.Now()
	for i := 0; i < 5; i++ {
		require.NoError(t, repo.Tag(ctx, ConcernObservation{
			ConcernID: cID, EventID: uuid.New().String(),
			Source:   ConcernTagSourceModel,
			TaggedAt: now.Add(time.Duration(i) * time.Minute),
		}))
	}

	rows, err := repo.ListByConcern(ctx, cID, 3)
	require.NoError(t, err)
	require.Len(t, rows, 3)
	for i := 0; i < len(rows)-1; i++ {
		require.True(t, rows[i].TaggedAt.After(rows[i+1].TaggedAt) || rows[i].TaggedAt.Equal(rows[i+1].TaggedAt),
			"rows must be tagged_at DESC")
	}
}

func TestConcernObservationRepo_ListByEvent(t *testing.T) {
	db := openTestDB(t)
	cRepo := &ConcernRepo{DB: db}
	repo := &ConcernObservationRepo{DB: db}
	ctx := context.Background()

	evID := uuid.New().String()
	cA := makeConcern(t, ctx, cRepo, "Concern A")
	cB := makeConcern(t, ctx, cRepo, "Concern B")
	require.NoError(t, repo.Tag(ctx, ConcernObservation{ConcernID: cA, EventID: evID, Source: ConcernTagSourceModel}))
	require.NoError(t, repo.Tag(ctx, ConcernObservation{ConcernID: cB, EventID: evID, Source: ConcernTagSourceUser}))

	rows, err := repo.ListByEvent(ctx, evID)
	require.NoError(t, err)
	require.Len(t, rows, 2, "an event can be tagged to multiple concerns")
}

func TestConcernObservationRepo_LatestTaggedAt(t *testing.T) {
	db := openTestDB(t)
	cRepo := &ConcernRepo{DB: db}
	repo := &ConcernObservationRepo{DB: db}
	ctx := context.Background()

	cID := makeConcern(t, ctx, cRepo, "Stale")
	t0 := time.Now().Add(-48 * time.Hour)
	t1 := time.Now()
	require.NoError(t, repo.Tag(ctx, ConcernObservation{ConcernID: cID, EventID: "ev1", Source: ConcernTagSourceModel, TaggedAt: t0}))
	require.NoError(t, repo.Tag(ctx, ConcernObservation{ConcernID: cID, EventID: "ev2", Source: ConcernTagSourceModel, TaggedAt: t1}))

	got, err := repo.LatestTaggedAt(ctx, cID)
	require.NoError(t, err)
	require.WithinDuration(t, t1, got, time.Second)
}

func TestConcernObservationRepo_ReassignToConcern_MergeShape(t *testing.T) {
	db := openTestDB(t)
	cRepo := &ConcernRepo{DB: db}
	repo := &ConcernObservationRepo{DB: db}
	ctx := context.Background()

	src := makeConcern(t, ctx, cRepo, "Frankfurt trip")
	tgt := makeConcern(t, ctx, cRepo, "Travel Q3")

	for _, ev := range []string{"a", "b", "c"} {
		require.NoError(t, repo.Tag(ctx, ConcernObservation{
			ConcernID: src, EventID: ev,
			Source:     ConcernTagSourceModel,
			Confidence: 0.6,
		}))
	}
	// Pre-existing collision: target already has "b".
	require.NoError(t, repo.Tag(ctx, ConcernObservation{
		ConcernID: tgt, EventID: "b",
		Source:     ConcernTagSourceUser,
		Confidence: 0.9,
	}))

	moved, err := repo.ReassignToConcern(ctx, src, tgt)
	require.NoError(t, err)
	require.EqualValues(t, 3, moved)

	// Source has no visible rows.
	srcCount, _ := repo.CountByConcern(ctx, src)
	require.EqualValues(t, 0, srcCount)

	// Target has all three events; "b" preserved its original (user) source.
	tgtRows, _ := repo.ListByConcern(ctx, tgt, 100)
	require.Len(t, tgtRows, 3)
	bRow := findEvent(t, tgtRows, "b")
	require.Equal(t, ConcernTagSourceUser, bRow.Source, "target's pre-existing user tag must NOT be overwritten by source's model tag")
}

func TestConcernObservationRepo_PartitionToConcerns_SplitShape(t *testing.T) {
	db := openTestDB(t)
	cRepo := &ConcernRepo{DB: db}
	repo := &ConcernObservationRepo{DB: db}
	ctx := context.Background()

	src := makeConcern(t, ctx, cRepo, "Mixed concern")
	tgtA := makeConcern(t, ctx, cRepo, "Split A")
	tgtB := makeConcern(t, ctx, cRepo, "Split B")

	for _, ev := range []string{"a1", "a2", "b1", "leftover"} {
		require.NoError(t, repo.Tag(ctx, ConcernObservation{
			ConcernID: src, EventID: ev, Source: ConcernTagSourceModel,
		}))
	}

	moved, err := repo.PartitionToConcerns(ctx, src, map[string]string{
		"a1": tgtA,
		"a2": tgtA,
		"b1": tgtB,
		// "leftover" not in the map → stays tagged to src
	})
	require.NoError(t, err)
	require.EqualValues(t, 2, moved[tgtA])
	require.EqualValues(t, 1, moved[tgtB])

	srcRows, _ := repo.ListByConcern(ctx, src, 100)
	require.Len(t, srcRows, 1)
	require.Equal(t, "leftover", srcRows[0].EventID)

	aRows, _ := repo.ListByConcern(ctx, tgtA, 100)
	require.Len(t, aRows, 2)
	bRows, _ := repo.ListByConcern(ctx, tgtB, 100)
	require.Len(t, bRows, 1)
}

func findEvent(t *testing.T, rows []ConcernObservation, evID string) ConcernObservation {
	t.Helper()
	for _, r := range rows {
		if r.EventID == evID {
			return r
		}
	}
	t.Fatalf("event %q not in rows", evID)
	return ConcernObservation{}
}
