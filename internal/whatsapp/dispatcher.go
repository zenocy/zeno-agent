package whatsapp

import (
	"context"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// MessageHandler is the callback the dispatcher invokes for each
// Process decision. Phase 3 supplies the real implementation: classify
// → synth.Ask → client.SendText. Phase 2 wires a no-op when the Service
// is constructed without a handler.
type MessageHandler func(ctx context.Context, decision Decision) error

// Dispatcher serializes message processing per chat (so Zeno never
// emits two replies in the same chat at once), enforces a global
// concurrency cap on synth calls (so a chatty group doesn't starve
// the LLM), and applies a per-chat min-interval throttle (mitigates
// ban risk from burst-replies).
//
// Lifecycle:
//
//   - NewDispatcher constructs a stopped dispatcher.
//   - Start runs background workers; safe to call once.
//   - Enqueue is non-blocking; over-capacity per-chat inboxes
//     drop-OLDEST so Zeno responds to the freshest message in a
//     fast-moving chat (vs. catching up on stale ones the user has
//     already moved past).
//   - Stop drains the global semaphore and exits all workers.
type Dispatcher struct {
	cfg     RuntimeConfig
	handler MessageHandler
	log     *logrus.Entry
	now     func() time.Time

	// throttle is the shared per-chat min-interval gate. V2.12 lifted
	// the per-chat lastSentAt out of chatInbox so the proactive
	// `send_whatsapp` executor and the receive-reply path apply one
	// floor. NewDispatcher constructs a private one when none is
	// injected; the Service wires its singleton in production.
	throttle *Throttle

	sem chan struct{} // global concurrency cap

	mu     sync.Mutex
	chats  map[string]*chatInbox // keyed by ChatJID
	closed bool

	// wg tracks every per-chat worker goroutine; Stop waits on it.
	wg sync.WaitGroup

	parentCtx    context.Context
	parentCancel context.CancelFunc
}

// chatInbox holds per-JID queue state. The dispatcher creates one on
// first message for a JID and never reaps them — a single user has at
// most a few dozen active chats, so the memory cost is negligible and
// reaping introduces edge cases.
//
// V2.12: lastSentAt moved to the shared Throttle so proactive sends
// observe the same floor.
type chatInbox struct {
	mu      sync.Mutex
	queue   []Decision
	notify  chan struct{} // 1-slot wakeup
	skipped int           // count since last log line
}

// NewDispatcher constructs a Dispatcher. Start MUST be called before
// Enqueue is useful — Enqueue still buffers, but no worker drains it.
//
// The constructed dispatcher owns a private Throttle. Call SetThrottle
// to swap in a shared instance when wiring proactive sends.
func NewDispatcher(cfg RuntimeConfig, handler MessageHandler, log *logrus.Entry) *Dispatcher {
	if log == nil {
		log = logrus.NewEntry(logrus.New())
	}
	cfg = cfg.Normalize()
	return &Dispatcher{
		cfg:      cfg,
		handler:  handler,
		log:      log,
		now:      time.Now,
		throttle: NewThrottle(),
		sem:      make(chan struct{}, cfg.MaxConcurrentSynth),
		chats:    map[string]*chatInbox{},
	}
}

// SetThrottle replaces the shared throttle. The provided value is used
// for all per-chat send floors going forward; callers that wire the
// proactive send path pass the same instance to the action executor.
// nil is rejected (a private throttle is always present).
func (d *Dispatcher) SetThrottle(t *Throttle) {
	if t == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.throttle = t
}

// Throttle returns the shared throttle. Used by the action executor to
// observe the same floor without taking a separate dependency on the
// Service.
func (d *Dispatcher) Throttle() *Throttle {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.throttle
}

// SetClock replaces the time source — tests use it for deterministic
// throttle assertions.
func (d *Dispatcher) SetClock(now func() time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.now = now
}

// SetHandler swaps the registered handler. Allows the Service to wire
// the synth-driven handler post-construction (Phase 3).
func (d *Dispatcher) SetHandler(h MessageHandler) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.handler = h
}

// SetConfig hot-reloads the dispatcher tuning. Per-chat buffer changes
// only affect freshly-created chat workers (existing inboxes keep
// their original capacity until they're emptied — no graceful resize).
func (d *Dispatcher) SetConfig(cfg RuntimeConfig) {
	cfg = cfg.Normalize()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cfg = cfg
	// MaxConcurrentSynth changes require a fresh sem; resizing safely
	// while workers hold tokens is hard, so we only resize on Start.
}

// Start spawns the supervisor and returns immediately.
func (d *Dispatcher) Start(ctx context.Context) {
	d.mu.Lock()
	if d.parentCtx != nil {
		d.mu.Unlock()
		return
	}
	d.parentCtx, d.parentCancel = context.WithCancel(ctx)
	d.mu.Unlock()
}

// Stop cancels the parent context and waits for in-flight handlers to
// exit. After Stop the dispatcher is unusable; create a new one to
// resume.
func (d *Dispatcher) Stop() {
	d.mu.Lock()
	if d.parentCancel == nil {
		d.mu.Unlock()
		return
	}
	d.closed = true
	cancel := d.parentCancel
	d.parentCancel = nil
	d.mu.Unlock()
	cancel()
	d.wg.Wait()
}

// Enqueue routes a Process decision into the per-chat queue. Drop
// decisions are NOT enqueued — the privacy contract is enforced at the
// classifier; the dispatcher must never see them.
func (d *Dispatcher) Enqueue(decision Decision) {
	if decision.Action != ActionProcess {
		return
	}

	d.mu.Lock()
	if d.closed || d.parentCtx == nil {
		d.mu.Unlock()
		return
	}
	inbox, ok := d.chats[decision.ChatJID]
	if !ok {
		inbox = &chatInbox{notify: make(chan struct{}, 1)}
		d.chats[decision.ChatJID] = inbox
		d.wg.Add(1)
		go d.runChatWorker(d.parentCtx, decision.ChatJID, inbox)
	}
	cap := d.cfg.PerChatBuffer
	d.mu.Unlock()

	skipped := pushInbox(inbox, decision, cap)
	if skipped > 0 && skipped%cap == 0 {
		d.log.WithFields(logrus.Fields{
			"chat":    decision.ChatJID,
			"skipped": skipped,
		}).Warn("whatsapp: dispatcher dropping older messages while busy")
	}
}

// pushInbox pushes a Decision under inbox.mu. When the queue is at
// capacity it drops the oldest entry — see the package comment for
// why drop-oldest beats drop-newest in this domain.
func pushInbox(inbox *chatInbox, d Decision, capacity int) int {
	inbox.mu.Lock()
	defer inbox.mu.Unlock()
	if len(inbox.queue) >= capacity {
		inbox.queue = inbox.queue[1:]
		inbox.skipped++
	}
	inbox.queue = append(inbox.queue, d)
	select {
	case inbox.notify <- struct{}{}:
	default:
	}
	return inbox.skipped
}

func popInbox(inbox *chatInbox) (Decision, bool) {
	inbox.mu.Lock()
	defer inbox.mu.Unlock()
	if len(inbox.queue) == 0 {
		return Decision{}, false
	}
	d := inbox.queue[0]
	inbox.queue = inbox.queue[1:]
	return d, true
}

// runChatWorker processes Decisions for one ChatJID strictly serially.
// It applies the per-chat throttle BEFORE acquiring a global semaphore
// slot so a throttled chat doesn't hold an LLM slot it can't use.
func (d *Dispatcher) runChatWorker(ctx context.Context, chatJID string, inbox *chatInbox) {
	defer d.wg.Done()

	logEntry := d.log.WithField("chat", chatJID)
	for {
		decision, ok := popInbox(inbox)
		if !ok {
			select {
			case <-inbox.notify:
				continue
			case <-ctx.Done():
				return
			}
		}

		// Per-chat throttle (shared with proactive sends so the floor
		// is observed across paths).
		d.mu.Lock()
		minInterval := d.cfg.MinChatInterval
		throttle := d.throttle
		d.mu.Unlock()
		if err := throttle.Wait(ctx, chatJID, minInterval); err != nil {
			return
		}

		// Global concurrency cap.
		select {
		case d.sem <- struct{}{}:
		case <-ctx.Done():
			return
		}

		d.mu.Lock()
		handler := d.handler
		d.mu.Unlock()
		if handler == nil {
			logEntry.Debug("whatsapp: dispatcher has no handler; dropping decision")
			<-d.sem
			continue
		}

		err := handler(ctx, decision)
		if err != nil {
			logEntry.WithError(err).Warn("whatsapp: handler failed")
		}

		<-d.sem

		throttle.MarkSent(chatJID)
	}
}
