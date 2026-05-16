package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// CardDAVContact is one cached vCard from the user's CardDAV address book.
// V2.12 — written by the carddav sensor, read by whatsapp.Resolver to map
// "my wife" → vCard → phone → JID without round-tripping the live server.
//
// The cache is reference data, not observation: rows are upserted in place
// on every successful sync, soft-deleted when the server's MultiStatus
// returns 404 for a previously-seen Href, and full-rebuild when the
// SyncToken is rejected by the server (RFC 6578 §3.6 fallback).
type CardDAVContact struct {
	UID         string         `gorm:"column:uid;primaryKey;type:text"  json:"uid"`
	Href        string         `gorm:"index;not null"                   json:"href"`
	DisplayName string         `gorm:"index;not null"                   json:"display_name"`
	GivenName   string         `                                        json:"given_name,omitempty"`
	FamilyName  string         `                                        json:"family_name,omitempty"`
	Nicknames   string         `gorm:"type:text"                        json:"-"` // JSON array
	Phones      string         `gorm:"type:text"                        json:"-"` // JSON array
	Emails      string         `gorm:"type:text"                        json:"-"` // JSON array
	ETag        string         `gorm:"column:etag"                      json:"etag,omitempty"`
	LastSyncAt  time.Time      `                                        json:"last_sync_at"`
	CreatedAt   time.Time      `                                        json:"-"`
	UpdatedAt   time.Time      `                                        json:"updated_at"`
	DeletedAt   gorm.DeletedAt `gorm:"index"                            json:"-"`
}

// Phone is one TEL value from a vCard. Types is the semantic role list
// ("CELL", "WORK", "VOICE", ...). Pref is the RFC 6350 PREF parameter
// (1 = highest); 0 means "no PREF set". Resolver picks Pref=1 first,
// then any TYPE=CELL/MOBILE, then the first phone.
type Phone struct {
	Value string   `json:"value"`
	Types []string `json:"types,omitempty"`
	Pref  int      `json:"pref,omitempty"`
}

// Email is one EMAIL value from a vCard. Reserved for future use.
type Email struct {
	Value string   `json:"value"`
	Types []string `json:"types,omitempty"`
}

// PhoneList decodes the JSON-encoded Phones column.
func (c *CardDAVContact) PhoneList() []Phone {
	if c.Phones == "" {
		return nil
	}
	var out []Phone
	_ = json.Unmarshal([]byte(c.Phones), &out)
	return out
}

// NicknameList decodes the JSON-encoded Nicknames column.
func (c *CardDAVContact) NicknameList() []string {
	if c.Nicknames == "" {
		return nil
	}
	var out []string
	_ = json.Unmarshal([]byte(c.Nicknames), &out)
	return out
}

// EmailList decodes the JSON-encoded Emails column.
func (c *CardDAVContact) EmailList() []Email {
	if c.Emails == "" {
		return nil
	}
	var out []Email
	_ = json.Unmarshal([]byte(c.Emails), &out)
	return out
}

// PreferredPhone returns the phone the WhatsApp send path should use when
// the link row's PreferredPhone is empty. Order: Pref=1, then TYPE=CELL or
// MOBILE, then the first non-empty value. Returns "" when no phone is set.
func (c *CardDAVContact) PreferredPhone() string {
	phones := c.PhoneList()
	if len(phones) == 0 {
		return ""
	}
	// PREF=1 wins.
	for _, p := range phones {
		if p.Pref == 1 && strings.TrimSpace(p.Value) != "" {
			return p.Value
		}
	}
	// CELL/MOBILE next.
	for _, p := range phones {
		for _, t := range p.Types {
			tu := strings.ToUpper(t)
			if (tu == "CELL" || tu == "MOBILE") && strings.TrimSpace(p.Value) != "" {
				return p.Value
			}
		}
	}
	// First non-empty.
	for _, p := range phones {
		if strings.TrimSpace(p.Value) != "" {
			return p.Value
		}
	}
	return ""
}

// CardDAVRepo persists and reads CardDAVContact rows.
type CardDAVRepo struct {
	DB *gorm.DB
}

// Migrate creates the table.
func (r *CardDAVRepo) Migrate() error {
	return r.DB.AutoMigrate(&CardDAVContact{})
}

// Upsert writes one contact, replacing the row by UID.
func (r *CardDAVRepo) Upsert(ctx context.Context, c CardDAVContact) error {
	return r.DB.WithContext(ctx).
		Clauses(onConflictUpdateAll("uid")).Create(&c).Error
}

// UpsertBatch upserts a slice of contacts atomically (one transaction).
func (r *CardDAVRepo) UpsertBatch(ctx context.Context, items []CardDAVContact) error {
	if len(items) == 0 {
		return nil
	}
	return r.DB.WithContext(ctx).
		Clauses(onConflictUpdateAll("uid")).Create(&items).Error
}

// SoftDeleteByHref marks one contact deleted (server returned 404).
func (r *CardDAVRepo) SoftDeleteByHref(ctx context.Context, href string) error {
	return r.DB.WithContext(ctx).
		Where("href = ?", href).Delete(&CardDAVContact{}).Error
}

// GetByUID returns the contact or nil.
func (r *CardDAVRepo) GetByUID(ctx context.Context, uid string) (*CardDAVContact, error) {
	var c CardDAVContact
	return firstOrNil(r.DB.WithContext(ctx).Where("uid = ?", uid), &c)
}

// ListAll returns every visible contact ordered by display name.
func (r *CardDAVRepo) ListAll(ctx context.Context) ([]CardDAVContact, error) {
	var out []CardDAVContact
	err := r.DB.WithContext(ctx).Order("display_name ASC").Find(&out).Error
	return out, err
}

// Search returns contacts whose DisplayName, GivenName, FamilyName, or
// Nicknames JSON contains q (case-insensitive substring). Bounded by limit.
// q is trimmed and lowercased; an empty q returns ListAll capped at limit.
func (r *CardDAVRepo) Search(ctx context.Context, q string, limit int) ([]CardDAVContact, error) {
	if limit <= 0 {
		limit = 25
	}
	q = strings.TrimSpace(strings.ToLower(q))
	tx := r.DB.WithContext(ctx)
	if q == "" {
		var out []CardDAVContact
		err := tx.Order("display_name ASC").Limit(limit).Find(&out).Error
		return out, err
	}
	pat := "%" + q + "%"
	var out []CardDAVContact
	err := tx.
		Where("LOWER(display_name) LIKE ? OR LOWER(given_name) LIKE ? OR LOWER(family_name) LIKE ? OR LOWER(nicknames) LIKE ?",
			pat, pat, pat, pat).
		Order("display_name ASC").
		Limit(limit).
		Find(&out).Error
	return out, err
}

// Count returns the visible contact count (used by the Settings UI).
func (r *CardDAVRepo) Count(ctx context.Context) (int64, error) {
	var n int64
	err := r.DB.WithContext(ctx).Model(&CardDAVContact{}).Count(&n).Error
	return n, err
}

// FindByEmail returns the first non-deleted CardDAV contact whose Emails
// JSON contains an exact case-insensitive match for the given address.
// Used by the V2.13.0 calendar-attendee → contact path: the cards loop
// walks today's event attendees and asks the CardDAV cache "do I know
// this email?" before proposing a confirm-by-WhatsApp action.
//
// The shortlist is a SQL LIKE on the JSON column for speed; the exact
// match runs in Go on EmailList() output. This avoids false positives
// like "j.doe@x.com" matching "doe@x.com" via substring.
//
// Deterministic ordering on collisions (rare — two vCards sharing an
// email): first by updated_at DESC, then UID.
func (r *CardDAVRepo) FindByEmail(ctx context.Context, email string) (*CardDAVContact, error) {
	addr := strings.ToLower(strings.TrimSpace(email))
	if addr == "" {
		return nil, nil
	}
	pat := "%" + addr + "%"
	var rows []CardDAVContact
	err := r.DB.WithContext(ctx).
		Where("LOWER(emails) LIKE ?", pat).
		Order("updated_at DESC, uid ASC").
		Limit(20).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	for i := range rows {
		for _, e := range rows[i].EmailList() {
			if strings.EqualFold(strings.TrimSpace(e.Value), addr) {
				return &rows[i], nil
			}
		}
	}
	return nil, nil
}

// EncodePhones marshals a Phone slice into the JSON shape the table stores.
func EncodePhones(phones []Phone) string {
	if len(phones) == 0 {
		return ""
	}
	b, err := json.Marshal(phones)
	if err != nil {
		return ""
	}
	return string(b)
}

// EncodeStringList marshals a string slice (nicknames).
func EncodeStringList(items []string) string {
	if len(items) == 0 {
		return ""
	}
	b, err := json.Marshal(items)
	if err != nil {
		return ""
	}
	return string(b)
}

// EncodeEmails marshals an Email slice.
func EncodeEmails(emails []Email) string {
	if len(emails) == 0 {
		return ""
	}
	b, err := json.Marshal(emails)
	if err != nil {
		return ""
	}
	return string(b)
}

// NormalizePhone strips a phone string down to the digit-only E.164 form
// the WhatsApp protocol uses to derive a JID. Leading "+" / "00" are
// stripped, all non-digit characters are removed. Returns "" when no
// digits remain.
func NormalizePhone(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// Trim a leading "00" international prefix as a synonym for "+".
	raw = strings.TrimPrefix(raw, "00")
	var b strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// PhoneToJID derives a WhatsApp DM JID from an E.164-style phone number.
// Returns "" when the normalized phone is empty.
func PhoneToJID(raw string) string {
	d := NormalizePhone(raw)
	if d == "" {
		return ""
	}
	return fmt.Sprintf("%s@s.whatsapp.net", d)
}
