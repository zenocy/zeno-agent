package store

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

// MigrateRemindersToTasks is the V2.11 one-shot import that copies live
// rows from the legacy `reminders` table into the unified `tasks`
// table, then drops `reminders`. Idempotent: a second call is a no-op
// because the source table no longer exists.
//
// Behavior:
//   - If the `reminders` table doesn't exist (fresh install or
//     post-first-boot), the function returns nil immediately.
//   - Otherwise wraps the import + drop in a single transaction so a
//     failed import does not lose data.
//   - Only rows that are not soft-deleted and not yet fired are
//     migrated; already-fired rows would just be noise in the new
//     schema and the sweeper has already finished its job for them.
//   - Reminder UUID becomes the task UUID, so any source_card_id
//     linkbacks survive the migration.
//   - The reminder's body becomes the task's body; title becomes the
//     task's title; due_at becomes fire_at. Priority defaults to 'med'.
func MigrateRemindersToTasks(ctx context.Context, db *gorm.DB) error {
	// 1. Detect whether the reminders table exists. SQLite-specific —
	// matches the deployment runtime (the only DB driver this repo
	// supports). On other engines the same query exists in their
	// information_schema; gating to SQLite is fine because the rest of
	// the codebase is SQLite-only.
	exists, err := remindersTableExists(ctx, db)
	if err != nil {
		return fmt.Errorf("detect reminders table: %w", err)
	}
	if !exists {
		return nil
	}

	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Insert in one statement so the import is all-or-nothing
		// inside the transaction. The NOT EXISTS clause makes the
		// import safe to retry even if the drop step fails (callers
		// would then rerun and get a second no-op import).
		insertSQL := `
			INSERT INTO tasks
				(id, title, body, completed, due_date, priority, fire_at, source_card_id, created_at, updated_at)
			SELECT
				r.id,
				COALESCE(NULLIF(r.title, ''), 'Reminder'),
				COALESCE(r.body, ''),
				0,
				'',
				'med',
				r.due_at,
				COALESCE(r.source_card_id, ''),
				r.created_at,
				r.created_at
			FROM reminders r
			WHERE r.deleted_at IS NULL
			  AND r.fired_at IS NULL
			  AND NOT EXISTS (SELECT 1 FROM tasks t WHERE t.id = r.id)`
		if err := tx.Exec(insertSQL).Error; err != nil {
			return fmt.Errorf("import reminders into tasks: %w", err)
		}

		// 2. Drop the legacy table. After this point the rollback path
		// for the user is "restore data/zeno.db from a backup" — the
		// plan called this out as the cutover risk.
		if err := tx.Exec("DROP TABLE IF EXISTS reminders").Error; err != nil {
			return fmt.Errorf("drop reminders table: %w", err)
		}
		return nil
	})
}

// remindersTableExists queries SQLite's sqlite_master to see whether
// the legacy reminders table is still present.
func remindersTableExists(ctx context.Context, db *gorm.DB) (bool, error) {
	var name string
	err := db.WithContext(ctx).Raw(
		"SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'reminders'",
	).Row().Scan(&name)
	if err != nil {
		// gorm/sqlite returns a plain "sql: no rows in result set" for
		// missing rows — treat that as "table does not exist".
		if isNoRowsErr(err) {
			return false, nil
		}
		return false, err
	}
	return name == "reminders", nil
}

// isNoRowsErr identifies the database/sql ErrNoRows without forcing
// callers (or this file) to import database/sql directly.
func isNoRowsErr(err error) bool {
	if err == nil {
		return false
	}
	return err.Error() == "sql: no rows in result set"
}
