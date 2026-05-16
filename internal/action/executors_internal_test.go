package action

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/llm"
	logp "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/store"
	"github.com/zenocy/zeno-v2/internal/synth"
)

func openInternalTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_journal_mode=WAL&_foreign_keys=on"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, (&store.ConcernRepo{DB: db}).Migrate())
	require.NoError(t, (&store.MemoryRepo{DB: db}).Migrate())
	return db
}

// ----------------------------------------------------------------------
// AddConcernExec
// ----------------------------------------------------------------------

func TestAddConcern_PersistsActive(t *testing.T) {
	db := openInternalTestDB(t)
	repo := &store.ConcernRepo{DB: db}
	ex := &AddConcernExec{Concerns: repo, Now: func() time.Time { return time.Date(2026, 5, 7, 8, 0, 0, 0, time.UTC) }}
	require.Equal(t, Mode1Click, ex.Mode())

	res, err := ex.Execute(context.Background(), ExecCtx{
		Target: map[string]any{"name": "Series B narrative", "description": "Cohort table for slide 14."},
	})
	require.NoError(t, err)
	require.True(t, res.OK)
	require.Equal(t, logp.KindConcernAddedViaAction, res.EventKind)

	rows, err := repo.ListActive(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "Series B narrative", rows[0].Name)
	require.Equal(t, store.ConcernSourceUser, rows[0].Source)
}

func TestAddConcern_DedupsByNormName(t *testing.T) {
	db := openInternalTestDB(t)
	repo := &store.ConcernRepo{DB: db}
	ex := &AddConcernExec{Concerns: repo, Now: time.Now}

	res1, err := ex.Execute(context.Background(), ExecCtx{
		Target: map[string]any{"name": "Series B"},
	})
	require.NoError(t, err)
	require.True(t, res1.OK)

	res2, err := ex.Execute(context.Background(), ExecCtx{
		Target: map[string]any{"name": "  series b  "}, // normalizes to same
	})
	require.NoError(t, err)
	require.True(t, res2.OK)
	require.Contains(t, res2.Toast, "Already tracked")

	rows, err := repo.ListActive(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 1)
}

func TestAddConcern_FromCardWhenTargetMissing(t *testing.T) {
	db := openInternalTestDB(t)
	repo := &store.ConcernRepo{DB: db}
	ex := &AddConcernExec{Concerns: repo, Now: time.Now}

	res, err := ex.Execute(context.Background(), ExecCtx{
		Card: &store.Card{Title: "Pricing redline", Sub: "Two questions remain."},
	})
	require.NoError(t, err)
	require.True(t, res.OK)
	rows, err := repo.ListActive(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "Pricing redline", rows[0].Name)
}

// ----------------------------------------------------------------------
// AddMemoryExec
// ----------------------------------------------------------------------

func TestAddMemory_PersistsHighConfidence(t *testing.T) {
	db := openInternalTestDB(t)
	repo := &store.MemoryRepo{DB: db}
	ex := &AddMemoryExec{Memory: repo, Now: func() time.Time { return time.Date(2026, 5, 7, 8, 0, 0, 0, time.UTC) }}
	require.Equal(t, Mode1Click, ex.Mode())

	res, err := ex.Execute(context.Background(), ExecCtx{
		Target: map[string]any{"subject": "commute", "fact": "30 minutes by bike from home to office.", "category": "logistics"},
	})
	require.NoError(t, err)
	require.True(t, res.OK)
	require.Equal(t, logp.KindMemoryAddedViaAction, res.EventKind)

	rows, err := repo.ListAllVisible(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "commute", rows[0].Subject)
	require.Equal(t, "user", rows[0].Source)
	require.Equal(t, "high", rows[0].Confidence)
}

func TestAddMemory_DedupsBySubject(t *testing.T) {
	db := openInternalTestDB(t)
	repo := &store.MemoryRepo{DB: db}
	ex := &AddMemoryExec{Memory: repo, Now: time.Now}

	_, err := ex.Execute(context.Background(), ExecCtx{
		Target: map[string]any{"subject": "commute", "fact": "30 minutes."},
	})
	require.NoError(t, err)

	res2, err := ex.Execute(context.Background(), ExecCtx{
		Target: map[string]any{"subject": "Commute", "fact": "Different note."},
	})
	require.NoError(t, err)
	require.True(t, res2.OK)
	require.Contains(t, res2.Toast, "Already remembered")

	rows, err := repo.ListAllVisible(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 1)
}

// openMemoryLinkTestDB extends the base test DB with the CardDAV +
// MemoryContactLink tables so the V2.13 link path can be exercised.
func openMemoryLinkTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := openInternalTestDB(t)
	require.NoError(t, (&store.CardDAVRepo{DB: db}).Migrate())
	require.NoError(t, (&store.ContactLinkRepo{DB: db}).Migrate())
	return db
}

// V2.13: when add_memory's subject matches exactly one CardDAV
// contact, the executor must (a) create a MemoryContactLink keyed
// to that vCard's UID, (b) override the LLM-passed category to
// MemoryCategoryContactWhatsApp so the WhatsApp Resolver finds the
// fact, and (c) use the deterministic BuildContactID for the row.
// The pre-fix gap was that this linking never happened — facts about
// known contacts were orphaned and resolve_contact couldn't find them.
func TestAddMemory_LinksWhenCardDAVMatches(t *testing.T) {
	db := openMemoryLinkTestDB(t)
	memRepo := &store.MemoryRepo{DB: db}
	linkRepo := &store.ContactLinkRepo{DB: db}
	cardRepo := &store.CardDAVRepo{DB: db}

	require.NoError(t, cardRepo.Upsert(context.Background(), store.CardDAVContact{
		UID:         "vcard-dana",
		DisplayName: "Dana Lopez",
	}))

	ex := &AddMemoryExec{
		Memory:  memRepo,
		Link:    linkRepo,
		CardDAV: cardRepo,
		Now:     func() time.Time { return time.Date(2026, 5, 10, 11, 0, 0, 0, time.UTC) },
	}

	res, err := ex.Execute(context.Background(), ExecCtx{
		Target: map[string]any{
			"subject":  "dana lopez",
			"fact":     "best man",
			"category": "people", // intentionally wrong — must be overridden
		},
	})
	require.NoError(t, err)
	require.True(t, res.OK, res.Toast)
	require.Contains(t, res.Toast, "linked to Dana Lopez")
	require.Equal(t, "vcard-dana", res.EventPayload["carddav_uid"])

	rows, err := memRepo.ListAllVisible(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, store.MemoryCategoryContactWhatsApp, rows[0].Category,
		"category must be overridden to contact_whatsapp so the Resolver finds the fact")
	require.True(t, len(rows[0].ID) > 3 && rows[0].ID[:3] == "ct-",
		"linked memory must use deterministic BuildContactID (got %q)", rows[0].ID)

	link, err := linkRepo.GetByID(t.Context(), rows[0].ID)
	require.NoError(t, err)
	require.NotNil(t, link, "MemoryContactLink must exist with same ID as MemoryFact")
	require.Equal(t, "vcard-dana", link.CardDAVUID)
	require.Equal(t, store.ChannelWhatsApp, link.Channel, "ValidateLink must default channel")
}

// Counterpart: the no-match case must preserve every aspect of the
// pre-V2.13 behavior — UUID-style ID, no link row, category
// untouched. Otherwise non-people facts ("morning routine") would
// pay the linking-path tax.
func TestAddMemory_NoLinkWhenNoCardDAVMatch(t *testing.T) {
	db := openMemoryLinkTestDB(t)
	memRepo := &store.MemoryRepo{DB: db}
	linkRepo := &store.ContactLinkRepo{DB: db}
	cardRepo := &store.CardDAVRepo{DB: db}

	// Seed an unrelated contact so Search isn't trivially empty.
	require.NoError(t, cardRepo.Upsert(context.Background(), store.CardDAVContact{
		UID:         "vcard-alex",
		DisplayName: "Alex Smith",
	}))

	ex := &AddMemoryExec{Memory: memRepo, Link: linkRepo, CardDAV: cardRepo, Now: time.Now}

	res, err := ex.Execute(context.Background(), ExecCtx{
		Target: map[string]any{"subject": "morning routine", "fact": "starts at 6:30", "category": "logistics"},
	})
	require.NoError(t, err)
	require.True(t, res.OK)
	require.NotContains(t, res.Toast, "linked")

	rows, err := memRepo.ListAllVisible(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "logistics", rows[0].Category, "non-matching subjects must keep the LLM-passed category")
	require.False(t, len(rows[0].ID) > 3 && rows[0].ID[:3] == "ct-",
		"unlinked memory must NOT use BuildContactID (got %q)", rows[0].ID)

	links, err := linkRepo.ListAll(t.Context())
	require.NoError(t, err)
	require.Empty(t, links, "no link row should be created when no CardDAV contact matches")
}

// Defensive: when two contacts share the same display name (e.g. two
// "Sam"s), we can't tell which the user meant. Skip the link rather
// than guess. The user can disambiguate later via the Settings UI.
func TestAddMemory_NoLinkWhenAmbiguousCardDAVMatch(t *testing.T) {
	db := openMemoryLinkTestDB(t)
	memRepo := &store.MemoryRepo{DB: db}
	linkRepo := &store.ContactLinkRepo{DB: db}
	cardRepo := &store.CardDAVRepo{DB: db}

	for _, c := range []store.CardDAVContact{
		{UID: "vcard-sam-1", DisplayName: "Sam"},
		{UID: "vcard-sam-2", DisplayName: "Sam"},
	} {
		require.NoError(t, cardRepo.Upsert(context.Background(), c))
	}

	ex := &AddMemoryExec{Memory: memRepo, Link: linkRepo, CardDAV: cardRepo, Now: time.Now}

	res, err := ex.Execute(context.Background(), ExecCtx{
		Target: map[string]any{"subject": "sam", "fact": "loves espresso"},
	})
	require.NoError(t, err)
	require.True(t, res.OK)
	require.NotContains(t, res.Toast, "linked")

	links, err := linkRepo.ListAll(t.Context())
	require.NoError(t, err)
	require.Empty(t, links, "ambiguous CardDAV match must skip linking")
}

// V2.13 compatibility: when Link/CardDAV are nil (current eval/replay
// paths and the existing tests above), the executor must behave
// byte-equal to v1 — UUID id, no link row, category preserved.
func TestAddMemory_NilDepsDegradesGracefully(t *testing.T) {
	db := openMemoryLinkTestDB(t)
	memRepo := &store.MemoryRepo{DB: db}

	ex := &AddMemoryExec{Memory: memRepo, Now: time.Now} // Link=nil, CardDAV=nil

	res, err := ex.Execute(context.Background(), ExecCtx{
		Target: map[string]any{"subject": "dana lopez", "fact": "best man"},
	})
	require.NoError(t, err)
	require.True(t, res.OK)
	require.NotContains(t, res.Toast, "linked")

	rows, err := memRepo.ListAllVisible(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.False(t, len(rows[0].ID) > 3 && rows[0].ID[:3] == "ct-",
		"nil-deps path must NOT use BuildContactID")
	require.Equal(t, "general", rows[0].Category, "category default preserved when no resolver wired")
}

// ----------------------------------------------------------------------
// AskFollowupExec
// ----------------------------------------------------------------------

func TestAskFollowup_ReturnsCardOnSuccess(t *testing.T) {
	called := false
	ex := &AskFollowupExec{
		AskFn: func(_ context.Context, query string) (synth.Card, llm.Trace, []llm.MemoryCandidate, error) {
			called = true
			require.Contains(t, query, "Saru Patel")
			return synth.Card{
				ID: "ask-x", Title: "Reply suggested",
				Sub:    "Acknowledge and ask Lin to weigh in.",
				Source: "ask", Rel: "med",
			}, llm.Trace{}, nil, nil
		},
	}
	require.Equal(t, Mode1Click, ex.Mode())

	res, err := ex.Execute(context.Background(), ExecCtx{
		Card:   &store.Card{Title: "Saru Patel · re: redline", Sub: "Two questions remain."},
		Target: map[string]any{"seed": "Draft a reply that defers"},
	})
	require.NoError(t, err)
	require.True(t, res.OK)
	require.True(t, called)
	require.NotNil(t, res.Followup)
	require.Equal(t, "Reply suggested", res.Followup.Title)
}

func TestAskFollowup_DegradedAskStillSucceeds(t *testing.T) {
	ex := &AskFollowupExec{
		AskFn: func(_ context.Context, _ string) (synth.Card, llm.Trace, []llm.MemoryCandidate, error) {
			return synth.Card{ID: "", Title: "Degraded", Sub: "...", Source: "ask", Rel: "low"}, llm.Trace{}, nil, errors.New("timeout")
		},
	}
	res, err := ex.Execute(context.Background(), ExecCtx{
		Target: map[string]any{"seed": "anything"},
	})
	require.NoError(t, err)
	require.True(t, res.OK, "ask_followup must surface the degraded card rather than fail")
	require.NotNil(t, res.Followup)
	require.NotEmpty(t, res.Followup.ID)
}

// ----------------------------------------------------------------------
// AddTaskExec
// ----------------------------------------------------------------------

func TestAddTask_InsertsRow(t *testing.T) {
	repo := newTaskRepo(t)

	ex := &AddTaskExec{Tasks: repo}
	require.Equal(t, Mode1Click, ex.Mode())

	res, err := ex.Execute(context.Background(), ExecCtx{
		Target: map[string]any{
			"title":    "Renew passport",
			"due":      "2026-06-01",
			"priority": "high",
			"tags":     []any{"admin", "#personal"}, // mixed-case + leading #
		},
	})
	require.NoError(t, err)
	require.True(t, res.OK)
	require.Equal(t, logp.KindTaskAddedViaAction, res.EventKind)
	uid, _ := res.EventPayload["uid"].(string)
	require.NotEmpty(t, uid)

	got, err := repo.Get(context.Background(), uid)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "Renew passport", got.Title)
	require.Equal(t, "2026-06-01", got.DueDate)
	require.Equal(t, "high", got.Priority)
	require.Nil(t, got.FireAt)
	var tags []string
	require.NoError(t, json.Unmarshal(got.Tags, &tags))
	require.Equal(t, []string{"admin", "personal"}, tags)
}

// TestAddTask_FireAt covers the V2.11 alarm path. fire_at can be RFC3339
// or relative ("+30m") — store.ParseRelative is the source of truth.
func TestAddTask_FireAt(t *testing.T) {
	repo := newTaskRepo(t)
	ex := &AddTaskExec{Tasks: repo, Now: fixedNow("2026-05-09 12:00:00")}

	res, err := ex.Execute(context.Background(), ExecCtx{
		Target: map[string]any{
			"title":   "kettle",
			"fire_at": "+30m",
		},
	})
	require.NoError(t, err)
	require.True(t, res.OK)
	uid := res.EventPayload["uid"].(string)

	got, err := repo.Get(context.Background(), uid)
	require.NoError(t, err)
	require.NotNil(t, got.FireAt)
	require.Equal(t, "2026-05-09T12:30:00Z", got.FireAt.UTC().Format(time.RFC3339))
}

func TestAddTask_FireAt_BadShape(t *testing.T) {
	repo := newTaskRepo(t)
	ex := &AddTaskExec{Tasks: repo, Now: fixedNow("2026-05-09 12:00:00")}

	res, err := ex.Execute(context.Background(), ExecCtx{
		Target: map[string]any{"title": "x", "fire_at": "tomorrow"},
	})
	require.NoError(t, err)
	require.False(t, res.OK)
	require.Contains(t, res.Toast, "fire_at")
}

func TestAddTask_NoStoreFails(t *testing.T) {
	ex := &AddTaskExec{}
	res, err := ex.Execute(context.Background(), ExecCtx{
		Target: map[string]any{"title": "x"},
	})
	require.NoError(t, err)
	require.False(t, res.OK)
	require.Contains(t, res.Toast, "not configured")
}

func TestAddTask_FromCardWhenTitleMissing(t *testing.T) {
	repo := newTaskRepo(t)
	ex := &AddTaskExec{Tasks: repo}
	res, err := ex.Execute(context.Background(), ExecCtx{
		Card: &store.Card{ID: "card-1", Title: "Pay invoice"},
	})
	require.NoError(t, err)
	require.True(t, res.OK)
	uid := res.EventPayload["uid"].(string)

	got, err := repo.Get(context.Background(), uid)
	require.NoError(t, err)
	require.Equal(t, "Pay invoice", got.Title)
	require.Equal(t, "card-1", got.SourceCardID, "card-bound add must preserve the source linkback")
}

func TestAskFollowup_RequiresSeedOrCard(t *testing.T) {
	ex := &AskFollowupExec{AskFn: func(_ context.Context, _ string) (synth.Card, llm.Trace, []llm.MemoryCandidate, error) {
		return synth.Card{}, llm.Trace{}, nil, nil
	}}
	res, err := ex.Execute(context.Background(), ExecCtx{})
	require.NoError(t, err)
	require.False(t, res.OK)
}
