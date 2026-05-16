// Package whatsapp implements the bidirectional WhatsApp integration
// (V2.7). It is NOT a Sensor: WhatsApp is a persistent push socket, not
// a poll target, so it bypasses the cron scheduler and runs as a
// long-lived Service started directly from cmd/zeno/main.go.
//
// The package is structured around a Client seam (interface in
// client.go) so the whatsmeow library never leaks past this directory.
// The real adapter (client_real.go) wraps *whatsmeow.Client; the test
// double (whatsapptest/fake.go) drives Service tests without a network.
//
// Privacy boundary: messages are classified by classify.go BEFORE
// anything is appended to the observation log. Group messages that do
// not @-mention Zeno and DMs from non-allowlisted senders are dropped
// at the receive boundary — they never reach the log, the LLM, or
// memory candidates. This is an explicit design constraint and is
// asserted by tests.
package whatsapp

import "time"

// IncomingMessage is the package-internal DTO representing a message
// received from WhatsApp. It is decoupled from whatsmeow's events.Message
// so tests don't need to construct whatsmeow types and so the rest of
// the codebase doesn't transitively import the library.
type IncomingMessage struct {
	MessageID  string
	Timestamp  time.Time
	SenderJID  string // e.g. "12345@s.whatsapp.net"
	SenderName string // pushName from WhatsApp; may be empty
	ChatJID    string // for DMs == SenderJID; for groups, the group JID
	GroupName  string // empty for DMs
	IsFromMe   bool
	IsGroup    bool
	Type       MessageType
	Text       string   // empty when Type != MessageTypeText
	Mentions   []string // JIDs explicitly mentioned in the message
}

// MessageType is the coarse classification of an inbound payload. We
// only act on text; everything else is acknowledged with a fixed
// "I can't see images yet"-style reply so the user knows the message
// arrived but Zeno can't act on it.
type MessageType string

const (
	MessageTypeText  MessageType = "text"
	MessageTypeImage MessageType = "image"
	MessageTypeVoice MessageType = "voice"
	MessageTypeVideo MessageType = "video"
	MessageTypeOther MessageType = "other"
)

// Event is the sealed sum type the Client seam emits. Real and fake
// clients both translate their underlying events into one of these
// concrete variants before invoking registered handlers.
type Event interface {
	isWhatsAppEvent()
}

// EventIncomingMessage carries one inbound chat message.
type EventIncomingMessage struct {
	Msg IncomingMessage
}

func (EventIncomingMessage) isWhatsAppEvent() {}

// EventConnected is emitted when the socket is fully established AND a
// session exists (i.e. we are logged in to a paired account).
type EventConnected struct{}

func (EventConnected) isWhatsAppEvent() {}

// EventDisconnected fires when the socket drops. Err may be nil for
// clean shutdowns. whatsmeow auto-reconnects; this event is purely for
// status surfacing.
type EventDisconnected struct {
	Err error
}

func (EventDisconnected) isWhatsAppEvent() {}

// EventLoggedOut is emitted when WhatsApp explicitly invalidates our
// session — typically because the user removed the linked device from
// their phone, or WhatsApp rotated the account. The session must be
// re-paired.
type EventLoggedOut struct {
	Reason string
}

func (EventLoggedOut) isWhatsAppEvent() {}

// EventPairSuccess fires once when a fresh QR pair completes. The Service
// uses it to flip status and stop the QR refresh loop.
type EventPairSuccess struct {
	JID string
}

func (EventPairSuccess) isWhatsAppEvent() {}

// QREvent is one frame from a pairing QR channel.
type QREvent struct {
	// Event is the underlying state: "code" (Code is set), "success"
	// (Code is empty), "timeout", "error", or any vendor-specific value
	// whatsmeow forwards. Callers should only render UI for "code" and
	// transition state on "success" / "timeout" / "error".
	Event string
	// Code is the QR string to render (for the "code" event only).
	Code string
	// Err is populated when Event == "error".
	Err error
}

// SentMessage is a record of one message Zeno emitted; the test fake
// captures these for assertions and the real adapter does not.
type SentMessage struct {
	To        string
	Text      string
	Timestamp time.Time
}
