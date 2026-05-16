package eval

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/llm"
)

// fakeJudgeClient is a stub implementing JudgeClient for hermetic tests.
// content is what ChatCompletion returns; err takes precedence when set.
type fakeJudgeClient struct {
	content string
	err     error
	calls   int
}

func (f *fakeJudgeClient) ChatCompletion(
	_ context.Context,
	_ []llm.Message,
	_ []llm.ToolDefinition,
	_ ...llm.ChatOption,
) (llm.ChatResult, error) {
	f.calls++
	if f.err != nil {
		return llm.ChatResult{}, f.err
	}
	return llm.ChatResult{Content: f.content}, nil
}

// TestLLMJudge_NilClient_ReturnsSentinel verifies the offline path: a nil
// JudgeClient yields Score{Value:-1} so report tooling can render "—".
// This is the contract the V2.2 stub semantics need to keep.
func TestLLMJudge_NilClient_ReturnsSentinel(t *testing.T) {
	score, err := LLMJudge(context.Background(), nil, JudgeRequest{
		Dimension: "concern_voice",
		System:    "x",
		User:      "y",
	})
	require.NoError(t, err)
	require.Equal(t, "concern_voice", score.Dimension)
	require.Equal(t, -1, score.Value)
}

func TestLLMJudge_ParsesValidResponse(t *testing.T) {
	cli := &fakeJudgeClient{content: `{"value": 2, "notes": "decent voice"}`}
	score, err := LLMJudge(context.Background(), cli, JudgeRequest{
		Dimension: "concern_voice",
		System:    "system",
		User:      "user",
	})
	require.NoError(t, err)
	require.Equal(t, 2, score.Value)
	require.Equal(t, []string{"decent voice"}, score.Hits)
	require.Equal(t, 1, cli.calls)
}

// TestLLMJudge_HandlesMalformedJSON confirms parse failures surface a
// sentinel + the wrapped parse error. Crucial: callers must be able to
// distinguish "judge unavailable" from "judge returned junk."
func TestLLMJudge_HandlesMalformedJSON(t *testing.T) {
	cli := &fakeJudgeClient{content: `not json`}
	score, err := LLMJudge(context.Background(), cli, JudgeRequest{
		Dimension: "concern_voice",
		System:    "system",
		User:      "user",
	})
	require.Error(t, err)
	require.Equal(t, -1, score.Value)
}

func TestLLMJudge_RejectsOutOfRange(t *testing.T) {
	cli := &fakeJudgeClient{content: `{"value": 7, "notes": "way off"}`}
	score, err := LLMJudge(context.Background(), cli, JudgeRequest{
		Dimension: "concern_voice",
		System:    "system",
		User:      "user",
	})
	require.Error(t, err)
	require.Equal(t, -1, score.Value)
}

func TestLLMJudge_PropagatesClientError(t *testing.T) {
	cli := &fakeJudgeClient{err: errors.New("network down")}
	score, err := LLMJudge(context.Background(), cli, JudgeRequest{
		Dimension: "concern_voice",
		System:    "system",
		User:      "user",
	})
	require.Error(t, err)
	require.Equal(t, -1, score.Value)
}

func TestLLMJudge_RejectsEmptyPrompt(t *testing.T) {
	cli := &fakeJudgeClient{content: `{"value":1,"notes":"x"}`}
	_, err := LLMJudge(context.Background(), cli, JudgeRequest{
		Dimension: "concern_voice",
		System:    "",
		User:      "y",
	})
	require.Error(t, err)
	require.Equal(t, 0, cli.calls, "must reject before calling client")
}

// TestLLMJudgeMemoryGrounding_SentinelOnNilClient pins the V2.2 offline
// behavior: callers can pass nil and get the same sentinel as the old
// stubLLMJudgeMemoryGrounding helper.
func TestLLMJudgeMemoryGrounding_SentinelOnNilClient(t *testing.T) {
	score, err := LLMJudgeMemoryGrounding(context.Background(), nil, "with", "without")
	require.NoError(t, err)
	require.Equal(t, "memory_with_vs_empty", score.Dimension)
	require.Equal(t, -1, score.Value)
}

func TestLLMJudgeConcernRecognition_RoutesThroughGenericWrapper(t *testing.T) {
	cli := &fakeJudgeClient{content: `{"value": 3, "notes": "matches"}`}
	score, err := LLMJudgeConcernRecognition(context.Background(), cli,
		[]string{"Construction at the house — kitchen tile pending"},
		[]string{"Construction at the house"})
	require.NoError(t, err)
	require.Equal(t, "concern_recognition", score.Dimension)
	require.Equal(t, 3, score.Value)
}

func TestLLMJudgeConcernVoice_RoutesThroughGenericWrapper(t *testing.T) {
	cli := &fakeJudgeClient{content: `{"value": 2, "notes": "calm"}`}
	score, err := LLMJudgeConcernVoice(context.Background(), cli,
		"Construction at the house",
		"Kitchen tile is the open question; contractor's update lands Thursday.")
	require.NoError(t, err)
	require.Equal(t, "concern_voice", score.Dimension)
	require.Equal(t, 2, score.Value)
}

// TestStubLLMJudgeMemoryGrounding_RetainsV22Sentinel keeps the historical
// helper alive — eval/scoring.go callers may still use it directly.
func TestStubLLMJudgeMemoryGrounding_RetainsV22Sentinel(t *testing.T) {
	score := stubLLMJudgeMemoryGrounding()
	require.Equal(t, "memory_with_vs_empty", score.Dimension)
	require.Equal(t, -1, score.Value)
}
