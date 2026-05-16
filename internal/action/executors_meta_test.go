package action

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/store"
)

func TestDismissExec_PersistedCard(t *testing.T) {
	db := openTestDB(t)
	cards := &store.CardRepo{DB: db}
	require.NoError(t, cards.Upsert(t.Context(), []store.Card{
		{ID: "c1", Date: "2026-04-25", Source: "mail", Rel: "high", Title: "card one", CreatedAt: time.Now()},
	}))

	ex := &DismissExec{Cards: cards}
	require.Equal(t, Mode1Click, ex.Mode())

	got, err := cards.GetByID(t.Context(), "c1")
	require.NoError(t, err)
	res, err := ex.Execute(context.Background(), ExecCtx{Card: got})
	require.NoError(t, err)
	require.True(t, res.OK)
	require.True(t, res.OptimisticHide)

	rows, err := cards.ListByDate(t.Context(), "2026-04-25")
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestDismissExec_ReactiveCard_NoCardNoCrash(t *testing.T) {
	ex := &DismissExec{Cards: nil}
	res, err := ex.Execute(context.Background(), ExecCtx{Card: nil})
	require.NoError(t, err)
	require.True(t, res.OK)
}

func TestSnoozeExec_HidesForToday(t *testing.T) {
	db := openTestDB(t)
	cards := &store.CardRepo{DB: db}
	require.NoError(t, cards.Upsert(t.Context(), []store.Card{
		{ID: "c2", Date: "2026-04-25", Source: "mail", Rel: "high", Title: "card two", CreatedAt: time.Now()},
	}))

	ex := &SnoozeExec{Cards: cards}
	require.Equal(t, Mode1Click, ex.Mode())

	got, err := cards.GetByID(t.Context(), "c2")
	require.NoError(t, err)
	res, err := ex.Execute(context.Background(), ExecCtx{Card: got, Today: "2026-04-25"})
	require.NoError(t, err)
	require.True(t, res.OK)

	rows, err := cards.ListByDate(t.Context(), "2026-04-25")
	require.NoError(t, err)
	require.Empty(t, rows)
}

// TestSetReminderExec_InsertMode covers the "no task_uid in target"
// branch: a new task is inserted with fire_at set + source_card_id
// inherited from the card. V2.11 unification: this is what the V2.8.1
// "set reminder on a briefing card" flow looks like now.
func TestSetReminderExec_InsertMode(t *testing.T) {
	repo := newTaskRepo(t)
	ex := &SetReminderExec{Tasks: repo, TZ: func() *time.Location { return time.UTC }}
	require.Equal(t, Mode1Click, ex.Mode())

	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	res, err := ex.Execute(context.Background(), ExecCtx{
		Now:    now,
		TZ:     time.UTC,
		Card:   &store.Card{ID: "c1", Title: "Hydrate"},
		Target: map[string]any{"when": "+30m"},
	})
	require.NoError(t, err)
	require.True(t, res.OK)

	due, err := repo.DueBefore(context.Background(), now.Add(time.Hour), 10)
	require.NoError(t, err)
	require.Len(t, due, 1)
	require.Equal(t, "Hydrate", due[0].Title)
	require.Equal(t, "c1", due[0].SourceCardID)
	require.NotNil(t, due[0].FireAt)
	require.True(t, due[0].FireAt.Equal(now.Add(30*time.Minute)))
}

// TestSetReminderExec_UpdateMode covers the "task_uid in target"
// branch: an existing task's fire_at is updated, no new row inserted.
// This is the path /api/tasks/:uid/reminder takes after V2.11.
func TestSetReminderExec_UpdateMode(t *testing.T) {
	repo := newTaskRepo(t)
	uid := seedTask(t, repo, "existing")

	ex := &SetReminderExec{Tasks: repo, TZ: func() *time.Location { return time.UTC }}
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	res, err := ex.Execute(context.Background(), ExecCtx{
		Now: now,
		TZ:  time.UTC,
		Target: map[string]any{
			"when":     "+1h",
			"task_uid": uid,
		},
	})
	require.NoError(t, err)
	require.True(t, res.OK)

	// Existing row got fire_at; no new row created.
	got, err := repo.Get(context.Background(), uid)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.FireAt)
	require.True(t, got.FireAt.Equal(now.Add(time.Hour)))

	all, err := repo.List(context.Background(), store.TaskFilter{Status: "all"})
	require.NoError(t, err)
	require.Len(t, all, 1, "update mode must not create a duplicate row")
}

func TestSetReminderExec_UpdateMode_NotFound(t *testing.T) {
	repo := newTaskRepo(t)
	ex := &SetReminderExec{Tasks: repo, TZ: func() *time.Location { return time.UTC }}
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	res, err := ex.Execute(context.Background(), ExecCtx{
		Now:    now,
		Target: map[string]any{"when": "+1h", "task_uid": "missing"},
	})
	require.NoError(t, err)
	require.False(t, res.OK)
	require.Equal(t, "Task not found.", res.Toast)
}

func TestSetReminderExec_RequiresFutureTime(t *testing.T) {
	repo := newTaskRepo(t)
	ex := &SetReminderExec{Tasks: repo}
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	res, err := ex.Execute(context.Background(), ExecCtx{
		Now:    now,
		Target: map[string]any{"when": "+0m"},
	})
	require.NoError(t, err)
	require.False(t, res.OK)
}

func TestOpenURLExec_RequiresURL(t *testing.T) {
	ex := &OpenURLExec{}
	require.Equal(t, Mode1Click, ex.Mode())

	res, err := ex.Execute(context.Background(), ExecCtx{Target: nil})
	require.NoError(t, err)
	require.False(t, res.OK)
	require.NotEmpty(t, res.Toast)

	res, err = ex.Execute(context.Background(), ExecCtx{Target: map[string]any{"url": "https://example.com"}})
	require.NoError(t, err)
	require.True(t, res.OK)
}
