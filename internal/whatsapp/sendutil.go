package whatsapp

import (
	"context"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"
)

// DefaultSendBackoffs is the schedule the receive-reply path has used since
// V2.7 (0, 1s, 4s, 16s). The proactive `send_whatsapp` action surface
// re-uses it so failure-recovery posture is identical across paths.
//
// 0 means "first attempt has no delay"; the loop applies the index-i delay
// BEFORE attempt i+1, so the schedule is:
//
//	attempt 1 — immediate
//	attempt 2 — after 1s
//	attempt 3 — after 4s
//	attempt 4 — after 16s
//
// Total wall time is bounded by the deadline argument; default 60s.
var DefaultSendBackoffs = []time.Duration{0, time.Second, 4 * time.Second, 16 * time.Second}

// DefaultSendDeadline is the wall-clock budget for SendWithBackoff.
const DefaultSendDeadline = time.Minute

// SendWithBackoff retries client.SendText according to backoffs. Returns
// the number of attempts made and the final error. ctx cancellation
// aborts immediately. Total wall time is bounded by deadline.
//
// Pulled out of SynthHandler in V2.12 so the proactive `send_whatsapp`
// executor uses the same retry posture as the reactive reply path.
// Both call sites pass the package defaults; tests can pass shorter
// schedules to keep them quick.
func SendWithBackoff(
	ctx context.Context,
	c Client,
	to, text string,
	backoffs []time.Duration,
	deadline time.Duration,
	sleep func(time.Duration),
	now func() time.Time,
	log *logrus.Entry,
) (int, error) {
	if c == nil {
		return 0, fmt.Errorf("whatsapp: send: nil client")
	}
	if len(backoffs) == 0 {
		backoffs = DefaultSendBackoffs
	}
	if deadline <= 0 {
		deadline = DefaultSendDeadline
	}
	if sleep == nil {
		sleep = time.Sleep
	}
	if now == nil {
		now = time.Now
	}

	start := now()
	var lastErr error
	for i, delay := range backoffs {
		if delay > 0 {
			if now().Add(delay).Sub(start) > deadline {
				break
			}
			select {
			case <-ctx.Done():
				return i, ctx.Err()
			default:
			}
			sleep(delay)
		}
		if ctx.Err() != nil {
			return i, ctx.Err()
		}
		err := c.SendText(ctx, to, text)
		if err == nil {
			if i > 0 && log != nil {
				log.WithField("attempts", i+1).Info("whatsapp: send recovered after retry")
			}
			return i + 1, nil
		}
		lastErr = err
		if log != nil {
			log.WithError(err).WithFields(logrus.Fields{
				"to":      to,
				"attempt": i + 1,
			}).Warn("whatsapp: send attempt failed")
		}
	}
	return len(backoffs), fmt.Errorf("send to %s: %w", to, lastErr)
}
