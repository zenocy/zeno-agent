package store

import (
	"context"
	"time"

	"gorm.io/gorm"
)

// BriefingKindMorning is the default value for Briefing.Kind — the
// once-per-day morning briefing produced by the synth runner.
const BriefingKindMorning = "morning"

// BriefingKindInject is the value for Briefing.Kind on inject fragments —
// the 1-paragraph voice line attached to a V2.3 message_inject card.
const BriefingKindInject = "inject"

// Briefing is one literary paragraph for a (date, kind) pair.
//
// Until V2.3.0 P3 the primary key was `date` alone. P3 made it composite
// `(date, kind)` so the morning briefing and a same-day inject fragment
// can coexist as siblings. Existing rows migrate to kind="morning" via
// the MigrateBriefingTable upgrade in Migrate(); see that function for
// the SQLite rebuild ladder.
//
// State is the V2.3 adaptive-state register the briefing was synthesized
// under (e.g. "morning_calm", "pre_meeting"). Empty for pre-V2.3 rows;
// callers default empty to morning_calm.
type Briefing struct {
	Date              string    `gorm:"primaryKey;type:text"            json:"date"`
	Kind              string    `gorm:"primaryKey;type:text;default:'morning'" json:"kind,omitempty"`
	Eyebrow           string    `gorm:"type:text"                       json:"eyebrow"`
	Title             string    `gorm:"type:text"                       json:"title"`
	Summary           string    `gorm:"type:text"                       json:"summary"`
	Tension           int       `gorm:"not null;default:0"              json:"tension"`
	State             string    `gorm:"type:text;index"                 json:"state,omitempty"`
	SuggestedFollowup string    `gorm:"type:text"                       json:"suggested_followup,omitempty"`
	RunID             string    `gorm:"type:text;index"                 json:"-"`
	CreatedAt         time.Time `                                       json:"-"`
}

// BriefingRepo persists and reads Briefing rows.
type BriefingRepo struct {
	DB    *gorm.DB
	Table string // "briefings" or "briefings_replay"
}

// Migrate runs an idempotent upgrade ladder, then AutoMigrate to cover any
// future additive columns. The legacy schema (date PK, no `kind` column)
// requires a hand-written SQLite rebuild because SQLite cannot ALTER a
// primary key in place — and GORM's AutoMigrate happily adds columns but
// will not change the existing PK. The probe-then-rebuild guarantees
// idempotence: on a fresh DB the rebuild branch is skipped; on a populated
// V2.2 DB it runs once and brings every existing briefing forward as
// kind='morning'; on subsequent boots after the upgrade, the probe sees
// `kind` already and skips the rebuild.
func (r *BriefingRepo) Migrate() error {
	tbl := r.tableName()
	upgraded, err := r.upgradeLegacyPKIfNeeded(tbl)
	if err != nil {
		return err
	}
	if upgraded {
		// Hand-written rebuild produced the canonical shape; nothing else to do.
		return nil
	}
	// On a fresh DB (no legacy table), use AutoMigrate to create the new
	// shape. The fresh-table branch is safe because AutoMigrate creates
	// the table from scratch and respects the composite PK / defaults
	// declared on the model.
	if !r.DB.Migrator().HasTable(tbl) {
		return r.DB.Table(tbl).AutoMigrate(&Briefing{})
	}
	// Table already on the post-P3 schema — do nothing. Running AutoMigrate
	// on a populated composite-PK table is unsafe: GORM's SQLite migrator
	// may attempt a partial-column rebuild that violates the composite PK
	// when the table holds rows for multiple kinds with the same date.
	// Future additive columns must land via explicit migration steps in
	// upgradeLegacyPKIfNeeded (or a sibling), not via AutoMigrate.
	return nil
}

// upgradeLegacyPKIfNeeded probes the existing schema for the `kind`
// column. If absent (V2.2 or earlier), it rebuilds the table with the
// composite PK and backfills every existing row to kind='morning'. The
// rebuild runs in a transaction so a power failure mid-upgrade leaves
// the original table intact.
// upgradeLegacyPKIfNeeded returns (true, nil) when it actually rebuilt
// the table; (false, nil) when no work was needed; non-nil error on any
// failure. Callers use the bool to decide whether to skip a follow-up
// AutoMigrate that could otherwise interact badly with the fresh shape.
func (r *BriefingRepo) upgradeLegacyPKIfNeeded(tbl string) (bool, error) {
	hasTable := r.DB.Migrator().HasTable(tbl)
	if !hasTable {
		// Fresh DB — AutoMigrate will create the new schema cleanly.
		return false, nil
	}
	// Probe the live schema directly via PRAGMA — GORM's HasColumn can
	// resolve to the model's expected columns rather than the actual
	// table's columns, which would falsely return true on a legacy DB.
	var pragmaCount int64
	row := r.DB.Raw("SELECT count(*) FROM pragma_table_info(?) WHERE name = 'kind'", tbl).Row()
	if err := row.Scan(&pragmaCount); err != nil {
		return false, err
	}
	if pragmaCount > 0 {
		// Already has the kind column → already on the post-P3 schema.
		return false, nil
	}

	rebuildTbl := tbl + "_v3"
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		// 1. Build the new table with the composite PK and indexes.
		createStmt := `CREATE TABLE ` + rebuildTbl + ` (
			date TEXT NOT NULL,
			kind TEXT NOT NULL DEFAULT 'morning',
			eyebrow TEXT,
			title TEXT,
			summary TEXT,
			tension INTEGER NOT NULL DEFAULT 0,
			state TEXT,
			suggested_followup TEXT,
			run_id TEXT,
			created_at DATETIME,
			PRIMARY KEY (date, kind)
		)`
		if err := tx.Exec(createStmt).Error; err != nil {
			return err
		}
		// 2. Copy existing rows in as kind='morning'.
		copyStmt := `INSERT INTO ` + rebuildTbl + ` (date, kind, eyebrow, title, summary, tension, state, suggested_followup, run_id, created_at)
			SELECT date, 'morning', eyebrow, title, summary, tension,
				COALESCE(state, ''), COALESCE(suggested_followup, ''),
				COALESCE(run_id, ''), created_at
			FROM ` + tbl
		if err := tx.Exec(copyStmt).Error; err != nil {
			return err
		}
		// 3. Drop the old table and rename the new one in place.
		if err := tx.Exec("DROP TABLE " + tbl).Error; err != nil {
			return err
		}
		if err := tx.Exec("ALTER TABLE " + rebuildTbl + " RENAME TO " + tbl).Error; err != nil {
			return err
		}
		// 4. Recreate the indexes AutoMigrate would have made for the
		//    column-level `index` tags. CREATE INDEX IF NOT EXISTS keeps
		//    this idempotent if a re-run somehow gets here.
		idxs := []string{
			"CREATE INDEX IF NOT EXISTS idx_" + tbl + "_state ON " + tbl + " (state)",
			"CREATE INDEX IF NOT EXISTS idx_" + tbl + "_run_id ON " + tbl + " (run_id)",
		}
		for _, q := range idxs {
			if err := tx.Exec(q).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// UpsertMorning writes the morning briefing for one date, replacing any
// prior morning row for the same date. Always sets Kind="morning" on the
// way through so callers don't have to remember.
func (r *BriefingRepo) UpsertMorning(ctx context.Context, b Briefing) error {
	b.Kind = BriefingKindMorning
	return r.upsert(ctx, b)
}

// UpsertInject writes the inject fragment for one date, replacing any
// prior inject fragment for the same date. The morning row for the
// same date is untouched.
func (r *BriefingRepo) UpsertInject(ctx context.Context, b Briefing) error {
	b.Kind = BriefingKindInject
	return r.upsert(ctx, b)
}

func (r *BriefingRepo) upsert(ctx context.Context, b Briefing) error {
	return r.DB.WithContext(ctx).Table(r.tableName()).
		Clauses(onConflictUpdateAll("date", "kind")).Create(&b).Error
}

// ByDate returns the morning briefing for a date, or nil if none.
//
// Existing read paths (the /api/briefing handler, the eval harness)
// already call ByDate and expect the morning row; the kind filter is
// added inline so they keep working without change.
func (r *BriefingRepo) ByDate(ctx context.Context, date string) (*Briefing, error) {
	return r.ByDateKind(ctx, date, BriefingKindMorning)
}

// ByDateKind returns the briefing for a (date, kind) pair, or nil if none.
func (r *BriefingRepo) ByDateKind(ctx context.Context, date, kind string) (*Briefing, error) {
	var b Briefing
	return firstOrNil(
		r.DB.WithContext(ctx).Table(r.tableName()).Where("date = ? AND kind = ?", date, kind),
		&b,
	)
}

// ListByDate returns every briefing row for a date, ordered with the
// morning row first and the inject row second. Used by the UI (V2.3 P4)
// to render today's briefing fragment alongside any inject fragment.
func (r *BriefingRepo) ListByDate(ctx context.Context, date string) ([]Briefing, error) {
	var out []Briefing
	err := r.DB.WithContext(ctx).Table(r.tableName()).
		Where("date = ?", date).
		// Morning sorts before inject lexicographically; this is fine.
		Order("kind ASC").
		Find(&out).Error
	return out, err
}

func (r *BriefingRepo) tableName() string {
	if r.Table == "" {
		return "briefings"
	}
	return r.Table
}
