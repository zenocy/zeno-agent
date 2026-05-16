// Package replycard composes the deterministic in-app card surfaced when
// an inbound WhatsApp message satisfies an open assistant-mode
// ExpectedReply. The card text is built without an LLM call so the
// content is trustworthy (verbatim quote of the recipient's reply) and
// latency-free (no streaming wait between reply and surface).
package replycard

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"gorm.io/datatypes"

	"github.com/zenocy/zeno-v2/internal/eventbus"
	"github.com/zenocy/zeno-v2/internal/store"
	"github.com/zenocy/zeno-v2/internal/whatsapp"
)

// inboundQuoteMax bounds the inbound body we paste into the card sub.
// WhatsApp messages can be long; the card surface is a one-glance
// affordance, not a thread viewer.
const inboundQuoteMax = 280

// Notifier writes a `kind=reply_received` Card to the persistence layer
// and fires the SSE event so the live rail picks it up. Implements
// internal/whatsapp.ReplyReceivedNotifier.
type Notifier struct {
	Cards *store.CardRepo
	Bus   *eventbus.Bus
	Now   func() time.Time // optional; defaults to time.Now
	Log   *logrus.Entry
}

// Notify composes and persists the reply-received card.
func (n *Notifier) Notify(ctx context.Context, sig whatsapp.ReplyReceivedSignal) error {
	if n == nil || n.Cards == nil {
		return fmt.Errorf("replycard: notifier not wired")
	}
	now := n.Now
	if now == nil {
		now = time.Now
	}
	ts := sig.ReceivedAt
	if ts.IsZero() {
		ts = now()
	}

	name := strings.TrimSpace(sig.RecipientName)
	if name == "" {
		name = "Contact"
	}
	reply := strings.TrimSpace(sig.ReplyText)
	if reply == "" {
		reply = "(empty message)"
	}
	if len(reply) > inboundQuoteMax {
		reply = reply[:inboundQuoteMax-1] + "…"
	}

	card := store.Card{
		ID:       "reply-" + uuid.NewString(),
		Date:     ts.Format("2006-01-02"),
		Kind:     "reply_received",
		Source:   "personal",
		SrcLabel: "WhatsApp · " + name,
		Rel:      "med",
		Origin:   "reply_received",
		Title:    name + " replied",
		Sub:      composeSub(name, reply),
		Meta:     mustMarshal([]string{ts.Format("15:04")}),
		Actions: mustMarshal([]map[string]any{
			{
				"label":   "Reply",
				"primary": true,
				"intent":  "send_whatsapp",
				"target": map[string]any{
					"recipient": name,
				},
			},
			{
				"label":  "Dismiss",
				"intent": "dismiss",
			},
		}),
		CreatedAt: now(),
	}

	if err := n.Cards.Upsert(ctx, []store.Card{card}); err != nil {
		return fmt.Errorf("replycard: upsert: %w", err)
	}
	if n.Bus != nil {
		n.Bus.Publish(eventbus.CardAppendedEvent{Card: card})
	}
	if n.Log != nil {
		n.Log.WithFields(logrus.Fields{
			"chat_jid":     sig.ChatJID,
			"context_kind": sig.ContextKind,
			"context_id":   sig.ContextID,
			"card_id":      card.ID,
		}).Info("replycard: surfaced reply-received card")
	}
	return nil
}

// composeSub builds the card sub line. The recipient's reply is quoted
// verbatim (no LLM rewrite) so the user sees what was actually said.
func composeSub(name, reply string) string {
	return fmt.Sprintf("%s replied to your message: %q", name, reply)
}

func mustMarshal(v any) datatypes.JSON {
	b, err := json.Marshal(v)
	if err != nil {
		// Marshaling a fixed shape can't fail; if it ever does the
		// Card schema validator on the read side will reject the row.
		return datatypes.JSON([]byte("[]"))
	}
	return datatypes.JSON(b)
}
