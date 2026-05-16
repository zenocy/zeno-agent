package store

import (
	"context"
	"time"

	"gorm.io/gorm"
)

// settingsRowID is the fixed primary key for the singleton settings row.
// All Upsert calls coerce ID to this value so the table always holds at
// most one row.
const settingsRowID = "default"

// AppSettings holds system-wide scalar preferences the user manages from
// the Settings UI. The table is a singleton — exactly one row keyed by
// "default" — so future scalar settings just add a column. Collections
// (IMAP accounts, CalDAV calendars, etc.) get their own tables.
type AppSettings struct {
	ID        string    `gorm:"primaryKey;type:text" json:"-"`
	Timezone  string    `                            json:"timezone"`
	City      string    `                            json:"city"`
	Country   string    `                            json:"country"`
	Latitude  float64   `                            json:"latitude"`
	Longitude float64   `                            json:"longitude"`
	UpdatedAt time.Time `                            json:"updated_at"`

	// Stock sensor settings. StockTickers is a CSV (e.g. "AAPL,GOOGL");
	// empty disables the sensor. StockThresholdPct is the absolute
	// percent move that fires a stock.alert (0 disables alerting).
	// StockAlwaysPoll bypasses the default US market-hours gate — set
	// true if the watchlist holds non-US tickers and 24/7 polling is
	// preferable to silence during American off-hours.
	StockTickers      string  `json:"stock_tickers"`
	StockThresholdPct float64 `json:"stock_threshold_pct"`
	StockAlwaysPoll   bool    `json:"stock_always_poll"`

	// WorldClocks is a CSV of IANA timezone strings (e.g.
	// "America/Los_Angeles,Europe/London") rendered by the world-clock
	// widget. Empty disables the widget; invalid entries are silently
	// dropped on parse.
	WorldClocks string `json:"world_clocks"`

	// V2.13.0: assistant persona. UserName is the principal Zeno acts
	// for ("Jamie"); AssistantName is the EA's name ("Aria") —
	// empty disables the feature, drafts revert to first-person voice.
	// AssistantTone is an optional one-line steer ("warm but brisk")
	// appended after the voice canon so canon hard rules win on conflict.
	UserName      string `json:"user_name"`
	AssistantName string `json:"assistant_name"`
	AssistantTone string `json:"assistant_tone"`
}

// SettingsRepo persists and reads the singleton AppSettings row.
type SettingsRepo struct {
	DB *gorm.DB
}

// Migrate runs AutoMigrate for the app_settings table.
func (r *SettingsRepo) Migrate() error {
	return r.DB.AutoMigrate(&AppSettings{})
}

// Get returns the current settings row or nil if none has been saved yet.
func (r *SettingsRepo) Get(ctx context.Context) (*AppSettings, error) {
	var s AppSettings
	return firstOrNil(r.DB.WithContext(ctx).Where("id = ?", settingsRowID), &s)
}

// Upsert writes the settings row, coercing ID to the singleton value so
// the caller can't accidentally create a second row.
func (r *SettingsRepo) Upsert(ctx context.Context, s AppSettings) error {
	s.ID = settingsRowID
	s.UpdatedAt = time.Now()
	return r.DB.WithContext(ctx).Clauses(onConflictUpdateAll("id")).Create(&s).Error
}
