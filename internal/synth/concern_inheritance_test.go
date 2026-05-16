package synth

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/projection"
	"github.com/zenocy/zeno-v2/internal/store"
)

// inheritanceTestRig sets up the minimum store + repos needed to
// exercise ResolveCardConcern: an in-memory SQLite with concerns +
// concern_observations migrated. No LLM, no log reader — Phase 3
// inheritance is entirely store-side.
type inheritanceTestRig struct {
	DB       *gorm.DB
	Concerns *store.ConcernRepo
	Tags     *store.ConcernObservationRepo
}

func newInheritanceRig(t *testing.T) *inheritanceTestRig {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	c := &store.ConcernRepo{DB: db, Table: "concerns"}
	o := &store.ConcernObservationRepo{DB: db, Table: "concern_observations"}
	require.NoError(t, c.Migrate())
	require.NoError(t, o.Migrate())
	return &inheritanceTestRig{DB: db, Concerns: c, Tags: o}
}

func (r *inheritanceTestRig) seedConcern(t *testing.T, id, name, state string) {
	t.Helper()
	now := time.Now()
	require.NoError(t, r.Concerns.Insert(context.Background(), store.Concern{
		ID: id, Name: name, NormName: store.NormalizeConcernName(name),
		Description: name, State: state, Source: store.ConcernSourceUser,
		LastActiveAt: now, CreatedAt: now, UpdatedAt: now, Confidence: 1,
	}))
}

func (r *inheritanceTestRig) tag(t *testing.T, concernID, eventID string) {
	t.Helper()
	require.NoError(t, r.Tags.Tag(context.Background(), store.ConcernObservation{
		ConcernID: concernID, EventID: eventID, Source: store.ConcernTagSourceUser,
	}))
}

// TestResolveCardConcern_CalendarCardByUID pins the happy path of the
// only inheritance route Phase 3 ships: a calendar card whose SrcLabel
// matches a calendar event's title gets the event's tagged concern_id.
// This is what makes the meeting-prep card carry concern context onto
// the review surface.
func TestResolveCardConcern_CalendarCardByUID(t *testing.T) {
	rig := newInheritanceRig(t)
	rig.seedConcern(t, "c-frank", "Frankfurt trip", store.ConcernStateActive)
	rig.tag(t, "c-frank", "evt-frankfurt-1")

	cal := []projection.CalendarEvent{
		{UID: "evt-frankfurt-1", Title: "Frankfurt — Heim review (kickoff)"},
		{UID: "evt-other-1", Title: "1:1 — Lin"},
	}
	card := Card{Source: "calendar", SrcLabel: "Frankfurt — Heim review (kickoff)"}

	got := ResolveCardConcern(context.Background(), rig.Concerns, rig.Tags, card, cal, nil, quietEntry())
	require.NotNil(t, got)
	require.Equal(t, "c-frank", *got)
}

// TestResolveCardConcern_AbbreviatedTitleStillMatches covers the model's
// common abbreviation pattern — a card titled "Frankfurt" must still
// resolve to a calendar event titled "Frankfurt — Heim review". The
// either-direction substring check is the load-bearing affordance.
func TestResolveCardConcern_AbbreviatedTitleStillMatches(t *testing.T) {
	rig := newInheritanceRig(t)
	rig.seedConcern(t, "c-frank", "Frankfurt trip", store.ConcernStateActive)
	rig.tag(t, "c-frank", "evt-frankfurt-1")
	cal := []projection.CalendarEvent{
		{UID: "evt-frankfurt-1", Title: "Frankfurt — Heim review (kickoff)"},
	}
	card := Card{Source: "calendar", SrcLabel: "Frankfurt"}

	got := ResolveCardConcern(context.Background(), rig.Concerns, rig.Tags, card, cal, nil, quietEntry())
	require.NotNil(t, got)
	require.Equal(t, "c-frank", *got)
}

// TestResolveCardConcern_TerminalConcernsSkipped pins the contract that
// a merged or ended concern never bleeds into a fresh card —
// `ConcernsForEvent` filters those, and the inheritance pass returns
// nil cleanly. Without this, a tombstone could resurface in the cards
// surface after a user-initiated end/merge.
func TestResolveCardConcern_TerminalConcernsSkipped(t *testing.T) {
	rig := newInheritanceRig(t)
	rig.seedConcern(t, "c-old", "Old trip", store.ConcernStateActive)
	rig.tag(t, "c-old", "evt-1")
	// Transition to ended.
	require.NoError(t, rig.Concerns.Transition(context.Background(), "c-old", store.ConcernStateEnded))

	cal := []projection.CalendarEvent{{UID: "evt-1", Title: "Old trip review"}}
	card := Card{Source: "calendar", SrcLabel: "Old trip review"}

	got := ResolveCardConcern(context.Background(), rig.Concerns, rig.Tags, card, cal, nil, quietEntry())
	require.Nil(t, got, "terminal concern must not propagate to the card")
}

// TestResolveCardConcern_NonCalendarSourceReturnsNil pins the deferred
// inheritance paths. Phase 3 only resolves calendar cards; mail / personal
// / tasks / ask all return nil. This is the regression gate against
// silently shipping mail-card inheritance via subject substring before
// the resolution path is sound.
func TestResolveCardConcern_NonCalendarSourceReturnsNil(t *testing.T) {
	rig := newInheritanceRig(t)
	rig.seedConcern(t, "c-1", "Construction", store.ConcernStateActive)
	rig.tag(t, "c-1", "evt-1")
	cal := []projection.CalendarEvent{{UID: "evt-1", Title: "Construction kickoff"}}

	for _, src := range []string{"mail", "personal", "tasks", "ask"} {
		card := Card{Source: src, SrcLabel: "Construction kickoff"}
		got := ResolveCardConcern(context.Background(), rig.Concerns, rig.Tags, card, cal, nil, quietEntry())
		require.Nilf(t, got, "source=%s should not inherit a concern", src)
	}
}

// TestResolveCardConcern_EmptySrcLabelReturnsNil pins input validation:
// a card whose model output left SrcLabel blank must NOT match the
// first event by accident.
func TestResolveCardConcern_EmptySrcLabelReturnsNil(t *testing.T) {
	rig := newInheritanceRig(t)
	rig.seedConcern(t, "c-1", "X", store.ConcernStateActive)
	rig.tag(t, "c-1", "evt-1")
	cal := []projection.CalendarEvent{{UID: "evt-1", Title: "Anything"}}

	card := Card{Source: "calendar", SrcLabel: "   "}
	got := ResolveCardConcern(context.Background(), rig.Concerns, rig.Tags, card, cal, nil, quietEntry())
	require.Nil(t, got)
}

// TestResolveCardConcern_NoMatchingEventReturnsNil pins the silent-fail
// path: the card's SrcLabel doesn't match any calendar event → nil
// without an error. The inheritance pass is always best-effort and
// must never break the cards persist transaction.
func TestResolveCardConcern_NoMatchingEventReturnsNil(t *testing.T) {
	rig := newInheritanceRig(t)
	rig.seedConcern(t, "c-1", "X", store.ConcernStateActive)
	rig.tag(t, "c-1", "evt-1")
	cal := []projection.CalendarEvent{{UID: "evt-1", Title: "Different event"}}

	card := Card{Source: "calendar", SrcLabel: "Unrelated thing"}
	got := ResolveCardConcern(context.Background(), rig.Concerns, rig.Tags, card, cal, nil, quietEntry())
	require.Nil(t, got)
}

// TestResolveCardConcern_NilReposReturnsNilCleanly pins the V2.4-compat
// path: callers without concerns wiring (eval-only, replay-only) get
// nil silently. No panic, no error.
func TestResolveCardConcern_NilReposReturnsNilCleanly(t *testing.T) {
	cal := []projection.CalendarEvent{{UID: "evt-1", Title: "X"}}
	card := Card{Source: "calendar", SrcLabel: "X"}

	got := ResolveCardConcern(context.Background(), nil, nil, card, cal, nil, quietEntry())
	require.Nil(t, got)
}

func quietEntry() *logrus.Entry {
	l := logrus.New()
	l.Out = nil
	return logrus.NewEntry(l)
}
