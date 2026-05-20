package gemini

import (
	"context"
	"errors"
	"io"
	"math/rand"
	"net"
	"strings"
	"time"

	"google.golang.org/genai"

	"github.com/sirupsen/logrus"
	"github.com/zenocy/zeno-v2/internal/llm"
)

// classifyForRetry decides whether an error returned by the SDK should
// be retried. Mirrors the llm package's transient-error policy:
//   - context cancellation / deadline → non-retryable (caller's budget)
//   - genai.APIError with retryable HTTP status → retry
//   - net.Error timeouts and io.EOF / connection-reset / broken-pipe → retry
//   - everything else → non-retryable
func classifyForRetry(err error) (retryable bool, status int) {
	if err == nil {
		return false, 0
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false, 0
	}
	var apiErr genai.APIError
	if errors.As(err, &apiErr) {
		if llm.IsRetryableStatus(apiErr.Code) {
			return true, apiErr.Code
		}
		return false, apiErr.Code
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true, 0
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true, 0
	}
	msg := err.Error()
	if strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") {
		return true, 0
	}
	return false, 0
}

// retryCall runs fn with the configured retry policy. Generic over the
// SDK return type so both the non-streaming GenerateContent path and
// any future single-response call can share the same backoff /
// instrumentation. Streaming is NOT routed through this — the stream
// loop maintains its own iteration state.
func retryCall[T any](ctx context.Context, pol llm.RetryPolicy, fn func(context.Context) (T, error)) (T, error) {
	var zero T
	maxAttempts := pol.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	initial := pol.InitialBackoff
	if initial <= 0 {
		initial = 250 * time.Millisecond
	}
	cap := pol.MaxBackoff
	if cap <= 0 {
		cap = 8 * time.Second
	}
	backoff := initial

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		out, err := fn(ctx)
		if err == nil {
			if attempt > 1 && pol.Observer != nil {
				pol.Observer("succeeded")
			}
			return out, nil
		}
		lastErr = err
		retryable, status := classifyForRetry(err)
		if !retryable {
			return zero, err
		}
		if attempt == maxAttempts {
			break
		}
		if pol.Logger != nil {
			pol.Logger.WithFields(logrus.Fields{
				"attempt":  attempt,
				"max":      maxAttempts,
				"status":   status,
				"backoff":  backoff.String(),
				"err":      err.Error(),
			}).Debug("gemini: retrying")
		}
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-time.After(jitter(backoff)):
		}
		backoff *= 2
		if backoff > cap {
			backoff = cap
		}
	}
	if pol.Observer != nil {
		pol.Observer("exhausted")
	}
	return zero, lastErr
}

// jitter applies ±20% random variation to d. Matches the OpenAI
// client's retry jitter so both providers share an observable backoff
// signature.
func jitter(d time.Duration) time.Duration {
	delta := float64(d) * 0.2
	off := (rand.Float64()*2 - 1) * delta
	return d + time.Duration(off)
}
