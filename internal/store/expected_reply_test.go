package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestExpectedReplyRepo_LifecycleAndQueries(t *testing.T) {
	db := openTestDB(t)
	repo := &ExpectedReplyRepo{DB: db}
	require.NoError(t, repo.Migrate())
	ctx := context.Background()
	now := time.Date(2026, 5, 10, 20, 0, 0, 0, time.UTC)

	// Insert a row.
	row := ExpectedReply{
		ChatJID:       "447700900111@s.whatsapp.net",
		SentAt:        now,
		ExpiresAt:     now.Add(24 * time.Hour),
		ContextKind:   "event",
		ContextID:     "evt-dinner-2026-05-10",
		RecipientName: "Dana",
		DraftBody:     "Hi Dana — Jamie asked me to confirm dinner tonight at 8pm.",
	}
	require.NoError(t, repo.Insert(ctx, &row))
	require.NotEmpty(t, row.ID, "Insert should have generated an ID")

	// OpenForJID returns the row when no other open rows exist.
	open, err := repo.OpenForJID(ctx, row.ChatJID, now.Add(time.Minute))
	require.NoError(t, err)
	require.NotNil(t, open)
	require.Equal(t, row.ID, open.ID)
	require.Equal(t, "Dana", open.RecipientName)

	// OpenContextIDs lists the row's context_id.
	ids, err := repo.OpenContextIDs(ctx, now.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, []string{"evt-dinner-2026-05-10"}, ids)

	// UpdateOutboundMsgID stamps the wire-side message ID.
	require.NoError(t, repo.UpdateOutboundMsgID(ctx, row.ID, "WAMSG-42"))
	open, err = repo.OpenForJID(ctx, row.ChatJID, now.Add(time.Minute))
	require.NoError(t, err)
	require.NotNil(t, open)
	require.Equal(t, "WAMSG-42", open.OutboundMsgID)

	// MarkResolved flips the row out of the open set.
	resolveAt := now.Add(2 * time.Hour)
	require.NoError(t, repo.MarkResolved(ctx, row.ID, "INBOUND-7", "Yes, 7pm works", resolveAt))
	open, err = repo.OpenForJID(ctx, row.ChatJID, now.Add(2*time.Hour+time.Minute))
	require.NoError(t, err)
	require.Nil(t, open, "resolved row should not be returned")

	// And drops out of the context-id list.
	ids, err = repo.OpenContextIDs(ctx, now.Add(2*time.Hour+time.Minute))
	require.NoError(t, err)
	require.Empty(t, ids)
}

func TestExpectedReplyRepo_OpenForJID_Expired(t *testing.T) {
	db := openTestDB(t)
	repo := &ExpectedReplyRepo{DB: db}
	require.NoError(t, repo.Migrate())
	ctx := context.Background()

	sent := time.Date(2026, 5, 9, 10, 0, 0, 0, time.UTC)
	row := ExpectedReply{
		ChatJID:   "447700900111@s.whatsapp.net",
		SentAt:    sent,
		ExpiresAt: sent.Add(24 * time.Hour),
		ContextID: "evt-old",
	}
	require.NoError(t, repo.Insert(ctx, &row))

	// 25 hours later — past expiry; OpenForJID returns nil.
	open, err := repo.OpenForJID(ctx, row.ChatJID, sent.Add(25*time.Hour))
	require.NoError(t, err)
	require.Nil(t, open)
}

func TestExpectedReplyRepo_OpenForJID_MostRecentWins(t *testing.T) {
	db := openTestDB(t)
	repo := &ExpectedReplyRepo{DB: db}
	require.NoError(t, repo.Migrate())
	ctx := context.Background()

	jid := "447700900111@s.whatsapp.net"
	older := ExpectedReply{
		ChatJID:   jid,
		SentAt:    time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC),
		ExpiresAt: time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC),
		ContextID: "evt-lunch",
	}
	newer := ExpectedReply{
		ChatJID:   jid,
		SentAt:    time.Date(2026, 5, 10, 20, 0, 0, 0, time.UTC),
		ExpiresAt: time.Date(2026, 5, 11, 20, 0, 0, 0, time.UTC),
		ContextID: "evt-dinner",
	}
	require.NoError(t, repo.Insert(ctx, &older))
	require.NoError(t, repo.Insert(ctx, &newer))

	open, err := repo.OpenForJID(ctx, jid, time.Date(2026, 5, 10, 21, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.NotNil(t, open)
	require.Equal(t, "evt-dinner", open.ContextID)
}

func TestExpectedReplyRepo_DeleteExpired(t *testing.T) {
	db := openTestDB(t)
	repo := &ExpectedReplyRepo{DB: db}
	require.NoError(t, repo.Migrate())
	ctx := context.Background()

	old := ExpectedReply{
		ChatJID:   "a@s.whatsapp.net",
		SentAt:    time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		ExpiresAt: time.Date(2026, 4, 2, 0, 0, 0, 0, time.UTC),
		ContextID: "evt-old",
	}
	fresh := ExpectedReply{
		ChatJID:   "b@s.whatsapp.net",
		SentAt:    time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC),
		ExpiresAt: time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC),
		ContextID: "evt-fresh",
	}
	require.NoError(t, repo.Insert(ctx, &old))
	require.NoError(t, repo.Insert(ctx, &fresh))

	// Delete rows with expires_at < cutoff.
	cutoff := time.Date(2026, 5, 4, 0, 0, 0, 0, time.UTC)
	deleted, err := repo.DeleteExpired(ctx, cutoff)
	require.NoError(t, err)
	require.Equal(t, int64(1), deleted)

	ids, err := repo.OpenContextIDs(ctx, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Equal(t, []string{"evt-fresh"}, ids)
}

// TestExpectedReplyRepo_Migrate_HealsOrphanColumn verifies the V2.13.1
// self-heal: a DB migrated under the original (no column override)
// struct ended up with an orphan NOT NULL `chat_j_id`. Migrate now
// detects and drops it.
func TestExpectedReplyRepo_Migrate_HealsOrphanColumn(t *testing.T) {
	db := openTestDB(t)
	// Simulate the broken schema: a manually-created table with the
	// orphan column.
	require.NoError(t, db.Exec(`
		CREATE TABLE expected_replies (
			id TEXT PRIMARY KEY,
			chat_j_id TEXT NOT NULL,
			outbound_msg_id TEXT,
			sent_at datetime NOT NULL,
			expires_at datetime NOT NULL,
			context_kind TEXT,
			context_id TEXT,
			recipient_name TEXT,
			draft_body TEXT,
			resolved_at datetime,
			inbound_msg_id TEXT,
			inbound_body TEXT,
			created_at datetime,
			deleted_at datetime
		)
	`).Error)

	repo := &ExpectedReplyRepo{DB: db}
	require.NoError(t, repo.Migrate(), "migrate should self-heal the orphan column")

	// chat_j_id is gone; chat_jid is present.
	require.False(t, db.Migrator().HasColumn(&ExpectedReply{}, "chat_j_id"),
		"orphan chat_j_id should be dropped")
	require.True(t, db.Migrator().HasColumn(&ExpectedReply{}, "chat_jid"),
		"correctly-named chat_jid should exist")

	// Insert must succeed end-to-end now.
	row := ExpectedReply{
		ChatJID:   "self-heal@s.whatsapp.net",
		SentAt:    time.Now(),
		ExpiresAt: time.Now().Add(24 * time.Hour),
		ContextID: "evt-self-heal",
	}
	require.NoError(t, repo.Insert(context.Background(), &row))
}

// TestExpectedReplyRepo_ListRecent verifies V2.13.2's projection feed:
// rows since `since`, ordered newest-first, capped, both resolved and
// unresolved included.
func TestExpectedReplyRepo_ListRecent(t *testing.T) {
	db := openTestDB(t)
	repo := &ExpectedReplyRepo{DB: db}
	require.NoError(t, repo.Migrate())
	ctx := context.Background()

	now := time.Date(2026, 5, 10, 18, 0, 0, 0, time.UTC)
	// Three rows: one outside the window, two inside. Mix resolved + open.
	out := ExpectedReply{
		ChatJID:   "old@s.whatsapp.net",
		SentAt:    now.Add(-30 * time.Hour),
		ExpiresAt: now.Add(-6 * time.Hour),
		ContextID: "out-of-window",
	}
	open := ExpectedReply{
		ChatJID:   "open@s.whatsapp.net",
		SentAt:    now.Add(-2 * time.Hour),
		ExpiresAt: now.Add(22 * time.Hour),
		ContextID: "open-row",
	}
	resolvedAt := now.Add(-30 * time.Minute)
	resolved := ExpectedReply{
		ChatJID:    "resolved@s.whatsapp.net",
		SentAt:     now.Add(-1 * time.Hour),
		ExpiresAt:  now.Add(23 * time.Hour),
		ContextID:  "resolved-row",
		ResolvedAt: &resolvedAt,
	}
	require.NoError(t, repo.Insert(ctx, &out))
	require.NoError(t, repo.Insert(ctx, &open))
	require.NoError(t, repo.Insert(ctx, &resolved))

	since := now.Add(-24 * time.Hour)
	rows, err := repo.ListRecent(ctx, since, 5)
	require.NoError(t, err)
	require.Len(t, rows, 2, "outside-window row excluded")
	// Newest first: resolved (−1h) before open (−2h).
	require.Equal(t, "resolved-row", rows[0].ContextID)
	require.Equal(t, "open-row", rows[1].ContextID)

	// Cap.
	rows, err = repo.ListRecent(ctx, since, 1)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "resolved-row", rows[0].ContextID)
}

func TestExpectedReplyRepo_DeleteOnFailure(t *testing.T) {
	db := openTestDB(t)
	repo := &ExpectedReplyRepo{DB: db}
	require.NoError(t, repo.Migrate())
	ctx := context.Background()

	row := ExpectedReply{
		ChatJID:   "fail@s.whatsapp.net",
		SentAt:    time.Now(),
		ExpiresAt: time.Now().Add(24 * time.Hour),
		ContextID: "evt-fail",
	}
	require.NoError(t, repo.Insert(ctx, &row))
	require.NoError(t, repo.Delete(ctx, row.ID))

	open, err := repo.OpenForJID(ctx, row.ChatJID, time.Now())
	require.NoError(t, err)
	require.Nil(t, open)
}
