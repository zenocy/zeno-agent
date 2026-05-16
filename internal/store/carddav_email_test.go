package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCardDAVRepo_FindByEmail_ExactCaseInsensitive(t *testing.T) {
	db := openTestDB(t)
	repo := &CardDAVRepo{DB: db}
	require.NoError(t, repo.Migrate())
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, repo.Upsert(ctx, CardDAVContact{
		UID:         "vc-dana",
		Href:        "/dav/u/dana.vcf",
		DisplayName: "Dana Smith",
		Emails: EncodeEmails([]Email{
			{Value: "Dana@example.com", Types: []string{"INTERNET"}},
		}),
		Phones:     EncodePhones([]Phone{{Value: "+447700900111", Pref: 1}}),
		LastSyncAt: now,
	}))

	got, err := repo.FindByEmail(ctx, "dana@example.com")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "vc-dana", got.UID)

	// Case-insensitive match.
	got, err = repo.FindByEmail(ctx, "DANA@EXAMPLE.COM")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "vc-dana", got.UID)
}

func TestCardDAVRepo_FindByEmail_NoSubstring(t *testing.T) {
	db := openTestDB(t)
	repo := &CardDAVRepo{DB: db}
	require.NoError(t, repo.Migrate())
	ctx := context.Background()

	// Stored email contains the searched address as a substring; the
	// post-filter must reject the false positive.
	require.NoError(t, repo.Upsert(ctx, CardDAVContact{
		UID:         "vc-doe",
		Href:        "/dav/u/doe.vcf",
		DisplayName: "Jane Doe",
		Emails: EncodeEmails([]Email{
			{Value: "j.doe@example.com"},
		}),
		LastSyncAt: time.Now(),
	}))

	got, err := repo.FindByEmail(ctx, "doe@example.com")
	require.NoError(t, err)
	require.Nil(t, got, "substring match must be rejected")
}

func TestCardDAVRepo_FindByEmail_Empty(t *testing.T) {
	db := openTestDB(t)
	repo := &CardDAVRepo{DB: db}
	require.NoError(t, repo.Migrate())
	ctx := context.Background()

	got, err := repo.FindByEmail(ctx, "")
	require.NoError(t, err)
	require.Nil(t, got)

	got, err = repo.FindByEmail(ctx, "   ")
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestCardDAVRepo_FindByEmail_MultipleMatches_DeterministicOrder(t *testing.T) {
	db := openTestDB(t)
	repo := &CardDAVRepo{DB: db}
	require.NoError(t, repo.Migrate())
	ctx := context.Background()

	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	require.NoError(t, repo.Upsert(ctx, CardDAVContact{
		UID:         "vc-old",
		Href:        "/dav/u/old.vcf",
		DisplayName: "Old Match",
		Emails:      EncodeEmails([]Email{{Value: "shared@example.com"}}),
		UpdatedAt:   older,
		LastSyncAt:  older,
	}))
	require.NoError(t, repo.Upsert(ctx, CardDAVContact{
		UID:         "vc-new",
		Href:        "/dav/u/new.vcf",
		DisplayName: "New Match",
		Emails:      EncodeEmails([]Email{{Value: "shared@example.com"}}),
		UpdatedAt:   newer,
		LastSyncAt:  newer,
	}))

	// FindByEmail orders by updated_at DESC — the newer row wins.
	got, err := repo.FindByEmail(ctx, "shared@example.com")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "vc-new", got.UID)
}
