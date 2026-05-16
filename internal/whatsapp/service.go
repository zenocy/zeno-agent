package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/eventbus"
)

// ServiceConfig holds the boot-time wiring for the WhatsApp service.
// Per-conversation tunables (allowlist, mention name, throttle) live in
// the whatsapp_config SQLite table — see internal/store/whatsapp_config.go.
type ServiceConfig struct {
	// Enabled mirrors cfg.Sensors.WhatsApp.Enabled and is captured here
	// so the Service can refuse Start when the operator turned it off.
	Enabled bool
	// DBPath is the SQLite file the whatsmeow sqlstore.Container owns.
	// Tests pass "" because the FakeClient does not open a store.
	DBPath string
}

// ClientFactory builds a fresh Client instance. The Service calls it
// once at Start and again after every Unlink so post-logout state is
// truly fresh (whatsmeow's *Client cannot be re-paired after Logout).
type ClientFactory func(ctx context.Context) (Client, error)

// RealClientFactory returns a ClientFactory that opens
// whatsmeow's sqlstore at dbPath. It is the production wiring; tests
// supply their own factory returning a *whatsapptest.FakeClient.
func RealClientFactory(dbPath string, log *logrus.Entry) ClientFactory {
	return func(ctx context.Context) (Client, error) {
		return NewRealClient(ctx, dbPath, log)
	}
}

// Status is the snapshot rendered by /api/whatsapp/status. It is a
// value type so callers can compare two snapshots cheaply.
type Status struct {
	Enabled     bool      `json:"enabled"`
	HasSession  bool      `json:"has_session"`
	Connected   bool      `json:"connected"`
	LoggedIn    bool      `json:"logged_in"`
	OwnJID      string    `json:"own_jid,omitempty"`
	OwnPushName string    `json:"own_push_name,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	LastSeenAt  time.Time `json:"last_seen_at,omitempty"`
	PairedAt    time.Time `json:"paired_at,omitempty"`
}

// Service is the long-lived owner of the WhatsApp socket. It bridges
// the Client seam to the rest of Zeno: lifecycle in this file,
// classification + dispatch in classify.go / dispatcher.go (Phase 2),
// synth integration in service_send.go (Phase 3).
//
// One Service per process. Concurrency:
//   - mu guards the client pointer + Status.
//   - All public methods are safe for concurrent callers; the event
//     handler runs on whatsmeow's goroutine and only invokes
//     onEvent which acquires mu briefly.
type Service struct {
	cfg     ServiceConfig
	factory ClientFactory
	log     *logrus.Entry
	now     func() time.Time

	mu        sync.RWMutex
	client    Client
	handlerID uint32
	status    Status

	// pairMu serializes BeginPair entry. A new BeginPair pre-empts any
	// in-flight pair via pairCancel and waits for the prior goroutine to
	// finish (signaled via pairDone) before claiming the slot. This is
	// the only correct shape under React StrictMode dev double-effect:
	// CAS-based rejection 409s the second mount because Go's HTTP server
	// detects client connection-close hundreds of milliseconds after the
	// browser aborts, leaving pairState held longer than any reasonable
	// UI retry budget.
	pairMu     sync.Mutex
	pairCancel context.CancelFunc
	pairDone   chan struct{}

	rtCfg      RuntimeConfig
	dispatcher *Dispatcher

	// bus is optional; when set, every Status transition broadcasts a
	// WhatsAppStatusEvent so the UI status hook can replace the 5s poll.
	// SetBus wires it post-construction so NewService stays a leaf-level
	// constructor (callers that don't care about SSE pass nothing).
	bus *eventbus.Bus

	// V2.13.3: optional callback that reports whether there's an open
	// assistant-mode ExpectedReply for a given chat JID. When set, the
	// classifier auto-eligibles inbound DMs from JIDs we're waiting on
	// — same 24h window as the V2.13 correlation table. Nil leaves the
	// allowlist behavior byte-equal to V2.7. Function-typed (not
	// interface) so the whatsapp package stays free of an internal/store
	// import; main.go wires the closure to ExpectedReplyRepo.OpenForJID.
	openReplyChecker OpenReplyChecker
}

// OpenReplyChecker reports whether chatJID has an open assistant-mode
// ExpectedReply (V2.13). Implementations should be cheap — invoked
// once per inbound DM on the receive path.
type OpenReplyChecker func(ctx context.Context, chatJID string, now time.Time) bool

// SetBus wires the event bus used to broadcast status transitions. Safe
// to call at any time; nil clears it.
func (s *Service) SetBus(bus *eventbus.Bus) {
	s.mu.Lock()
	s.bus = bus
	s.mu.Unlock()
}

// SetOpenReplyChecker wires the V2.13.3 auto-eligibility hook. When
// non-nil, the classifier admits inbound DMs from JIDs that have an
// open ExpectedReply, even if they aren't in the static allowlist.
// The check fires per inbound DM; keep the closure cheap.
func (s *Service) SetOpenReplyChecker(c OpenReplyChecker) {
	s.mu.Lock()
	s.openReplyChecker = c
	s.mu.Unlock()
}

// publishStatus snapshots the current Status + RuntimeConfig and
// publishes a WhatsAppStatusEvent on the bus (if configured). Caller
// must NOT hold mu — Status() / RuntimeConfig() take a read lock; bus
// Publish runs outside the service's locks so a slow subscriber can't
// stall whatsmeow's goroutine.
func (s *Service) publishStatus() {
	s.mu.RLock()
	bus := s.bus
	s.mu.RUnlock()
	if bus == nil {
		return
	}
	st := s.Status()
	rt := s.RuntimeConfig()
	bus.Publish(eventbus.WhatsAppStatusEvent{
		Enabled:     st.Enabled,
		HasSession:  st.HasSession,
		Connected:   st.Connected,
		LoggedIn:    st.LoggedIn,
		OwnJID:      st.OwnJID,
		OwnPushName: st.OwnPushName,
		LastError:   st.LastError,
		LastSeenAt:  st.LastSeenAt,
		PairedAt:    st.PairedAt,
		Config: eventbus.WhatsAppConfigPayload{
			MentionName:        rt.MentionName,
			AllowedDMs:         rt.AllowedDMs,
			MinChatIntervalMs:  int(rt.MinChatInterval / time.Millisecond),
			MaxConcurrentSynth: rt.MaxConcurrentSynth,
			PerChatBuffer:      rt.PerChatBuffer,
		},
	})
}

// NewService constructs a stopped Service. Start must be called before
// any other lifecycle method has effect.
func NewService(cfg ServiceConfig, factory ClientFactory, log *logrus.Entry) *Service {
	if log == nil {
		log = logrus.NewEntry(logrus.New())
	}
	rt := DefaultRuntimeConfig()
	s := &Service{
		cfg:     cfg,
		factory: factory,
		log:     log,
		now:     time.Now,
		status:  Status{Enabled: cfg.Enabled},
		rtCfg:   rt,
	}
	// The dispatcher is constructed eagerly so SetRuntimeConfig and
	// SetMessageHandler called BEFORE Start still take effect. It does
	// nothing until Start spins up the worker context.
	s.dispatcher = NewDispatcher(rt, nil, log.WithField("c", "whatsapp-dispatch"))
	return s
}

// SetRuntimeConfig hot-reloads the operator-tunable knobs (allowlist,
// mention name, throttle). Safe to call at any time.
func (s *Service) SetRuntimeConfig(cfg RuntimeConfig) {
	cfg = cfg.Normalize()
	s.mu.Lock()
	s.rtCfg = cfg
	d := s.dispatcher
	s.mu.Unlock()
	if d != nil {
		d.SetConfig(cfg)
	}
	s.publishStatus()
}

// RuntimeConfig returns the current operator-tunable settings.
func (s *Service) RuntimeConfig() RuntimeConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rtCfg
}

// SetMessageHandler installs the callback the dispatcher invokes for
// each Process decision. Phase 3 wires the synth-driven handler.
func (s *Service) SetMessageHandler(h MessageHandler) {
	s.mu.RLock()
	d := s.dispatcher
	s.mu.RUnlock()
	if d != nil {
		d.SetHandler(h)
	}
}

// SetClock replaces the time source. Tests use it for deterministic
// timestamps on Status.PairedAt and SentMessage records.
func (s *Service) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// Start initializes the underlying Client. When a paired session
// exists on disk, Start additionally calls Connect so the socket comes
// up immediately. Otherwise the Service stays idle until BeginPair.
//
// Calling Start when the integration is disabled is a soft-no-op: the
// Service holds no client and reports Enabled:false in Status.
func (s *Service) Start(ctx context.Context) error {
	if !s.cfg.Enabled {
		s.log.Info("whatsapp: disabled in config; service idle")
		return nil
	}
	s.mu.Lock()
	if s.client != nil {
		s.mu.Unlock()
		return errors.New("whatsapp: service already started")
	}
	d := s.dispatcher
	s.mu.Unlock()
	if d != nil {
		d.Start(ctx)
	}
	if err := s.initClient(ctx); err != nil {
		return err
	}

	s.mu.RLock()
	hasSession := s.client.HasSession()
	s.mu.RUnlock()
	if hasSession {
		if err := s.connectAndRecord(ctx); err != nil {
			s.log.WithError(err).Warn("whatsapp: initial connect failed; pair via Settings to recover")
			return nil // soft-fail: status surfaces the error, daemon stays up
		}
	} else {
		s.log.Info("whatsapp: no paired session; awaiting BeginPair via Settings")
	}
	return nil
}

// Stop disconnects the client. Idempotent.
func (s *Service) Stop(ctx context.Context) error {
	s.mu.Lock()
	d := s.dispatcher
	if s.client == nil {
		s.mu.Unlock()
		if d != nil {
			d.Stop()
		}
		return nil
	}
	if s.handlerID != 0 {
		s.client.RemoveEventHandler(s.handlerID)
		s.handlerID = 0
	}
	s.client.Disconnect()
	s.status.Connected = false
	s.status.LoggedIn = false
	s.mu.Unlock()
	if d != nil {
		d.Stop()
	}
	s.publishStatus()
	return nil
}

// Status returns a snapshot of the current service state.
func (s *Service) Status() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := s.status
	if s.client != nil {
		st.HasSession = s.client.HasSession()
		st.OwnJID = s.client.OwnJID()
		st.OwnPushName = s.client.OwnPushName()
		// Trust the client's live flags over our cached lifecycle bits;
		// auto-reconnect can flip Connected without firing an Event we
		// translate (translateEvent ignores some whatsmeow internals).
		st.Connected = s.client.IsConnected()
		st.LoggedIn = s.client.IsLoggedIn()
	}
	return st
}

// Client returns the underlying Client for the dispatcher (Phase 2)
// and outgoing-send pipeline (Phase 3). Returns nil when Start has not
// been called or the integration is disabled.
func (s *Service) Client() Client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.client
}

// BeginPair drives a fresh QR pairing. The returned channel emits
// QREvents until the pair completes (Event="success"), times out, or
// the underlying Client's QR channel closes. It is an error to call
// BeginPair when a session already exists; use Unlink first.
//
// The returned channel is closed by the Service when the pair flow
// terminates; callers must drain it.
func (s *Service) BeginPair(ctx context.Context) (<-chan QREvent, error) {
	if !s.cfg.Enabled {
		return nil, errors.New("whatsapp: integration disabled in config")
	}

	// Pre-empt any in-flight pair. The new caller (e.g. React's second
	// StrictMode mount, a user retry, or another browser tab) wins; the
	// prior's pair goroutine is told to stop via its pairCtx and we
	// wait for its defer chain to release the slot before claiming it.
	s.preemptPair()

	pairCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.pairMu.Lock()
	s.pairCancel = cancel
	s.pairDone = done
	s.pairMu.Unlock()

	// If we don't reach the goroutine launch (setup error), clean up
	// the slot we just claimed so the NEXT BeginPair doesn't think
	// we're still alive.
	started := false
	defer func() {
		if started {
			return
		}
		cancel()
		s.pairMu.Lock()
		if s.pairDone == done {
			s.pairCancel = nil
			s.pairDone = nil
		}
		s.pairMu.Unlock()
		close(done)
	}()

	s.mu.Lock()
	if s.client == nil {
		s.mu.Unlock()
		if err := s.initClient(pairCtx); err != nil {
			return nil, err
		}
		s.mu.Lock()
	}
	if s.client.HasSession() {
		s.mu.Unlock()
		return nil, errors.New("whatsapp: already paired; unlink first to re-pair")
	}
	client := s.client
	s.mu.Unlock()

	// A prior pair attempt that timed out or errored leaves the
	// underlying whatsmeow client connected with no session. whatsmeow
	// refuses GetQRChannel in that state ("must be called before
	// connecting"), so drop the stranded socket before re-entering.
	if client.IsConnected() {
		client.Disconnect()
		s.mu.Lock()
		s.status.Connected = false
		s.mu.Unlock()
	}

	src, err := client.GetQRChannel(pairCtx)
	if err != nil {
		return nil, fmt.Errorf("whatsapp: get qr channel: %w", err)
	}
	if err := s.connectAndRecord(pairCtx); err != nil {
		return nil, fmt.Errorf("whatsapp: connect for pair: %w", err)
	}

	s.log.Info("whatsapp: pair flow started; awaiting QR codes from server")
	pairStart := s.now()
	out := make(chan QREvent, 4)
	go func() {
		// Slot release runs LAST (registered first). Order guarantees
		// the next BeginPair waiting on `done` only proceeds after our
		// Disconnect defer has run, so the underlying *Client is in a
		// clean state when the next call's GetQRChannel fires.
		defer func() {
			s.pairMu.Lock()
			if s.pairDone == done {
				s.pairCancel = nil
				s.pairDone = nil
			}
			s.pairMu.Unlock()
			cancel()
			close(done)
		}()
		defer close(out)
		paired := false
		defer func() {
			if paired {
				return
			}
			// Pair did not complete: drop the socket so the next
			// BeginPair can call GetQRChannel on a fresh client.
			client.Disconnect()
			s.mu.Lock()
			s.status.Connected = false
			s.mu.Unlock()
		}()
		drain := false
		for !drain {
			select {
			case ev, ok := <-src:
				if !ok {
					drain = true
					continue
				}
				fields := logrus.Fields{"event": ev.Event}
				if ev.Code != "" {
					fields["code_len"] = len(ev.Code)
				}
				if ev.Err != nil {
					fields["err"] = ev.Err.Error()
				}
				s.log.WithFields(fields).Info("whatsapp: pair event")
				select {
				case out <- ev:
				case <-pairCtx.Done():
					s.log.WithError(pairCtx.Err()).Info("whatsapp: pair flow cancelled (mid-send)")
					return
				}
				if ev.Event == "success" {
					paired = true
					return
				}
				if ev.Event == "timeout" || ev.Event == "error" {
					return
				}
			case <-pairCtx.Done():
				s.log.WithError(pairCtx.Err()).Info("whatsapp: pair flow cancelled")
				return
			}
		}
		// Channel closed without a terminal event. whatsmeow's qrchan
		// has two such paths (output buffer full, or qrc.ctx.Done) plus
		// any silent socket EOF that doesn't propagate as Disconnected.
		// Capture the smoking-gun fields so the operator can tell which:
		// connected==false && ctx_err==<nil> usually means the WhatsApp
		// server EOF'd us silently after the QR scan.
		s.log.WithFields(logrus.Fields{
			"elapsed_ms": s.now().Sub(pairStart).Milliseconds(),
			"ctx_err":    fmt.Sprint(pairCtx.Err()),
			"connected":  client.IsConnected(),
			"logged_in":  client.IsLoggedIn(),
		}).Warn("whatsapp: pair flow ended without terminal event")
	}()
	started = true
	return out, nil
}

// preemptPair signals any in-flight pair to stop and waits for its
// goroutine to finish releasing the slot. Caller must NOT hold pairMu.
// No-op when no pair is running.
func (s *Service) preemptPair() {
	s.pairMu.Lock()
	cancel := s.pairCancel
	done := s.pairDone
	s.pairMu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

// Unlink invalidates the server-side session, deletes the local device
// row, and re-initializes a fresh Client so the next BeginPair starts
// from zero without a process restart.
func (s *Service) Unlink(ctx context.Context) error {
	// Stop any in-flight pair flow before tearing down the client; the
	// pair goroutine holds a reference to `client` and would otherwise
	// see use-after-Logout state.
	s.preemptPair()
	s.mu.Lock()
	if s.client == nil {
		s.mu.Unlock()
		return nil
	}
	client := s.client
	id := s.handlerID
	s.client = nil
	s.handlerID = 0
	s.status.Connected = false
	s.status.LoggedIn = false
	s.status.OwnJID = ""
	s.status.OwnPushName = ""
	s.status.PairedAt = time.Time{}
	s.mu.Unlock()

	if id != 0 {
		client.RemoveEventHandler(id)
	}
	if err := client.Logout(ctx); err != nil {
		return fmt.Errorf("whatsapp: logout: %w", err)
	}
	s.publishStatus()
	if !s.cfg.Enabled {
		return nil
	}
	return s.initClient(ctx)
}

// initClient builds a fresh client via the factory and registers the
// internal event handler. Caller must NOT hold mu.
func (s *Service) initClient(ctx context.Context) error {
	client, err := s.factory(ctx)
	if err != nil {
		return fmt.Errorf("whatsapp: factory: %w", err)
	}
	id := client.AddEventHandler(s.onEvent)
	s.mu.Lock()
	s.client = client
	s.handlerID = id
	s.mu.Unlock()
	return nil
}

// connectAndRecord calls Client.Connect and records timing into
// status. Caller must NOT hold mu.
func (s *Service) connectAndRecord(ctx context.Context) error {
	s.mu.RLock()
	client := s.client
	now := s.now
	s.mu.RUnlock()
	if client == nil {
		return errors.New("whatsapp: not initialized")
	}
	if err := client.Connect(ctx); err != nil {
		s.mu.Lock()
		s.status.LastError = err.Error()
		s.mu.Unlock()
		s.publishStatus()
		return err
	}
	s.mu.Lock()
	s.status.LastError = ""
	s.status.LastSeenAt = now()
	if client.HasSession() && s.status.PairedAt.IsZero() {
		s.status.PairedAt = now()
	}
	s.mu.Unlock()
	s.publishStatus()
	return nil
}

// onEvent is the single point through which whatsmeow events flow into
// the Service. Phase 1 handles lifecycle; Phase 2 will branch into the
// dispatcher for EventIncomingMessage.
func (s *Service) onEvent(ev Event) {
	switch e := ev.(type) {
	case EventConnected:
		s.mu.Lock()
		s.status.Connected = true
		s.status.LastSeenAt = s.now()
		s.status.LastError = ""
		s.mu.Unlock()
		s.publishStatus()
	case EventDisconnected:
		s.mu.Lock()
		s.status.Connected = false
		if e.Err != nil {
			s.status.LastError = e.Err.Error()
		}
		s.mu.Unlock()
		s.publishStatus()
	case EventLoggedOut:
		s.mu.Lock()
		s.status.Connected = false
		s.status.LoggedIn = false
		s.status.HasSession = false
		s.status.OwnJID = ""
		s.status.OwnPushName = ""
		s.status.LastError = "logged out: " + e.Reason
		s.mu.Unlock()
		s.log.WithField("reason", e.Reason).Warn("whatsapp: logged out by server; user must re-pair")
		s.publishStatus()
	case EventPairSuccess:
		s.mu.Lock()
		s.status.LoggedIn = true
		s.status.PairedAt = s.now()
		s.status.OwnJID = e.JID
		s.status.LastError = ""
		s.mu.Unlock()
		s.log.WithField("jid", e.JID).Info("whatsapp: pair success")
		s.publishStatus()
	case EventIncomingMessage:
		s.routeInbound(e.Msg)
	}
}

// routeInbound runs the classifier and forwards Process decisions into
// the dispatcher. Drop decisions are logged at debug level only — they
// must NOT touch the observation log, projection state, or memory; the
// privacy contract demands group messages without mentions and DMs
// from non-allowlisted senders leave no trace.
func (s *Service) routeInbound(msg IncomingMessage) {
	s.mu.RLock()
	rtCfg := s.rtCfg
	pairTime := s.status.PairedAt
	disp := s.dispatcher
	now := s.now
	checker := s.openReplyChecker
	ownJID := ""
	if s.client != nil {
		ownJID = s.client.OwnJID()
	}
	s.mu.RUnlock()

	// V2.13.3c: defensive JID normalization. The wire format from
	// whatsmeow is already digit-only (no `+`) so this is a no-op
	// in production, but it cheaply guards against any future code
	// path that might surface a non-canonical JID into routing.
	msg.SenderJID = NormalizeJID(msg.SenderJID)
	msg.ChatJID = NormalizeJID(msg.ChatJID)

	// V2.13.3: when an assistant-mode send is awaiting a reply from
	// this DM sender, treat them as eligible for this single
	// classification. Self-revoking — once the row resolves or
	// expires, the check returns false again.
	if !msg.IsGroup && checker != nil && msg.ChatJID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		open := checker(ctx, msg.ChatJID, now())
		cancel()
		if open {
			rtCfg = rtCfg.WithAdditionalAllowedDM(msg.ChatJID)
			s.log.WithField("chat_jid", msg.ChatJID).
				Debug("whatsapp: auto-allowlist for open expected_reply")
		}
	}

	dec := Classify(msg, rtCfg, ownJID, pairTime, now())
	if dec.Action == ActionDrop {
		s.log.WithFields(logrus.Fields{
			"sender":   msg.SenderJID,
			"is_group": msg.IsGroup,
			"reason":   dec.Reason,
		}).Debug("whatsapp: classifier dropped message")
		return
	}
	if disp == nil {
		s.log.Warn("whatsapp: dispatcher not initialized; dropping process decision")
		return
	}
	disp.Enqueue(dec)
}
