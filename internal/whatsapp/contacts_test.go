package whatsapp

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/store"
)

func openResolverDB(t *testing.T) (*store.MemoryRepo, *store.ContactLinkRepo, *store.CardDAVRepo) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	m := &store.MemoryRepo{DB: db}
	require.NoError(t, m.Migrate())
	l := &store.ContactLinkRepo{DB: db}
	require.NoError(t, l.Migrate())
	c := &store.CardDAVRepo{DB: db}
	require.NoError(t, c.Migrate())
	return m, l, c
}

func seedContact(t *testing.T, m *store.MemoryRepo, l *store.ContactLinkRepo, id, subject, fact, cardDAVUID, jid string) {
	t.Helper()
	now := time.Now()
	require.NoError(t, m.Upsert(context.Background(), []store.MemoryFact{
		{
			ID:             id,
			Subject:        subject,
			Fact:           fact,
			Category:       store.MemoryCategoryContactWhatsApp,
			Confidence:     "high",
			Source:         "user",
			EvidenceCount:  1,
			FirstSeen:      now,
			LastReinforced: now,
		},
	}))
	link := store.MemoryContactLink{ID: id}
	if cardDAVUID != "" {
		link.CardDAVUID = cardDAVUID
	}
	if jid != "" {
		link.JID = jid
	}
	require.NoError(t, l.Insert(context.Background(), link))
}

func seedVCard(t *testing.T, c *store.CardDAVRepo, uid, name string, phones []store.Phone) {
	t.Helper()
	require.NoError(t, c.Upsert(context.Background(), store.CardDAVContact{
		UID:         uid,
		Href:        "/p/" + uid + ".vcf",
		DisplayName: name,
		Phones:      store.EncodePhones(phones),
		LastSyncAt:  time.Now(),
	}))
}

func TestResolver_ExactMemorySubject(t *testing.T) {
	m, l, c := openResolverDB(t)
	seedVCard(t, c, "vc-sam", "Sam Carter", []store.Phone{{Value: "+447700900111", Pref: 1}})
	seedContact(t, m, l, "fact-sam", "sam carter", "Wife. Aliases: Sam, partner, my wife.", "vc-sam", "")

	r := &Resolver{Memory: m, Link: l, CardDAV: c}
	got, err := r.Resolve(context.Background(), "Sam Carter")
	require.NoError(t, err)
	require.Equal(t, "sam carter", got.Name)
	require.Equal(t, "447700900111@s.whatsapp.net", got.JID)
	require.False(t, got.IsGroup)
}

func TestResolver_AliasInFactText(t *testing.T) {
	m, l, c := openResolverDB(t)
	seedVCard(t, c, "vc-sam", "Sam Carter", []store.Phone{{Value: "+447700900222"}})
	seedContact(t, m, l, "fact-sam", "sam carter", "Wife. Aliases: Sam, partner, my wife.", "vc-sam", "")

	r := &Resolver{Memory: m, Link: l, CardDAV: c}
	got, err := r.Resolve(context.Background(), "my wife")
	require.NoError(t, err)
	require.Equal(t, "sam carter", got.Name)
	require.Equal(t, "447700900222@s.whatsapp.net", got.JID)
}

func TestResolver_GroupJID(t *testing.T) {
	m, l, _ := openResolverDB(t)
	seedContact(t, m, l, "fact-fam", "family group", "Living-room family chat.", "", "120001@g.us")

	r := &Resolver{Memory: m, Link: l}
	got, err := r.Resolve(context.Background(), "family group")
	require.NoError(t, err)
	require.True(t, got.IsGroup)
	require.Equal(t, "120001@g.us", got.JID)
}

func TestResolver_AmbiguousMemoryMatch(t *testing.T) {
	m, l, c := openResolverDB(t)
	seedVCard(t, c, "vc-1", "Sam Carter", []store.Phone{{Value: "1"}})
	seedVCard(t, c, "vc-2", "Sam Other", []store.Phone{{Value: "2"}})
	seedContact(t, m, l, "fact-sam-1", "sam carter", "wife.", "vc-1", "")
	seedContact(t, m, l, "fact-sam-2", "sam other", "another sam.", "vc-2", "")

	r := &Resolver{Memory: m, Link: l, CardDAV: c}
	_, err := r.Resolve(context.Background(), "sam")
	require.Error(t, err)
	var amb *ErrAmbiguous
	require.True(t, errors.As(err, &amb))
	require.Len(t, amb.Candidates, 2)
}

func TestResolver_NotFound(t *testing.T) {
	m, l, c := openResolverDB(t)
	r := &Resolver{Memory: m, Link: l, CardDAV: c}
	_, err := r.Resolve(context.Background(), "nobody")
	require.Error(t, err)
	var nf *ErrContactNotFound
	require.True(t, errors.As(err, &nf))
}

func TestResolver_CardDAVDirectHitWhenNoMemoryFact(t *testing.T) {
	m, l, c := openResolverDB(t)
	seedVCard(t, c, "vc-ad", "Jamie Reyes", []store.Phone{{Value: "+447700900333"}})

	r := &Resolver{Memory: m, Link: l, CardDAV: c}
	got, err := r.Resolve(context.Background(), "jamie reyes")
	require.NoError(t, err)
	require.Equal(t, "Jamie Reyes", got.Name)
	require.Equal(t, "447700900333@s.whatsapp.net", got.JID)
	require.Equal(t, "vc-ad", got.CardDAVUID)
	require.Empty(t, got.FactID, "direct CardDAV hits have no fact id")
}

func TestResolver_MemoryFactWinsOverCardDAVDirect(t *testing.T) {
	m, l, c := openResolverDB(t)
	seedVCard(t, c, "vc-real", "Sam Carter", []store.Phone{{Value: "+44 1"}})
	seedVCard(t, c, "vc-other", "Sam Other", []store.Phone{{Value: "+44 2"}})
	// Memory fact pinned to vc-real even though "sam" matches both.
	seedContact(t, m, l, "fact-sam", "sam", "wife.", "vc-real", "")

	r := &Resolver{Memory: m, Link: l, CardDAV: c}
	got, err := r.Resolve(context.Background(), "sam")
	require.NoError(t, err)
	require.Equal(t, "vc-real", got.CardDAVUID)
}

func TestResolver_AmbiguousCardDAVDirect(t *testing.T) {
	m, l, c := openResolverDB(t)
	seedVCard(t, c, "vc-1", "Sam Carter", []store.Phone{{Value: "1"}})
	seedVCard(t, c, "vc-2", "Sam Other", []store.Phone{{Value: "2"}})

	r := &Resolver{Memory: m, Link: l, CardDAV: c}
	_, err := r.Resolve(context.Background(), "sam")
	var amb *ErrAmbiguous
	require.True(t, errors.As(err, &amb))
}

func TestResolver_PreferredPhoneOverride(t *testing.T) {
	m, l, c := openResolverDB(t)
	seedVCard(t, c, "vc-sam", "Sam Carter", []store.Phone{
		{Value: "+447700900111"},
		{Value: "+447700900222", Pref: 1}, // PREF would win normally
	})
	// Link override picks the non-PREF phone.
	require.NoError(t, m.Upsert(context.Background(), []store.MemoryFact{{
		ID:             "fact-sam",
		Subject:        "sam carter",
		Fact:           "wife.",
		Category:       store.MemoryCategoryContactWhatsApp,
		Confidence:     "high",
		Source:         "user",
		EvidenceCount:  1,
		FirstSeen:      time.Now(),
		LastReinforced: time.Now(),
	}}))
	require.NoError(t, l.Insert(context.Background(), store.MemoryContactLink{
		ID:             "fact-sam",
		CardDAVUID:     "vc-sam",
		PreferredPhone: "+447700900111",
	}))

	r := &Resolver{Memory: m, Link: l, CardDAV: c}
	got, err := r.Resolve(context.Background(), "sam carter")
	require.NoError(t, err)
	require.Equal(t, "447700900111@s.whatsapp.net", got.JID)
}

func TestResolver_JIDInputForAllowedGroup(t *testing.T) {
	m, l, _ := openResolverDB(t)
	seedContact(t, m, l, "fact-fam", "family group", "", "", "120001@g.us")

	r := &Resolver{Memory: m, Link: l}
	got, err := r.Resolve(context.Background(), "120001@g.us")
	require.NoError(t, err)
	require.True(t, got.IsGroup)
	require.Equal(t, "family group", got.Name)
}

func TestResolver_JIDInputRejectsUnknownJID(t *testing.T) {
	m, l, c := openResolverDB(t)
	r := &Resolver{Memory: m, Link: l, CardDAV: c}
	_, err := r.Resolve(context.Background(), "999@s.whatsapp.net")
	var nf *ErrContactNotFound
	require.True(t, errors.As(err, &nf))
}

func TestResolver_JIDInputResolvesCardDAVDerivedDM(t *testing.T) {
	m, l, c := openResolverDB(t)
	seedVCard(t, c, "vc-sam", "Sam Carter", []store.Phone{{Value: "+447700900222", Pref: 1}})
	seedContact(t, m, l, "fact-sam", "sam carter", "wife.", "vc-sam", "")

	r := &Resolver{Memory: m, Link: l, CardDAV: c}
	got, err := r.Resolve(context.Background(), "447700900222@s.whatsapp.net")
	require.NoError(t, err)
	require.Equal(t, "sam carter", got.Name)
	require.Equal(t, "vc-sam", got.CardDAVUID)
}

func TestResolver_List(t *testing.T) {
	m, l, c := openResolverDB(t)
	seedVCard(t, c, "vc-sam", "Sam Carter", []store.Phone{{Value: "1"}})
	seedVCard(t, c, "vc-stand", "Standalone", []store.Phone{{Value: "2"}})
	seedContact(t, m, l, "fact-sam", "sam carter", "wife.", "vc-sam", "")
	seedContact(t, m, l, "fact-fam", "family group", "", "", "120001@g.us")

	r := &Resolver{Memory: m, Link: l, CardDAV: c}
	got, err := r.List(context.Background())
	require.NoError(t, err)
	// Expect 3: linked-fact (sam), group-fact (family), CardDAV-direct (standalone).
	require.Len(t, got, 3)

	// vc-sam is exposed via the linked fact, not as a duplicate CardDAV row.
	var seen int
	for _, x := range got {
		if x.CardDAVUID == "vc-sam" {
			seen++
		}
	}
	require.Equal(t, 1, seen, "linked CardDAV uid must not appear twice")
}
