package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// TestBriefingRepo_MigrateUpgradesLegacyPK seeds the V2.2 schema (single
// `date` PK, no `kind` column) with a populated row, runs Migrate(),
// and asserts: the column exists, the row survives with kind='morning',
// the new shape supports inserting a same-date inject row, and a second
// Migrate() is a no-op.
//
// This is the load-bearing safety test for the V2.3.0 P3 schema upgrade.
// If a real production DB cannot be brought forward by this code path,
// the daemon won't boot on existing data.
func TestBriefingRepo_MigrateUpgradesLegacyPK(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "legacy.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)

	// 1. Hand-create the V2.2 schema: date as the sole PK, NO kind column.
	//    Includes the V2.3.0 P0 additive `state` column (the populated DB
	//    we have to migrate is already on the post-P0 V2.2 shape).
	require.NoError(t, db.Exec(`CREATE TABLE briefings (
		date TEXT PRIMARY KEY,
		eyebrow TEXT,
		title TEXT,
		summary TEXT,
		tension INTEGER NOT NULL DEFAULT 0,
		state TEXT,
		suggested_followup TEXT,
		run_id TEXT,
		created_at DATETIME
	)`).Error)
	require.NoError(t, db.Exec(`CREATE INDEX idx_briefings_state ON briefings(state)`).Error)
	require.NoError(t, db.Exec(`CREATE INDEX idx_briefings_run_id ON briefings(run_id)`).Error)

	// 2. Seed two legacy rows for different dates so we can assert nothing
	//    is dropped during the rebuild.
	require.NoError(t, db.Exec(`INSERT INTO briefings(date, eyebrow, title, summary, tension, state, suggested_followup, run_id, created_at)
		VALUES ('2026-04-25', 'morning', 'V2.2 row 1', 'summary 1', 35, 'morning_calm', '', 'run-a', ?)`, time.Now()).Error)
	require.NoError(t, db.Exec(`INSERT INTO briefings(date, eyebrow, title, summary, tension, state, suggested_followup, run_id, created_at)
		VALUES ('2026-04-26', 'morning', 'V2.2 row 2', 'summary 2', 28, 'morning_calm', '', 'run-b', ?)`, time.Now()).Error)

	// 3. Run Migrate — this should rebuild the table with the composite PK
	//    and bring the existing rows forward as kind='morning'.
	repo := &BriefingRepo{DB: db}
	require.NoError(t, repo.Migrate())

	// 4. Schema check: the `kind` column now exists.
	var pragmaCount int64
	require.NoError(t, db.Raw("SELECT count(*) FROM pragma_table_info('briefings') WHERE name = 'kind'").Row().Scan(&pragmaCount))
	require.EqualValues(t, 1, pragmaCount, "kind column must exist after migration")

	// 5. Data preservation: both legacy rows survive with kind='morning'.
	ctx := context.Background()
	got1, err := repo.ByDate(ctx, "2026-04-25")
	require.NoError(t, err)
	require.NotNil(t, got1)
	require.Equal(t, "V2.2 row 1", got1.Title)
	require.Equal(t, BriefingKindMorning, got1.Kind, "legacy row backfilled to kind=morning")

	got2, err := repo.ByDate(ctx, "2026-04-26")
	require.NoError(t, err)
	require.NotNil(t, got2)
	require.Equal(t, "V2.2 row 2", got2.Title)

	// 6. The new shape supports a same-date inject row alongside morning.
	require.NoError(t, repo.UpsertInject(ctx, Briefing{
		Date: "2026-04-25", Title: "inject after migration", Tension: 80, CreatedAt: time.Now(),
	}))
	all, err := repo.ListByDate(ctx, "2026-04-25")
	require.NoError(t, err)
	require.Len(t, all, 2)

	// 7. Idempotence: a second Migrate() must be a no-op.
	require.NoError(t, repo.Migrate())
	gotAgain, err := repo.ByDate(ctx, "2026-04-25")
	require.NoError(t, err)
	require.NotNil(t, gotAgain)
	require.Equal(t, "V2.2 row 1", gotAgain.Title, "data must survive a second Migrate")

	// And the inject row from step 6 is still there.
	injectRow, err := repo.ByDateKind(ctx, "2026-04-25", BriefingKindInject)
	require.NoError(t, err)
	require.NotNil(t, injectRow)
	require.Equal(t, "inject after migration", injectRow.Title)
}

// TestBriefingRepo_MigrateOnFreshDB pins that AutoMigrate creates the new
// schema cleanly when no legacy table exists.
func TestBriefingRepo_MigrateOnFreshDB(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "fresh.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)

	repo := &BriefingRepo{DB: db}
	require.NoError(t, repo.Migrate())

	// Insert + read back round-trips both kinds.
	ctx := context.Background()
	require.NoError(t, repo.UpsertMorning(ctx, Briefing{Date: "2026-04-25", Title: "morning", CreatedAt: time.Now()}))
	require.NoError(t, repo.UpsertInject(ctx, Briefing{Date: "2026-04-25", Title: "inject", CreatedAt: time.Now()}))
	all, err := repo.ListByDate(ctx, "2026-04-25")
	require.NoError(t, err)
	require.Len(t, all, 2)
}
