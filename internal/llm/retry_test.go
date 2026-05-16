package llm

import (
	"context"
	"errors"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

func TestRetryChat_SucceedsFirstAttempt(t *testing.T) {
	calls := 0
	resp, err := retryChat(context.Background(), RetryPolicy{MaxAttempts: 3, InitialBackoff: time.Millisecond},
		func(_ context.Context) (openai.ChatCompletionResponse, error) {
			calls++
			return openai.ChatCompletionResponse{ID: "ok"}, nil
		})
	require.NoError(t, err)
	require.Equal(t, "ok", resp.ID)
	require.Equal(t, 1, calls)
}

func TestRetryChat_RetriesOn503ThenSucceeds(t *testing.T) {
	calls := 0
	resp, err := retryChat(context.Background(), RetryPolicy{MaxAttempts: 3, InitialBackoff: time.Millisecond},
		func(_ context.Context) (openai.ChatCompletionResponse, error) {
			calls++
			if calls < 3 {
				return openai.ChatCompletionResponse{}, &openai.APIError{HTTPStatusCode: 503, Message: "boom"}
			}
			return openai.ChatCompletionResponse{ID: "ok"}, nil
		})
	require.NoError(t, err)
	require.Equal(t, "ok", resp.ID)
	require.Equal(t, 3, calls)
}

func TestRetryChat_ExhaustsAndWrapsTransient(t *testing.T) {
	calls := 0
	_, err := retryChat(context.Background(), RetryPolicy{MaxAttempts: 2, InitialBackoff: time.Millisecond},
		func(_ context.Context) (openai.ChatCompletionResponse, error) {
			calls++
			return openai.ChatCompletionResponse{}, &openai.APIError{HTTPStatusCode: 502, Message: "still bad"}
		})
	require.Error(t, err)
	require.Equal(t, 2, calls)

	tf, ok := AsTransientFailure(err)
	require.True(t, ok, "expected TransientFailureError, got %T", err)
	require.Equal(t, 2, tf.Attempts)
	require.Equal(t, 502, tf.LastStatus)
}

func TestRetryChat_DoesNotRetryNonRetryable(t *testing.T) {
	calls := 0
	_, err := retryChat(context.Background(), RetryPolicy{MaxAttempts: 3, InitialBackoff: time.Millisecond},
		func(_ context.Context) (openai.ChatCompletionResponse, error) {
			calls++
			return openai.ChatCompletionResponse{}, &openai.APIError{HTTPStatusCode: 400, Message: "bad request"}
		})
	require.Error(t, err)
	require.Equal(t, 1, calls)

	// 400 is not retryable, so we should NOT have wrapped in TransientFailureError.
	_, ok := AsTransientFailure(err)
	require.False(t, ok)
}

func TestRetryChat_AbortsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	calls := 0
	_, err := retryChat(ctx, RetryPolicy{MaxAttempts: 3, InitialBackoff: time.Millisecond},
		func(_ context.Context) (openai.ChatCompletionResponse, error) {
			calls++
			return openai.ChatCompletionResponse{}, errors.New("anything")
		})
	require.Error(t, err)
	require.Equal(t, 1, calls, "should not retry once ctx is cancelled")
}

func TestClassifyForRetry(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		retryable bool
	}{
		{"503", &openai.APIError{HTTPStatusCode: 503}, true},
		{"429", &openai.APIError{HTTPStatusCode: 429}, true},
		{"400", &openai.APIError{HTTPStatusCode: 400}, false},
		{"connection refused", errors.New("dial tcp: connect: connection refused"), true},
		{"random error", errors.New("something else"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := classifyForRetry(tc.err)
			require.Equal(t, tc.retryable, got)
		})
	}
}
