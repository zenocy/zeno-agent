package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func openMigrationDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_journal_mode=WAL&_foreign_keys=on"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	return db
}

// createLegacyRemindersTable mirrors the V2.8.1 reminders schema without
// importing the deleted ReminderRepo type. Used by the migration tests
// to simulate an upgrade from a pre-V2.11 DB.
func createLegacyRemindersTable(t *testing.T, db *gorm.DB) {
	t.Helper()
	require.NoError(t, db.Exec(`
		CREATE TABLE reminders (
			id TEXT PRIMARY KEY,
			due_at DATETIME NOT NULL,
			title TEXT NOT NULL,
			body TEXT,
			source_card_id TEXT,
			fired_at DATETIME,
			created_at DATETIME,
			deleted_at DATETIME
		)
	`).Error)
}

// insertLegacyReminder writes one row to the legacy table. Sets deleted_at
// or fired_at when the test wants to exercise the skip-rows path.
type legacyReminder struct {
	ID           string
	DueAt        time.Time
	Title        string
	Body         string
	SourceCardID string
	CreatedAt    time.Time
	FiredAt      *time.Time
	DeletedAt    *time.Time
}

func insertLegacyReminder(t *testing.T, db *gorm.DB, r legacyReminder) {
	t.Helper()
	require.NoError(t, db.Exec(
		`INSERT INTO reminders (id, due_at, title, body, source_card_id, created_at, fired_at, deleted_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.DueAt, r.Title, r.Body, r.SourceCardID, r.CreatedAt, r.FiredAt, r.DeletedAt,
	).Error)
}

// TestMigrateRemindersToTasks_HappyPath is the highest-stakes test in
// the V2.11 cutover: the user's live reminders DB must round-trip into
// the new tasks table without losing data. Seeds an open reminder, an
// already-fired reminder, and a soft-deleted reminder; asserts only the
// open one lands in tasks; asserts the legacy table is gone.
func TestMigrateRemindersToTasks_HappyPath(t *testing.T) {
	db := openMigrationDB(t)
	ctx := context.Background()

	// Bring up both schemas. In production the new schema migrates
	// first, then the legacy data flows in via this function.
	require.NoError(t, (&TaskRepo{DB: db}).Migrate())
	createLegacyRemindersTable(t, db)

	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	openID := uuid.NewString()
	firedID := uuid.NewString()
	deletedID := uuid.NewString()

	insertLegacyReminder(t, db, legacyReminder{
		ID:           openID,
		DueAt:        now.Add(time.Hour),
		Title:        "ping me",
		Body:         "the kettle",
		SourceCardID: "card-42",
		CreatedAt:    now,
	})
	fired := now.Add(-30 * time.Minute)
	insertLegacyReminder(t, db, legacyReminder{
		ID:        firedID,
		DueAt:     now.Add(-time.Hour),
		Title:     "already-fired",
		CreatedAt: now,
		FiredAt:   &fired,
	})
	deletedAt := now
	insertLegacyReminder(t, db, legacyReminder{
		ID:        deletedID,
		DueAt:     now.Add(time.Hour),
		Title:     "deleted",
		CreatedAt: now,
		DeletedAt: &deletedAt,
	})

	require.NoError(t, MigrateRemindersToTasks(ctx, db))

	// 1. Open reminder landed in tasks with fields intact.
	tRepo := &TaskRepo{DB: db}
	got, err := tRepo.Get(ctx, openID)
	require.NoError(t, err)
	require.NotNil(t, got, "open reminder must land in tasks table")
	require.Equal(t, "ping me", got.Title)
	require.Equal(t, "the kettle", got.Body)
	require.Equal(t, "card-42", got.SourceCardID)
	require.NotNil(t, got.FireAt)
	require.True(t, got.FireAt.Equal(now.Add(time.Hour)))
	require.Nil(t, got.FiredAt)

	// 2. Already-fired reminder is dropped (the alarm has already done
	// its job; carrying it forward as an unfired task would re-fire).
	got, err = tRepo.Get(ctx, firedID)
	require.NoError(t, err)
	require.Nil(t, got, "already-fired reminder must not land in tasks")

	// 3. Soft-deleted reminder is dropped.
	got, err = tRepo.Get(ctx, deletedID)
	require.NoError(t, err)
	require.Nil(t, got, "soft-deleted reminder must not land in tasks")

	// 4. Legacy table is gone.
	exists, err := remindersTableExists(ctx, db)
	require.NoError(t, err)
	require.False(t, exists, "legacy reminders table must be dropped after migration")
}

// TestMigrateRemindersToTasks_Idempotent confirms a second call is a
// no-op — important because boot will run the migration unconditionally.
func TestMigrateRemindersToTasks_Idempotent(t *testing.T) {
	db := openMigrationDB(t)
	ctx := context.Background()

	require.NoError(t, (&TaskRepo{DB: db}).Migrate())
	createLegacyRemindersTable(t, db)

	id := uuid.NewString()
	insertLegacyReminder(t, db, legacyReminder{
		ID:        id,
		DueAt:     time.Now().Add(time.Hour),
		Title:     "x",
		CreatedAt: time.Now(),
	})

	require.NoError(t, MigrateRemindersToTasks(ctx, db))
	require.NoError(t, MigrateRemindersToTasks(ctx, db),
		"second migration call must be a no-op (table is gone)")

	tRepo := &TaskRepo{DB: db}
	got, err := tRepo.Get(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, got, "migrated row must still be present after second call")

	all, err := tRepo.List(ctx, TaskFilter{Status: "all"})
	require.NoError(t, err)
	require.Len(t, all, 1, "second call must not duplicate the row")
}

// TestMigrateRemindersToTasks_NoLegacyTable models a fresh install
// where the reminders table never existed. The migration must return
// cleanly without erroring or creating the legacy table.
func TestMigrateRemindersToTasks_NoLegacyTable(t *testing.T) {
	db := openMigrationDB(t)
	ctx := context.Background()

	require.NoError(t, (&TaskRepo{DB: db}).Migrate())
	// No legacy reminders table created → table absent.

	require.NoError(t, MigrateRemindersToTasks(ctx, db))

	exists, err := remindersTableExists(ctx, db)
	require.NoError(t, err)
	require.False(t, exists)
}

// TestMigrateRemindersToTasks_PreservesExistingTasks confirms a row
// already in the tasks table (sharing an ID with a legacy reminder) is
// not duplicated. The NOT EXISTS clause is the safety guard.
func TestMigrateRemindersToTasks_PreservesExistingTasks(t *testing.T) {
	db := openMigrationDB(t)
	ctx := context.Background()

	require.NoError(t, (&TaskRepo{DB: db}).Migrate())
	createLegacyRemindersTable(t, db)

	id := uuid.NewString()
	insertLegacyReminder(t, db, legacyReminder{
		ID:        id,
		DueAt:     time.Now().Add(time.Hour),
		Title:     "from-reminders",
		CreatedAt: time.Now(),
	})

	tRepo := &TaskRepo{DB: db}
	require.NoError(t, tRepo.Insert(context.Background(), Task{ID: id, Title: "from-tasks"}))

	require.NoError(t, MigrateRemindersToTasks(ctx, db))

	got, err := tRepo.Get(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "from-tasks", got.Title, "existing task row must win over the legacy reminder")
}
