package whatsapp

import (
	"context"
	"sync"
	"time"
)

// Throttle is a per-JID minimum-interval gate. Both the dispatcher
// (receive-reply path) and the proactive `send_whatsapp` action surface
// consult it so the floor — typically 3s between two messages in the
// same chat — applies regardless of which path emits.
//
// The dispatcher used to track this on chatInbox.lastSentAt directly;
// V2.12 lifted it into a process-wide singleton so a proactive send
// can't sneak under the floor while a receive-reply has just fired in
// the same chat.
//
// Wait blocks up to the minimum interval (or returns ctx.Err() on
// cancel). MarkSent stamps the chat at the current clock. A
// freshly-constructed Throttle treats every JID as never-sent.
type Throttle struct {
	mu   sync.Mutex
	last map[string]time.Time
	now  func() time.Time
	// sleep is the delay primitive; tests inject a deterministic stub.
	sleep func(time.Duration)
}

// NewThrottle returns a ready Throttle.
func NewThrottle() *Throttle {
	return &Throttle{
		last:  map[string]time.Time{},
		now:   time.Now,
		sleep: time.Sleep,
	}
}

// SetClock replaces the time source — tests use it for deterministic
// throttle assertions.
func (t *Throttle) SetClock(now func() time.Time) {
	if now == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.now = now
}

// SetSleep replaces the delay primitive — tests inject a no-op so the
// retry/backoff loop runs without real wall-clock sleeps.
func (t *Throttle) SetSleep(sleep func(time.Duration)) {
	if sleep == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sleep = sleep
}

// LastSent returns the most recent MarkSent stamp for jid, or zero when
// the JID has never been marked.
func (t *Throttle) LastSent(jid string) time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.last[jid]
}

// MarkSent stamps jid with the current time.
func (t *Throttle) MarkSent(jid string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.last[jid] = t.now()
}

// Wait blocks until the per-JID min-interval has passed since the last
// MarkSent. Returns ctx.Err() if the context is cancelled while waiting.
// Zero or negative minInterval is treated as "no wait".
func (t *Throttle) Wait(ctx context.Context, jid string, minInterval time.Duration) error {
	if minInterval <= 0 {
		return nil
	}
	t.mu.Lock()
	last := t.last[jid]
	now := t.now
	sleep := t.sleep
	t.mu.Unlock()

	if last.IsZero() {
		return nil
	}
	gap := now().Sub(last)
	if gap >= minInterval {
		return nil
	}

	wait := minInterval - gap
	// Using sleep directly preserves the deterministic-test stub. The
	// ctx-cancel race is handled below.
	done := make(chan struct{})
	go func() {
		sleep(wait)
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
