// Package whatsapptest provides an in-memory FakeClient that satisfies
// the whatsapp.Client seam. It exists so tests for the Service,
// classifier, and dispatcher can drive incoming messages and assert on
// outgoing sends without ever opening a network socket or a SQLite
// store.
//
// FakeClient is goroutine-safe: tests typically call Inject* from the
// test goroutine and SentMessages from an assertion helper running on
// the same goroutine, but concurrent driver routines work too.
package whatsapptest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zenocy/zeno-v2/internal/whatsapp"
)

// FakeClient is the test-mode implementation of whatsapp.Client.
type FakeClient struct {
	mu sync.Mutex

	hasSession  bool
	ownJID      string
	ownPushName string
	connected   bool
	loggedIn    bool

	handlerMu  sync.Mutex
	nextID     uint32
	handlers   map[uint32]func(whatsapp.Event)
	qrChannels []chan whatsapp.QREvent

	sent     []whatsapp.SentMessage
	sendErr  error
	sendHook func(to, text string) error

	// sendMsgIDSeq feeds SendTextWithID deterministic IDs (FAKEMSG-1,
	// FAKEMSG-2, ...). Tests asserting on the returned ID must read it
	// directly rather than predicting a value.
	sendMsgIDSeq uint64

	now func() time.Time
}

// New returns a fresh FakeClient with no session.
func New() *FakeClient {
	return &FakeClient{
		handlers: map[uint32]func(whatsapp.Event){},
		now:      time.Now,
	}
}

// SetClock replaces the timestamp source on SentMessage records. Useful
// for tests that need deterministic ordering.
func (f *FakeClient) SetClock(now func() time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = now
}

// SetSession marks the fake as paired with the given JID + push name.
// Tests use this to bypass the QR flow when they only care about
// receive/send behavior.
func (f *FakeClient) SetSession(jid, pushName string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hasSession = true
	f.ownJID = jid
	f.ownPushName = pushName
}

// SetConnected toggles the IsConnected flag.
func (f *FakeClient) SetConnected(b bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connected = b
}

// SetLoggedIn toggles the IsLoggedIn flag.
func (f *FakeClient) SetLoggedIn(b bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loggedIn = b
}

// SetSendError makes the next N SendText calls fail with err. After the
// counter is exhausted, sends succeed again.
func (f *FakeClient) SetSendError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendErr = err
}

// SetSendHook replaces the default send behavior. Useful when a test
// wants per-call branching (succeed twice, fail once, succeed).
func (f *FakeClient) SetSendHook(hook func(to, text string) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendHook = hook
}

// Inject sends an Event to every registered handler.
//
// Inject also updates the FakeClient's internal session/connection
// flags to match what the real adapter would do on the same event:
// EventPairSuccess sets the session, EventLoggedOut clears it,
// EventConnected/EventDisconnected toggle the socket flag. This keeps
// tests honest — Status() pulls live flags from the Client, so a fake
// that doesn't mirror these transitions would diverge from production.
func (f *FakeClient) Inject(ev whatsapp.Event) {
	switch e := ev.(type) {
	case whatsapp.EventPairSuccess:
		f.mu.Lock()
		f.hasSession = true
		f.ownJID = e.JID
		f.loggedIn = true
		f.mu.Unlock()
	case whatsapp.EventLoggedOut:
		f.mu.Lock()
		f.hasSession = false
		f.ownJID = ""
		f.ownPushName = ""
		f.loggedIn = false
		f.connected = false
		f.mu.Unlock()
	case whatsapp.EventConnected:
		f.mu.Lock()
		f.connected = true
		if f.hasSession {
			f.loggedIn = true
		}
		f.mu.Unlock()
	case whatsapp.EventDisconnected:
		f.mu.Lock()
		f.connected = false
		f.mu.Unlock()
	}

	f.handlerMu.Lock()
	hs := make([]func(whatsapp.Event), 0, len(f.handlers))
	for _, h := range f.handlers {
		hs = append(hs, h)
	}
	f.handlerMu.Unlock()
	for _, h := range hs {
		h(ev)
	}
}

// InjectMessage is a convenience for emitting an inbound text/group
// message to handlers.
func (f *FakeClient) InjectMessage(msg whatsapp.IncomingMessage) {
	f.Inject(whatsapp.EventIncomingMessage{Msg: msg})
}

// InjectQR pushes a QR frame onto every active GetQRChannel stream.
func (f *FakeClient) InjectQR(ev whatsapp.QREvent) {
	f.handlerMu.Lock()
	chs := append([]chan whatsapp.QREvent(nil), f.qrChannels...)
	f.handlerMu.Unlock()
	for _, ch := range chs {
		ch <- ev
	}
}

// CloseQR closes all QR streams.
func (f *FakeClient) CloseQR() {
	f.handlerMu.Lock()
	chs := f.qrChannels
	f.qrChannels = nil
	f.handlerMu.Unlock()
	for _, ch := range chs {
		close(ch)
	}
}

// SentMessages returns a snapshot of every SendText call made so far.
func (f *FakeClient) SentMessages() []whatsapp.SentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]whatsapp.SentMessage, len(f.sent))
	copy(out, f.sent)
	return out
}

// ResetSent clears the captured outgoing log.
func (f *FakeClient) ResetSent() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = nil
}

// --- Client interface ---

// HasSession reports whether SetSession has been called.
func (f *FakeClient) HasSession() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hasSession
}

// OwnJID returns the JID configured via SetSession.
func (f *FakeClient) OwnJID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ownJID
}

// OwnPushName returns the push name configured via SetSession.
func (f *FakeClient) OwnPushName() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ownPushName
}

// Connect flips connected=true and is a no-op otherwise.
func (f *FakeClient) Connect(ctx context.Context) error {
	f.mu.Lock()
	f.connected = true
	hadSession := f.hasSession
	f.mu.Unlock()
	if hadSession {
		// Resumed sessions are logged in immediately; fresh pairs
		// flip loggedIn after InjectQR(success).
		f.mu.Lock()
		f.loggedIn = true
		f.mu.Unlock()
		f.Inject(whatsapp.EventConnected{})
	}
	return nil
}

// Disconnect flips connected=false.
func (f *FakeClient) Disconnect() {
	f.mu.Lock()
	f.connected = false
	f.mu.Unlock()
}

// IsConnected reports the current flag.
func (f *FakeClient) IsConnected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}

// IsLoggedIn reports the current flag.
func (f *FakeClient) IsLoggedIn() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loggedIn
}

// GetQRChannel registers a fresh QR stream that tests drive via InjectQR.
func (f *FakeClient) GetQRChannel(ctx context.Context) (<-chan whatsapp.QREvent, error) {
	f.handlerMu.Lock()
	defer f.handlerMu.Unlock()
	if f.hasSession {
		return nil, errors.New("fake: already paired")
	}
	ch := make(chan whatsapp.QREvent, 4)
	f.qrChannels = append(f.qrChannels, ch)
	return ch, nil
}

// SendText records the message and applies any installed hook/error.
// Discards the deterministic ID; tests that need it should call
// SendTextWithID instead.
func (f *FakeClient) SendText(ctx context.Context, to string, text string) error {
	_, err := f.SendTextWithID(ctx, to, text)
	return err
}

// SendTextWithID records the message and returns a deterministic ID
// (FAKEMSG-1, FAKEMSG-2, ...). Hook/error injection works the same as
// SendText. Returns "" when the hook or sendErr aborts the send.
func (f *FakeClient) SendTextWithID(ctx context.Context, to string, text string) (string, error) {
	f.mu.Lock()
	hook := f.sendHook
	err := f.sendErr
	now := f.now
	f.mu.Unlock()
	if hook != nil {
		if hookErr := hook(to, text); hookErr != nil {
			return "", hookErr
		}
	} else if err != nil {
		return "", err
	}
	f.mu.Lock()
	f.sendMsgIDSeq++
	msgID := fmt.Sprintf("FAKEMSG-%d", f.sendMsgIDSeq)
	f.sent = append(f.sent, whatsapp.SentMessage{
		To:        to,
		Text:      text,
		Timestamp: now(),
	})
	f.mu.Unlock()
	return msgID, nil
}

// AddEventHandler returns a monotonic id; tests typically don't bother
// removing handlers, but RemoveEventHandler is honored.
func (f *FakeClient) AddEventHandler(handler func(whatsapp.Event)) uint32 {
	f.handlerMu.Lock()
	defer f.handlerMu.Unlock()
	id := atomic.AddUint32(&f.nextID, 1)
	f.handlers[id] = handler
	return id
}

// RemoveEventHandler removes the handler with the given id.
func (f *FakeClient) RemoveEventHandler(id uint32) bool {
	f.handlerMu.Lock()
	defer f.handlerMu.Unlock()
	if _, ok := f.handlers[id]; !ok {
		return false
	}
	delete(f.handlers, id)
	return true
}

// Logout clears session state and disconnects.
func (f *FakeClient) Logout(ctx context.Context) error {
	f.mu.Lock()
	f.hasSession = false
	f.ownJID = ""
	f.ownPushName = ""
	f.connected = false
	f.loggedIn = false
	f.mu.Unlock()
	return nil
}

// Compile-time guard: FakeClient must satisfy the seam.
var _ whatsapp.Client = (*FakeClient)(nil)
