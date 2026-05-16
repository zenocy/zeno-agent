package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// RealClient wraps *whatsmeow.Client behind the package's Client seam.
// One instance owns one device row in the sqlstore.Container — Logout
// truncates that row, after which the instance is dead and a new one
// must be constructed via NewRealClient before re-pairing.
type RealClient struct {
	wm        *whatsmeow.Client
	container *sqlstore.Container
	log       *logrus.Entry
}

// NewRealClient initializes a whatsmeow client backed by a SQLite store
// at dbPath. dbPath is used as a SQLite DSN suffix; foreign-keys are
// enabled because whatsmeow's schema relies on cascading deletes.
//
// On first call the store is empty and HasSession() returns false; the
// caller must run BeginPair → user scans QR before SendText etc. work.
// On subsequent boots the persisted device is loaded automatically.
func NewRealClient(ctx context.Context, dbPath string, log *logrus.Entry) (*RealClient, error) {
	dsn := fmt.Sprintf("file:%s?_foreign_keys=on", dbPath)
	dbLog := waLog.Stdout("wadb", "WARN", true)
	container, err := sqlstore.New(ctx, "sqlite3", dsn, dbLog)
	if err != nil {
		return nil, fmt.Errorf("whatsapp: open sqlstore: %w", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("whatsapp: get first device: %w", err)
	}
	clientLog := waLog.Stdout("wac", "WARN", true)
	wm := whatsmeow.NewClient(device, clientLog)
	return &RealClient{wm: wm, container: container, log: log}, nil
}

// HasSession reports whether the device store has a paired account.
func (c *RealClient) HasSession() bool {
	return c.wm != nil && c.wm.Store != nil && c.wm.Store.ID != nil
}

// OwnJID renders the paired account JID, empty when not paired.
func (c *RealClient) OwnJID() string {
	if !c.HasSession() {
		return ""
	}
	return c.wm.Store.ID.String()
}

// OwnPushName returns the paired account's display name, empty before
// the first server-acknowledged login.
func (c *RealClient) OwnPushName() string {
	if c.wm == nil || c.wm.Store == nil {
		return ""
	}
	return c.wm.Store.PushName
}

// Connect opens the socket. Caller must have invoked GetQRChannel first
// when HasSession is false.
func (c *RealClient) Connect(ctx context.Context) error {
	if c.wm == nil {
		return errors.New("whatsapp: client not initialized")
	}
	if err := c.wm.Connect(); err != nil {
		return fmt.Errorf("whatsapp: connect: %w", err)
	}
	return nil
}

// Disconnect closes the socket. Idempotent.
func (c *RealClient) Disconnect() {
	if c.wm == nil {
		return
	}
	c.wm.Disconnect()
}

// IsConnected reports whether the underlying socket is currently up.
func (c *RealClient) IsConnected() bool {
	return c.wm != nil && c.wm.IsConnected()
}

// IsLoggedIn reports whether the server has acknowledged our session.
func (c *RealClient) IsLoggedIn() bool {
	return c.wm != nil && c.wm.IsLoggedIn()
}

// GetQRChannel returns the pairing QR stream, translating whatsmeow's
// QRChannelItem into our QREvent shape.
func (c *RealClient) GetQRChannel(ctx context.Context) (<-chan QREvent, error) {
	if c.wm == nil {
		return nil, errors.New("whatsapp: client not initialized")
	}
	src, err := c.wm.GetQRChannel(ctx)
	if err != nil {
		return nil, fmt.Errorf("whatsapp: get qr channel: %w", err)
	}
	out := make(chan QREvent, 4)
	go func() {
		defer close(out)
		for item := range src {
			ev := QREvent{Event: item.Event, Code: item.Code}
			if item.Error != nil {
				ev.Err = item.Error
			}
			out <- ev
		}
	}()
	return out, nil
}

// SendText sends a plain-text message to the recipient JID. Discards
// the server-assigned ID; callers that need correlation should use
// SendTextWithID.
//
// Important: whatsmeow's SendMessage decrypts and re-encrypts per
// recipient. We deliberately do not send typing presence beforehand —
// keeping Zeno's automated activity invisible from the recipient's
// perspective is part of the privacy posture documented in
// docs/whatsapp.md.
func (c *RealClient) SendText(ctx context.Context, to string, text string) error {
	_, err := c.SendTextWithID(ctx, to, text)
	return err
}

// SendTextWithID sends and returns whatsmeow's client-assigned message
// ID. The ID is stable across the wire and is what an inbound reply's
// ContextInfo.StanzaID matches when the recipient quotes the original.
// Empty ID is possible (older whatsmeow versions, certain media types);
// callers must tolerate that.
func (c *RealClient) SendTextWithID(ctx context.Context, to string, text string) (string, error) {
	if c.wm == nil {
		return "", errors.New("whatsapp: client not initialized")
	}
	jid, err := types.ParseJID(to)
	if err != nil {
		return "", fmt.Errorf("whatsapp: parse jid %q: %w", to, err)
	}
	msg := &waE2E.Message{Conversation: stringPtr(text)}
	resp, err := c.wm.SendMessage(ctx, jid, msg)
	if err != nil {
		return "", fmt.Errorf("whatsapp: send: %w", err)
	}
	return resp.ID, nil
}

// AddEventHandler installs a translating handler that converts
// whatsmeow's interface{} events into our typed Event sum. Every
// whatsmeow event is also logged at INFO so failure modes the typed
// sum doesn't cover (PairError, ConnectFailure, TemporaryBan,
// ClientOutdated, StreamError, …) are visible during pair-flow
// debugging without enabling whatsmeow's own DEBUG log.
//
// V2.13.3: when the inbound is a Message, sender/chat JIDs are
// rewritten from `@lid` to `@s.whatsapp.net` form before downstream
// processing. WhatsApp's multi-device routing sometimes delivers
// inbound messages with the recipient's Linked ID (LID) namespace
// instead of the phone-based JID — that breaks the classifier's
// allowlist match (which stores phone-based JIDs) and the V2.13
// ExpectedReply correlation (keyed on phone-based JID from the
// outbound send). Resolution path: prefer `Info.SenderAlt` /
// `Info.RecipientAlt` when whatsmeow already attached the alt; fall
// back to `Store.LIDs.GetPNForLID` for cached mappings.
func (c *RealClient) AddEventHandler(handler func(Event)) uint32 {
	return c.wm.AddEventHandler(func(raw interface{}) {
		c.logRawEvent(raw)
		ev := translateEvent(raw)
		if ev == nil {
			return
		}
		if msgEv, ok := ev.(EventIncomingMessage); ok {
			if me, ok2 := raw.(*events.Message); ok2 {
				msgEv.Msg.SenderJID = c.resolveSenderPN(me.Info.Sender, me.Info.SenderAlt)
				msgEv.Msg.ChatJID = c.resolveSenderPN(me.Info.Chat, me.Info.RecipientAlt)
			}
			ev = msgEv
		}
		handler(ev)
	})
}

// resolveSenderPN normalizes a JID to its phone-based form
// (`@s.whatsapp.net`) when possible. LID-server JIDs are resolved via
// SenderAlt/RecipientAlt first, then via the Store's LID mapping.
// Non-LID inputs are returned unchanged. The result is always
// `.ToNonAD().String()` (no device suffix) so it's directly comparable
// to allowlist entries and ExpectedReply.ChatJID rows.
func (c *RealClient) resolveSenderPN(primary, alt types.JID) string {
	if resolved := resolveSenderPNFromAlt(primary, alt); resolved != "" {
		return resolved
	}
	// Persistent LID map fallback (only reached for LID inputs whose
	// alt didn't carry a phone-based JID).
	if c.wm != nil && c.wm.Store != nil && c.wm.Store.LIDs != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if pn, err := c.wm.Store.LIDs.GetPNForLID(ctx, primary); err == nil && !pn.IsEmpty() {
			return pn.ToNonAD().String()
		} else if err != nil && c.log != nil {
			c.log.WithError(err).
				WithField("lid", primary.String()).
				Debug("whatsapp: GetPNForLID failed; falling back to LID form")
		}
	}
	// Resolution failed — surface the LID form so the message at
	// least appears in the audit log. The classifier will drop it
	// (allowlist mismatch), which is the safe default.
	return primary.ToNonAD().String()
}

// resolveSenderPNFromAlt is the pure-function half of resolveSenderPN:
// returns the phone-based JID string when either (a) primary is
// already non-LID, or (b) the alt carries a phone-based JID. Empty
// string means the caller must fall back to the Store mapping.
//
// Split out so the alt-preference logic is unit-testable without a
// live whatsmeow.Container.
func resolveSenderPNFromAlt(primary, alt types.JID) string {
	if primary.IsEmpty() {
		return ""
	}
	if primary.Server != types.HiddenUserServer {
		return primary.ToNonAD().String()
	}
	if !alt.IsEmpty() && alt.Server == types.DefaultUserServer {
		return alt.ToNonAD().String()
	}
	return ""
}

// logRawEvent emits one INFO line per whatsmeow event with the
// failure-mode-relevant fields extracted. Falls back to the type name
// when we don't have a richer formatter for the event.
func (c *RealClient) logRawEvent(raw interface{}) {
	if c.log == nil {
		return
	}
	fields := logrus.Fields{"wm_event": fmt.Sprintf("%T", raw)}
	switch e := raw.(type) {
	case *events.PairError:
		fields["jid"] = e.ID.String()
		fields["platform"] = e.Platform
		if e.Error != nil {
			fields["err"] = e.Error.Error()
		}
	case *events.PairSuccess:
		fields["jid"] = e.ID.String()
		fields["platform"] = e.Platform
	case *events.ConnectFailure:
		fields["reason"] = e.Reason.String()
		fields["message"] = e.Message
	case *events.LoggedOut:
		fields["reason"] = e.Reason.String()
		fields["on_connect"] = e.OnConnect
	case *events.TemporaryBan:
		fields["code"] = int(e.Code)
		fields["expire"] = e.Expire.String()
	case *events.StreamError:
		fields["code"] = e.Code
	case *events.ClientOutdated, *events.StreamReplaced, *events.QRScannedWithoutMultidevice:
		// Type name alone is the diagnostic.
	case *events.Connected, *events.Disconnected, *events.Message,
		*events.KeepAliveTimeout, *events.KeepAliveRestored:
		// Routine lifecycle events; type name suffices.
	default:
		// Unknown event types: type name is still useful.
	}
	c.log.WithFields(fields).Info("whatsapp: wm event")
}

// RemoveEventHandler delegates to whatsmeow.
func (c *RealClient) RemoveEventHandler(id uint32) bool {
	if c.wm == nil {
		return false
	}
	return c.wm.RemoveEventHandler(id)
}

// Logout invalidates the server-side session, deletes the local device
// row, and disconnects. After Logout the RealClient is dead — the
// Service must construct a fresh one via NewRealClient to re-pair.
func (c *RealClient) Logout(ctx context.Context) error {
	if c.wm == nil {
		return nil
	}
	if c.wm.IsLoggedIn() {
		if err := c.wm.Logout(ctx); err != nil {
			// whatsmeow returns ErrNotLoggedIn here for already-clean
			// states; treat all other failures as a warning and still
			// proceed to local cleanup so the user can re-pair.
			if c.log != nil {
				c.log.WithError(err).Warn("whatsapp: server-side logout failed; continuing with local cleanup")
			}
		}
	}
	c.wm.Disconnect()
	if c.wm.Store != nil && c.wm.Store.ID != nil {
		if err := c.wm.Store.Delete(ctx); err != nil {
			return fmt.Errorf("whatsapp: delete device: %w", err)
		}
	}
	return nil
}

// translateEvent converts a whatsmeow interface{} event into our typed
// sum. Unknown events return nil and are dropped — the Service does not
// need them.
func translateEvent(raw interface{}) Event {
	switch e := raw.(type) {
	case *events.Message:
		return EventIncomingMessage{Msg: incomingFromWhatsmeow(e)}
	case *events.Connected:
		return EventConnected{}
	case *events.Disconnected:
		return EventDisconnected{}
	case *events.LoggedOut:
		return EventLoggedOut{Reason: loggedOutReason(e)}
	case *events.PairSuccess:
		return EventPairSuccess{JID: e.ID.String()}
	default:
		return nil
	}
}

// incomingFromWhatsmeow lifts an *events.Message into our DTO.
func incomingFromWhatsmeow(e *events.Message) IncomingMessage {
	out := IncomingMessage{
		MessageID:  e.Info.ID,
		Timestamp:  e.Info.Timestamp,
		SenderJID:  e.Info.Sender.ToNonAD().String(),
		SenderName: e.Info.PushName,
		ChatJID:    e.Info.Chat.ToNonAD().String(),
		IsFromMe:   e.Info.IsFromMe,
		IsGroup:    e.Info.IsGroup,
	}
	if e.Info.IsGroup {
		// Push the group display name through if the message carries
		// it; the proper resolver lives on the Client itself but is
		// async, so we accept "" when not inline.
		out.GroupName = ""
	}
	if e.Message == nil {
		out.Type = MessageTypeOther
		return out
	}
	switch {
	case e.Message.GetConversation() != "":
		out.Type = MessageTypeText
		out.Text = e.Message.GetConversation()
	case e.Message.GetExtendedTextMessage() != nil:
		ext := e.Message.GetExtendedTextMessage()
		out.Type = MessageTypeText
		out.Text = ext.GetText()
		if ctx := ext.GetContextInfo(); ctx != nil {
			out.Mentions = append(out.Mentions, ctx.GetMentionedJID()...)
		}
	case e.Message.GetImageMessage() != nil:
		out.Type = MessageTypeImage
	case e.Message.GetAudioMessage() != nil:
		out.Type = MessageTypeVoice
	case e.Message.GetVideoMessage() != nil:
		out.Type = MessageTypeVideo
	default:
		out.Type = MessageTypeOther
	}
	return out
}

// loggedOutReason maps whatsmeow's reason enum to a human-readable
// string for logging and status display.
func loggedOutReason(e *events.LoggedOut) string {
	if e == nil {
		return "unknown"
	}
	r := strings.TrimPrefix(e.Reason.String(), "ConnectFailureReason_")
	if r == "" {
		return "unknown"
	}
	return r
}

func stringPtr(s string) *string { return &s }

// _ = time import to keep the time package referenced if future
// adapters need it; cheap insurance against import drift.
var _ = time.Now

// Compile-time guard: RealClient must satisfy the Client seam.
var _ Client = (*RealClient)(nil)

// _ = store import keeps the device.Store reference compiling against
// any whatsmeow upgrade that re-exports types.
var _ = store.Device{}
