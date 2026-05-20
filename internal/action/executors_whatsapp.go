package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/llm"
	logp "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/store"
	"github.com/zenocy/zeno-v2/internal/synth"
)

// AssistantPersonaFn surfaces live assistant-persona settings to the
// executor without a hard dependency on the settings package. Production
// closes over `settings.Service.Snapshot()`. Returns the principal's name
// (e.g. "Jamie"), the assistant's display name (e.g. "Aria"; empty
// disables the feature), and an optional one-line tone steer.
type AssistantPersonaFn func() (userName, assistantName, tone string)

// WhatsAppContact mirrors whatsapp.Contact through a value-only struct so
// the action package can stay free of an internal/whatsapp import. main.go
// builds an adapter that converts the concrete whatsapp.Contact returned
// by whatsapp.Resolver into this shape before handing it back.
type WhatsAppContact struct {
	Name       string
	JID        string
	IsGroup    bool
	FactID     string
	CardDAVUID string
}

// WhatsAppResolver is the seam between the executor and the concrete
// internal/whatsapp.Resolver. Returns ResolveErrAmbiguous /
// ResolveErrNotFound (also defined here) for the executor's
// error-classification path.
type WhatsAppResolver interface {
	Resolve(ctx context.Context, query string) (WhatsAppContact, error)
}

// WhatsAppSender is the seam to the live whatsmeow client. The closure
// shape mirrors what the V2.9 reminder sweeper already uses
// (internal/schedule/reminder_sweeper.go) so the same factory can drive
// both call sites. V2.13.0 added SendTextWithID so the assistant-mode
// send path can capture the wire-side message ID for reply correlation.
type WhatsAppSender interface {
	SendText(ctx context.Context, to, text string) error
	SendTextWithID(ctx context.Context, to, text string) (msgID string, err error)
}

// WhatsAppThrottle abstracts internal/whatsapp.Throttle so the executor
// can apply the per-chat floor without an import dependency.
type WhatsAppThrottle interface {
	Wait(ctx context.Context, jid string, minInterval time.Duration) error
	MarkSent(jid string)
}

// ResolveErrAmbiguous is the error type the executor checks via
// errors.As. main.go's adapter wraps the concrete whatsapp.ErrAmbiguous
// into this type so the action package doesn't need to import the
// whatsapp package for type assertion.
type ResolveErrAmbiguous struct {
	Query      string
	Candidates []string
}

func (e *ResolveErrAmbiguous) Error() string {
	return fmt.Sprintf("contact %q is ambiguous (%d candidates)", e.Query, len(e.Candidates))
}

// ResolveErrNotFound mirrors whatsapp.ErrContactNotFound.
type ResolveErrNotFound struct {
	Query string
}

func (e *ResolveErrNotFound) Error() string {
	return fmt.Sprintf("contact %q not found", e.Query)
}

// WhatsAppDeps bundles the dependencies the V2.12 WhatsApp send executor
// shares with the LLM tools that propose the action. Built once at boot
// in cmd/zeno/main.go.
type WhatsAppDeps struct {
	// Sender returns the live sender. Closure so re-pair across Unlink
	// uses the new client without rebuilding the executor.
	Sender func() WhatsAppSender

	Resolver WhatsAppResolver
	Throttle WhatsAppThrottle

	LLM   llm.Provider
	Voice string

	Reader   logp.Reader
	EventLog logp.Writer
	Logger   *logrus.Entry

	// MinChatInterval mirrors RuntimeConfig.MinChatInterval. Zero falls
	// back to 3 * time.Second.
	MinChatInterval time.Duration

	// DailySendCap bounds proactive sends per user-tz day. 0 disables
	// the cap.
	DailySendCap int

	// V2.13.0: assistant-mode wiring. AssistantPersona supplies the
	// principal/assistant names and tone steer at draft time; nil or
	// empty assistant name → first-person register (legacy behavior).
	// AssistantRegister is the parsed `## Register: assistant` block from
	// `_voice.md`, inlined into the system prompt only when persona is
	// active. ExpectedReplies is the correlation table; nil disables
	// the assistant-mode reply tracking but does not block sends.
	AssistantPersona  AssistantPersonaFn
	AssistantRegister string
	ExpectedReplies   *store.ExpectedReplyRepo
}

// SendWhatsAppExec is the V2.12 outbound WhatsApp action surface. It
// always preflights — first POST returns a draft preview, second POST
// (with confirm:true) commits.
//
// Target shape (consult stringFromTarget):
//   - recipient    string  required; name | alias | JID
//   - message      string  optional; pre-composed body. When set, synth
//     is skipped and the body is used verbatim.
//   - context_kind string  optional; "event" | "mail" | "none"
//   - context_id   string  optional; event UID / mail subject
//   - steer        string  optional; natural-language compose instructions
type SendWhatsAppExec struct {
	Deps WhatsAppDeps
}

func (e *SendWhatsAppExec) Mode() Mode { return ModePreflight }

func (e *SendWhatsAppExec) Execute(ctx context.Context, ec ExecCtx) (Result, error) {
	if e.Deps.Resolver == nil {
		return Result{OK: false, Toast: "WhatsApp send is not configured."}, nil
	}
	senderFn := e.Deps.Sender
	if senderFn == nil {
		return Result{OK: false, Toast: "WhatsApp client unavailable."}, nil
	}

	recipient := stringFromTarget(ec.Target, "recipient")
	if recipient == "" {
		return Result{OK: false, Toast: "Tell me who to message."}, nil
	}

	contact, err := e.Deps.Resolver.Resolve(ctx, recipient)
	if err != nil {
		return whatsAppResolveErrorResult(recipient, err), nil
	}

	preComposed := stringFromTarget(ec.Target, "message")
	steer := stringFromTarget(ec.Target, "steer")
	contextKind := stringFromTarget(ec.Target, "context_kind")
	contextID := stringFromTarget(ec.Target, "context_id")

	personaUser, personaAssistant, personaTone := e.persona()
	asAssistant := personaAssistant != ""

	body := preComposed
	if body == "" {
		body = e.composeBody(ctx, contact, contextKind, contextID, steer, personaUser, personaAssistant, personaTone)
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return Result{OK: false, Toast: "Couldn't compose a message — try giving more steer."}, nil
	}

	if !ec.Confirm {
		preview := map[string]any{
			"to_name":  contact.Name,
			"is_group": contact.IsGroup,
			"body":     body,
			"send":     true,
			"channel":  "whatsapp",
		}
		if contextKind != "" {
			preview["context_kind"] = contextKind
		}
		if contextID != "" {
			preview["context_id"] = contextID
		}
		if asAssistant {
			preview["as_assistant"] = true
			preview["assistant_name"] = personaAssistant
		}
		return Result{
			OK:           true,
			NeedsConfirm: true,
			Preview:      preview,
		}, nil
	}

	// Commit phase.
	sender := senderFn()
	if sender == nil {
		return Result{OK: false, Toast: "WhatsApp is not paired right now."}, nil
	}

	// Daily cap check.
	if e.Deps.DailySendCap > 0 {
		count, err := e.countTodaysProactiveSends(ctx, ec)
		if err != nil && e.Deps.Logger != nil {
			e.Deps.Logger.WithError(err).Warn("send_whatsapp: daily-cap count failed; allowing send")
		} else if count >= e.Deps.DailySendCap {
			return Result{
				OK:    false,
				Toast: fmt.Sprintf("Daily WhatsApp send cap (%d) reached — Zeno will reset at midnight.", e.Deps.DailySendCap),
			}, nil
		}
	}

	// Per-chat throttle.
	if e.Deps.Throttle != nil {
		minInterval := e.Deps.MinChatInterval
		if minInterval <= 0 {
			minInterval = 3 * time.Second
		}
		if err := e.Deps.Throttle.Wait(ctx, contact.JID, minInterval); err != nil {
			return Result{OK: false, Toast: "Send was cancelled."}, err
		}
	}

	// V2.13.0: when assistant mode is enabled and this is a DM-attached
	// event proposal, write the ExpectedReply correlation row BEFORE the
	// wire send so an inbound reply that lands in the wire-time window
	// can never slip past the inbound dispatcher's correlation gate.
	// We update the row with the wire-side message ID after a successful
	// send; on failure the row is deleted so the inbound path doesn't
	// suppress unrelated traffic.
	var expectedReplyID string
	if asAssistant && e.Deps.ExpectedReplies != nil &&
		!contact.IsGroup && contextKind == "event" && strings.TrimSpace(contextID) != "" {
		row := store.ExpectedReply{
			ChatJID:       contact.JID,
			SentAt:        nowOrFallback(ec.Now),
			ContextKind:   contextKind,
			ContextID:     contextID,
			RecipientName: contact.Name,
			DraftBody:     body,
		}
		row.ExpiresAt = row.SentAt.Add(24 * time.Hour)
		if err := e.Deps.ExpectedReplies.Insert(ctx, &row); err != nil {
			if e.Deps.Logger != nil {
				e.Deps.Logger.WithError(err).Warn("send_whatsapp: write expected_reply failed; continuing with send (no correlation)")
			}
		} else {
			expectedReplyID = row.ID
		}
	}

	msgID, err := sender.SendTextWithID(ctx, contact.JID, body)
	if err != nil {
		// Roll back the correlation row so an unrelated inbound on the
		// same JID isn't suppressed in error.
		if expectedReplyID != "" && e.Deps.ExpectedReplies != nil {
			if delErr := e.Deps.ExpectedReplies.Delete(ctx, expectedReplyID); delErr != nil && e.Deps.Logger != nil {
				e.Deps.Logger.WithError(delErr).Warn("send_whatsapp: cleanup expected_reply after failed send")
			}
		}
		failPayload := buildWhatsAppFailedPayload(contact, err)
		if e.Deps.EventLog != nil {
			if _, appendErr := e.Deps.EventLog.Append(ctx, logp.KindWhatsAppMessageFailed, "send_whatsapp", failPayload); appendErr != nil && e.Deps.Logger != nil {
				e.Deps.Logger.WithError(appendErr).Warn("send_whatsapp: append message.failed failed")
			}
		}
		return Result{OK: false, Toast: "WhatsApp send failed; nothing was delivered."}, err
	}

	// Stamp the wire-side message ID on the correlation row.
	if expectedReplyID != "" && msgID != "" && e.Deps.ExpectedReplies != nil {
		if updErr := e.Deps.ExpectedReplies.UpdateOutboundMsgID(ctx, expectedReplyID, msgID); updErr != nil && e.Deps.Logger != nil {
			e.Deps.Logger.WithError(updErr).Warn("send_whatsapp: stamp outbound_msg_id failed")
		}
	}

	if e.Deps.Throttle != nil {
		e.Deps.Throttle.MarkSent(contact.JID)
	}

	payload := buildWhatsAppSentPayload(contact, body)
	if msgID != "" {
		payload["outbound_msg_id"] = msgID
	}
	if asAssistant {
		payload["as_assistant"] = true
		payload["assistant_name"] = personaAssistant
	}
	if contextKind != "" {
		payload["context_kind"] = contextKind
	}
	if contextID != "" {
		payload["context_id"] = contextID
	}

	toastTarget := contact.Name
	if contact.IsGroup {
		toastTarget = "group " + contact.Name
	}
	return Result{
		OK:           true,
		EventKind:    logp.KindWhatsAppMessageSent,
		EventPayload: payload,
		Toast:        fmt.Sprintf("Sent to %s.", toastTarget),
	}, nil
}

// persona reads live assistant-persona settings via the closure. Returns
// empty strings when the closure is unset (legacy boot path) or when
// the user hasn't configured an assistant — both equivalent to "feature
// off".
func (e *SendWhatsAppExec) persona() (userName, assistantName, tone string) {
	if e.Deps.AssistantPersona == nil {
		return "", "", ""
	}
	return e.Deps.AssistantPersona()
}

// nowOrFallback prefers ec.Now (so tests stay deterministic) and falls
// back to time.Now when zero.
func nowOrFallback(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}

// composeBody runs synth.DraftWhatsApp using whatever context the action
// surfaces. It never blocks the executor on synth failure: any error
// (timeout, LLM unavailable, empty body) falls through to a deterministic
// FallbackWhatsAppBody so the preview still has something to show.
func (e *SendWhatsAppExec) composeBody(ctx context.Context, contact WhatsAppContact, kind, id, steer, userName, assistantName, tone string) string {
	dctx := synth.DraftWhatsAppContext{
		RecipientName:     contact.Name,
		IsGroup:           contact.IsGroup,
		UserSteer:         steer,
		UserName:          userName,
		AssistantName:     assistantName,
		AssistantTone:     tone,
		AssistantRegister: e.Deps.AssistantRegister,
	}
	switch kind {
	case "event":
		dctx.EventTitle = id
	case "mail":
		dctx.MailSubject = id
	}

	if e.Deps.LLM != nil {
		body, err := synth.DraftWhatsApp(ctx, synth.DraftWhatsAppOpts{
			LLM:   e.Deps.LLM,
			Voice: e.Deps.Voice,
		}, dctx)
		if err == nil && strings.TrimSpace(body) != "" {
			return body
		}
		if err != nil && e.Deps.Logger != nil {
			e.Deps.Logger.WithError(err).Debug("send_whatsapp: synth draft failed; falling back")
		}
	}
	return synth.FallbackWhatsAppBody(dctx)
}

// countTodaysProactiveSends counts KindWhatsAppMessageSent events with
// proactive:true since midnight in the user's TZ.
func (e *SendWhatsAppExec) countTodaysProactiveSends(ctx context.Context, ec ExecCtx) (int, error) {
	if e.Deps.Reader == nil {
		return 0, errors.New("send_whatsapp: reader unavailable")
	}
	tz := ec.TZ
	if tz == nil {
		tz = time.UTC
	}
	now := ec.Now
	if now.IsZero() {
		now = time.Now()
	}
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, tz)

	events, err := e.Deps.Reader.Since(ctx, midnight)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, ev := range events {
		if ev.Kind != logp.KindWhatsAppMessageSent {
			continue
		}
		var p map[string]any
		if len(ev.Payload) == 0 {
			continue
		}
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			continue
		}
		if v, ok := p["proactive"].(bool); ok && v {
			count++
		}
	}
	return count, nil
}

// whatsAppResolveErrorResult turns a Resolver error into a user-friendly
// Result.
func whatsAppResolveErrorResult(query string, err error) Result {
	var amb *ResolveErrAmbiguous
	if errors.As(err, &amb) {
		return Result{
			OK:    false,
			Toast: fmt.Sprintf("Multiple matches for %q — say which one (%s).", query, strings.Join(amb.Candidates, ", ")),
		}
	}
	var nf *ResolveErrNotFound
	if errors.As(err, &nf) {
		return Result{
			OK:    false,
			Toast: fmt.Sprintf("I don't have %q saved as a contact.", query),
		}
	}
	return Result{OK: false, Toast: "Couldn't resolve that contact."}
}

// buildWhatsAppSentPayload — payload shape for a successful proactive
// send.
func buildWhatsAppSentPayload(contact WhatsAppContact, body string) map[string]any {
	out := map[string]any{
		"proactive": true,
		"to_name":   contact.Name,
		"is_group":  contact.IsGroup,
		"body":      body,
		"chat_jid":  contact.JID,
	}
	if contact.FactID != "" {
		out["fact_id"] = contact.FactID
	}
	if contact.CardDAVUID != "" {
		out["carddav_uid"] = contact.CardDAVUID
	}
	return out
}

// buildWhatsAppFailedPayload — payload shape for an exhausted send.
func buildWhatsAppFailedPayload(contact WhatsAppContact, sendErr error) map[string]any {
	out := map[string]any{
		"proactive": true,
		"to_name":   contact.Name,
		"is_group":  contact.IsGroup,
		"chat_jid":  contact.JID,
		"error":     sendErr.Error(),
	}
	if contact.FactID != "" {
		out["fact_id"] = contact.FactID
	}
	return out
}
