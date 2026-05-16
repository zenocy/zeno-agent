package metrics

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

type fakeWriter struct {
	mu     sync.Mutex
	events []recordedEvent
}

type recordedEvent struct {
	Kind    string
	Source  string
	Payload []byte
}

func (f *fakeWriter) Append(_ context.Context, kind, source string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.events = append(f.events, recordedEvent{Kind: kind, Source: source, Payload: b})
	f.mu.Unlock()
	return nil
}

func (f *fakeWriter) Snapshot() []recordedEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedEvent, len(f.events))
	copy(out, f.events)
	return out
}

func TestEmitter_TickAppendsSnapshotEvent(t *testing.T) {
	m := NewForTest()
	m.ObserveSynthRun("cards", "ok", 12*time.Second)
	m.SetMemoryFacts(7)

	w := &fakeWriter{}
	e := NewEmitter(m, EmitterConfig{
		Append:   w.Append,
		Source:   "metrics",
		Interval: 10 * time.Second, // never used; we call tick directly
		Now:      func() time.Time { return time.Date(2026, 5, 5, 9, 0, 0, 0, time.UTC) },
	})
	e.EmitOnce(context.Background())

	events := w.Snapshot()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != SnapshotKind {
		t.Fatalf("event kind = %q, want %q", events[0].Kind, SnapshotKind)
	}
	if events[0].Source != "metrics" {
		t.Fatalf("event source = %q, want metrics", events[0].Source)
	}

	var got Snapshot
	if err := json.Unmarshal(events[0].Payload, &got); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if got.MemoryFacts != 7 {
		t.Errorf("snap.MemoryFacts = %d, want 7", got.MemoryFacts)
	}
	if got.Synth["cards"].Runs != 1 {
		t.Errorf("snap.Synth[cards].Runs = %d, want 1", got.Synth["cards"].Runs)
	}
}

func TestEmitter_NilAppendDisablesGoroutine(t *testing.T) {
	m := NewForTest()
	e := NewEmitter(m, EmitterConfig{Interval: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	e.Start(ctx)
	cancel()
	e.Stop() // should return immediately even with no goroutine running
}

func TestEmitter_HookRunsBeforeSnapshot(t *testing.T) {
	m := NewForTest()
	w := &fakeWriter{}
	calls := 0
	hook := func(mm *Metrics) {
		calls++
		mm.SetSSESubscribers(calls)
	}
	e := NewEmitter(m, EmitterConfig{Append: w.Append, Hooks: []SnapshotHook{hook}})
	e.EmitOnce(context.Background())
	e.EmitOnce(context.Background())

	if calls != 2 {
		t.Errorf("hook calls = %d, want 2", calls)
	}
	events := w.Snapshot()
	var last Snapshot
	if err := json.Unmarshal(events[len(events)-1].Payload, &last); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if last.SSE.Subscribers != 2 {
		t.Errorf("subscribers in last snapshot = %d, want 2", last.SSE.Subscribers)
	}
}
