package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func newConcern(t *testing.T, name, state, source string) Concern {
	t.Helper()
	now := time.Now()
	return Concern{
		ID:           uuid.New().String(),
		Name:         name,
		NormName:     NormalizeConcernName(name),
		Description:  name + " — placeholder description",
		State:        state,
		Source:       source,
		Confidence:   0.8,
		LastActiveAt: now,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
}

func TestConcernRepo_InsertGetByID(t *testing.T) {
	db := openTestDB(t)
	repo := &ConcernRepo{DB: db}
	ctx := context.Background()

	c := newConcern(t, "Construction at the house", ConcernStateProposed, ConcernSourceModel)
	require.NoError(t, repo.Insert(ctx, c))

	got, err := repo.GetByID(ctx, c.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, c.ID, got.ID)
	require.Equal(t, ConcernStateProposed, got.State)
	require.Equal(t, "construction at the house", got.NormName)
}

func TestConcernRepo_GetByNormName_DenylistAfterDismiss(t *testing.T) {
	db := openTestDB(t)
	repo := &ConcernRepo{DB: db}
	ctx := context.Background()

	c := newConcern(t, "Frankfurt trip", ConcernStateProposed, ConcernSourceModel)
	require.NoError(t, repo.Insert(ctx, c))

	// Dismiss = soft-delete.
	require.NoError(t, repo.SoftDelete(ctx, c.ID))

	// Visible lookup returns nothing — recognition that ignores the denylist
	// would re-propose, which is the bug we want to prevent.
	visible, err := repo.GetByNormName(ctx, "frankfurt trip", false)
	require.NoError(t, err)
	require.Nil(t, visible)

	// Denylist-aware lookup returns the soft-deleted row.
	denylisted, err := repo.GetByNormName(ctx, "frankfurt trip", true)
	require.NoError(t, err)
	require.NotNil(t, denylisted)
	require.True(t, denylisted.DeletedAt.Valid, "soft-deleted row must surface DeletedAt")
}

func TestConcernRepo_Transition_ValidPath(t *testing.T) {
	db := openTestDB(t)
	repo := &ConcernRepo{DB: db}
	ctx := context.Background()

	c := newConcern(t, "Hiring search", ConcernStateProposed, ConcernSourceModel)
	require.NoError(t, repo.Insert(ctx, c))

	// proposed → active
	require.NoError(t, repo.Transition(ctx, c.ID, ConcernStateActive))
	got, _ := repo.GetByID(ctx, c.ID)
	require.Equal(t, ConcernStateActive, got.State)
	require.WithinDuration(t, time.Now(), got.LastActiveAt, time.Second)

	// active → paused
	require.NoError(t, repo.Transition(ctx, c.ID, ConcernStatePaused))
	got, _ = repo.GetByID(ctx, c.ID)
	require.Equal(t, ConcernStatePaused, got.State)

	// paused → active (resume bumps LastActiveAt)
	earlier := got.LastActiveAt
	time.Sleep(2 * time.Millisecond)
	require.NoError(t, repo.Transition(ctx, c.ID, ConcernStateActive))
	got, _ = repo.GetByID(ctx, c.ID)
	require.Equal(t, ConcernStateActive, got.State)
	require.True(t, got.LastActiveAt.After(earlier), "resume must bump last_active_at")

	// active → ended (sets EndedAt)
	require.NoError(t, repo.Transition(ctx, c.ID, ConcernStateEnded))
	got, _ = repo.GetByID(ctx, c.ID)
	require.Equal(t, ConcernStateEnded, got.State)
	require.NotNil(t, got.EndedAt)
}

func TestConcernRepo_Transition_RejectsInvalid(t *testing.T) {
	db := openTestDB(t)
	repo := &ConcernRepo{DB: db}
	ctx := context.Background()

	c := newConcern(t, "Travel plans", ConcernStateProposed, ConcernSourceModel)
	require.NoError(t, repo.Insert(ctx, c))

	// proposed → paused: not in the matrix.
	err := repo.Transition(ctx, c.ID, ConcernStatePaused)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidConcernTransition))

	// proposed → merged: not in the matrix (must use Merge for atomicity).
	err = repo.Transition(ctx, c.ID, ConcernStateMerged)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidConcernTransition))

	// Move to ended terminal.
	require.NoError(t, repo.Transition(ctx, c.ID, ConcernStateEnded))

	// ended → active is not a regular transition; must use Reopen.
	err = repo.Transition(ctx, c.ID, ConcernStateActive)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidConcernTransition))
}

func TestConcernRepo_Transition_UnknownTarget(t *testing.T) {
	db := openTestDB(t)
	repo := &ConcernRepo{DB: db}
	ctx := context.Background()

	c := newConcern(t, "x", ConcernStateActive, ConcernSourceUser)
	require.NoError(t, repo.Insert(ctx, c))

	err := repo.Transition(ctx, c.ID, "in_progress")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidConcernTransition))
}

func TestConcernRepo_Reopen_RequiresEnded(t *testing.T) {
	db := openTestDB(t)
	repo := &ConcernRepo{DB: db}
	ctx := context.Background()

	c := newConcern(t, "Re-open me", ConcernStateActive, ConcernSourceUser)
	require.NoError(t, repo.Insert(ctx, c))

	// Reopen on a non-ended row is a no-op (RowsAffected=0 → ErrConcernNotFound).
	err := repo.Reopen(ctx, c.ID)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrConcernNotFound))

	// End it, then reopen.
	require.NoError(t, repo.Transition(ctx, c.ID, ConcernStateEnded))
	require.NoError(t, repo.Reopen(ctx, c.ID))
	got, _ := repo.GetByID(ctx, c.ID)
	require.Equal(t, ConcernStateActive, got.State)
	require.Nil(t, got.EndedAt, "reopen must clear ended_at")
}

func TestConcernRepo_Merge_AtomicMoveAndPointer(t *testing.T) {
	db := openTestDB(t)
	repo := &ConcernRepo{DB: db}
	ctx := context.Background()

	src := newConcern(t, "Frankfurt trip", ConcernStateActive, ConcernSourceUser)
	tgt := newConcern(t, "Travel — Q3 2026", ConcernStateActive, ConcernSourceUser)
	require.NoError(t, repo.Insert(ctx, src))
	require.NoError(t, repo.Insert(ctx, tgt))

	require.NoError(t, repo.Merge(ctx, src.ID, tgt.ID))

	gotSrc, _ := repo.GetByID(ctx, src.ID)
	require.Equal(t, ConcernStateMerged, gotSrc.State)
	require.NotNil(t, gotSrc.MergedIntoID)
	require.Equal(t, tgt.ID, *gotSrc.MergedIntoID)
	require.NotNil(t, gotSrc.EndedAt)

	gotTgt, _ := repo.GetByID(ctx, tgt.ID)
	require.WithinDuration(t, time.Now(), gotTgt.LastActiveAt, time.Second, "target last_active_at must bump on merge")
}

func TestConcernRepo_Merge_RejectsSelfAndTerminalTarget(t *testing.T) {
	db := openTestDB(t)
	repo := &ConcernRepo{DB: db}
	ctx := context.Background()

	a := newConcern(t, "A", ConcernStateActive, ConcernSourceUser)
	b := newConcern(t, "B", ConcernStateActive, ConcernSourceUser)
	require.NoError(t, repo.Insert(ctx, a))
	require.NoError(t, repo.Insert(ctx, b))

	require.Error(t, repo.Merge(ctx, a.ID, a.ID), "self-merge rejected")

	// End b, attempt merge into ended target.
	require.NoError(t, repo.Transition(ctx, b.ID, ConcernStateEnded))
	err := repo.Merge(ctx, a.ID, b.ID)
	require.Error(t, err, "merge into ended target rejected")

	// merge into nonexistent target rejected
	err = repo.Merge(ctx, a.ID, "nonexistent")
	require.Error(t, err)
}

func TestConcernRepo_ListByState(t *testing.T) {
	db := openTestDB(t)
	repo := &ConcernRepo{DB: db}
	ctx := context.Background()

	now := time.Now()
	rows := []Concern{
		newConcern(t, "First", ConcernStateActive, ConcernSourceUser),
		newConcern(t, "Second", ConcernStateActive, ConcernSourceUser),
		newConcern(t, "Third", ConcernStateProposed, ConcernSourceModel),
	}
	rows[0].LastActiveAt = now.Add(-2 * time.Hour)
	rows[1].LastActiveAt = now
	for _, r := range rows {
		require.NoError(t, repo.Insert(ctx, r))
	}

	active, err := repo.ListActive(ctx)
	require.NoError(t, err)
	require.Len(t, active, 2)
	require.Equal(t, "Second", active[0].Name, "list ordered by last_active_at DESC")
	require.Equal(t, "First", active[1].Name)

	proposed, err := repo.ListByState(ctx, ConcernStateProposed)
	require.NoError(t, err)
	require.Len(t, proposed, 1)
}

func TestConcernRepo_ListInactiveSince(t *testing.T) {
	db := openTestDB(t)
	repo := &ConcernRepo{DB: db}
	ctx := context.Background()

	now := time.Now()
	stale := newConcern(t, "Stale", ConcernStateActive, ConcernSourceUser)
	stale.LastActiveAt = now.Add(-100 * 24 * time.Hour)
	fresh := newConcern(t, "Fresh", ConcernStateActive, ConcernSourceUser)
	fresh.LastActiveAt = now
	paused := newConcern(t, "Paused stale", ConcernStatePaused, ConcernSourceUser)
	paused.LastActiveAt = now.Add(-200 * 24 * time.Hour)

	require.NoError(t, repo.Insert(ctx, stale))
	require.NoError(t, repo.Insert(ctx, fresh))
	require.NoError(t, repo.Insert(ctx, paused))

	// 90 days ago threshold — only "Stale" is active and older.
	rows, err := repo.ListInactiveSince(ctx, now.Add(-90*24*time.Hour))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "Stale", rows[0].Name, "paused concerns are not auto-retire candidates")
}

func TestConcernRepo_UpdateName_RederivesNormName(t *testing.T) {
	db := openTestDB(t)
	repo := &ConcernRepo{DB: db}
	ctx := context.Background()

	c := newConcern(t, "Old name", ConcernStateActive, ConcernSourceUser)
	require.NoError(t, repo.Insert(ctx, c))

	require.NoError(t, repo.UpdateName(ctx, c.ID, "  New  Name  "))
	got, _ := repo.GetByID(ctx, c.ID)
	require.Equal(t, "  New  Name  ", got.Name, "name preserves user input verbatim")
	require.Equal(t, "new name", got.NormName, "norm_name re-derived")
}

func TestConcernRepo_BumpLastActive(t *testing.T) {
	db := openTestDB(t)
	repo := &ConcernRepo{DB: db}
	ctx := context.Background()

	c := newConcern(t, "Bumpable", ConcernStateActive, ConcernSourceUser)
	c.LastActiveAt = time.Now().Add(-24 * time.Hour)
	require.NoError(t, repo.Insert(ctx, c))

	target := time.Date(2026, 5, 4, 9, 30, 0, 0, time.UTC)
	require.NoError(t, repo.BumpLastActive(ctx, c.ID, target))

	got, _ := repo.GetByID(ctx, c.ID)
	require.WithinDuration(t, target, got.LastActiveAt, time.Second)
}

func TestConcernRepo_UpsertReplay_BypassesGuards(t *testing.T) {
	db := openTestDB(t)
	repo := &ConcernRepo{DB: db, Table: "concerns"}
	ctx := context.Background()

	// UpsertReplay is the replay tool path: it overwrites by ID without
	// running through Transition's lifecycle matrix. Two passes with the
	// same ID land on the second value.
	id := uuid.New().String()
	now := time.Now()
	first := Concern{
		ID: id, Name: "Replay 1", NormName: "replay 1",
		Description: "first", State: ConcernStateProposed,
		Source: ConcernSourceModel, Confidence: 0.4,
		LastActiveAt: now, CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, repo.UpsertReplay(ctx, []Concern{first}))

	got, _ := repo.GetByID(ctx, id)
	require.Equal(t, ConcernStateProposed, got.State)

	second := first
	second.State = ConcernStateMerged
	second.MergedIntoID = ptrString("anywhere-else")
	second.Description = "second"
	require.NoError(t, repo.UpsertReplay(ctx, []Concern{second}))

	got, _ = repo.GetByID(ctx, id)
	require.Equal(t, ConcernStateMerged, got.State, "UpsertReplay must bypass Transition guards")
	require.Equal(t, "second", got.Description)
	require.NotNil(t, got.MergedIntoID)
	require.Equal(t, "anywhere-else", *got.MergedIntoID)
}

func TestConcernRepo_Reopen_RejectedAfterMerge(t *testing.T) {
	db := openTestDB(t)
	repo := &ConcernRepo{DB: db}
	ctx := context.Background()

	src := newConcern(t, "Will be merged", ConcernStateActive, ConcernSourceUser)
	tgt := newConcern(t, "Target", ConcernStateActive, ConcernSourceUser)
	require.NoError(t, repo.Insert(ctx, src))
	require.NoError(t, repo.Insert(ctx, tgt))
	require.NoError(t, repo.Merge(ctx, src.ID, tgt.ID))

	// Reopen requires state=ended; merged is a different terminal so the
	// row stays put. This guarantees a merge tombstone can never be
	// resurrected as if it had been ended.
	err := repo.Reopen(ctx, src.ID)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrConcernNotFound))
}

// ptrString is a test-local helper for setting *string fields on
// UpsertReplay rows.
func ptrString(s string) *string { return &s }

func TestNormalizeConcernName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Construction", "construction"},
		{"  Frankfurt   Trip  ", "frankfurt trip"},
		{"hiring SEARCH for engineering lead", "hiring search for engineering lead"},
		{"", ""},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, NormalizeConcernName(tc.in), "input=%q", tc.in)
	}
}
