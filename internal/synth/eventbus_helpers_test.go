package synth

import (
	"testing"
	"time"

	"github.com/zenocy/zeno-v2/internal/eventbus"
)

// drainBus reads from sub until it has been quiet for quietFor (no event
// arrived in that window). Returns events in arrival order. Bounded by a
// hard 5s ceiling so tests fail fast instead of hanging.
func drainBus(t *testing.T, sub <-chan eventbus.Event, quietFor time.Duration) []eventbus.Event {
	t.Helper()
	var out []eventbus.Event
	hardStop := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-sub:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-time.After(quietFor):
			return out
		case <-hardStop:
			t.Fatalf("drainBus: 5s ceiling reached after %d events", len(out))
			return out
		}
	}
}

// collapsedKinds returns the kind sequence of events with consecutive
// duplicates collapsed. Used to assert event ordering at coarse grain
// (e.g., started → trace.step → synth.delta → completed → card.appended).
func collapsedKinds(events []eventbus.Event) []string {
	if len(events) == 0 {
		return nil
	}
	out := make([]string, 0, len(events))
	prev := ""
	for _, ev := range events {
		k := ev.Kind()
		if k != prev {
			out = append(out, k)
			prev = k
		}
	}
	return out
}

// eventsByKind filters events by Kind() string.
func eventsByKind(events []eventbus.Event, kind string) []eventbus.Event {
	var out []eventbus.Event
	for _, ev := range events {
		if ev.Kind() == kind {
			out = append(out, ev)
		}
	}
	return out
}
