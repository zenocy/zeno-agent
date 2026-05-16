// Package eventbus is a tiny in-process pub/sub for live UI updates.
//
// V2.3.0 Phase 3 introduced it to fan out inject cards from the cron-driven
// inject pipeline to any open SSE subscriber on /api/today/stream. The
// scope was intentionally minimal — single process, no persistence, no
// replay, no topics, one payload type (store.Card).
//
// V2.4.0 generalizes the payload to a typed Event union (see
// event_types.go) so the bus can carry trace steps, body-token deltas,
// run-lifecycle events, and sensor observations alongside the original
// card publishes. The wire-level guarantee is preserved: SSE clients
// continue to receive `event: card.appended` with the marshaled card
// as the data payload — the V2.3 React consumer keeps working.
//
// Publish is non-blocking: a subscriber that can't keep up has its
// buffered slot skipped and a WARN logged. The card / event is not
// lost from the system — it's just dropped from the live stream —
// because the durable copy exists in the database (cards, traces,
// observation log).
package eventbus

import (
	"sync"

	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/store"
)

// DefaultBufferSize is the per-subscriber channel capacity. V2.4 bumps
// this from V2.3's 32 to 256 so token-delta floods (synth.delta during
// streaming) have headroom before the slow-subscriber drop kicks in.
// Publishers also coalesce body deltas server-side (≤16ms or 8 chars),
// so the 256 ceiling is rarely the constraint in practice.
const DefaultBufferSize = 256

// Bus is a fan-out hub for typed Event publishes.
type Bus struct {
	mu         sync.RWMutex
	subs       []chan Event
	bufferSize int
	logger     *logrus.Entry

	// onDropped, when set, is invoked once per dropped event with the
	// event Kind. Wired by main.go to metrics.IncSSEDropped.
	onDropped func(kind string)
}

// New constructs a Bus with the default buffer size.
func New(logger *logrus.Entry) *Bus {
	return &Bus{bufferSize: DefaultBufferSize, logger: logger}
}

// WithDropObserver wires a callback fired once per slow-subscriber drop. The
// argument is the dropped event Kind (e.g. "card.appended"). Pass nil to
// disable.
func (b *Bus) WithDropObserver(fn func(kind string)) *Bus {
	b.onDropped = fn
	return b
}

// WithBufferSize overrides the per-subscriber buffer (used by tests).
func (b *Bus) WithBufferSize(n int) *Bus {
	if n > 0 {
		b.bufferSize = n
	}
	return b
}

// Subscribe registers a new subscriber and returns the channel it should
// read from. Always pair with Unsubscribe (typically via defer) so the
// subscriber slice doesn't grow unbounded across SSE reconnects.
func (b *Bus) Subscribe() chan Event {
	ch := make(chan Event, b.bufferSize)
	b.mu.Lock()
	b.subs = append(b.subs, ch)
	count := len(b.subs)
	b.mu.Unlock()
	if b.logger != nil {
		b.logger.WithField("subscribers", count).Debug("eventbus: subscribed")
	}
	return ch
}

// Unsubscribe removes the subscriber and closes its channel. Safe to call
// with an unknown channel — that's a no-op.
func (b *Bus) Unsubscribe(ch chan Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, sub := range b.subs {
		if sub == ch {
			b.subs = append(b.subs[:i], b.subs[i+1:]...)
			close(sub)
			if b.logger != nil {
				b.logger.WithField("subscribers", len(b.subs)).Debug("eventbus: unsubscribed")
			}
			return
		}
	}
}

// Publish fans the event out to every subscriber. **Non-blocking** —
// a subscriber whose buffer is full has the event dropped (with a WARN)
// rather than blocking the publisher. Durable persistence in the
// database (cards, traces, observation log) backstops every drop;
// dropped events only lose the live notification.
func (b *Bus) Publish(ev Event) {
	if ev == nil {
		return
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, sub := range b.subs {
		select {
		case sub <- ev:
		default:
			if b.logger != nil {
				b.logger.WithField("kind", ev.Kind()).
					Warn("eventbus: subscriber buffer full, dropping event from live stream")
			}
			if b.onDropped != nil {
				b.onDropped(ev.Kind())
			}
		}
	}
}

// PublishCard is a V2.3-compat shim. It wraps the card in a
// CardAppendedEvent and forwards to Publish. Existing callers that
// pre-date V2.4's typed bus can keep using this; new callers should
// construct the event explicitly.
func (b *Bus) PublishCard(card store.Card) {
	b.Publish(CardAppendedEvent{Card: card})
}

// SubscriberCount reports the current number of active subscribers.
// Used by tests; not load-bearing for runtime behavior.
func (b *Bus) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}
