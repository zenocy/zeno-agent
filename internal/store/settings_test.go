package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func openSettingsTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_journal_mode=WAL&_foreign_keys=on"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, (&SettingsRepo{DB: db}).Migrate())
	return db
}

func TestSettingsRepo_MigrateIsIdempotent(t *testing.T) {
	db := openSettingsTestDB(t)
	require.NoError(t, (&SettingsRepo{DB: db}).Migrate())
	require.NoError(t, (&SettingsRepo{DB: db}).Migrate())
}

func TestSettingsRepo_GetEmptyReturnsNil(t *testing.T) {
	db := openSettingsTestDB(t)
	repo := &SettingsRepo{DB: db}
	got, err := repo.Get(context.Background())
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestSettingsRepo_UpsertThenGetRoundTrips(t *testing.T) {
	db := openSettingsTestDB(t)
	repo := &SettingsRepo{DB: db}
	ctx := context.Background()

	require.NoError(t, repo.Upsert(ctx, AppSettings{
		Timezone: "Europe/Athens",
		City:     "Athens",
		Country:  "Greece",
		Latitude: 37.9838, Longitude: 23.7275,
	}))

	got, err := repo.Get(ctx)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "Europe/Athens", got.Timezone)
	require.Equal(t, "Athens", got.City)
	require.Equal(t, "Greece", got.Country)
	require.InDelta(t, 37.9838, got.Latitude, 1e-6)
	require.InDelta(t, 23.7275, got.Longitude, 1e-6)
	require.False(t, got.UpdatedAt.IsZero())
}

func TestSettingsRepo_UpsertOverwritesExisting(t *testing.T) {
	db := openSettingsTestDB(t)
	repo := &SettingsRepo{DB: db}
	ctx := context.Background()

	require.NoError(t, repo.Upsert(ctx, AppSettings{
		Timezone: "Europe/Athens", City: "Athens", Country: "Greece",
		Latitude: 37.9838, Longitude: 23.7275,
	}))
	require.NoError(t, repo.Upsert(ctx, AppSettings{
		Timezone: "America/New_York", City: "New York", Country: "USA",
		Latitude: 40.7128, Longitude: -74.0060,
	}))

	got, err := repo.Get(ctx)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "America/New_York", got.Timezone)
	require.Equal(t, "New York", got.City)

	// Still exactly one row in the table — no duplicates.
	var count int64
	require.NoError(t, db.Model(&AppSettings{}).Count(&count).Error)
	require.Equal(t, int64(1), count)
}

// Singleton invariant: caller-supplied IDs are ignored; everything goes
// to the "default" row.
func TestSettingsRepo_UpsertCoercesID(t *testing.T) {
	db := openSettingsTestDB(t)
	repo := &SettingsRepo{DB: db}
	ctx := context.Background()

	require.NoError(t, repo.Upsert(ctx, AppSettings{
		ID: "should-be-ignored", Timezone: "UTC", City: "X", Country: "Y",
	}))
	require.NoError(t, repo.Upsert(ctx, AppSettings{
		ID: "also-ignored", Timezone: "Europe/Berlin", City: "Berlin", Country: "Germany",
	}))

	var count int64
	require.NoError(t, db.Model(&AppSettings{}).Count(&count).Error)
	require.Equal(t, int64(1), count, "all upserts must collapse onto the singleton row")

	got, err := repo.Get(ctx)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "Berlin", got.City, "the second upsert must have won")
}
