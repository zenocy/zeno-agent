package llm

import (
	"errors"
	"fmt"
	"time"
)

// ToolArgsParseError reports a tool-call argument JSON parse failure. The
// loop uses these to drive a bounded repair round-trip that asks the model to
// re-emit valid JSON for the offending tool call.
type ToolArgsParseError struct {
	ToolCallID  string
	Name        string
	RawJSON     string
	ParseErrMsg string
}

func (e *ToolArgsParseError) Error() string {
	return fmt.Sprintf("tool %q (call %s) returned unparseable arguments: %s", e.Name, e.ToolCallID, e.ParseErrMsg)
}

// TruncatedStreamError indicates a streaming response ended without a final
// finish_reason and before any content was flushed.
type TruncatedStreamError struct {
	BytesReceived int
	LastChunkAge  time.Duration
}

func (e *TruncatedStreamError) Error() string {
	return fmt.Sprintf("stream truncated after %d bytes (last chunk %s ago)", e.BytesReceived, e.LastChunkAge)
}

// TransientFailureError wraps a transport-layer failure that retry middleware
// has already exhausted.
type TransientFailureError struct {
	Attempts       int
	LastStatus     int
	LastRetryAfter time.Duration
	Underlying     error
}

func (e *TransientFailureError) Error() string {
	return fmt.Sprintf("transient failure after %d attempts (last status %d): %v", e.Attempts, e.LastStatus, e.Underlying)
}

func (e *TransientFailureError) Unwrap() error { return e.Underlying }

// HTTPStatusError carries the HTTP status, body, and selected headers for a
// non-2xx response so retry middleware can classify it and observability can
// record structured diagnostics.
type HTTPStatusError struct {
	Status     int
	Body       []byte
	RetryAfter time.Duration
	RequestID  string
}

func (e *HTTPStatusError) Error() string {
	if len(e.Body) > 0 {
		return fmt.Sprintf("upstream returned status %d: %s", e.Status, string(e.Body))
	}
	return fmt.Sprintf("upstream returned status %d", e.Status)
}

// SSEErrorFrameError indicates the upstream sent an `event: error` SSE frame
// mid-stream. Surfaces as a hard failure rather than a silent empty completion.
type SSEErrorFrameError struct {
	Message string
	Raw     string
}

func (e *SSEErrorFrameError) Error() string {
	if e.Message != "" {
		return "upstream SSE error: " + e.Message
	}
	return "upstream SSE error: " + e.Raw
}

// IsRetryableStatus reports whether an HTTP status code should be retried.
func IsRetryableStatus(status int) bool {
	switch status {
	case 408, 425, 429, 500, 502, 503, 504:
		return true
	}
	return false
}

// AsHTTPStatusError unwraps err to an *HTTPStatusError, if present.
func AsHTTPStatusError(err error) (*HTTPStatusError, bool) {
	var h *HTTPStatusError
	if errors.As(err, &h) {
		return h, true
	}
	return nil, false
}
