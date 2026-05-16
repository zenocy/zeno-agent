package llm

import (
	"context"
	"errors"
	"math/rand/v2"
	"net"
	"time"

	openai "github.com/sashabaranov/go-openai"
	"github.com/sirupsen/logrus"
)

// RetryObserver is invoked once per retry-loop terminal outcome
// ("succeeded"|"exhausted") so callers can wire it to a Prometheus counter
// without this package importing internal/metrics.
type RetryObserver func(outcome string)

// RetryPolicy describes when and how to retry transient LLM endpoint failures.
// Defaults: 3 attempts total, 250ms initial backoff with 2× growth and ±20%
// jitter. Cap of 8s per backoff so a slow path doesn't blow the cron budget.
type RetryPolicy struct {
	MaxAttempts    int           // total attempts (1 = no retry); 0 → 3
	InitialBackoff time.Duration // first sleep before retry; 0 → 250ms
	MaxBackoff     time.Duration // upper bound per sleep; 0 → 8s

	// Logger emits per-attempt DEBUG lines and a terminal INFO/WARN. Nil
	// disables logging entirely (the path retains its prior silent behavior
	// for tests and embedded usage that supplies its own instrumentation).
	Logger *logrus.Entry
	// Observer is called exactly once per terminal outcome — either after
	// a successful retry (>1 attempt) or after retries are exhausted. Nil
	// disables.
	Observer RetryObserver
}

// withDefaults fills in zero-valued fields.
func (p RetryPolicy) withDefaults() RetryPolicy {
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = 3
	}
	if p.InitialBackoff <= 0 {
		p.InitialBackoff = 250 * time.Millisecond
	}
	if p.MaxBackoff <= 0 {
		p.MaxBackoff = 8 * time.Second
	}
	return p
}

// retryChat invokes fn up to policy.MaxAttempts times. Retries on:
//   - *openai.APIError with HTTPStatusCode in IsRetryableStatus
//   - net.Error with Timeout() true (excluding the call context's own deadline)
//
// All other errors return immediately. The final error is wrapped in
// *TransientFailureError when retries were attempted, so callers can observe
// attempt counts via errors.As.
func retryChat(
	ctx context.Context,
	policy RetryPolicy,
	fn func(context.Context) (openai.ChatCompletionResponse, error),
) (openai.ChatCompletionResponse, error) {
	policy = policy.withDefaults()

	var lastErr error
	var lastStatus int
	backoff := policy.InitialBackoff
	attemptsMade := 0
	retried := false

	for attempt := 1; attempt <= policy.MaxAttempts; attempt++ {
		attemptsMade = attempt
		resp, err := fn(ctx)
		if err == nil {
			if attempt > 1 {
				if policy.Logger != nil {
					policy.Logger.WithField("attempts", attempt).Info("llm: retry succeeded")
				}
				if policy.Observer != nil {
					policy.Observer("succeeded")
				}
			}
			return resp, nil
		}

		// Context cancelled or deadlined — no point retrying.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return openai.ChatCompletionResponse{}, err
		}

		retryable, status := classifyForRetry(err)
		lastErr = err
		if status > 0 {
			lastStatus = status
		}
		if policy.Logger != nil {
			policy.Logger.WithFields(logrus.Fields{
				"attempt":   attempt,
				"status":    status,
				"retryable": retryable,
			}).WithError(err).Debug("llm: call failed")
		}
		if !retryable || attempt == policy.MaxAttempts {
			break
		}

		// We're about to retry — record that fact so the wrap below knows.
		retried = true

		// Sleep with ±20% jitter, capped at MaxBackoff.
		sleep := jitter(backoff)
		if sleep > policy.MaxBackoff {
			sleep = policy.MaxBackoff
		}
		select {
		case <-time.After(sleep):
		case <-ctx.Done():
			return openai.ChatCompletionResponse{}, err
		}

		// Double for the next attempt.
		backoff *= 2
		if backoff > policy.MaxBackoff {
			backoff = policy.MaxBackoff
		}
	}

	// Wrap only when we actually retried — a non-retryable first failure
	// should pass through verbatim so the caller sees the original error.
	if retried {
		if policy.Logger != nil {
			policy.Logger.WithFields(logrus.Fields{
				"attempts":    attemptsMade,
				"last_status": lastStatus,
			}).WithError(lastErr).Warn("llm: retries exhausted")
		}
		if policy.Observer != nil {
			policy.Observer("exhausted")
		}
		return openai.ChatCompletionResponse{}, &TransientFailureError{
			Attempts:   attemptsMade,
			LastStatus: lastStatus,
			Underlying: lastErr,
		}
	}
	return openai.ChatCompletionResponse{}, lastErr
}

// classifyForRetry inspects an error from the openai client and reports
// whether it should be retried. Returns the HTTP status when known.
func classifyForRetry(err error) (retryable bool, status int) {
	var apiErr *openai.APIError
	if errors.As(err, &apiErr) {
		return IsRetryableStatus(apiErr.HTTPStatusCode), apiErr.HTTPStatusCode
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true, 0
	}
	// Bare i/o errors (connection refused, broken pipe) are also worth one
	// retry — they're frequent during local-model restarts.
	if isConnRefusedOrReset(err) {
		return true, 0
	}
	return false, 0
}

func isConnRefusedOrReset(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	// Cheap substring match — net.OpError doesn't expose ECONNREFUSED
	// portably.
	for _, fragment := range []string{"connection refused", "connection reset", "broken pipe", "EOF"} {
		if contains(s, fragment) {
			return true
		}
	}
	return false
}

// contains is a tiny case-insensitive substring helper to avoid pulling in
// strings just for one call.
func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	if len(s) < len(sub) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			a, b := s[i+j], sub[j]
			if a >= 'A' && a <= 'Z' {
				a += 32
			}
			if b >= 'A' && b <= 'Z' {
				b += 32
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// jitter returns d ± 20%.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	delta := float64(d) * 0.2
	return d + time.Duration((rand.Float64()*2-1)*delta)
}

// AsTransientFailure unwraps err to a *TransientFailureError if present.
// Callers can use this to log attempt counts after a retry-exhausted failure.
func AsTransientFailure(err error) (*TransientFailureError, bool) {
	var t *TransientFailureError
	if errors.As(err, &t) {
		return t, true
	}
	return nil, false
}
