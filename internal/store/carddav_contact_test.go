package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCardDAVRepo_UpsertAndGet(t *testing.T) {
	db := openTestDB(t)
	repo := &CardDAVRepo{DB: db}
	require.NoError(t, repo.Migrate())
	ctx := context.Background()
	now := time.Now()

	c := CardDAVContact{
		UID:         "vc-1",
		Href:        "/dav/addressbooks/user/u/Default/vc-1.vcf",
		DisplayName: "Sam Carter",
		GivenName:   "Sam",
		FamilyName:  "Carter",
		Nicknames:   EncodeStringList([]string{"Sammy"}),
		Phones: EncodePhones([]Phone{
			{Value: "+447700900111", Types: []string{"WORK"}, Pref: 0},
			{Value: "+447700900222", Types: []string{"CELL"}, Pref: 1},
		}),
		ETag:       `"abc"`,
		LastSyncAt: now,
	}
	require.NoError(t, repo.Upsert(ctx, c))

	got, err := repo.GetByUID(ctx, "vc-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "Sam Carter", got.DisplayName)

	// PreferredPhone picks PREF=1 (the second TEL).
	require.Equal(t, "+447700900222", got.PreferredPhone())
	require.Equal(t, []string{"Sammy"}, got.NicknameList())
	phones := got.PhoneList()
	require.Len(t, phones, 2)

	// Same-UID upsert replaces in place.
	c.DisplayName = "Sam L. (updated)"
	require.NoError(t, repo.Upsert(ctx, c))
	got, err = repo.GetByUID(ctx, "vc-1")
	require.NoError(t, err)
	require.Equal(t, "Sam L. (updated)", got.DisplayName)

	cnt, err := repo.Count(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), cnt)
}

func TestCardDAVRepo_PreferredPhoneFallbacks(t *testing.T) {
	cases := []struct {
		name   string
		phones []Phone
		want   string
	}{
		{
			name:   "no phones",
			phones: nil,
			want:   "",
		},
		{
			name:   "PREF wins over CELL",
			phones: []Phone{{Value: "111", Types: []string{"CELL"}}, {Value: "222", Pref: 1}},
			want:   "222",
		},
		{
			name:   "CELL wins when no PREF",
			phones: []Phone{{Value: "111", Types: []string{"WORK"}}, {Value: "222", Types: []string{"MOBILE"}}},
			want:   "222",
		},
		{
			name:   "first phone when no hints",
			phones: []Phone{{Value: "111"}, {Value: "222"}},
			want:   "111",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := CardDAVContact{Phones: EncodePhones(tc.phones)}
			require.Equal(t, tc.want, c.PreferredPhone())
		})
	}
}

func TestCardDAVRepo_Search(t *testing.T) {
	db := openTestDB(t)
	repo := &CardDAVRepo{DB: db}
	require.NoError(t, repo.Migrate())
	ctx := context.Background()

	require.NoError(t, repo.UpsertBatch(ctx, []CardDAVContact{
		{UID: "a", DisplayName: "Sam Carter", GivenName: "Sam", FamilyName: "Carter", Nicknames: EncodeStringList([]string{"Sammy"})},
		{UID: "b", DisplayName: "Jamie Reyes", GivenName: "Jamie", FamilyName: "Carter"},
		{UID: "c", DisplayName: "Bob Smith", GivenName: "Bob", FamilyName: "Smith"},
	}))

	hits, err := repo.Search(ctx, "carter", 10)
	require.NoError(t, err)
	require.Len(t, hits, 2)

	hits, err = repo.Search(ctx, "sammy", 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, "a", hits[0].UID)

	hits, err = repo.Search(ctx, "", 10)
	require.NoError(t, err)
	require.Len(t, hits, 3, "empty query returns all up to limit")
}

func TestCardDAVRepo_SoftDelete(t *testing.T) {
	db := openTestDB(t)
	repo := &CardDAVRepo{DB: db}
	require.NoError(t, repo.Migrate())
	ctx := context.Background()

	require.NoError(t, repo.Upsert(ctx, CardDAVContact{UID: "x", Href: "/path/x.vcf", DisplayName: "X"}))
	require.NoError(t, repo.SoftDeleteByHref(ctx, "/path/x.vcf"))

	got, err := repo.GetByUID(ctx, "x")
	require.NoError(t, err)
	require.Nil(t, got, "soft-deleted row should be hidden from default queries")
}

func TestNormalizePhone(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"+447700900111", "447700900111"},
		{"00447700900111", "447700900111"},
		{"447 7009-00111", "447700900111"},
		{"  +44 (770) 0900111 ", "447700900111"},
		{"", ""},
		{"abc", ""},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, NormalizePhone(tc.in), "input=%q", tc.in)
	}
}

func TestPhoneToJID(t *testing.T) {
	require.Equal(t, "447700900111@s.whatsapp.net", PhoneToJID("+44 7700 900111"))
	require.Equal(t, "", PhoneToJID(""))
	require.Equal(t, "", PhoneToJID("abc"))
}
