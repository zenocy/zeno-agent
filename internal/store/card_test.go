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

// TestCardRepo_NewColumnsRoundTrip verifies the V2.x continuity/digest/live
// columns persist and read back losslessly, including nil/empty.
func TestCardRepo_NewColumnsRoundTrip(t *testing.T) {
	db := openTestDB(t)
	repo := &CardRepo{DB: db}
	ctx := context.Background()

	mat := time.Date(2026, 4, 25, 9, 30, 0, 0, time.UTC)
	require.NoError(t, repo.Upsert(ctx, []Card{
		{
			ID: "thread:redline", Date: "2026-04-25", Source: "mail", Rel: "high",
			Title: "Saru · re: redline", CreatedAt: time.Now(),
			EntityKey: "thread:redline", LastMaterialAt: &mat, FirstShownDate: "2026-04-24",
			Items: datatypes.JSON(`[{"title":"item one"}]`),
			Live:  datatypes.JSON(`[{"slot":"meta","kind":"weather","ref":"current"}]`),
		},
		// A row with all new fields at their zero value must persist fine.
		{ID: "plain", Date: "2026-04-25", Source: "mail", Rel: "low", Title: "plain", CreatedAt: time.Now()},
	}))

	got, err := repo.GetByID(ctx, "thread:redline")
	require.NoError(t, err)
	require.Equal(t, "thread:redline", got.EntityKey)
	require.Equal(t, "2026-04-24", got.FirstShownDate)
	require.NotNil(t, got.LastMaterialAt)
	require.True(t, mat.Equal(*got.LastMaterialAt))
	require.JSONEq(t, `[{"title":"item one"}]`, string(got.Items))
	require.JSONEq(t, `[{"slot":"meta","kind":"weather","ref":"current"}]`, string(got.Live))

	plain, err := repo.GetByID(ctx, "plain")
	require.NoError(t, err)
	require.Empty(t, plain.EntityKey)
	require.Nil(t, plain.LastMaterialAt)
	require.Empty(t, plain.FirstShownDate)
}

// TestCardRepo_UpsertWithContinuity verifies the entity-aware upsert:
// FirstShownDate is set once and preserved, LastMaterialAt bumps only on a
// real content change, and user actions (dismissed) survive a re-run so a
// stable entity ID can't resurrect a dismissed card.
func TestCardRepo_UpsertWithContinuity(t *testing.T) {
	db := openTestDB(t)
	repo := &CardRepo{DB: db}
	ctx := context.Background()

	day1 := time.Date(2026, 4, 24, 7, 0, 0, 0, time.UTC)
	require.NoError(t, repo.UpsertWithContinuity(ctx, []Card{
		{ID: "thread:redline", EntityKey: "thread:redline", Date: "2026-04-24", Source: "mail", Rel: "high", Title: "Saru · redline", Sub: "Two questions remain.", CreatedAt: day1},
	}, day1))

	first, err := repo.GetByID(ctx, "thread:redline")
	require.NoError(t, err)
	require.Equal(t, "2026-04-24", first.FirstShownDate)
	require.NotNil(t, first.LastMaterialAt)
	require.True(t, day1.Equal(*first.LastMaterialAt))

	// Idempotent re-run next day, SAME content → FirstShownDate preserved,
	// LastMaterialAt NOT bumped.
	day2 := time.Date(2026, 4, 25, 7, 0, 0, 0, time.UTC)
	require.NoError(t, repo.UpsertWithContinuity(ctx, []Card{
		{ID: "thread:redline", EntityKey: "thread:redline", Date: "2026-04-25", Source: "mail", Rel: "high", Title: "Saru · redline", Sub: "Two questions remain.", CreatedAt: day2},
	}, day2))
	unchanged, err := repo.GetByID(ctx, "thread:redline")
	require.NoError(t, err)
	require.Equal(t, "2026-04-24", unchanged.FirstShownDate, "first-shown must be preserved")
	require.Equal(t, "2026-04-25", unchanged.Date, "date moves forward")
	require.True(t, day1.Equal(*unchanged.LastMaterialAt), "unchanged content must not bump LastMaterialAt")

	// Content change → LastMaterialAt bumps.
	day3 := time.Date(2026, 4, 26, 7, 0, 0, 0, time.UTC)
	require.NoError(t, repo.UpsertWithContinuity(ctx, []Card{
		{ID: "thread:redline", EntityKey: "thread:redline", Date: "2026-04-26", Source: "mail", Rel: "high", Title: "Saru · redline", Sub: "New reply: he agreed to the option pool.", CreatedAt: day3},
	}, day3))
	changed, err := repo.GetByID(ctx, "thread:redline")
	require.NoError(t, err)
	require.True(t, day3.Equal(*changed.LastMaterialAt), "content change must bump LastMaterialAt")

	// Dismiss it, then re-run with same entity ID → must STAY dismissed.
	require.NoError(t, repo.SetDismissed(ctx, "thread:redline"))
	day4 := time.Date(2026, 4, 27, 7, 0, 0, 0, time.UTC)
	require.NoError(t, repo.UpsertWithContinuity(ctx, []Card{
		{ID: "thread:redline", EntityKey: "thread:redline", Date: "2026-04-27", Source: "mail", Rel: "high", Title: "Saru · redline", Sub: "Still open.", CreatedAt: day4},
	}, day4))
	afterDismiss, err := repo.GetByID(ctx, "thread:redline")
	require.NoError(t, err)
	require.True(t, afterDismiss.Dismissed, "a re-run must not resurrect a dismissed entity card")
}

// TestCardRepo_ListByDate_FoldsByEntityKey verifies the cross-source dedup:
// a morning card and an ask card sharing an entity key collapse to one
// (freshest wins), while unanchored rows are never folded.
func TestCardRepo_ListByDate_FoldsByEntityKey(t *testing.T) {
	db := openTestDB(t)
	repo := &CardRepo{DB: db}
	ctx := context.Background()

	morning := time.Date(2026, 4, 25, 7, 0, 0, 0, time.UTC)
	ask := time.Date(2026, 4, 25, 9, 30, 0, 0, time.UTC)
	require.NoError(t, repo.Upsert(ctx, []Card{
		{ID: "thread:redline", EntityKey: "thread:redline", Date: "2026-04-25", Source: "mail", Rel: "med", Title: "Saru · redline (morning)", CreatedAt: morning},
		{ID: "ask-redline", EntityKey: "thread:redline", Date: "2026-04-25", Source: "mail", Rel: "high", Title: "Saru · redline (ask)", CreatedAt: ask},
		{ID: "lone", EntityKey: "", Date: "2026-04-25", Source: "calendar", Rel: "low", Title: "standalone", CreatedAt: morning},
	}))

	rows, err := repo.ListByDate(ctx, "2026-04-25")
	require.NoError(t, err)
	require.Len(t, rows, 2, "the two redline rows must fold to one; the lone card stays")

	var redline *Card
	for i := range rows {
		if rows[i].EntityKey == "thread:redline" {
			redline = &rows[i]
		}
	}
	require.NotNil(t, redline)
	require.Equal(t, "Saru · redline (ask)", redline.Title, "freshest card wins the fold")
}

func TestCardRepo_ListRecentEntities(t *testing.T) {
	db := openTestDB(t)
	repo := &CardRepo{DB: db}
	ctx := context.Background()

	t0 := time.Date(2026, 4, 24, 7, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 4, 25, 7, 0, 0, 0, time.UTC)
	require.NoError(t, repo.Upsert(ctx, []Card{
		// Same entity on two days — only the latest should come back.
		{ID: "thread:redline", EntityKey: "thread:redline", Date: "2026-04-24", Source: "mail", Rel: "high", Title: "old", CreatedAt: t0, FirstShownDate: "2026-04-24"},
		{ID: "cal:gym", EntityKey: "cal:gym", Date: "2026-04-25", Source: "personal", Rel: "low", Title: "Gymnastics", CreatedAt: t1, FirstShownDate: "2026-04-25"},
		// Unanchored row — excluded.
		{ID: "lone", EntityKey: "", Date: "2026-04-25", Source: "calendar", Rel: "low", Title: "lone", CreatedAt: t1},
		// Dismissed — excluded.
		{ID: "cal:dead", EntityKey: "cal:dead", Date: "2026-04-25", Source: "calendar", Rel: "low", Title: "dead", CreatedAt: t1, Dismissed: true},
	}))
	// Update the redline entity on day 2 (same ID, newer date).
	require.NoError(t, repo.Upsert(ctx, []Card{
		{ID: "thread:redline", EntityKey: "thread:redline", Date: "2026-04-25", Source: "mail", Rel: "high", Title: "new", CreatedAt: t1, FirstShownDate: "2026-04-24"},
	}))

	got, err := repo.ListRecentEntities(ctx, "2026-04-20")
	require.NoError(t, err)
	keys := map[string]RecentEntity{}
	for _, e := range got {
		keys[e.EntityKey] = e
	}
	require.Len(t, got, 2, "unanchored and dismissed rows excluded; entity deduped to latest")
	require.Equal(t, "new", keys["thread:redline"].Title, "latest row per entity wins")
	require.Equal(t, "2026-04-24", keys["thread:redline"].FirstShownDate)
	require.Contains(t, keys, "cal:gym")
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

// TestCardRepo_BodyColumn round-trips the Body column for the reactive
// Ask in-app surface. A populated multi-paragraph string must come back
// byte-equal; the zero value must come back as the empty string (no
// default-mangling). The migration adds the column on Migrate(),
// verified indirectly by the successful upsert on a fresh test DB.
func TestCardRepo_BodyColumn(t *testing.T) {
	db := openTestDB(t)
	repo := &CardRepo{DB: db}
	ctx := context.Background()

	body := "Paragraph one with concrete *detail*.\n\nParagraph two adding context.\n\nThird beat ending decisively."
	now := time.Now()
	require.NoError(t, repo.Upsert(ctx, []Card{
		{ID: "ask-body", Date: "2026-04-25", Source: "ask", Rel: "med", Title: "with body", Sub: "sub long enough", Body: body, Origin: "ask", CreatedAt: now},
		{ID: "morning-nobody", Date: "2026-04-25", Source: "mail", Rel: "high", Title: "no body", Sub: "sub long enough", CreatedAt: now},
	}))

	got, err := repo.GetByID(ctx, "ask-body")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, body, got.Body, "Body must round-trip byte-equal")

	empty, err := repo.GetByID(ctx, "morning-nobody")
	require.NoError(t, err)
	require.NotNil(t, empty)
	require.Equal(t, "", empty.Body, "zero-value Body must stay empty — no default-mangling")
}

// TestCardRepo_SourcesColumn round-trips the Sources JSON column.
// Populated entries must survive a save+load; null Sources (the
// common case for non-web ask cards and morning cards) must come
// back as zero-length JSON without erroring.
func TestCardRepo_SourcesColumn(t *testing.T) {
	db := openTestDB(t)
	repo := &CardRepo{DB: db}
	ctx := context.Background()

	sources := datatypes.JSON([]byte(`[{"t":"Reuters: pause","u":"https://reuters.com/iran"},{"t":"Bloomberg","u":"https://bloomberg.com/gulf"}]`))
	now := time.Now()
	require.NoError(t, repo.Upsert(ctx, []Card{
		{ID: "ask-with-sources", Date: "2026-04-25", Source: "ask", Rel: "med", Title: "with sources", Sub: "sub long enough", Sources: sources, Origin: "ask", CreatedAt: now},
		{ID: "ask-no-sources", Date: "2026-04-25", Source: "ask", Rel: "med", Title: "no sources", Sub: "sub long enough", Origin: "ask", CreatedAt: now},
	}))

	got, err := repo.GetByID(ctx, "ask-with-sources")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.JSONEq(t, string(sources), string(got.Sources), "Sources JSON must round-trip byte-equivalently")

	empty, err := repo.GetByID(ctx, "ask-no-sources")
	require.NoError(t, err)
	require.NotNil(t, empty)
	require.Len(t, empty.Sources, 0, "missing Sources must come back as zero-length JSON, not error")
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

// Unexpired ask cards must appear on the main rail alongside morning
// cards on their date; expired (and legacy NULL-ExpiresAt) ask cards
// must not. Together these pin the V2.x ask-card persistence window.
func TestCardRepo_ListByDate_AskCardExpiry(t *testing.T) {
	db := openTestDB(t)
	repo := &CardRepo{DB: db}
	ctx := context.Background()

	today := "2026-05-18"
	past := time.Now().Add(-1 * time.Hour)
	future := time.Now().Add(1 * time.Hour)
	require.NoError(t, repo.Upsert(ctx, []Card{
		{ID: "morning-mail", Date: today, Source: "mail", Rel: "high", Title: "morning", CreatedAt: time.Now()},
		{ID: "ask-fresh", Date: today, Source: "ask", Origin: "ask", Rel: "med", Title: "fresh ask", CreatedAt: time.Now(), ExpiresAt: &future},
		{ID: "ask-old", Date: today, Source: "ask", Origin: "ask", Rel: "med", Title: "old ask", CreatedAt: time.Now(), ExpiresAt: &past},
		{ID: "ask-legacy", Date: today, Source: "ask", Origin: "ask", Rel: "med", Title: "legacy ask (no expires_at)", CreatedAt: time.Now()},
	}))

	rows, err := repo.ListByDate(ctx, today)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, r := range rows {
		ids[r.ID] = true
	}
	require.True(t, ids["morning-mail"], "non-ask cards always show")
	require.True(t, ids["ask-fresh"], "ask card with future expires_at must surface on the main rail")
	require.False(t, ids["ask-old"], "ask card with past expires_at must NOT surface on the main rail")
	require.False(t, ids["ask-legacy"], "ask card with NULL expires_at (legacy pre-TTL row) must NOT surface — treat NULL as already expired")
}

// ListAllByDate is the archive query — no visibility filters at all.
// Every row for the date must come back regardless of dismissed,
// snoozed, or ask-expiry state, ordered newest-first.
func TestCardRepo_ListAllByDate(t *testing.T) {
	db := openTestDB(t)
	repo := &CardRepo{DB: db}
	ctx := context.Background()

	today := "2026-05-18"
	past := time.Now().Add(-1 * time.Hour)
	t0 := time.Now()
	t1 := t0.Add(1 * time.Millisecond)
	t2 := t0.Add(2 * time.Millisecond)
	t3 := t0.Add(3 * time.Millisecond)
	require.NoError(t, repo.Upsert(ctx, []Card{
		{ID: "morning", Date: today, Source: "mail", Rel: "high", Title: "morning", CreatedAt: t0},
		{ID: "dismissed", Date: today, Source: "calendar", Rel: "med", Title: "dismissed", Dismissed: true, CreatedAt: t1},
		{ID: "snoozed", Date: today, Source: "tasks", Rel: "low", Title: "snoozed", SnoozedDate: today, CreatedAt: t2},
		{ID: "ask-old", Date: today, Source: "ask", Origin: "ask", Rel: "med", Title: "expired ask", CreatedAt: t3, ExpiresAt: &past},
	}))
	// A row on a different date must NOT come back for today's archive.
	require.NoError(t, repo.Upsert(ctx, []Card{
		{ID: "yesterday", Date: "2026-05-17", Source: "mail", Rel: "high", Title: "yesterday", CreatedAt: t0},
	}))

	rows, err := repo.ListAllByDate(ctx, today)
	require.NoError(t, err)
	require.Len(t, rows, 4, "archive returns every today-row regardless of filters")
	// CreatedAt DESC → newest first.
	require.Equal(t, "ask-old", rows[0].ID)
	require.Equal(t, "snoozed", rows[1].ID)
	require.Equal(t, "dismissed", rows[2].ID)
	require.Equal(t, "morning", rows[3].ID)
}

// Migrate must add the expires_at column on a fresh DB so AutoMigrate
// rollouts onto existing zeno installs pick up the new schema cleanly.
func TestCardRepo_Migrate_AddsExpiresAtColumn(t *testing.T) {
	db := openTestDB(t)

	type col struct {
		Cid     int
		Name    string
		Type    string
		NotNull int `gorm:"column:notnull"`
		Dflt    *string
		Pk      int
	}
	var cols []col
	require.NoError(t, db.Raw("PRAGMA table_info(cards)").Scan(&cols).Error)

	names := map[string]bool{}
	for _, c := range cols {
		names[c.Name] = true
	}
	require.True(t, names["expires_at"], "cards table must have expires_at column after Migrate")
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
