package settings

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/store"
)

func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_journal_mode=WAL&_foreign_keys=on"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, (&store.SettingsRepo{DB: db}).Migrate())
	return db
}

func TestParseStockTickers(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"AAPL", []string{"AAPL"}},
		{"aapl", []string{"AAPL"}},
		{"AAPL,GOOGL", []string{"AAPL", "GOOGL"}},
		{" aapl ,  GOOGL,, ", []string{"AAPL", "GOOGL"}},
		{"AAPL,aapl,AAPL", []string{"AAPL"}},
		{"AAPL,GOOGL,googl", []string{"AAPL", "GOOGL"}},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := ParseStockTickers(tc.in)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestParseWorldClocks(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"single", "America/Los_Angeles", []string{"America/Los_Angeles"}},
		{"multi", "America/Los_Angeles,Europe/London", []string{"America/Los_Angeles", "Europe/London"}},
		{"trim_and_drop_empties", " America/Los_Angeles , , Europe/London ,, ", []string{"America/Los_Angeles", "Europe/London"}},
		{"dedupe_preserves_order", "Europe/London,America/Los_Angeles,Europe/London", []string{"Europe/London", "America/Los_Angeles"}},
		{"drops_invalid", "America/Los_Angeles,Foo/Bar,Europe/London", []string{"America/Los_Angeles", "Europe/London"}},
		{"all_invalid", "Foo/Bar,Not/A/Real", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseWorldClocks(tc.in)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestWorldClocks_RoundTripsThroughSnapshot(t *testing.T) {
	repo := &store.SettingsRepo{DB: openTestDB(t)}
	require.NoError(t, repo.Upsert(context.Background(), store.AppSettings{
		Timezone:    "UTC",
		WorldClocks: "America/Los_Angeles,Europe/London,Asia/Kolkata",
	}))

	svc := New(repo)
	require.NoError(t, svc.Load(context.Background()))

	snap := svc.Snapshot()
	require.Equal(t, []string{"America/Los_Angeles", "Europe/London", "Asia/Kolkata"}, snap.WorldClocks)
}

func TestStockConfig_RoundTrip(t *testing.T) {
	repo := &store.SettingsRepo{DB: openTestDB(t)}
	require.NoError(t, repo.Upsert(context.Background(), store.AppSettings{
		Timezone:          "UTC",
		StockTickers:      "AAPL,GOOGL",
		StockThresholdPct: 3.5,
		StockAlwaysPoll:   true,
	}))

	svc := New(repo)
	require.NoError(t, svc.Load(context.Background()))

	tickers, threshold, alwaysPoll, ok := svc.StockConfig()
	require.True(t, ok)
	require.Equal(t, []string{"AAPL", "GOOGL"}, tickers)
	require.InDelta(t, 3.5, threshold, 1e-6)
	require.True(t, alwaysPoll, "stock_always_poll round-trips through the snapshot")
}

func TestStockConfig_AlwaysPollDefaultFalse(t *testing.T) {
	repo := &store.SettingsRepo{DB: openTestDB(t)}
	require.NoError(t, repo.Upsert(context.Background(), store.AppSettings{
		Timezone:     "UTC",
		StockTickers: "AAPL",
	}))

	svc := New(repo)
	require.NoError(t, svc.Load(context.Background()))

	_, _, alwaysPoll, ok := svc.StockConfig()
	require.True(t, ok)
	require.False(t, alwaysPoll, "stock_always_poll defaults to false (US-hours gate ON)")
}

func TestStockConfig_EmptyTickers_NotOK(t *testing.T) {
	repo := &store.SettingsRepo{DB: openTestDB(t)}
	svc := New(repo)
	require.NoError(t, svc.Load(context.Background()))

	_, _, _, ok := svc.StockConfig()
	require.False(t, ok, "no tickers configured -> not ok")
}

func TestService_LoadEmpty(t *testing.T) {
	repo := &store.SettingsRepo{DB: openTestDB(t)}
	svc := New(repo)
	require.NoError(t, svc.Load(context.Background()))

	snap := svc.Snapshot()
	require.NotNil(t, snap)
	require.False(t, snap.Set)
	require.Equal(t, time.UTC, snap.Location)
	require.Equal(t, "", snap.Timezone)
}

func TestService_LoadPopulated(t *testing.T) {
	repo := &store.SettingsRepo{DB: openTestDB(t)}
	require.NoError(t, repo.Upsert(context.Background(), store.AppSettings{
		Timezone: "Europe/Athens", City: "Athens", Country: "Greece",
		Latitude: 37.9838, Longitude: 23.7275,
	}))

	svc := New(repo)
	require.NoError(t, svc.Load(context.Background()))

	snap := svc.Snapshot()
	require.True(t, snap.Set)
	require.Equal(t, "Europe/Athens", snap.Timezone)
	require.Equal(t, "Athens", snap.City)
	require.Equal(t, "Greece", snap.Country)
	require.InDelta(t, 37.9838, snap.Latitude, 1e-6)
	require.InDelta(t, 23.7275, snap.Longitude, 1e-6)

	expected, _ := time.LoadLocation("Europe/Athens")
	require.Equal(t, expected.String(), snap.Location.String())
}

// Reload picks up changes written through the repo by another path —
// the contract the PUT handler relies on.
func TestService_ReloadPicksUpUpsert(t *testing.T) {
	repo := &store.SettingsRepo{DB: openTestDB(t)}
	svc := New(repo)
	require.NoError(t, svc.Load(context.Background()))
	require.False(t, svc.Snapshot().Set)

	require.NoError(t, repo.Upsert(context.Background(), store.AppSettings{
		Timezone: "America/New_York", City: "New York", Country: "USA",
	}))
	require.NoError(t, svc.Reload(context.Background()))

	snap := svc.Snapshot()
	require.True(t, snap.Set)
	require.Equal(t, "America/New_York", snap.Timezone)
	require.Equal(t, "New York", snap.City)
}

// Invalid timezones in the DB shouldn't break the service — Location
// falls back to UTC, but the rest of the snapshot still loads.
func TestService_InvalidTimezoneFallsBackToUTC(t *testing.T) {
	repo := &store.SettingsRepo{DB: openTestDB(t)}
	require.NoError(t, repo.Upsert(context.Background(), store.AppSettings{
		Timezone: "Not/A/Real/Zone", City: "X", Country: "Y",
	}))
	svc := New(repo)
	require.NoError(t, svc.Load(context.Background()))

	snap := svc.Snapshot()
	require.True(t, snap.Set)
	require.Equal(t, "Not/A/Real/Zone", snap.Timezone)
	require.Equal(t, time.UTC, snap.Location)
}

// Concurrent Snapshot() calls during Reload must never see a torn state —
// atomic.Pointer publishes the whole struct or nothing. Run with -race.
func TestService_ConcurrentReadDuringReload(t *testing.T) {
	repo := &store.SettingsRepo{DB: openTestDB(t)}
	require.NoError(t, repo.Upsert(context.Background(), store.AppSettings{
		Timezone: "UTC", City: "A", Country: "B",
	}))

	svc := New(repo)
	require.NoError(t, svc.Load(context.Background()))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					snap := svc.Snapshot()
					// Both old and new snapshots have a non-nil Location.
					require.NotNil(t, snap.Location)
				}
			}
		}()
	}

	for range 50 {
		require.NoError(t, repo.Upsert(context.Background(), store.AppSettings{
			Timezone: "UTC", City: "city-" + time.Now().String(), Country: "B",
		}))
		require.NoError(t, svc.Reload(context.Background()))
	}
	close(stop)
	wg.Wait()
}

func TestService_SubscribeNotifiesOnReload(t *testing.T) {
	repo := &store.SettingsRepo{DB: openTestDB(t)}
	require.NoError(t, repo.Upsert(context.Background(), store.AppSettings{
		Timezone: "America/Los_Angeles",
	}))

	svc := New(repo)
	require.NoError(t, svc.Load(context.Background()))

	var mu sync.Mutex
	var seen []string
	unsub := svc.Subscribe(func(s *Snapshot) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, s.Timezone)
	})
	require.NotNil(t, unsub)

	// Subscribe runs after Reload (not on initial Load), so trigger one.
	require.NoError(t, repo.Upsert(context.Background(), store.AppSettings{
		Timezone: "Europe/Athens",
	}))
	require.NoError(t, svc.Reload(context.Background()))

	require.NoError(t, repo.Upsert(context.Background(), store.AppSettings{
		Timezone: "UTC",
	}))
	require.NoError(t, svc.Reload(context.Background()))

	mu.Lock()
	require.Equal(t, []string{"Europe/Athens", "UTC"}, seen)
	mu.Unlock()

	// Unsubscribe stops further notifications.
	unsub()
	require.NoError(t, repo.Upsert(context.Background(), store.AppSettings{
		Timezone: "America/New_York",
	}))
	require.NoError(t, svc.Reload(context.Background()))

	mu.Lock()
	require.Equal(t, []string{"Europe/Athens", "UTC"}, seen)
	mu.Unlock()

	// Idempotent unsubscribe.
	unsub()
}

func TestService_SubscribeNilFnIsNoop(t *testing.T) {
	repo := &store.SettingsRepo{DB: openTestDB(t)}
	require.NoError(t, repo.Upsert(context.Background(), store.AppSettings{
		Timezone: "UTC",
	}))
	svc := New(repo)
	require.NoError(t, svc.Load(context.Background()))

	unsub := svc.Subscribe(nil)
	require.NotNil(t, unsub)
	unsub() // does not panic.
	require.NoError(t, svc.Reload(context.Background()))
}
