package whatsapp

import "context"

// Client is the seam between the whatsapp.Service and whatsmeow. The
// real implementation (client_real.go) wraps *whatsmeow.Client; tests
// inject a fake (whatsapptest.FakeClient).
//
// Method semantics intentionally mirror whatsmeow so the real adapter
// stays a thin translation layer:
//
//   - Connect must be safe to call exactly once per Client lifecycle.
//   - GetQRChannel must be called BEFORE Connect on a fresh (unpaired)
//     device, otherwise whatsmeow returns ErrAlreadyConnected.
//   - The handler registered with AddEventHandler may be invoked from
//     any goroutine; handlers must be cheap and non-blocking.
//   - Logout is idempotent; calling it on an already-logged-out client
//     returns nil.
type Client interface {
	// HasSession reports whether a paired device exists. False on a
	// fresh container; true after a successful QR pair or a resumed
	// session.
	HasSession() bool

	// OwnJID returns the paired account's JID in canonical form. Empty
	// when HasSession is false.
	OwnJID() string

	// OwnPushName is the WhatsApp display name of the paired account.
	// Populated after the first successful login; empty before.
	OwnPushName() string

	// Connect opens the socket. Returns immediately on success; the
	// connection lifecycle continues asynchronously. EventConnected is
	// emitted once the socket is fully ready.
	Connect(ctx context.Context) error

	// Disconnect closes the socket cleanly. Idempotent.
	Disconnect()

	// IsConnected reports the current socket state.
	IsConnected() bool

	// IsLoggedIn reports whether the server has acknowledged our session.
	IsLoggedIn() bool

	// GetQRChannel returns the pairing QR stream. Must be called BEFORE
	// Connect on an unpaired Client. Errors when called on an already-
	// connected or already-paired Client.
	GetQRChannel(ctx context.Context) (<-chan QREvent, error)

	// SendText sends a plain-text message to the given JID. The Client
	// is responsible for typing-receipt suppression and for surfacing
	// any wrapped errors.
	SendText(ctx context.Context, to string, text string) error

	// SendTextWithID sends a plain-text message and returns the
	// server-assigned message ID. V2.13.0 added this for assistant-mode
	// reply correlation: the action executor records the ID alongside
	// the ExpectedReply row so an inbound reply can be tied back to a
	// specific outbound. Implementations may return an empty ID when
	// the wire layer doesn't expose one — callers fall back to
	// (chat_jid, sent_at) correlation.
	SendTextWithID(ctx context.Context, to string, text string) (msgID string, err error)

	// AddEventHandler registers a callback for Events; returns an opaque
	// handler ID for removal.
	AddEventHandler(handler func(Event)) uint32

	// RemoveEventHandler unregisters a previously-added handler.
	RemoveEventHandler(id uint32) bool

	// Logout invalidates the session on the server, clears the local
	// device store, and disconnects.
	Logout(ctx context.Context) error
}

// Compile-time check the FakeClient (in whatsapptest) and RealClient
// satisfy the seam. The whatsapptest package adds a similar assertion
// for FakeClient; this file is the canonical definition.
