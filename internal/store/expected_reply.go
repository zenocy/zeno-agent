package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ExpectedReply tracks a proactive outbound WhatsApp message for which
// the assistant is awaiting a response. Inserted by the V2.13 assistant
// send path before the wire send (with OutboundMsgID="") and updated
// post-send with the real message ID. Inbound DMs check for an open
// row keyed on ChatJID; a hit suppresses the reactive auto-reply and
// surfaces a deterministic "reply received" card instead.
//
// Lifetime: ExpiresAt = SentAt + 24h. Lazy expiry via the OpenForJID
// query; a weekly cleanup cron deletes rows older than 7 days for audit
// hygiene.
type ExpectedReply struct {
	ID            string         `gorm:"primaryKey;type:text"           json:"id"`
	ChatJID       string         `gorm:"column:chat_jid;index;not null" json:"chat_jid"`
	OutboundMsgID string         `gorm:"column:outbound_msg_id;index"   json:"outbound_msg_id,omitempty"`
	SentAt        time.Time      `gorm:"index;not null"                 json:"sent_at"`
	ExpiresAt     time.Time      `gorm:"index;not null"                 json:"expires_at"`
	ContextKind   string         `                                      json:"context_kind"`
	ContextID     string         `                                      json:"context_id"`
	RecipientName string         `                                      json:"recipient_name"`
	DraftBody     string         `                                      json:"draft_body"`
	ResolvedAt    *time.Time     `gorm:"index"                          json:"resolved_at,omitempty"`
	InboundMsgID  string         `gorm:"column:inbound_msg_id"          json:"inbound_msg_id,omitempty"`
	InboundBody   string         `                                      json:"inbound_body,omitempty"`
	CreatedAt     time.Time      `                                      json:"created_at"`
	DeletedAt     gorm.DeletedAt `gorm:"index"                          json:"-"`
}

// ExpectedReplyRepo persists and queries ExpectedReply rows.
type ExpectedReplyRepo struct {
	DB *gorm.DB
}

// Migrate runs AutoMigrate for the expected_replies table.
//
// V2.13.1 self-heal: an earlier dev build created the ChatJID column
// without an explicit `column:chat_jid` override, leaving GORM to pick
// the default name `chat_j_id`. The current struct overrides it to
// `chat_jid`, but on a DB migrated under the old version AutoMigrate
// adds the new column without dropping the old one — leaving an
// orphan NOT NULL `chat_j_id` that fails every Insert. Detect the
// orphan column and drop it so the table self-heals on next boot.
// V2.13 was the table's first release, so dropping the orphan loses
// no production data.
//
// Implementation note: GORM's Migrator().DropColumn name-maps the
// second arg through the struct schema and silently no-ops when it
// doesn't match a known field. The orphan name isn't a field, so we
// use raw SQL via SQLite's `pragma_table_info` + `ALTER TABLE DROP
// COLUMN`. SQLite has supported DROP COLUMN since 3.35 (March 2021).
func (r *ExpectedReplyRepo) Migrate() error {
	if err := r.DB.AutoMigrate(&ExpectedReply{}); err != nil {
		return err
	}
	var hasOrphan int64
	row := r.DB.Raw(
		`SELECT COUNT(*) FROM pragma_table_info('expected_replies') WHERE name = 'chat_j_id'`,
	).Row()
	if err := row.Scan(&hasOrphan); err != nil {
		// Non-SQLite drivers don't have pragma_table_info — silently
		// skip the self-heal rather than failing boot. Production is
		// SQLite, so this branch is for portability only.
		return nil
	}
	if hasOrphan > 0 {
		if err := r.DB.Exec(`ALTER TABLE expected_replies DROP COLUMN chat_j_id`).Error; err != nil {
			return fmt.Errorf("expected_replies: drop legacy chat_j_id column: %w", err)
		}
	}
	return nil
}

// Insert writes a new row. ID is generated when empty; CreatedAt is
// stamped server-side.
func (r *ExpectedReplyRepo) Insert(ctx context.Context, e *ExpectedReply) error {
	if e.ID == "" {
		e.ID = uuid.NewString()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	return r.DB.WithContext(ctx).Create(e).Error
}

// UpdateOutboundMsgID stamps the wire-side message ID after a successful
// send. Called from the action executor's commit branch.
func (r *ExpectedReplyRepo) UpdateOutboundMsgID(ctx context.Context, id, msgID string) error {
	return r.DB.WithContext(ctx).
		Model(&ExpectedReply{}).
		Where("id = ?", id).
		Update("outbound_msg_id", msgID).Error
}

// Delete removes a row outright. Used when the wire send fails so an
// inbound message can't correlate to a phantom send.
func (r *ExpectedReplyRepo) Delete(ctx context.Context, id string) error {
	return r.DB.WithContext(ctx).Delete(&ExpectedReply{}, "id = ?", id).Error
}

// OpenForJID returns the most-recent unresolved row for chatJID whose
// ExpiresAt is still in the future. Returns nil when nothing matches.
// The hot path on every inbound DM — keep cheap.
func (r *ExpectedReplyRepo) OpenForJID(ctx context.Context, chatJID string, now time.Time) (*ExpectedReply, error) {
	var e ExpectedReply
	q := r.DB.WithContext(ctx).
		Where("chat_jid = ? AND resolved_at IS NULL AND expires_at > ?", chatJID, now).
		Order("sent_at DESC")
	return firstOrNil(q, &e)
}

// OpenContextIDs returns the set of ContextIDs that still have an open
// expected-reply row. Cards-loop dedupe consumes this list to avoid
// re-emitting "text X to confirm" proposals while a send is in flight.
func (r *ExpectedReplyRepo) OpenContextIDs(ctx context.Context, now time.Time) ([]string, error) {
	var ids []string
	err := r.DB.WithContext(ctx).
		Model(&ExpectedReply{}).
		Where("resolved_at IS NULL AND expires_at > ? AND context_id <> ''", now).
		Distinct("context_id").
		Pluck("context_id", &ids).Error
	return ids, err
}

// ListRecent returns rows sent at or after `since`, ordered most-recent
// first, capped at `limit`. Used by the V2.13.2 WhatsAppActivity
// projection to surface "what did Zeno text on the user's behalf
// recently" into the reactive + converse prompts. Includes both
// resolved and unresolved rows — the caller derives status.
func (r *ExpectedReplyRepo) ListRecent(ctx context.Context, since time.Time, limit int) ([]ExpectedReply, error) {
	if limit <= 0 {
		limit = 5
	}
	var rows []ExpectedReply
	err := r.DB.WithContext(ctx).
		Where("sent_at >= ?", since).
		Order("sent_at DESC").
		Limit(limit).
		Find(&rows).Error
	return rows, err
}

// MarkResolved records that an inbound message satisfied an expected
// reply. Stamps ResolvedAt + the inbound message ID + body (truncated).
func (r *ExpectedReplyRepo) MarkResolved(ctx context.Context, id, inboundMsgID, inboundBody string, at time.Time) error {
	if len(inboundBody) > 4*1024 {
		inboundBody = inboundBody[:4*1024]
	}
	return r.DB.WithContext(ctx).
		Model(&ExpectedReply{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"resolved_at":    at,
			"inbound_msg_id": inboundMsgID,
			"inbound_body":   inboundBody,
		}).Error
}

// DeleteExpired hard-deletes rows older than `before`. Called from the
// scheduler weekly so the table stays bounded; resolved rows are kept
// for one week to support audit queries before they age out.
func (r *ExpectedReplyRepo) DeleteExpired(ctx context.Context, before time.Time) (int64, error) {
	res := r.DB.WithContext(ctx).
		Unscoped().
		Where("expires_at < ?", before).
		Delete(&ExpectedReply{})
	return res.RowsAffected, res.Error
}
