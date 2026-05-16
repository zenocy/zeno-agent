package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func openTaskDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_journal_mode=WAL&_foreign_keys=on"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, (&TaskRepo{DB: db}).Migrate())
	return db
}

func tagsJSON(t *testing.T, tags []string) datatypes.JSON {
	t.Helper()
	b, err := json.Marshal(tags)
	require.NoError(t, err)
	return datatypes.JSON(b)
}

// TestTaskRepo_InsertAndGet round-trips every column we care about. If a
// future migration drops or re-types one of these fields, this test will
// catch it before the DB-backing executors do.
func TestTaskRepo_InsertAndGet(t *testing.T) {
	db := openTaskDB(t)
	repo := &TaskRepo{DB: db}
	ctx := context.Background()

	fireAt := time.Date(2026, 5, 12, 9, 30, 0, 0, time.UTC)
	id := uuid.NewString()
	require.NoError(t, repo.Insert(ctx, Task{
		ID:           id,
		Title:        "buy milk",
		Body:         "after work",
		DueDate:      "2026-05-12",
		Priority:     TaskPriorityHigh,
		Tags:         tagsJSON(t, []string{"errands", "home"}),
		FireAt:       &fireAt,
		SourceCardID: "card-123",
	}))

	got, err := repo.Get(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "buy milk", got.Title)
	require.Equal(t, "after work", got.Body)
	require.Equal(t, "2026-05-12", got.DueDate)
	require.Equal(t, TaskPriorityHigh, got.Priority)
	require.False(t, got.Completed)
	require.NotNil(t, got.FireAt)
	require.True(t, got.FireAt.Equal(fireAt))
	require.Equal(t, "card-123", got.SourceCardID)
	require.Nil(t, got.FiredAt)
}

// TestTaskRepo_PriorityCoercion confirms the default-to-med behavior so
// callers can pass an empty Priority on insert and not panic the schema.
func TestTaskRepo_PriorityCoercion(t *testing.T) {
	db := openTaskDB(t)
	repo := &TaskRepo{DB: db}
	ctx := context.Background()

	id := uuid.NewString()
	require.NoError(t, repo.Insert(ctx, Task{ID: id, Title: "x"}))
	got, err := repo.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, TaskPriorityMed, got.Priority)
}

// TestTaskRepo_List_Filters seeds a deterministic fixture and asserts
// each filter returns exactly the right slice. Pinning the filter
// behavior here means the projection layer can stay thin.
func TestTaskRepo_List_Filters(t *testing.T) {
	db := openTaskDB(t)
	repo := &TaskRepo{DB: db}
	ctx := context.Background()

	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	today := now.Format("2006-01-02")
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")
	tomorrow := now.AddDate(0, 0, 1).Format("2006-01-02")

	// Five rows: open today, open tomorrow, overdue, completed today, completed yesterday.
	openToday := uuid.NewString()
	openTomorrow := uuid.NewString()
	overdue := uuid.NewString()
	doneToday := uuid.NewString()
	doneYesterday := uuid.NewString()
	withAlarm := uuid.NewString()

	dt := now
	dy := now.AddDate(0, 0, -1)

	require.NoError(t, repo.Insert(ctx, Task{ID: openToday, Title: "open today", DueDate: today}))
	require.NoError(t, repo.Insert(ctx, Task{ID: openTomorrow, Title: "open tomorrow", DueDate: tomorrow}))
	require.NoError(t, repo.Insert(ctx, Task{ID: overdue, Title: "overdue", DueDate: yesterday}))
	require.NoError(t, repo.Insert(ctx, Task{ID: doneToday, Title: "done today", DueDate: today, Completed: true, CompletedAt: &dt}))
	require.NoError(t, repo.Insert(ctx, Task{ID: doneYesterday, Title: "done yesterday", DueDate: yesterday, Completed: true, CompletedAt: &dy}))
	fireAt := now.Add(time.Hour)
	require.NoError(t, repo.Insert(ctx, Task{ID: withAlarm, Title: "alarm", FireAt: &fireAt}))

	check := func(t *testing.T, status string, want []string) {
		t.Helper()
		got, err := repo.List(ctx, TaskFilter{Status: status, Today: today})
		require.NoError(t, err)
		ids := make([]string, len(got))
		for i, x := range got {
			ids[i] = x.Title
		}
		require.ElementsMatch(t, want, ids, "filter %q", status)
	}

	check(t, "open", []string{"open today", "open tomorrow", "overdue", "alarm"})
	check(t, "due_today", []string{"open today"})
	check(t, "overdue", []string{"overdue"})
	check(t, "completed_today", []string{"done today"})
	check(t, "has_alarm", []string{"alarm"})
	check(t, "all", []string{"open today", "open tomorrow", "overdue", "done today", "done yesterday", "alarm"})

	_, err := repo.List(ctx, TaskFilter{Status: "bogus"})
	require.Error(t, err)
}

// TestTaskRepo_List_Limit pins the limit cap. Important because the
// LLM tool exposes a `limit` parameter the projection caps at 20.
func TestTaskRepo_List_Limit(t *testing.T) {
	db := openTaskDB(t)
	repo := &TaskRepo{DB: db}
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		require.NoError(t, repo.Insert(ctx, Task{ID: uuid.NewString(), Title: "t"}))
	}
	got, err := repo.List(ctx, TaskFilter{Limit: 3})
	require.NoError(t, err)
	require.Len(t, got, 3)
}

// TestTaskRepo_List_TagFilter exercises the tag filter (case-insensitive,
// LIKE-on-JSON). Tag-filter survives the projection layer; assert here.
func TestTaskRepo_List_TagFilter(t *testing.T) {
	db := openTaskDB(t)
	repo := &TaskRepo{DB: db}
	ctx := context.Background()

	require.NoError(t, repo.Insert(ctx, Task{ID: uuid.NewString(), Title: "a", Tags: tagsJSON(t, []string{"home"})}))
	require.NoError(t, repo.Insert(ctx, Task{ID: uuid.NewString(), Title: "b", Tags: tagsJSON(t, []string{"work"})}))

	got, err := repo.List(ctx, TaskFilter{Tag: "home"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "a", got[0].Title)
}

// TestTaskRepo_Complete_IsIdempotent confirms re-completing a completed
// task does not error and does not bump CompletedAt — important so the
// LLM can re-run "mark done" without polluting the audit row.
func TestTaskRepo_Complete_IsIdempotent(t *testing.T) {
	db := openTaskDB(t)
	repo := &TaskRepo{DB: db}
	ctx := context.Background()

	id := uuid.NewString()
	require.NoError(t, repo.Insert(ctx, Task{ID: id, Title: "x"}))

	first := time.Date(2026, 5, 9, 10, 0, 0, 0, time.UTC)
	second := first.Add(time.Hour)
	require.NoError(t, repo.Complete(ctx, id, first))
	require.NoError(t, repo.Complete(ctx, id, second))

	got, err := repo.Get(ctx, id)
	require.NoError(t, err)
	require.True(t, got.Completed)
	require.NotNil(t, got.CompletedAt)
	require.True(t, got.CompletedAt.Equal(first), "second Complete must not bump CompletedAt; got %v", got.CompletedAt)
}

func TestTaskRepo_Complete_NotFound(t *testing.T) {
	db := openTaskDB(t)
	repo := &TaskRepo{DB: db}
	require.ErrorIs(t, repo.Complete(context.Background(), "nope", time.Now()), ErrTaskNotFound)
}

// TestTaskRepo_Delete_SoftDeletes confirms the row is hidden from List
// but still in the table — gorm's DeletedAt soft-delete contract.
func TestTaskRepo_Delete_SoftDeletes(t *testing.T) {
	db := openTaskDB(t)
	repo := &TaskRepo{DB: db}
	ctx := context.Background()

	id := uuid.NewString()
	require.NoError(t, repo.Insert(ctx, Task{ID: id, Title: "x"}))
	require.NoError(t, repo.Delete(ctx, id))

	got, err := repo.Get(ctx, id)
	require.NoError(t, err)
	require.Nil(t, got, "soft-deleted task must be hidden from Get")

	list, err := repo.List(ctx, TaskFilter{Status: "all"})
	require.NoError(t, err)
	require.Empty(t, list, "soft-deleted task must be hidden from List")

	require.ErrorIs(t, repo.Delete(ctx, "nope"), ErrTaskNotFound)
}

// TestTaskRepo_DueBefore_LimitAndOrder pins sweeper-relevant invariants:
// only fire_at-set unfired rows return, oldest fire_at first, capped
// at the limit.
func TestTaskRepo_DueBefore_LimitAndOrder(t *testing.T) {
	db := openTaskDB(t)
	repo := &TaskRepo{DB: db}
	ctx := context.Background()

	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	mk := func(id, title string, off time.Duration, fired bool) {
		fa := now.Add(off)
		row := Task{ID: id, Title: title, FireAt: &fa}
		if fired {
			fired := now
			row.FiredAt = &fired
		}
		require.NoError(t, repo.Insert(ctx, row))
	}
	mk("a", "alarm-2h-ago", -2*time.Hour, false)
	mk("b", "alarm-1h-ago", -time.Hour, false)
	mk("c", "alarm-future", time.Hour, false) // not due yet
	mk("d", "alarm-already-fired", -3*time.Hour, true)
	require.NoError(t, repo.Insert(ctx, Task{ID: "e", Title: "no-alarm"}))

	due, err := repo.DueBefore(ctx, now, 10)
	require.NoError(t, err)
	require.Len(t, due, 2)
	require.Equal(t, "alarm-2h-ago", due[0].Title)
	require.Equal(t, "alarm-1h-ago", due[1].Title)

	// Limit caps the slice.
	due, err = repo.DueBefore(ctx, now, 1)
	require.NoError(t, err)
	require.Len(t, due, 1)
}

// TestTaskRepo_MarkFired_Race covers the "two sweepers tick on the same
// row" case. Without the WHERE-on-fired_at guard both would mark fired
// and the dispatch would double-fire.
func TestTaskRepo_MarkFired_Race(t *testing.T) {
	db := openTaskDB(t)
	repo := &TaskRepo{DB: db}
	ctx := context.Background()

	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	id := uuid.NewString()
	fa := now.Add(-time.Hour)
	require.NoError(t, repo.Insert(ctx, Task{ID: id, Title: "x", FireAt: &fa}))

	var wg sync.WaitGroup
	var ok int64
	var mu sync.Mutex
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := repo.MarkFired(ctx, id, now)
			require.NoError(t, err)
			mu.Lock()
			ok += n
			mu.Unlock()
		}()
	}
	wg.Wait()
	require.Equal(t, int64(1), ok, "exactly one MarkFired call must claim the alarm")
}

// TestTaskRepo_MarkFired_FireAtCleared models the "user clears the
// alarm between sweeper-query and sweeper-mark" race. MarkFired must
// return 0 so the sweeper drops the dispatch.
func TestTaskRepo_MarkFired_FireAtCleared(t *testing.T) {
	db := openTaskDB(t)
	repo := &TaskRepo{DB: db}
	ctx := context.Background()

	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	id := uuid.NewString()
	fa := now.Add(-time.Hour)
	require.NoError(t, repo.Insert(ctx, Task{ID: id, Title: "x", FireAt: &fa}))

	// User clears fire_at first.
	require.NoError(t, repo.SetFireAt(ctx, id, nil))

	n, err := repo.MarkFired(ctx, id, now)
	require.NoError(t, err)
	require.Equal(t, int64(0), n, "MarkFired must not claim a row whose fire_at was cleared")
}

// TestTaskRepo_SetFireAt_ResetsFiredAt covers the "user re-arms a
// previously fired alarm" flow — fired_at must clear so the sweeper
// fires it again.
func TestTaskRepo_SetFireAt_ResetsFiredAt(t *testing.T) {
	db := openTaskDB(t)
	repo := &TaskRepo{DB: db}
	ctx := context.Background()

	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	id := uuid.NewString()
	fa := now.Add(-time.Hour)
	require.NoError(t, repo.Insert(ctx, Task{ID: id, Title: "x", FireAt: &fa}))

	n, err := repo.MarkFired(ctx, id, now)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	// Re-arm: SetFireAt clears fired_at + sets a new fire_at.
	newFA := now.Add(-30 * time.Minute)
	require.NoError(t, repo.SetFireAt(ctx, id, &newFA))

	got, err := repo.Get(ctx, id)
	require.NoError(t, err)
	require.Nil(t, got.FiredAt)
	require.NotNil(t, got.FireAt)
	require.True(t, got.FireAt.Equal(newFA))

	n, err = repo.MarkFired(ctx, id, now)
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "re-armed alarm must be claimable again")

	require.ErrorIs(t, repo.SetFireAt(context.Background(), "nope", &newFA), ErrTaskNotFound)
}

// TestTaskRepo_Update_NotFound surfaces the missing-row error so the
// HTTP layer can return 404 cleanly.
func TestTaskRepo_Update_NotFound(t *testing.T) {
	db := openTaskDB(t)
	repo := &TaskRepo{DB: db}
	require.ErrorIs(t,
		repo.Update(context.Background(), "nope", map[string]any{"title": "x"}),
		ErrTaskNotFound)
}
