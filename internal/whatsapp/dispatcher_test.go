package whatsapp_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/whatsapp"
)

// TestDispatcher_PerJIDSerialization verifies that two messages in the
// same chat are processed strictly in order. The handler counts entries
// vs exits; if serialization works, the in-flight count for the same
// chat never exceeds 1.
func TestDispatcher_PerJIDSerialization(t *testing.T) {
	var inFlight int32
	var maxInFlight int32
	var done sync.WaitGroup
	done.Add(2)
	handler := func(ctx context.Context, d whatsapp.Decision) error {
		cur := atomic.AddInt32(&inFlight, 1)
		for {
			max := atomic.LoadInt32(&maxInFlight)
			if cur <= max || atomic.CompareAndSwapInt32(&maxInFlight, max, cur) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
		done.Done()
		return nil
	}

	cfg := whatsapp.DefaultRuntimeConfig()
	cfg.MinChatInterval = time.Microsecond
	d := whatsapp.NewDispatcher(cfg, handler, quietLog())
	d.Start(context.Background())
	defer d.Stop()

	d.Enqueue(whatsapp.Decision{Action: whatsapp.ActionProcess, ChatJID: "a", MessageID: "1"})
	d.Enqueue(whatsapp.Decision{Action: whatsapp.ActionProcess, ChatJID: "a", MessageID: "2"})

	done.Wait()
	assert.Equal(t, int32(1), atomic.LoadInt32(&maxInFlight),
		"per-chat serialization must hold")
}

// TestDispatcher_GlobalConcurrencyCap caps in-flight to MaxConcurrentSynth
// even across distinct chats.
func TestDispatcher_GlobalConcurrencyCap(t *testing.T) {
	var inFlight int32
	var maxInFlight int32

	cfg := whatsapp.DefaultRuntimeConfig()
	cfg.MaxConcurrentSynth = 2
	cfg.MinChatInterval = time.Microsecond
	cfg.PerChatBuffer = 1

	const totalChats = 6
	var done sync.WaitGroup
	done.Add(totalChats)

	handler := func(ctx context.Context, d whatsapp.Decision) error {
		cur := atomic.AddInt32(&inFlight, 1)
		for {
			max := atomic.LoadInt32(&maxInFlight)
			if cur <= max || atomic.CompareAndSwapInt32(&maxInFlight, max, cur) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
		done.Done()
		return nil
	}

	d := whatsapp.NewDispatcher(cfg, handler, quietLog())
	d.Start(context.Background())
	defer d.Stop()

	for i := 0; i < totalChats; i++ {
		d.Enqueue(whatsapp.Decision{
			Action:  whatsapp.ActionProcess,
			ChatJID: string(rune('a' + i)),
		})
	}
	done.Wait()
	assert.LessOrEqual(t, atomic.LoadInt32(&maxInFlight), int32(2),
		"global concurrency must be respected")
}

// TestDispatcher_DropOldestOnOverflow proves the bounded inbox keeps the
// freshest messages when the worker is busy.
func TestDispatcher_DropOldestOnOverflow(t *testing.T) {
	cfg := whatsapp.DefaultRuntimeConfig()
	cfg.PerChatBuffer = 2
	cfg.MaxConcurrentSynth = 1
	cfg.MinChatInterval = time.Microsecond

	processed := []string{}
	var mu sync.Mutex
	gate := make(chan struct{}, 1)
	handler := func(ctx context.Context, d whatsapp.Decision) error {
		// Block on the first message so we can stuff the inbox.
		if d.MessageID == "1" {
			<-gate
		}
		mu.Lock()
		processed = append(processed, d.MessageID)
		mu.Unlock()
		return nil
	}

	d := whatsapp.NewDispatcher(cfg, handler, quietLog())
	d.Start(context.Background())
	defer d.Stop()

	// "1" enters the worker and blocks on gate.
	d.Enqueue(whatsapp.Decision{Action: whatsapp.ActionProcess, ChatJID: "x", MessageID: "1"})
	// Give the worker time to start handling "1".
	time.Sleep(20 * time.Millisecond)
	// "2" and "3" fill the buffer.
	d.Enqueue(whatsapp.Decision{Action: whatsapp.ActionProcess, ChatJID: "x", MessageID: "2"})
	d.Enqueue(whatsapp.Decision{Action: whatsapp.ActionProcess, ChatJID: "x", MessageID: "3"})
	// "4" overflows: drop OLDEST (should drop "2"). "5" overflows again: drops "3".
	d.Enqueue(whatsapp.Decision{Action: whatsapp.ActionProcess, ChatJID: "x", MessageID: "4"})
	d.Enqueue(whatsapp.Decision{Action: whatsapp.ActionProcess, ChatJID: "x", MessageID: "5"})

	// Release the worker.
	gate <- struct{}{}

	// Wait for everything to drain.
	for i := 0; i < 30; i++ {
		mu.Lock()
		n := len(processed)
		mu.Unlock()
		if n >= 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, len(processed), 3)
	assert.Equal(t, "1", processed[0])
	// We expect the survivors to be the most recent two (4 and 5).
	assert.Equal(t, "4", processed[1], "drop-oldest should keep newer message #4")
	assert.Equal(t, "5", processed[2], "drop-oldest should keep newest message #5")
}

// TestDispatcher_MinChatIntervalThrottle proves the per-chat throttle
// inserts a delay between successive messages in the same chat.
func TestDispatcher_MinChatIntervalThrottle(t *testing.T) {
	cfg := whatsapp.DefaultRuntimeConfig()
	cfg.MinChatInterval = 100 * time.Millisecond
	cfg.MaxConcurrentSynth = 4

	var times []time.Time
	var mu sync.Mutex
	var done sync.WaitGroup
	done.Add(2)

	handler := func(ctx context.Context, d whatsapp.Decision) error {
		mu.Lock()
		times = append(times, time.Now())
		mu.Unlock()
		done.Done()
		return nil
	}

	d := whatsapp.NewDispatcher(cfg, handler, quietLog())
	d.Start(context.Background())
	defer d.Stop()

	d.Enqueue(whatsapp.Decision{Action: whatsapp.ActionProcess, ChatJID: "x", MessageID: "1"})
	d.Enqueue(whatsapp.Decision{Action: whatsapp.ActionProcess, ChatJID: "x", MessageID: "2"})

	done.Wait()
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, times, 2)
	gap := times[1].Sub(times[0])
	assert.GreaterOrEqual(t, gap, 80*time.Millisecond,
		"throttle should insert a delay close to MinChatInterval; got %v", gap)
}

// TestDispatcher_DropDecisionIgnored confirms that Action=Drop never
// reaches the handler (privacy contract).
func TestDispatcher_DropDecisionIgnored(t *testing.T) {
	cfg := whatsapp.DefaultRuntimeConfig()
	cfg.MinChatInterval = time.Microsecond

	called := atomic.Int32{}
	handler := func(ctx context.Context, d whatsapp.Decision) error {
		called.Add(1)
		return nil
	}

	d := whatsapp.NewDispatcher(cfg, handler, quietLog())
	d.Start(context.Background())
	defer d.Stop()

	d.Enqueue(whatsapp.Decision{Action: whatsapp.ActionDrop, ChatJID: "x", MessageID: "1"})
	time.Sleep(50 * time.Millisecond)
	assert.Zero(t, called.Load(), "drop decision must not invoke handler")
}
