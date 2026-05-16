package sensor

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/eventbus"
)

// fakePublisher records every event passed to Publish for later assertion.
type fakePublisher struct {
	events []eventbus.Event
}

func (f *fakePublisher) Publish(ev eventbus.Event) {
	f.events = append(f.events, ev)
}

func TestContextWithPublisher_RoundTrip(t *testing.T) {
	pub := &fakePublisher{}
	ctx := ContextWithPublisher(context.Background(), pub)

	got := PublisherFromContext(ctx)
	require.NotNil(t, got)
	require.Same(t, pub, got, "PublisherFromContext must return the same publisher attached")
}

func TestContextWithPublisher_NilPublisherIsNoOp(t *testing.T) {
	parent := context.Background()
	ctx := ContextWithPublisher(parent, nil)

	require.Equal(t, parent, ctx, "nil publisher must return the parent ctx unchanged")
	require.Nil(t, PublisherFromContext(ctx), "no publisher attached → PublisherFromContext returns nil")
}

func TestPublishObserved_NoPublisherInContext_NoOp(t *testing.T) {
	// No publisher attached. Must not panic, must publish nothing.
	require.NotPanics(t, func() {
		PublishObserved(context.Background(), "mail.received", "abc", map[string]any{"k": "v"})
	})
}

func TestPublishObserved_HappyPath(t *testing.T) {
	pub := &fakePublisher{}
	ctx := ContextWithPublisher(context.Background(), pub)

	payload := map[string]any{"folder": "INBOX", "uid": uint32(42)}
	PublishObserved(ctx, "mail.received", "INBOX:42:11", payload)

	require.Len(t, pub.events, 1)
	obs, ok := pub.events[0].(eventbus.SensorEventObservedEvent)
	require.True(t, ok, "published event must be SensorEventObservedEvent")
	require.Equal(t, "mail.received", obs.Kind_)
	require.Equal(t, "INBOX:42:11", obs.EvidenceID)
	require.Equal(t, payload, obs.Payload)
	require.Equal(t, "sensor.event_observed", obs.Kind(), "Kind() returns the SSE event-name (bus-internal, but stable)")
}
