package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// BuildContactID derives a deterministic ID from the contact's identity.
// The hash includes both the canonical name and the address so subject-only
// collisions across two CardDAV UIDs (e.g. "Sam (Work)" vs "Sam") get
// distinct IDs. The same ID is reused for the paired MemoryFact and
// MemoryContactLink rows so they share a primary key.
//
// Used by both:
//   - the Settings UI's POST /api/contacts handler (creates an alias row)
//   - AddMemoryExec when an LLM-driven add_memory matches a CardDAV
//     contact and produces a linked memory atomically
//
// Lives next to the link table it identifies so the two writers can
// share one canonical key derivation rather than drifting.
func BuildContactID(subject, fact, cardDAVUID, jid string) string {
	h := sha256.New()
	h.Write([]byte(subject))
	h.Write([]byte{0})
	h.Write([]byte(fact))
	h.Write([]byte{0})
	h.Write([]byte(cardDAVUID))
	h.Write([]byte{0})
	h.Write([]byte(jid))
	return "ct-" + hex.EncodeToString(h.Sum(nil))[:16]
}

// MemoryContactLink connects one MemoryFact (category=contact_whatsapp) to
// the addressable identifier the WhatsApp send path needs. Exactly one of
// CardDAVUID or JID is non-empty:
//
//   - Individuals: CardDAVUID points at a row in carddav_contacts and the
//     resolver derives the JID at send time from the linked vCard's phone.
//     PreferredPhone narrows the choice when the vCard has multiple TELs;
//     empty means "use CardDAVContact.PreferredPhone() heuristics."
//
//   - Groups: JID is set directly (e.g. "1203@g.us") and IsGroup=true.
//     Group JIDs are not in any address book — the user picks them from
//     the Settings UI's "groups Zeno has been mentioned in" list, which
//     is fed by the receive log.
//
// V2.12 — kept as a sibling table (not columns on memory_facts) so the
// memory schema stays prose-only and operational identifiers never leak
// into the LLM-visible memory dump.
type MemoryContactLink struct {
	ID             string         `gorm:"primaryKey;type:text"               json:"id"`
	Channel        string         `gorm:"index;not null"                     json:"channel"`
	CardDAVUID     string         `gorm:"column:carddav_uid;index"           json:"carddav_uid,omitempty"`
	PreferredPhone string         `gorm:"column:preferred_phone"             json:"preferred_phone,omitempty"`
	JID            string         `gorm:"column:jid;index"                   json:"jid,omitempty"`
	IsGroup        bool           `gorm:"column:is_group;not null;default:false" json:"is_group"`
	CreatedAt      time.Time      `                                          json:"created_at"`
	UpdatedAt      time.Time      `                                          json:"updated_at"`
	DeletedAt      gorm.DeletedAt `gorm:"index"                              json:"-"`
}

// ChannelWhatsApp is the canonical Channel value for WhatsApp links.
// Future: ChannelTelegram, ChannelSignal, etc.
const ChannelWhatsApp = "whatsapp"

// MemoryCategoryContactWhatsApp is the MemoryFact.Category used for
// contact facts. Centralized here so the resolver, HTTP API, and consolidator
// all share one constant.
const MemoryCategoryContactWhatsApp = "contact_whatsapp"

// JID suffix constants (whatsmeow protocol).
const (
	jidSuffixDM    = "@s.whatsapp.net"
	jidSuffixGroup = "@g.us"
)

// IsGroupJID returns true when jid ends with the group suffix.
func IsGroupJID(jid string) bool {
	return strings.HasSuffix(jid, jidSuffixGroup)
}

// IsDMJID returns true when jid ends with the DM suffix.
func IsDMJID(jid string) bool {
	return strings.HasSuffix(jid, jidSuffixDM)
}

// ValidateLink enforces the one-of-(CardDAVUID, JID) invariant and
// derives IsGroup from the JID suffix when the JID branch is used.
// Mutates link in place to fix derivable fields (IsGroup, channel default).
func ValidateLink(link *MemoryContactLink) error {
	if link == nil {
		return errors.New("contact_link: nil link")
	}
	if strings.TrimSpace(link.ID) == "" {
		return errors.New("contact_link: id required")
	}
	if link.Channel == "" {
		link.Channel = ChannelWhatsApp
	}
	link.CardDAVUID = strings.TrimSpace(link.CardDAVUID)
	link.JID = strings.TrimSpace(link.JID)
	link.PreferredPhone = strings.TrimSpace(link.PreferredPhone)

	switch {
	case link.CardDAVUID != "" && link.JID != "":
		return errors.New("contact_link: exactly one of carddav_uid or jid must be set")
	case link.CardDAVUID == "" && link.JID == "":
		return errors.New("contact_link: one of carddav_uid or jid must be set")
	case link.CardDAVUID != "":
		link.IsGroup = false
	case link.JID != "":
		if !IsGroupJID(link.JID) && !IsDMJID(link.JID) {
			return fmt.Errorf("contact_link: jid %q has no recognized suffix (need @s.whatsapp.net or @g.us)", link.JID)
		}
		link.IsGroup = IsGroupJID(link.JID)
	}
	return nil
}

// ContactLinkRepo persists and reads MemoryContactLink rows.
type ContactLinkRepo struct {
	DB *gorm.DB
}

// Migrate creates the table.
func (r *ContactLinkRepo) Migrate() error {
	return r.DB.AutoMigrate(&MemoryContactLink{})
}

// Insert validates the invariant and writes one row.
func (r *ContactLinkRepo) Insert(ctx context.Context, link MemoryContactLink) error {
	if err := ValidateLink(&link); err != nil {
		return err
	}
	return r.DB.WithContext(ctx).Create(&link).Error
}

// Upsert validates and writes (replacing on conflict by ID).
func (r *ContactLinkRepo) Upsert(ctx context.Context, link MemoryContactLink) error {
	if err := ValidateLink(&link); err != nil {
		return err
	}
	return r.DB.WithContext(ctx).
		Clauses(onConflictUpdateAll("id")).Create(&link).Error
}

// GetByID returns the link or nil.
func (r *ContactLinkRepo) GetByID(ctx context.Context, id string) (*MemoryContactLink, error) {
	var l MemoryContactLink
	return firstOrNil(r.DB.WithContext(ctx).Where("id = ?", id), &l)
}

// GetByJID returns the link whose JID matches (group rows only — DM rows
// have an empty JID column and store the address indirectly via CardDAVUID).
// Returns nil when no row matches.
func (r *ContactLinkRepo) GetByJID(ctx context.Context, jid string) (*MemoryContactLink, error) {
	var l MemoryContactLink
	return firstOrNil(r.DB.WithContext(ctx).Where("jid = ?", jid), &l)
}

// GetByCardDAVUID returns the link with the given CardDAVUID, or nil.
func (r *ContactLinkRepo) GetByCardDAVUID(ctx context.Context, uid string) (*MemoryContactLink, error) {
	var l MemoryContactLink
	return firstOrNil(r.DB.WithContext(ctx).Where("carddav_uid = ?", uid), &l)
}

// ListAll returns every visible link ordered by created_at desc.
func (r *ContactLinkRepo) ListAll(ctx context.Context) ([]MemoryContactLink, error) {
	var out []MemoryContactLink
	err := r.DB.WithContext(ctx).Order("created_at DESC").Find(&out).Error
	return out, err
}

// SoftDelete marks one link deleted. The companion MemoryFact is the
// caller's responsibility (the HTTP /api/contacts route deletes both).
func (r *ContactLinkRepo) SoftDelete(ctx context.Context, id string) error {
	return r.DB.WithContext(ctx).
		Where("id = ?", id).Delete(&MemoryContactLink{}).Error
}

// UpdatePreferredPhone updates only the preferred phone column. Used by
// the Settings UI when the user picks a different TEL on a linked vCard.
func (r *ContactLinkRepo) UpdatePreferredPhone(ctx context.Context, id, phone string) error {
	return r.DB.WithContext(ctx).Model(&MemoryContactLink{}).
		Where("id = ?", id).Update("preferred_phone", strings.TrimSpace(phone)).Error
}
