package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/action"
	zlog "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/store"
	"github.com/zenocy/zeno-v2/internal/synth"
)

// ReplyReceivedSignal is the payload published when an inbound DM
// resolves an open ExpectedReply. The replycard package consumes this
// to compose a deterministic "X replied" card; the audit log retains
// the original outbound + inbound bodies via the ExpectedReply row.
type ReplyReceivedSignal struct {
	ChatJID       string
	RecipientName string
	ContextKind   string
	ContextID     string
	DraftBody     string
	ReplyText     string
	ReceivedAt    time.Time
}

// ReplyReceivedNotifier is the seam the SynthHandler uses to publish
// ReplyReceivedSignals without taking a hard dependency on the
// replycard package. cmd/zeno/main.go wires this to a closure that
// calls the live replycard.Notifier.
type ReplyReceivedNotifier interface {
	Notify(ctx context.Context, sig ReplyReceivedSignal) error
}

// AskFunc is the synth-side bridge. It receives the inbound text and a
// Conversation context, and returns a Card whose Speech field carries
// the WhatsApp-ready reply. The cmd/zeno/main.go glue closes over the
// shared LLM client, projections, memory, etc., so this signature stays
// minimal and testable.
type AskFunc func(ctx context.Context, query string, conv *synth.ConversationContext) (synth.Card, error)

// IntentDispatcher is the slice of *action.Handler the WA handler uses
// to auto-execute task / reminder intents server-side. WhatsApp has no
// buttons, so the user can't tap an action; if synth proposes one, we
// run it and confirm in the reply.
type IntentDispatcher interface {
	DispatchIntent(ctx context.Context, in action.DispatchInput) (action.Result, error)
}

// undoAutoExecWindow is the wall time during which a literal "undo"
// reply will reverse the last auto-executed action. Keeps undo from
// tripping on stale state if the user comes back hours later.
const undoAutoExecWindow = 5 * time.Minute

// autoExecIntents is the set of LLM-emitted intents the WA handler
// runs server-side when seen on Card.Actions[0]. Other intents fall
// through to a normal reply (the user can tap them in-app, or the
// reply text already explains what to do).
var autoExecIntents = map[string]bool{
	"add_task":      true,
	"complete_task": true,
	"delete_task":   true,
	"set_reminder":  true,
}

// undoState tracks the last auto-executed action per chat JID so a
// follow-up literal "undo" can reverse it. Process-local map; lost
// across restarts (5-minute window means that's acceptable).
type undoEntry struct {
	intent       string         // the executed intent
	target       map[string]any // the executed target
	executedAt   time.Time
	resultID     string // reminder_id or task UID, when relevant
	resultTitle  string
	originalLine string // for delete_task: the line that was removed (for re-add)
}

// SynthHandler implements MessageHandler by routing each Process
// decision through synth.Ask (or a pre-canned reply for unsupported
// media) and sending the result back over WhatsApp. It also writes
// audit events to the observation log so the Settings UI / metrics
// surface can show recent activity.
//
// The handler is deliberately stateless aside from injected
// dependencies — concurrency is owned by the Dispatcher.
type SynthHandler struct {
	// Ask runs synth on the inbound text. Required.
	Ask AskFunc

	// Client returns the live whatsapp.Client for sending. The closure
	// is re-evaluated per message because the Service swaps clients
	// across Unlink → re-pair without process restart.
	Client func() Client

	// EventLog is the observation log writer. Required.
	EventLog zlog.Writer

	// Now sources current time for backoff bookkeeping. Defaults to
	// time.Now when nil.
	Now func() time.Time

	// Sleep is the delay primitive; tests inject a no-op to skip
	// real-time backoff. Defaults to time.Sleep when nil.
	Sleep func(time.Duration)

	// SendBackoffs is the per-attempt delay sequence. The 0th element
	// is a pre-attempt delay (typically 0); element N is the wait
	// AFTER attempt N before retrying. Defaults to {0, 1s, 4s, 16s}.
	SendBackoffs []time.Duration

	// SendDeadline caps the total wall time spent retrying a single
	// send. Defaults to 1 minute.
	SendDeadline time.Duration

	Logger *logrus.Entry

	// V2.9: Action is the bridge to the action.Handler.DispatchIntent
	// path. When non-nil, the handler auto-executes Card.Actions[0]
	// for whitelisted intents (add_task / set_reminder / complete_task
	// / delete_task) since WhatsApp has no buttons.
	Action IntentDispatcher

	// V2.13.0: assistant-mode reply correlation. When ExpectedReplies is
	// non-nil, every inbound DM is checked against the table; a match
	// resolves the row, suppresses the reactive auto-reply, and
	// notifies ReplyReceived (which composes the in-app card). Both
	// nil → legacy V2.7 receive behavior unchanged.
	ExpectedReplies *store.ExpectedReplyRepo
	ReplyReceived   ReplyReceivedNotifier

	// undoMu guards undoLast. The Dispatcher serializes per-chat so
	// races on the same JID don't happen, but cross-chat parallelism
	// can still touch the map concurrently.
	undoMu   sync.Mutex
	undoLast map[string]undoEntry // key: chat JID
}

// Handle processes one Process decision: log received, compute reply,
// send with backoff, log sent (or failed). Errors propagate to the
// Dispatcher, which logs at WARN; failure is otherwise non-fatal.
func (h *SynthHandler) Handle(ctx context.Context, dec Decision) error {
	if dec.Action != ActionProcess {
		return nil
	}
	if h.Logger == nil {
		h.Logger = logrus.NewEntry(logrus.New())
	}
	now := h.Now
	if now == nil {
		now = time.Now
	}

	if h.EventLog != nil {
		_, err := h.EventLog.Append(ctx, zlog.KindWhatsAppMessageRecv, "whatsapp", buildRecvPayload(dec))
		if err != nil {
			h.Logger.WithError(err).Warn("whatsapp: append message.received failed")
		}
	}

	// V2.13.0: assistant-mode reply correlation. If this inbound DM
	// satisfies an open ExpectedReply, suppress the existing reactive
	// auto-reply path (Dana isn't expecting Zeno to chat back —
	// he's expecting Jamie) and surface the response as a card on
	// the briefing rail instead.
	if dec.IsDM && h.ExpectedReplies != nil {
		if open, lookupErr := h.ExpectedReplies.OpenForJID(ctx, dec.ChatJID, now()); lookupErr != nil {
			h.Logger.WithError(lookupErr).Warn("whatsapp: lookup expected_reply failed; falling through to synth")
		} else if open != nil {
			if markErr := h.ExpectedReplies.MarkResolved(ctx, open.ID, dec.MessageID, dec.Text, now()); markErr != nil {
				h.Logger.WithError(markErr).Warn("whatsapp: mark expected_reply resolved failed")
			}
			if h.ReplyReceived != nil {
				sig := ReplyReceivedSignal{
					ChatJID:       open.ChatJID,
					RecipientName: open.RecipientName,
					ContextKind:   open.ContextKind,
					ContextID:     open.ContextID,
					DraftBody:     open.DraftBody,
					ReplyText:     dec.Text,
					ReceivedAt:    now(),
				}
				if notifyErr := h.ReplyReceived.Notify(ctx, sig); notifyErr != nil {
					h.Logger.WithError(notifyErr).Warn("whatsapp: reply-received notifier failed")
				}
			}
			if h.EventLog != nil {
				suppressPayload := map[string]any{
					"chat_jid":       dec.ChatJID,
					"reason":         "expected_reply",
					"context_kind":   open.ContextKind,
					"context_id":     open.ContextID,
					"recipient_name": open.RecipientName,
				}
				if _, err := h.EventLog.Append(ctx, zlog.KindWhatsAppMessageSuppressed, "whatsapp", suppressPayload); err != nil {
					h.Logger.WithError(err).Warn("whatsapp: append message.suppressed failed")
				}
			}
			return nil
		}
	}

	// V2.9: literal "undo" reverses the last auto-executed action for
	// this chat — only when (a) we have an action recorded, (b) it's
	// within the 5-minute window. Pre-classified before synth so the
	// LLM never has to handle this control verb. Anything else (even
	// "undo this please") falls through to synth as a regular Ask.
	if h.isUndoCommand(dec.Text) {
		if reply, ok := h.tryUndo(ctx, dec.ChatJID); ok {
			return h.sendAndAudit(ctx, dec, reply, "undo", now)
		}
		// No recent action → fall through to normal synth path.
	}

	var reply string
	var mode string
	if dec.PreCannedReply != "" {
		reply = dec.PreCannedReply
		mode = "precanned"
	} else {
		conv := &synth.ConversationContext{
			SenderName: dec.SenderName,
			GroupName:  dec.GroupName,
			IsDM:       dec.IsDM,
			IsMention:  dec.IsMention,
		}
		card, err := h.Ask(ctx, dec.Text, conv)
		if err != nil {
			h.recordFailure(ctx, dec, 0, err)
			return fmt.Errorf("synth ask: %w", err)
		}
		reply = card.Speech
		if reply == "" {
			h.Logger.WithField("title", card.Title).
				Warn("whatsapp: card produced no Speech; falling back to Sub")
			reply = card.Sub
		}
		mode = "synth"

		// V2.9: auto-execute task / reminder intents server-side. WA
		// has no buttons, so a card whose primary action is add_task
		// / set_reminder / complete_task / delete_task can't be tapped
		// — run it and append a confirmation + undo hint to the reply.
		if extra := h.tryAutoExec(ctx, dec.ChatJID, card, now()); extra != "" {
			reply = strings.TrimRight(reply, "\n ")
			if reply != "" {
				reply += "\n\n"
			}
			reply += extra
			mode = "synth+exec"
		}
	}

	return h.sendAndAudit(ctx, dec, reply, mode, now)
}

// sendAndAudit performs the SendText call (with backoff) and writes
// the message.sent / message.failed audit row. Pulled out of Handle so
// the V2.9 undo path can short-circuit synth and still get the same
// retry + audit treatment.
func (h *SynthHandler) sendAndAudit(ctx context.Context, dec Decision, reply, mode string, now func() time.Time) error {
	client := h.Client()
	if client == nil {
		err := errors.New("whatsapp: client unavailable")
		h.recordFailure(ctx, dec, 0, err)
		return err
	}

	attempts, err := h.sendWithBackoff(ctx, client, dec.ChatJID, reply, now)
	if err != nil {
		h.recordFailure(ctx, dec, attempts, err)
		return err
	}

	if h.EventLog != nil {
		_, appendErr := h.EventLog.Append(ctx, zlog.KindWhatsAppMessageSent, "whatsapp", buildSentPayload(dec, reply, mode, attempts))
		if appendErr != nil {
			h.Logger.WithError(appendErr).Warn("whatsapp: append message.sent failed")
		}
	}
	return nil
}

// sendWithBackoff retries SendText according to the configured backoff
// schedule. Thin wrapper over the shared SendWithBackoff helper so both
// the receive-reply path (this handler) and the proactive `send_whatsapp`
// action surface use one retry posture.
func (h *SynthHandler) sendWithBackoff(ctx context.Context, c Client, to, text string, now func() time.Time) (int, error) {
	return SendWithBackoff(ctx, c, to, text, h.SendBackoffs, h.SendDeadline, h.Sleep, now, h.Logger)
}

// recordFailure writes a whatsapp.message.failed audit row.
func (h *SynthHandler) recordFailure(ctx context.Context, dec Decision, attempts int, sendErr error) {
	if h.EventLog == nil {
		return
	}
	payload := map[string]any{
		"chat_jid":   dec.ChatJID,
		"source_id":  dec.MessageID,
		"sender_jid": dec.SenderJID,
		"is_dm":      dec.IsDM,
		"attempts":   attempts,
		"error":      sendErr.Error(),
	}
	if _, err := h.EventLog.Append(ctx, zlog.KindWhatsAppMessageFailed, "whatsapp", payload); err != nil && h.Logger != nil {
		h.Logger.WithError(err).Warn("whatsapp: append message.failed failed")
	}
}

const recvBodyPreviewMax = 4 * 1024

func buildRecvPayload(dec Decision) map[string]any {
	body := dec.Text
	if len(body) > recvBodyPreviewMax {
		body = body[:recvBodyPreviewMax]
	}
	return map[string]any{
		"chat_jid":     dec.ChatJID,
		"sender_jid":   dec.SenderJID,
		"sender_name":  dec.SenderName,
		"group_name":   dec.GroupName,
		"is_dm":        dec.IsDM,
		"is_mention":   dec.IsMention,
		"message_id":   dec.MessageID,
		"timestamp":    dec.Timestamp,
		"body_preview": body,
	}
}

func buildSentPayload(dec Decision, reply, mode string, attempts int) map[string]any {
	return map[string]any{
		"chat_jid":  dec.ChatJID,
		"source_id": dec.MessageID,
		"reply":     reply,
		"mode":      mode,
		"attempts":  attempts,
	}
}

// ----------------------------------------------------------------------
// V2.9: auto-execute + undo
// ----------------------------------------------------------------------

// isUndoCommand reports whether the inbound text is a literal "undo".
// Tight match: trimmed, lowercased, exact "undo" only. Phrases like
// "undo that" or "please undo" go through synth — anything that isn't
// a single-word command is the user expressing intent in prose, not
// reaching for a control verb.
func (h *SynthHandler) isUndoCommand(text string) bool {
	return strings.EqualFold(strings.TrimSpace(text), "undo")
}

// tryAutoExec inspects the LLM-produced card. If its primary action
// (Actions[0]) is in the auto-exec whitelist AND we have a dispatcher
// wired, run it server-side and return the confirmation+undo string
// to append to the user's reply. Returns "" when the card has no
// auto-exec action or when execution fails.
func (h *SynthHandler) tryAutoExec(ctx context.Context, chatJID string, card synth.Card, now time.Time) string {
	if h.Action == nil || len(card.Actions) == 0 {
		return ""
	}
	primary := card.Actions[0]
	if !autoExecIntents[primary.Intent] {
		return ""
	}

	target := primary.Target
	if target == nil {
		target = map[string]any{}
	}

	result, _ := h.Action.DispatchIntent(ctx, action.DispatchInput{
		Intent: primary.Intent,
		Target: target,
	})
	if !result.OK {
		// Surface the executor's reason so the user can correct on
		// retry (e.g. "Tasks file not configured."). No undo entry.
		if result.Toast != "" {
			return result.Toast
		}
		return ""
	}

	// Record for undo within the 5-minute window. The result payload
	// from set_reminder carries `reminder_id`; add_task does not have
	// a UID return, so we store the title and rely on tryUndo to
	// resolve the task→UID via the file at undo time.
	entry := undoEntry{
		intent:     primary.Intent,
		target:     target,
		executedAt: now,
	}
	// V2.11: add_task / set_reminder both emit "uid" (the new task's
	// UUID) in their outcome payload. The legacy "reminder_id" key is
	// gone. tryUndo() reads entry.resultID for both intents.
	if id, ok := result.EventPayload["uid"].(string); ok {
		entry.resultID = id
	} else if id, ok := result.EventPayload["task_uid"].(string); ok {
		entry.resultID = id
	}
	if title, ok := target["title"].(string); ok {
		entry.resultTitle = title
	}
	h.recordUndoEntry(chatJID, entry)

	confirm := result.Toast
	if confirm == "" {
		confirm = "Done."
	}
	if isReversibleIntent(primary.Intent) {
		return confirm + " Reply 'undo' to reverse."
	}
	return confirm
}

// isReversibleIntent reports whether tryUndo can reverse the given
// intent. Only the create-side verbs are reversible right now —
// complete_task / delete_task are tagged here as too-cheap-to-bother
// (the user can manually re-edit the file or recompletes are idempotent).
func isReversibleIntent(intent string) bool {
	switch intent {
	case "add_task", "set_reminder":
		return true
	}
	return false
}

// recordUndoEntry stores the latest auto-executed action for chatJID.
// Lazy-initializes the map.
func (h *SynthHandler) recordUndoEntry(chatJID string, e undoEntry) {
	h.undoMu.Lock()
	defer h.undoMu.Unlock()
	if h.undoLast == nil {
		h.undoLast = map[string]undoEntry{}
	}
	h.undoLast[chatJID] = e
}

// tryUndo reverses the last auto-executed action for chatJID when one
// is recorded within undoAutoExecWindow. Returns the user-facing reply
// text and ok=true on success; ok=false signals the caller to fall
// through to the normal synth path.
func (h *SynthHandler) tryUndo(ctx context.Context, chatJID string) (string, bool) {
	h.undoMu.Lock()
	entry, found := h.undoLast[chatJID]
	if found {
		// Single-shot: remove the entry whether or not the reversal
		// succeeds — preserves the "5 min window, one undo per
		// action" contract regardless of error.
		delete(h.undoLast, chatJID)
	}
	h.undoMu.Unlock()
	if !found {
		return "", false
	}
	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}
	if now.Sub(entry.executedAt) > undoAutoExecWindow {
		return "", false
	}
	if h.Action == nil {
		return "", false
	}

	switch entry.intent {
	case "add_task":
		// V2.11: Reverse via delete_task using the UUID captured from
		// the add_task outcome event.
		if entry.resultID == "" {
			return "Could not undo: task ID missing.", true
		}
		title, _ := entry.target["title"].(string)
		result, _ := h.Action.DispatchIntent(ctx, action.DispatchInput{
			Intent: "delete_task",
			Target: map[string]any{"uid": entry.resultID},
		})
		if !result.OK {
			return "Could not undo: task not found.", true
		}
		return fmt.Sprintf("Undone — removed: %s", title), true

	case "set_reminder":
		// V2.11: a reminder is now a task with fire_at set. Undo by
		// clearing fire_at on the underlying task. SetReminderExec's
		// outcome payload carries task_uid; entry.resultID was set
		// from that payload above.
		if entry.resultID == "" {
			return "Could not undo: reminder ID missing.", true
		}
		// We don't have a dedicated clear_reminder intent. The
		// cleanest path is to delete the underlying task — for the
		// "set reminder on a card" flow this is what the user wants
		// (the reminder-task is a one-shot). For an "attach reminder
		// to existing task" flow this is too aggressive; that path
		// won't undo through here today (future expansion).
		title := entry.resultTitle
		result, _ := h.Action.DispatchIntent(ctx, action.DispatchInput{
			Intent: "delete_task",
			Target: map[string]any{"uid": entry.resultID},
		})
		if !result.OK {
			return fmt.Sprintf("Reminder %q is already scheduled — let it fire and dismiss it then.", title), true
		}
		return fmt.Sprintf("Undone — cancelled reminder: %s", title), true
	}
	return "", false
}
