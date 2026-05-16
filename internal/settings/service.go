// Package settings holds the live in-memory snapshot of user-managed
// system settings (timezone, location, ...) so that consumers like the
// weather sensor and HTTP handlers see updates without a process restart.
//
// One Service exists per process. PUT /api/settings writes the DB row
// then calls Reload, which atomically swaps a fresh Snapshot in. Readers
// call Snapshot() once per operation; the returned pointer is immutable.
package settings

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zenocy/zeno-v2/internal/store"
)

// Snapshot is an immutable view of the current settings. Consumers should
// treat the returned pointer as read-only — Reload publishes a fresh
// pointer rather than mutating in place.
type Snapshot struct {
	Timezone  string
	Location  *time.Location
	City      string
	Country   string
	Latitude  float64
	Longitude float64
	UpdatedAt time.Time

	// Set is true once the user has saved settings at least once. Sensors
	// that need a location (weather) skip when Set is false; handlers fall
	// back to UTC.
	Set bool

	// StockTickers is the parsed/normalized list of tickers (uppercase,
	// trimmed, deduplicated). StockThresholdPct is the absolute percent
	// move that fires a stock.alert (0 disables alerting).
	// StockAlwaysPoll disables the default US market-hours gate when
	// true (use for watchlists with non-US tickers).
	StockTickers      []string
	StockThresholdPct float64
	StockAlwaysPoll   bool

	// WorldClocks is the parsed/validated list of IANA timezone strings
	// for the world-clock widget. Each entry is guaranteed to load with
	// time.LoadLocation; invalid inputs are dropped during parse.
	WorldClocks []string

	// V2.13.0: assistant persona. AssistantName empty disables the
	// feature; drafts revert to first-person voice.
	UserName      string
	AssistantName string
	AssistantTone string
}

// AssistantEnabled reports whether assistant-mode drafting is configured.
// True when AssistantName is non-empty after trim.
func (s *Snapshot) AssistantEnabled() bool {
	return s != nil && strings.TrimSpace(s.AssistantName) != ""
}

// Service holds the live snapshot.
type Service struct {
	repo *store.SettingsRepo
	snap atomic.Pointer[Snapshot]

	subMu  sync.Mutex
	subs   map[uint64]func(*Snapshot)
	nextID uint64
}

// TZ returns the current timezone Location, falling back to UTC. This is
// the canonical accessor handlers should bind to so they pick up live
// edits made via the Settings UI.
func (s *Service) TZ() *time.Location {
	snap := s.snap.Load()
	if snap == nil || snap.Location == nil {
		return time.UTC
	}
	return snap.Location
}

// Coords implements weather.Locator: returns the live (lat, lon, tz, ok)
// tuple. ok is false when the user hasn't saved settings yet.
func (s *Service) Coords() (lat, lon float64, tz string, ok bool) {
	snap := s.snap.Load()
	if snap == nil || !snap.Set {
		return 0, 0, "", false
	}
	return snap.Latitude, snap.Longitude, snap.Timezone, snap.Latitude != 0 || snap.Longitude != 0
}

// StockConfig returns the live (tickers, thresholdPct, alwaysPoll, ok)
// tuple for the stock sensor. ok is false when no tickers are
// configured — the sensor short-circuits and emits nothing, mirroring
// the Coords pattern.
func (s *Service) StockConfig() (tickers []string, thresholdPct float64, alwaysPoll bool, ok bool) {
	snap := s.snap.Load()
	if snap == nil || len(snap.StockTickers) == 0 {
		return nil, 0, false, false
	}
	out := make([]string, len(snap.StockTickers))
	copy(out, snap.StockTickers)
	return out, snap.StockThresholdPct, snap.StockAlwaysPoll, true
}

// New builds a Service. Call Load before reading.
func New(repo *store.SettingsRepo) *Service {
	s := &Service{repo: repo}
	s.snap.Store(emptySnapshot())
	return s
}

// Load reads the current row from the DB into the in-memory snapshot.
// Call once at boot, after Migrate.
func (s *Service) Load(ctx context.Context) error {
	return s.Reload(ctx)
}

// Reload re-reads from the DB and atomically swaps the snapshot. Call
// after a successful PUT. Subscribers (see Subscribe) are notified
// synchronously after the swap, in unspecified order.
func (s *Service) Reload(ctx context.Context) error {
	row, err := s.repo.Get(ctx)
	if err != nil {
		return err
	}
	snap := buildSnapshot(row)
	s.snap.Store(snap)
	s.notify(snap)
	return nil
}

// Subscribe registers fn to be invoked synchronously after every successful
// Reload, with the freshly published Snapshot. The returned function
// unsubscribes; calling it twice is a no-op.
//
// Subscribers run on the goroutine that called Reload — keep them fast and
// non-blocking. The scheduler uses this to retarget cron entries when the
// user's TZ changes via the Settings UI.
func (s *Service) Subscribe(fn func(*Snapshot)) func() {
	if fn == nil {
		return func() {}
	}
	s.subMu.Lock()
	if s.subs == nil {
		s.subs = make(map[uint64]func(*Snapshot))
	}
	id := s.nextID
	s.nextID++
	s.subs[id] = fn
	s.subMu.Unlock()
	return func() {
		s.subMu.Lock()
		delete(s.subs, id)
		s.subMu.Unlock()
	}
}

func (s *Service) notify(snap *Snapshot) {
	s.subMu.Lock()
	subs := make([]func(*Snapshot), 0, len(s.subs))
	for _, fn := range s.subs {
		subs = append(subs, fn)
	}
	s.subMu.Unlock()
	for _, fn := range subs {
		fn(snap)
	}
}

// Snapshot returns the current immutable snapshot. The returned pointer
// is safe to read concurrently with Reload — atomic.Pointer guarantees
// readers either see the old or the new pointer, never a torn state.
func (s *Service) Snapshot() *Snapshot {
	return s.snap.Load()
}

func emptySnapshot() *Snapshot {
	return &Snapshot{
		Location: time.UTC,
		Set:      false,
	}
}

func buildSnapshot(row *store.AppSettings) *Snapshot {
	if row == nil {
		return emptySnapshot()
	}
	loc := time.UTC
	if row.Timezone != "" {
		if l, err := time.LoadLocation(row.Timezone); err == nil {
			loc = l
		}
	}
	return &Snapshot{
		Timezone:          row.Timezone,
		Location:          loc,
		City:              row.City,
		Country:           row.Country,
		Latitude:          row.Latitude,
		Longitude:         row.Longitude,
		UpdatedAt:         row.UpdatedAt,
		Set:               true,
		StockTickers:      ParseStockTickers(row.StockTickers),
		StockThresholdPct: row.StockThresholdPct,
		StockAlwaysPoll:   row.StockAlwaysPoll,
		WorldClocks:       ParseWorldClocks(row.WorldClocks),
		UserName:          strings.TrimSpace(row.UserName),
		AssistantName:     strings.TrimSpace(row.AssistantName),
		AssistantTone:     strings.TrimSpace(row.AssistantTone),
	}
}

// ParseWorldClocks normalizes a CSV of IANA timezone strings: split on
// commas, trim, drop empties, drop entries that don't load with
// time.LoadLocation, and dedupe (preserving first-seen order). Case is
// preserved because IANA tz names are case-sensitive.
func ParseWorldClocks(csv string) []string {
	if csv == "" {
		return nil
	}
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t == "" || seen[t] {
			continue
		}
		if _, err := time.LoadLocation(t); err != nil {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ParseStockTickers normalizes a CSV ticker string: split on commas,
// trim, uppercase, drop empties, dedupe (preserving first-seen order).
// Exported so the HTTP handler can validate input before persisting.
func ParseStockTickers(csv string) []string {
	if csv == "" {
		return nil
	}
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, p := range parts {
		t := strings.ToUpper(strings.TrimSpace(p))
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}
