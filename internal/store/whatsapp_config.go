package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
)

// whatsappConfigRowID is the fixed primary key for the singleton row.
const whatsappConfigRowID = "default"

// WhatsAppConfigRow is the persisted operator-tunable configuration
// for the V2.7 WhatsApp integration. Lives in its own table (not in
// app_settings) because the allowlist is a slice and would awkwardly
// pollute the scalar settings shape; storing it as JSON in a dedicated
// table keeps the singleton-row pattern intact.
type WhatsAppConfigRow struct {
	ID                 string    `gorm:"primaryKey;type:text"`
	MentionName        string    `gorm:"type:text"`
	AllowedDMsJSON     string    `gorm:"type:text"`
	MinChatIntervalMs  int       `gorm:"not null;default:3000"`
	MaxConcurrentSynth int       `gorm:"not null;default:4"`
	PerChatBuffer      int       `gorm:"not null;default:4"`
	UpdatedAt          time.Time
}

// TableName overrides the default gorm pluralization to keep the
// schema readable.
func (WhatsAppConfigRow) TableName() string { return "whatsapp_config" }

// WhatsAppConfigRepo persists and reads the singleton config row.
type WhatsAppConfigRepo struct {
	DB *gorm.DB
}

// Migrate runs AutoMigrate for whatsapp_config.
func (r *WhatsAppConfigRepo) Migrate() error {
	return r.DB.AutoMigrate(&WhatsAppConfigRow{})
}

// Get returns the current row or nil if no config has been saved.
func (r *WhatsAppConfigRepo) Get(ctx context.Context) (*WhatsAppConfigRow, error) {
	var row WhatsAppConfigRow
	return firstOrNil(r.DB.WithContext(ctx).Where("id = ?", whatsappConfigRowID), &row)
}

// Upsert writes the singleton row.
func (r *WhatsAppConfigRepo) Upsert(ctx context.Context, row WhatsAppConfigRow) error {
	row.ID = whatsappConfigRowID
	row.UpdatedAt = time.Now()
	return r.DB.WithContext(ctx).Clauses(onConflictUpdateAll("id")).Create(&row).Error
}

// AllowedDMs unmarshals the JSON-encoded list. A blank string returns
// nil — the integration treats nil as "deny all DMs".
func (row WhatsAppConfigRow) AllowedDMs() []string {
	s := strings.TrimSpace(row.AllowedDMsJSON)
	if s == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

// SetAllowedDMs marshals the list into the JSON column. Empty input
// stores "" so a Get returns AllowedDMs() == nil.
func (row *WhatsAppConfigRow) SetAllowedDMs(jids []string) error {
	if len(jids) == 0 {
		row.AllowedDMsJSON = ""
		return nil
	}
	b, err := json.Marshal(jids)
	if err != nil {
		return err
	}
	row.AllowedDMsJSON = string(b)
	return nil
}

// ErrInvalidJID is returned by validation helpers below when a JID
// fails the minimal "<digits>@<host>" shape check.
var ErrInvalidJID = errors.New("invalid jid")
