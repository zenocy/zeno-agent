package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_journal_mode=WAL&_foreign_keys=on"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, (&CardRepo{DB: db}).Migrate())
	require.NoError(t, (&BriefingRepo{DB: db}).Migrate())
	require.NoError(t, (&TraceRepo{DB: db}).Migrate())
	require.NoError(t, (&MemoryRepo{DB: db}).Migrate())
	require.NoError(t, (&ConcernRepo{DB: db}).Migrate())
	require.NoError(t, (&ConcernObservationRepo{DB: db}).Migrate())
	return db
}

func TestCardRepo_UpsertByID(t *testing.T) {
	db := openTestDB(t)
	repo := &CardRepo{DB: db}
	ctx := context.Background()

	require.NoError(t, repo.Upsert(ctx, []Card{
		{ID: "a", Date: "2026-04-25", Kind: "", Source: "mail", Title: "first", Rel: "high", CreatedAt: time.Now()},
		{ID: "b", Date: "2026-04-25", Kind: "personal", Source: "personal", Title: "second", Rel: "low", CreatedAt: time.Now()},
	}))
	rows, err := repo.ListByDate(ctx, "2026-04-25")
	require.NoError(t, err)
	require.Len(t, rows, 2)

	// Re-upsert with the same ID overwrites in place (idempotent re-run).
	require.NoError(t, repo.Upsert(ctx, []Card{
		{ID: "a", Date: "2026-04-25", Kind: "", Source: "mail", Title: "first-updated", Rel: "high", CreatedAt: time.Now()},
	}))
	rows, err = repo.ListByDate(ctx, "2026-04-25")
	require.NoError(t, err)
	require.Len(t, rows, 2, "same-id upsert must replace not duplicate")
	titles := []string{rows[0].Title, rows[1].Title}
	require.Contains(t, titles, "first-updated")
	require.Contains(t, titles, "second")
}

// Three personal cards on the same date must all persist. The legacy
// (date, kind, source) unique index would have collapsed them to one;
// uniqueness is now per-ID.
func TestCardRepo_UpsertAllowsMultipleSameSource(t *testing.T) {
	db := openTestDB(t)
	repo := &CardRepo{DB: db}
	ctx := context.Background()

	now := time.Now()
	require.NoError(t, repo.Upsert(ctx, []Card{
		{ID: "lia", Date: "2026-04-25", Kind: "personal", Source: "personal", Rel: "high", Title: "Lia recital", CreatedAt: now},
		{ID: "run", Date: "2026-04-25", Kind: "personal", Source: "personal", Rel: "low", Title: "Run window", CreatedAt: now.Add(time.Millisecond)},
		{ID: "anniv", Date: "2026-04-25", Kind: "personal", Source: "personal", Rel: "low", Title: "Anniversary", CreatedAt: now.Add(2 * time.Millisecond)},
	}))
	rows, err := repo.ListByDate(ctx, "2026-04-25")
	require.NoError(t, err)
	require.Len(t, rows, 3, "three cards sharing (date, kind, source) must all persist")
}

func TestCardRepo_OrderByRel(t *testing.T) {
	db := openTestDB(t)
	repo := &CardRepo{DB: db}
	ctx := context.Background()

	now := time.Now()
	require.NoError(t, repo.Upsert(ctx, []Card{
		{ID: "low", Date: "2026-04-25", Source: "personal", Rel: "low", Title: "low", CreatedAt: now.Add(time.Millisecond)},
		{ID: "med", Date: "2026-04-25", Source: "tasks", Rel: "med", Title: "med", CreatedAt: now.Add(2 * time.Millisecond)},
		{ID: "high", Date: "2026-04-25", Source: "mail", Rel: "high", Title: "high", CreatedAt: now.Add(3 * time.Millisecond)},
	}))
	rows, err := repo.ListByDate(ctx, "2026-04-25")
	require.NoError(t, err)
	require.Equal(t, "high", rows[0].Rel)
	require.Equal(t, "med", rows[1].Rel)
	require.Equal(t, "low", rows[2].Rel)
}

func TestCardRepo_DeleteStale(t *testing.T) {
	db := openTestDB(t)
	repo := &CardRepo{DB: db}
	ctx := context.Background()

	now := time.Now()
	require.NoError(t, repo.Upsert(ctx, []Card{
		{ID: "old1", Date: "2026-04-25", Source: "mail", RunID: "run-1", Title: "old", CreatedAt: now},
		{ID: "old2", Date: "2026-04-25", Source: "tasks", RunID: "run-1", Title: "old2", CreatedAt: now},
	}))
	require.NoError(t, repo.Upsert(ctx, []Card{
		{ID: "new1", Date: "2026-04-25", Kind: "", Source: "personal", RunID: "run-2", Title: "new", CreatedAt: now},
	}))
	require.NoError(t, repo.DeleteStale(ctx, "2026-04-25", "run-2"))
	rows, err := repo.ListByDate(ctx, "2026-04-25")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "new1", rows[0].ID)
}

func TestBriefingRepo_UpsertByDate(t *testing.T) {
	db := openTestDB(t)
	repo := &BriefingRepo{DB: db}
	ctx := context.Background()

	require.NoError(t, repo.UpsertMorning(ctx, Briefing{Date: "2026-04-25", Title: "first", Tension: 30, CreatedAt: time.Now()}))
	require.NoError(t, repo.UpsertMorning(ctx, Briefing{Date: "2026-04-25", Title: "second", Tension: 70, CreatedAt: time.Now()}))
	got, err := repo.ByDate(ctx, "2026-04-25")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "second", got.Title)
	require.Equal(t, 70, got.Tension)
}

func TestCardRepo_SetDismissed(t *testing.T) {
	db := openTestDB(t)
	repo := &CardRepo{DB: db}
	ctx := context.Background()

	require.NoError(t, repo.Upsert(ctx, []Card{
		{ID: "c1", Date: "2026-04-25", Source: "mail", Rel: "high", Title: "keep", CreatedAt: time.Now()},
		{ID: "c2", Date: "2026-04-25", Source: "calendar", Rel: "med", Title: "dismiss me", CreatedAt: time.Now()},
	}))

	require.NoError(t, repo.SetDismissed(ctx, "c2"))

	rows, err := repo.ListByDate(ctx, "2026-04-25")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "c1", rows[0].ID)
}

func TestCardRepo_SetPinned_SurvivesAcrossDates(t *testing.T) {
	db := openTestDB(t)
	repo := &CardRepo{DB: db}
	ctx := context.Background()

	require.NoError(t, repo.Upsert(ctx, []Card{
		{ID: "c1", Date: "2026-04-25", Source: "mail", Rel: "high", Title: "old day", CreatedAt: time.Now()},
		{ID: "c2", Date: "2026-04-26", Source: "calendar", Rel: "med", Title: "today", CreatedAt: time.Now()},
	}))

	// Pin the older card.
	require.NoError(t, repo.SetPinned(ctx, "c1", true))

	// ListByDate for the newer date returns BOTH the date-matching card
	// AND the pinned older card. Pinned rows sort first.
	rows, err := repo.ListByDate(ctx, "2026-04-26")
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, "c1", rows[0].ID, "pinned row sorts first")
	require.Equal(t, "c2", rows[1].ID)

	// Unpin clears the cross-date carry.
	require.NoError(t, repo.SetPinned(ctx, "c1", false))
	rows, err = repo.ListByDate(ctx, "2026-04-26")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "c2", rows[0].ID)
}

func TestCardRepo_SetSnoozed(t *testing.T) {
	db := openTestDB(t)
	repo := &CardRepo{DB: db}
	ctx := context.Background()

	require.NoError(t, repo.Upsert(ctx, []Card{
		{ID: "c1", Date: "2026-04-25", Source: "mail", Rel: "high", Title: "keep", CreatedAt: time.Now()},
		{ID: "c2", Date: "2026-04-25", Source: "calendar", Rel: "med", Title: "snooze me", CreatedAt: time.Now()},
	}))

	// Snooze c2 for today — should disappear from list.
	require.NoError(t, repo.SetSnoozed(ctx, "c2", "2026-04-25"))
	rows, err := repo.ListByDate(ctx, "2026-04-25")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "c1", rows[0].ID)

	// Snoozed card appears again on a different date.
	rows, err = repo.ListByDate(ctx, "2026-04-26")
	require.NoError(t, err)
	require.Len(t, rows, 0) // c1 has date 2026-04-25, so nothing on 2026-04-26
}

// TestBriefingRepo_StateColumn pins the V2.3.0 additive State column
// round-trips both populated and empty so existing pre-V2.3 rows read
// back unchanged.
func TestBriefingRepo_StateColumn(t *testing.T) {
	db := openTestDB(t)
	repo := &BriefingRepo{DB: db}
	ctx := context.Background()

	require.NoError(t, repo.UpsertMorning(ctx, Briefing{Date: "2026-04-25", Title: "with state", State: "pre_meeting", Tension: 75, CreatedAt: time.Now()}))
	got, err := repo.ByDate(ctx, "2026-04-25")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "pre_meeting", got.State)

	// Empty State round-trip — back-compat with pre-V2.3 rows.
	require.NoError(t, repo.UpsertMorning(ctx, Briefing{Date: "2026-04-26", Title: "empty state", Tension: 30, CreatedAt: time.Now()}))
	got, err = repo.ByDate(ctx, "2026-04-26")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "", got.State)
}

// TestCardRepo_OriginColumn pins the V2.3.0 additive Origin column
// round-trips both populated and empty so existing pre-V2.3 cards read
// back unchanged.
func TestCardRepo_OriginColumn(t *testing.T) {
	db := openTestDB(t)
	repo := &CardRepo{DB: db}
	ctx := context.Background()

	now := time.Now()
	require.NoError(t, repo.Upsert(ctx, []Card{
		{ID: "morning-1", Date: "2026-04-25", Source: "mail", Rel: "high", Title: "morning card", CreatedAt: now},
		{ID: "inject-1", Date: "2026-04-25", Source: "mail", Rel: "high", Title: "inject card", Origin: "inject", CreatedAt: now.Add(time.Millisecond)},
	}))

	rows, err := repo.ListByDate(ctx, "2026-04-25")
	require.NoError(t, err)
	require.Len(t, rows, 2)
	origins := map[string]string{rows[0].ID: rows[0].Origin, rows[1].ID: rows[1].Origin}
	require.Equal(t, "", origins["morning-1"])
	require.Equal(t, "inject", origins["inject-1"])
}

// V2.3.0 P3: morning + inject briefings must coexist for the same date
// under the composite (date, kind) PK. ByDate returns morning by default;
// ByDateKind reaches the inject row; ListByDate returns both.
func TestBriefingRepo_MorningAndInjectCoexist(t *testing.T) {
	db := openTestDB(t)
	repo := &BriefingRepo{DB: db}
	ctx := context.Background()

	require.NoError(t, repo.UpsertMorning(ctx, Briefing{
		Date: "2026-04-30", Title: "morning brief", Tension: 35, State: "morning_calm", CreatedAt: time.Now(),
	}))
	require.NoError(t, repo.UpsertInject(ctx, Briefing{
		Date: "2026-04-30", Title: "Saru — board call moved to 10:30", Tension: 85, State: "message_inject", CreatedAt: time.Now(),
	}))

	morning, err := repo.ByDate(ctx, "2026-04-30")
	require.NoError(t, err)
	require.NotNil(t, morning)
	require.Equal(t, "morning brief", morning.Title)
	require.Equal(t, BriefingKindMorning, morning.Kind)

	inject, err := repo.ByDateKind(ctx, "2026-04-30", BriefingKindInject)
	require.NoError(t, err)
	require.NotNil(t, inject)
	require.Equal(t, "Saru — board call moved to 10:30", inject.Title)

	all, err := repo.ListByDate(ctx, "2026-04-30")
	require.NoError(t, err)
	require.Len(t, all, 2)
	// kind ASC: inject (i) sorts before morning (m).
	require.Equal(t, BriefingKindInject, all[0].Kind)
	require.Equal(t, BriefingKindMorning, all[1].Kind)

	// UpsertInject is idempotent — re-upserting overwrites the inject row,
	// not the morning row.
	require.NoError(t, repo.UpsertInject(ctx, Briefing{
		Date: "2026-04-30", Title: "newer inject", Tension: 90, CreatedAt: time.Now(),
	}))
	again, err := repo.ListByDate(ctx, "2026-04-30")
	require.NoError(t, err)
	require.Len(t, again, 2, "inject upsert must replace, not duplicate")
}

// V2.3.0 P3: DeleteStale must NEVER touch inject cards. The morning
// re-run carries a different RunID; without the origin guard, the
// re-run would silently wipe injects produced earlier in the day.
func TestCardRepo_DeleteStalePreservesInjectCards(t *testing.T) {
	db := openTestDB(t)
	repo := &CardRepo{DB: db}
	ctx := context.Background()

	now := time.Now()
	require.NoError(t, repo.Upsert(ctx, []Card{
		{ID: "morning-old", Date: "2026-04-30", Source: "mail", RunID: "morning-1", Title: "old morning", CreatedAt: now},
		{ID: "inject-1", Date: "2026-04-30", Source: "mail", RunID: "inject-cron-1", Origin: "inject", Title: "inject card", CreatedAt: now.Add(time.Hour)},
		{ID: "morning-new", Date: "2026-04-30", Source: "mail", RunID: "morning-2", Title: "new morning", CreatedAt: now.Add(2 * time.Hour)},
	}))

	// Sweep stale relative to the new morning run-id.
	require.NoError(t, repo.DeleteStale(ctx, "2026-04-30", "morning-2"))

	rows, err := repo.ListByDate(ctx, "2026-04-30")
	require.NoError(t, err)
	require.Len(t, rows, 2, "old morning swept; inject preserved; new morning kept")

	ids := map[string]bool{}
	for _, c := range rows {
		ids[c.ID] = true
	}
	require.True(t, ids["inject-1"], "inject card must survive morning DeleteStale")
	require.True(t, ids["morning-new"])
	require.False(t, ids["morning-old"], "stale morning card must be swept")
}

func TestTraceRepo_CreateAndGet(t *testing.T) {
	db := openTestDB(t)
	repo := &TraceRepo{DB: db}
	ctx := context.Background()

	tr := Trace{
		ID:        "tr-1",
		RunID:     "run-1",
		Date:      "2026-04-25",
		Stopped:   "ok",
		TotalMs:   120,
		Steps:     datatypes.JSON([]byte(`[{"kind":"tool","op":"READ"}]`)),
		CreatedAt: time.Now(),
	}
	require.NoError(t, repo.Create(ctx, tr))

	got, err := repo.Get(ctx, "tr-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "ok", got.Stopped)
	require.Equal(t, int64(120), got.TotalMs)
}
