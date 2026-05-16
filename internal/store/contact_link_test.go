package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateLink_OneOfInvariant(t *testing.T) {
	cases := []struct {
		name string
		link MemoryContactLink
		ok   bool
	}{
		{
			name: "carddav-only ok",
			link: MemoryContactLink{ID: "a", CardDAVUID: "vc-1"},
			ok:   true,
		},
		{
			name: "jid-only DM ok",
			link: MemoryContactLink{ID: "b", JID: "447700900111@s.whatsapp.net"},
			ok:   true,
		},
		{
			name: "jid-only group ok",
			link: MemoryContactLink{ID: "c", JID: "120001@g.us"},
			ok:   true,
		},
		{
			name: "neither set fails",
			link: MemoryContactLink{ID: "d"},
			ok:   false,
		},
		{
			name: "both set fails",
			link: MemoryContactLink{ID: "e", CardDAVUID: "vc-1", JID: "x@s.whatsapp.net"},
			ok:   false,
		},
		{
			name: "unknown JID suffix fails",
			link: MemoryContactLink{ID: "f", JID: "abc@example.com"},
			ok:   false,
		},
		{
			name: "missing id fails",
			link: MemoryContactLink{CardDAVUID: "vc-1"},
			ok:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateLink(&tc.link)
			if tc.ok {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
		})
	}
}

func TestValidateLink_DerivesIsGroup(t *testing.T) {
	dm := MemoryContactLink{ID: "a", JID: "447700900111@s.whatsapp.net"}
	require.NoError(t, ValidateLink(&dm))
	require.False(t, dm.IsGroup)

	g := MemoryContactLink{ID: "b", JID: "120001@g.us"}
	require.NoError(t, ValidateLink(&g))
	require.True(t, g.IsGroup)

	cd := MemoryContactLink{ID: "c", CardDAVUID: "vc-1"}
	require.NoError(t, ValidateLink(&cd))
	require.False(t, cd.IsGroup)
}

func TestValidateLink_DefaultsChannel(t *testing.T) {
	link := MemoryContactLink{ID: "a", CardDAVUID: "vc-1"}
	require.NoError(t, ValidateLink(&link))
	require.Equal(t, ChannelWhatsApp, link.Channel)
}

func TestContactLinkRepo_InsertAndLookup(t *testing.T) {
	db := openTestDB(t)
	repo := &ContactLinkRepo{DB: db}
	require.NoError(t, repo.Migrate())
	ctx := context.Background()

	require.NoError(t, repo.Insert(ctx, MemoryContactLink{
		ID: "fact-1", CardDAVUID: "vc-1", PreferredPhone: "+447700900111",
	}))
	require.NoError(t, repo.Insert(ctx, MemoryContactLink{
		ID: "fact-2", JID: "120001@g.us",
	}))

	got, err := repo.GetByID(ctx, "fact-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "vc-1", got.CardDAVUID)

	g, err := repo.GetByJID(ctx, "120001@g.us")
	require.NoError(t, err)
	require.NotNil(t, g)
	require.True(t, g.IsGroup)

	all, err := repo.ListAll(ctx)
	require.NoError(t, err)
	require.Len(t, all, 2)

	require.NoError(t, repo.UpdatePreferredPhone(ctx, "fact-1", "+447700900222"))
	got, _ = repo.GetByID(ctx, "fact-1")
	require.Equal(t, "+447700900222", got.PreferredPhone)

	require.NoError(t, repo.SoftDelete(ctx, "fact-1"))
	got, err = repo.GetByID(ctx, "fact-1")
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestContactLinkRepo_RejectInvalid(t *testing.T) {
	db := openTestDB(t)
	repo := &ContactLinkRepo{DB: db}
	require.NoError(t, repo.Migrate())
	ctx := context.Background()

	require.Error(t, repo.Insert(ctx, MemoryContactLink{ID: "x"}))
	require.Error(t, repo.Insert(ctx, MemoryContactLink{ID: "x", CardDAVUID: "vc-1", JID: "y@s.whatsapp.net"}))
}

func TestIsGroupJID_IsDMJID(t *testing.T) {
	require.True(t, IsGroupJID("120001@g.us"))
	require.False(t, IsGroupJID("447700900111@s.whatsapp.net"))
	require.True(t, IsDMJID("447700900111@s.whatsapp.net"))
	require.False(t, IsDMJID("120001@g.us"))
}
