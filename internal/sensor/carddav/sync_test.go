package carddav

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/emersion/go-vcard"
	"github.com/emersion/go-webdav/carddav"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/store"
)

// fakeProvider returns a canned slice of AddressObjects.
type fakeProvider struct {
	objs []carddav.AddressObject
	err  error
	hits int
}

func (f *fakeProvider) ListAll(ctx context.Context) ([]carddav.AddressObject, error) {
	f.hits++
	return f.objs, f.err
}

func openDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, (&store.CardDAVRepo{DB: db}).Migrate())
	return db
}

func newCard(uid, fn, given, family string, nicks []string, phones []phoneSpec) vcard.Card {
	c := vcard.Card{}
	if uid != "" {
		c.SetValue(vcard.FieldUID, uid)
	}
	if fn != "" {
		c.SetValue(vcard.FieldFormattedName, fn)
	}
	if given != "" || family != "" {
		c.SetName(&vcard.Name{GivenName: given, FamilyName: family})
	}
	if len(nicks) > 0 {
		c.SetValue(vcard.FieldNickname, joinNicks(nicks))
	}
	for _, p := range phones {
		f := &vcard.Field{
			Value:  p.value,
			Params: vcard.Params{},
		}
		for _, t := range p.types {
			f.Params.Add(vcard.ParamType, t)
		}
		if p.pref > 0 {
			f.Params.Set(vcard.ParamPreferred, intStr(p.pref))
		}
		c.Add(vcard.FieldTelephone, f)
	}
	return c
}

type phoneSpec struct {
	value string
	types []string
	pref  int
}

func joinNicks(in []string) string {
	out := ""
	for i, n := range in {
		if i > 0 {
			out += ","
		}
		out += n
	}
	return out
}

func intStr(n int) string {
	if n <= 0 {
		return ""
	}
	// Avoid pulling strconv into the test fixture noise.
	switch n {
	case 1:
		return "1"
	case 2:
		return "2"
	case 3:
		return "3"
	}
	return ""
}

func TestSensor_SyncUpsertsAndExtracts(t *testing.T) {
	db := openDB(t)
	repo := &store.CardDAVRepo{DB: db}

	provider := &fakeProvider{
		objs: []carddav.AddressObject{
			{
				Path: "/dav/abooks/u/Default/sam.vcf",
				ETag: `"etag-sam"`,
				Card: newCard("uid-sam", "Sam Carter", "Sam", "Carter",
					[]string{"Sammy", "wifey"},
					[]phoneSpec{
						{value: "+447700900111", types: []string{"WORK"}},
						{value: "+447700900222", types: []string{"CELL"}, pref: 1},
					}),
			},
		},
	}
	s := New(provider, repo, logrus.NewEntry(logrus.New()))
	require.NoError(t, s.Sync(context.Background()))

	got, err := repo.GetByUID(context.Background(), "uid-sam")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "Sam Carter", got.DisplayName)
	require.Equal(t, "Sam", got.GivenName)
	require.Equal(t, "Carter", got.FamilyName)
	require.Equal(t, []string{"Sammy", "wifey"}, got.NicknameList())
	require.Equal(t, "+447700900222", got.PreferredPhone(),
		"PREF=1 should win even though CELL also wins")

	phones := got.PhoneList()
	require.Len(t, phones, 2)
}

func TestSensor_SyncFallsBackToGivenFamilyWhenFNMissing(t *testing.T) {
	db := openDB(t)
	repo := &store.CardDAVRepo{DB: db}

	provider := &fakeProvider{
		objs: []carddav.AddressObject{
			{
				Path: "/dav/abooks/u/Default/x.vcf",
				Card: newCard("uid-1", "" /* no FN */, "Jamie", "Carter", nil, nil),
			},
		},
	}
	s := New(provider, repo, logrus.NewEntry(logrus.New()))
	require.NoError(t, s.Sync(context.Background()))

	got, _ := repo.GetByUID(context.Background(), "uid-1")
	require.NotNil(t, got)
	require.Equal(t, "Jamie Carter", got.DisplayName)
}

func TestSensor_SyncSkipsCardsWithoutUID(t *testing.T) {
	db := openDB(t)
	repo := &store.CardDAVRepo{DB: db}

	provider := &fakeProvider{
		objs: []carddav.AddressObject{
			{Path: "/dav/abooks/u/Default/no-uid.vcf", Card: newCard("", "Anon", "", "", nil, nil)},
			{Path: "/dav/abooks/u/Default/yes.vcf", Card: newCard("uid-yes", "Yes", "", "", nil, nil)},
		},
	}
	s := New(provider, repo, logrus.NewEntry(logrus.New()))
	require.NoError(t, s.Sync(context.Background()))

	got, _ := repo.GetByUID(context.Background(), "uid-yes")
	require.NotNil(t, got)
	cnt, _ := repo.Count(context.Background())
	require.Equal(t, int64(1), cnt, "card without UID must be skipped")
}

func TestSensor_SyncSoftDeletesMissingCards(t *testing.T) {
	db := openDB(t)
	repo := &store.CardDAVRepo{DB: db}

	// First sync: two contacts.
	provider := &fakeProvider{
		objs: []carddav.AddressObject{
			{Path: "/a.vcf", Card: newCard("uid-a", "A", "", "", nil, nil)},
			{Path: "/b.vcf", Card: newCard("uid-b", "B", "", "", nil, nil)},
		},
	}
	s := New(provider, repo, logrus.NewEntry(logrus.New()))
	require.NoError(t, s.Sync(context.Background()))
	cnt, _ := repo.Count(context.Background())
	require.Equal(t, int64(2), cnt)

	// Second sync: only A. B should be soft-deleted.
	provider.objs = provider.objs[:1]
	require.NoError(t, s.Sync(context.Background()))

	cnt, _ = repo.Count(context.Background())
	require.Equal(t, int64(1), cnt, "missing vCard should be soft-deleted")

	a, _ := repo.GetByUID(context.Background(), "uid-a")
	b, _ := repo.GetByUID(context.Background(), "uid-b")
	require.NotNil(t, a)
	require.Nil(t, b)
}

func TestSensor_SyncReUpsertsAfterReturn(t *testing.T) {
	db := openDB(t)
	repo := &store.CardDAVRepo{DB: db}

	provider := &fakeProvider{
		objs: []carddav.AddressObject{
			{Path: "/a.vcf", Card: newCard("uid-a", "A", "", "", nil, nil)},
		},
	}
	s := New(provider, repo, logrus.NewEntry(logrus.New())).WithNow(func() time.Time {
		return time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	})
	require.NoError(t, s.Sync(context.Background()))

	// Vanish.
	provider.objs = nil
	require.NoError(t, s.Sync(context.Background()))
	got, _ := repo.GetByUID(context.Background(), "uid-a")
	require.Nil(t, got, "should be soft-deleted")

	// Returns. Same UID, same Href — upsert undeletes by overwriting.
	// (gorm soft-delete: the row stays, but DeletedAt is set; conflict
	// upsert with UpdateAll re-sets DeletedAt to NULL via the new value.)
	provider.objs = []carddav.AddressObject{
		{Path: "/a.vcf", Card: newCard("uid-a", "A (returned)", "", "", nil, nil)},
	}
	require.NoError(t, s.Sync(context.Background()))
	got, _ = repo.GetByUID(context.Background(), "uid-a")
	require.NotNil(t, got)
	require.Equal(t, "A (returned)", got.DisplayName)
}

func TestSensor_SyncProviderError(t *testing.T) {
	db := openDB(t)
	repo := &store.CardDAVRepo{DB: db}
	provider := &fakeProvider{err: errors.New("boom")}
	s := New(provider, repo, logrus.NewEntry(logrus.New()))
	err := s.Sync(context.Background())
	require.Error(t, err)
}

func TestParsePref(t *testing.T) {
	require.Equal(t, 0, parsePref(""))
	require.Equal(t, 0, parsePref("not-a-number"))
	require.Equal(t, 1, parsePref("1"))
	require.Equal(t, 5, parsePref("5"))
	require.Equal(t, 5, parsePref(" 5 "))
}
