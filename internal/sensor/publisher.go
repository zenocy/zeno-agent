package sensor

import (
	"context"

	"github.com/zenocy/zeno-v2/internal/eventbus"
)

// EventPublisher is the narrow seam sensors use to push observations onto
// the typed eventbus. *eventbus.Bus satisfies this interface implicitly so
// no wrapper is needed at the call site; tests can also pass a tiny fake.
//
// Sensors NEVER hold a publisher directly. The scheduler's SyncAll attaches
// one to ctx via ContextWithPublisher; sensors extract via PublishObserved.
// Unit tests that don't wire a bus get the publisher-less ctx and the
// publish call is a silent no-op.
type EventPublisher interface {
	Publish(ev eventbus.Event)
}

type publisherKey struct{}

// ContextWithPublisher attaches a publisher to ctx for downstream extraction
// by PublishObserved. A nil publisher is a no-op (the input ctx is returned
// unchanged) — this preserves the unit-test contract that sensors run fine
// without a bus.
func ContextWithPublisher(ctx context.Context, pub EventPublisher) context.Context {
	if pub == nil {
		return ctx
	}
	return context.WithValue(ctx, publisherKey{}, pub)
}

// PublisherFromContext returns the publisher attached to ctx, or nil if
// none is attached.
func PublisherFromContext(ctx context.Context) EventPublisher {
	pub, _ := ctx.Value(publisherKey{}).(EventPublisher)
	return pub
}

// PublishObserved emits a SensorEventObservedEvent if a publisher is attached
// to ctx. A no-op otherwise. Safe to call from any sensor on any ctx.
//
// **Ordering contract**: sensors MUST call this strictly AFTER a successful
// log append for the same observation. The inject detector folds projections
// from the durable log; if publish ran first, the freshly-arrived event
// would be invisible at Detect time and the subscriber would synth on stale
// state.
func PublishObserved(ctx context.Context, kind, evidenceID string, payload map[string]any) {
	pub := PublisherFromContext(ctx)
	if pub == nil {
		return
	}
	pub.Publish(eventbus.SensorEventObservedEvent{
		Kind_:      kind,
		EvidenceID: evidenceID,
		Payload:    payload,
	})
}
