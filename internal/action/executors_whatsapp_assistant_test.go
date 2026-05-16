package action

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/store"
)

func openExpectedReplyRepo(t *testing.T) *store.ExpectedReplyRepo {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "store.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	repo := &store.ExpectedReplyRepo{DB: db}
	require.NoError(t, repo.Migrate())
	return repo
}

// TestSendWhatsAppExec_AssistantPreviewIncludesBadge verifies the
// preview map carries `as_assistant` + `assistant_name` when persona
// is configured. The preview is what the ActionConfirmModal renders
// the "From X (your assistant)" badge from.
func TestSendWhatsAppExec_AssistantPreviewIncludesBadge(t *testing.T) {
	resolver := &fakeResolver{contact: WhatsAppContact{
		Name: "Dana", JID: "1@s.whatsapp.net",
	}}
	sender := &fakeSender{}
	exec := &SendWhatsAppExec{Deps: WhatsAppDeps{
		Resolver: resolver,
		Sender:   func() WhatsAppSender { return sender },
		AssistantPersona: func() (string, string, string) {
			return "Jamie", "Aria", "warm but brisk"
		},
	}}

	res, err := exec.Execute(context.Background(), ExecCtx{
		Target: map[string]any{
			"recipient": "Dana",
			"message":   "Hi Dana — Jamie asked me to confirm dinner tonight.\n— Aria",
		},
		Confirm: false,
	})
	require.NoError(t, err)
	require.True(t, res.OK)
	require.True(t, res.NeedsConfirm)
	require.NotNil(t, res.Preview)

	asAssistant, _ := res.Preview["as_assistant"].(bool)
	assert.True(t, asAssistant, "preview must carry as_assistant=true when persona is set")
	assert.Equal(t, "Aria", res.Preview["assistant_name"])
}

// TestSendWhatsAppExec_NoPersona_NoBadge ensures the preview shape stays
// V2.12-compatible when persona is unset.
func TestSendWhatsAppExec_NoPersona_NoBadge(t *testing.T) {
	resolver := &fakeResolver{contact: WhatsAppContact{
		Name: "Sam", JID: "1@s.whatsapp.net",
	}}
	exec := &SendWhatsAppExec{Deps: WhatsAppDeps{
		Resolver: resolver,
		Sender:   func() WhatsAppSender { return &fakeSender{} },
	}}
	res, err := exec.Execute(context.Background(), ExecCtx{
		Target:  map[string]any{"recipient": "Sam", "message": "Hi"},
		Confirm: false,
	})
	require.NoError(t, err)
	require.True(t, res.OK)
	require.NotContains(t, res.Preview, "as_assistant")
	require.NotContains(t, res.Preview, "assistant_name")
}

// TestSendWhatsAppExec_AssistantWritesExpectedReplyRow asserts the
// commit branch writes the correlation row with the wire-side message ID
// stamped, when persona is on AND context is an event DM.
func TestSendWhatsAppExec_AssistantWritesExpectedReplyRow(t *testing.T) {
	repo := openExpectedReplyRepo(t)
	resolver := &fakeResolver{contact: WhatsAppContact{
		Name: "Dana", JID: "447700900111@s.whatsapp.net",
	}}
	sender := &fakeSender{msgID: "WAMSG-77"}
	exec := &SendWhatsAppExec{Deps: WhatsAppDeps{
		Resolver: resolver,
		Sender:   func() WhatsAppSender { return sender },
		AssistantPersona: func() (string, string, string) {
			return "Jamie", "Aria", ""
		},
		ExpectedReplies: repo,
	}}

	res, err := exec.Execute(context.Background(), ExecCtx{
		Target: map[string]any{
			"recipient":    "Dana",
			"message":      "Hi Dana — Jamie asked me to confirm dinner.\n— Aria",
			"context_kind": "event",
			"context_id":   "evt-dinner-2026-05-10",
		},
		Confirm: true,
		Now:     time.Now(),
	})
	require.NoError(t, err)
	require.True(t, res.OK)

	// The row should be open with the wire-side message ID stamped.
	open, err := repo.OpenForJID(context.Background(), "447700900111@s.whatsapp.net", time.Now())
	require.NoError(t, err)
	require.NotNil(t, open)
	assert.Equal(t, "WAMSG-77", open.OutboundMsgID)
	assert.Equal(t, "evt-dinner-2026-05-10", open.ContextID)
	assert.Equal(t, "Dana", open.RecipientName)

	// The audit payload carries as_assistant + outbound_msg_id.
	assert.Equal(t, true, res.EventPayload["as_assistant"])
	assert.Equal(t, "Aria", res.EventPayload["assistant_name"])
	assert.Equal(t, "WAMSG-77", res.EventPayload["outbound_msg_id"])
}

// TestSendWhatsAppExec_AssistantSkipsRowOnFailure asserts a wire-send
// failure deletes the correlation row so an unrelated inbound on the
// same JID is never suppressed in error.
func TestSendWhatsAppExec_AssistantSkipsRowOnFailure(t *testing.T) {
	logStore := openLogStore(t)
	repo := openExpectedReplyRepo(t)
	resolver := &fakeResolver{contact: WhatsAppContact{
		Name: "Dana", JID: "447700900111@s.whatsapp.net",
	}}
	sender := &fakeSender{err: errors.New("network down")}
	exec := &SendWhatsAppExec{Deps: WhatsAppDeps{
		Resolver: resolver,
		Sender:   func() WhatsAppSender { return sender },
		EventLog: logStore,
		AssistantPersona: func() (string, string, string) {
			return "Jamie", "Aria", ""
		},
		ExpectedReplies: repo,
	}}

	_, err := exec.Execute(context.Background(), ExecCtx{
		Target: map[string]any{
			"recipient":    "Dana",
			"message":      "Hi.\n— Aria",
			"context_kind": "event",
			"context_id":   "evt-failed",
		},
		Confirm: true,
		Now:     time.Now(),
	})
	require.Error(t, err)

	// No open row should remain.
	open, err := repo.OpenForJID(context.Background(), "447700900111@s.whatsapp.net", time.Now())
	require.NoError(t, err)
	assert.Nil(t, open, "row should be deleted on send failure")
}

// TestSendWhatsAppExec_AssistantNoRowForGroupOrNonEvent asserts the
// correlation row is only written for DM event proposals — group sends
// or mail-context sends shouldn't track replies (group chats can have
// many speakers; mail context is one-shot).
func TestSendWhatsAppExec_AssistantNoRowForGroup(t *testing.T) {
	repo := openExpectedReplyRepo(t)
	resolver := &fakeResolver{contact: WhatsAppContact{
		Name: "The Squad", JID: "1234@g.us", IsGroup: true,
	}}
	sender := &fakeSender{}
	exec := &SendWhatsAppExec{Deps: WhatsAppDeps{
		Resolver: resolver,
		Sender:   func() WhatsAppSender { return sender },
		AssistantPersona: func() (string, string, string) {
			return "Jamie", "Aria", ""
		},
		ExpectedReplies: repo,
	}}

	res, err := exec.Execute(context.Background(), ExecCtx{
		Target: map[string]any{
			"recipient":    "The Squad",
			"message":      "Hi all.\n— Aria",
			"context_kind": "event",
			"context_id":   "evt-group",
		},
		Confirm: true,
		Now:     time.Now(),
	})
	require.NoError(t, err)
	require.True(t, res.OK)

	open, err := repo.OpenForJID(context.Background(), "1234@g.us", time.Now())
	require.NoError(t, err)
	assert.Nil(t, open, "no correlation row should exist for group sends")
}
