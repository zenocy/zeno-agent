package projection

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/log/logtest"
	"github.com/zenocy/zeno-v2/internal/store"
)

// V2.11: OpenTasks reads from store.TaskRepo. The event-log fold of
// task.snapshot rows is gone. These tests re-pin the projection's
// behavior on the new shape.

func newProjTaskRepo(t *testing.T) *store.TaskRepo {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_journal_mode=WAL&_foreign_keys=on"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	repo := &store.TaskRepo{DB: db}
	require.NoError(t, repo.Migrate())
	return repo
}

func cfgAt(today string) Config {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	d, _ := time.ParseInLocation("2006-01-02", today, loc)
	d = d.Add(10 * time.Hour) // mid-morning local
	return Config{
		TZ:           loc,
		LookbackDays: 30,
		Now:          func() time.Time { return d },
	}
}

func TestOpenTasks_EmptyRepo(t *testing.T) {
	repo := newProjTaskRepo(t)
	got, err := (OpenTasks{Cfg: cfgAt("2026-05-05"), Tasks: repo}).Compute(context.Background(), logtest.NewMemReader())
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestOpenTasks_NoRepoConfigured(t *testing.T) {
	_, err := (OpenTasks{Cfg: cfgAt("2026-05-05")}).Compute(context.Background(), logtest.NewMemReader())
	require.ErrorIs(t, err, ErrTasksRepoNotConfigured)
}

func TestOpenTasks_OneOpen(t *testing.T) {
	repo := newProjTaskRepo(t)
	require.NoError(t, repo.Insert(context.Background(), store.Task{
		ID:       "a",
		Title:    "Ship V2.6",
		DueDate:  "2026-05-10",
		Priority: "high",
	}))
	got, err := (OpenTasks{Cfg: cfgAt("2026-05-05"), Tasks: repo}).Compute(context.Background(), logtest.NewMemReader())
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "Ship V2.6", got[0].Title)
}

// TestOpenTasks_CompletedTodayKept_CompletedYesterdayDropped pins the
// "show me what closed today" branch. completed_at-today rows survive;
// completed_at-yesterday rows drop.
func TestOpenTasks_CompletedTodayKept_CompletedYesterdayDropped(t *testing.T) {
	repo := newProjTaskRepo(t)
	loc, _ := time.LoadLocation("America/Los_Angeles")
	today := time.Date(2026, 5, 5, 14, 0, 0, 0, loc)
	yesterday := time.Date(2026, 5, 4, 14, 0, 0, 0, loc)

	require.NoError(t, repo.Insert(context.Background(), store.Task{
		ID:          "today",
		Title:       "shipped today",
		Completed:   true,
		CompletedAt: &today,
		Priority:    "med",
	}))
	require.NoError(t, repo.Insert(context.Background(), store.Task{
		ID:          "yest",
		Title:       "old",
		Completed:   true,
		CompletedAt: &yesterday,
		Priority:    "med",
	}))
	got, err := (OpenTasks{Cfg: cfgAt("2026-05-05"), Tasks: repo}).Compute(context.Background(), logtest.NewMemReader())
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "shipped today", got[0].Title)
}

// TestOpenTasks_OrderingOverdueTodaySoonNodate pins the V2.6/V2.10
// sort: alarms-due first, then overdue → today → soon → no-date →
// completed-today.
func TestOpenTasks_Ordering(t *testing.T) {
	repo := newProjTaskRepo(t)

	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: "nd", Title: "no date", Priority: "med"}))
	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: "od", Title: "overdue", DueDate: "2026-05-01", Priority: "med"}))
	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: "td", Title: "today", DueDate: "2026-05-05", Priority: "med"}))

	got, err := (OpenTasks{Cfg: cfgAt("2026-05-05"), Tasks: repo}).Compute(context.Background(), logtest.NewMemReader())
	require.NoError(t, err)
	require.Len(t, got, 3)
	require.Equal(t, "overdue", got[0].Title)
	require.Equal(t, "today", got[1].Title)
	require.Equal(t, "no date", got[2].Title)
}

// TestOpenTasks_AlarmsRiseToTop confirms tasks-with-fire_at-due-soon
// outrank everything else regardless of due_date.
func TestOpenTasks_AlarmsRiseToTop(t *testing.T) {
	repo := newProjTaskRepo(t)
	loc, _ := time.LoadLocation("America/Los_Angeles")
	fa := time.Date(2026, 5, 5, 11, 0, 0, 0, loc)

	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: "od", Title: "overdue", DueDate: "2026-05-01"}))
	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: "alarm", Title: "alarm", FireAt: &fa}))

	got, err := (OpenTasks{Cfg: cfgAt("2026-05-05"), Tasks: repo}).Compute(context.Background(), logtest.NewMemReader())
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "alarm", got[0].Title, "alarms-due rank above overdue todos")
}

// TestOpenTasks_PriorityTiebreakWithinBand pins priority as the
// secondary sort within a band.
func TestOpenTasks_PriorityTiebreakWithinBand(t *testing.T) {
	repo := newProjTaskRepo(t)

	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: "lo", Title: "low",  Priority: "low"}))
	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: "hi", Title: "high", Priority: "high"}))
	require.NoError(t, repo.Insert(context.Background(), store.Task{ID: "md", Title: "med",  Priority: "med"}))

	got, err := (OpenTasks{Cfg: cfgAt("2026-05-05"), Tasks: repo}).Compute(context.Background(), logtest.NewMemReader())
	require.NoError(t, err)
	require.Len(t, got, 3)
	require.Equal(t, "high", got[0].Title)
	require.Equal(t, "med", got[1].Title)
	require.Equal(t, "low", got[2].Title)
}

// TestOpenTasks_TagsRoundTrip confirms the Tags JSON column survives
// the projection layer's unmarshal.
func TestOpenTasks_TagsRoundTrip(t *testing.T) {
	repo := newProjTaskRepo(t)
	require.NoError(t, repo.Insert(context.Background(), store.Task{
		ID:    uuid.NewString(),
		Title: "x",
		Tags:  []byte(`["home","admin"]`),
	}))
	got, err := (OpenTasks{Cfg: cfgAt("2026-05-05"), Tasks: repo}).Compute(context.Background(), logtest.NewMemReader())
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, []string{"home", "admin"}, got[0].Tags)
}

// TestOpenTasks_CapAt50 verifies the OpenTasksMax cap. Cards/reactive
// loops never want hundreds of tasks in the projection; this guards
// against a runaway insert blowing the prompt budget.
func TestOpenTasks_CapAt50(t *testing.T) {
	repo := newProjTaskRepo(t)
	for i := 0; i < OpenTasksMax+10; i++ {
		require.NoError(t, repo.Insert(context.Background(), store.Task{
			ID:    uuid.NewString(),
			Title: "x",
		}))
	}
	got, err := (OpenTasks{Cfg: cfgAt("2026-05-05"), Tasks: repo}).Compute(context.Background(), logtest.NewMemReader())
	require.NoError(t, err)
	require.Len(t, got, OpenTasksMax)
}
