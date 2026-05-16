package synth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/llm"
)

func TestParseExtractionOutput_None(t *testing.T) {
	require.Nil(t, parseExtractionOutput("none"))
	require.Nil(t, parseExtractionOutput("  none  "))
	require.Nil(t, parseExtractionOutput("NONE"))
	require.Nil(t, parseExtractionOutput(""))
	require.Nil(t, parseExtractionOutput("\n  \n"))
}

func TestParseExtractionOutput_SingleFact(t *testing.T) {
	mems := parseExtractionOutput("remember: wife: Pat Morgan, +447700900222, pat.morgan@example.com")
	require.Len(t, mems, 1)
	require.Equal(t, "wife", mems[0].Subject)
	require.Equal(t, "Pat Morgan, +447700900222, pat.morgan@example.com", mems[0].Predicate)
}

func TestParseExtractionOutput_MultipleFacts(t *testing.T) {
	out := `remember: diet: vegetarian
remember: partner: Sam, vegetarian`
	mems := parseExtractionOutput(out)
	require.Len(t, mems, 2)
	require.Equal(t, "diet", mems[0].Subject)
	require.Equal(t, "vegetarian", mems[0].Predicate)
	require.Equal(t, "partner", mems[1].Subject)
}

func TestParseExtractionOutput_StripsCodeFence(t *testing.T) {
	// Some local models wrap the output in ``` even when told not to.
	out := "```\nremember: wife: Pat Morgan\n```"
	mems := parseExtractionOutput(out)
	require.Len(t, mems, 1)
	require.Equal(t, "wife", mems[0].Subject)
}

func TestParseExtractionOutput_IgnoresNonRememberLines(t *testing.T) {
	out := `Sure, here it is:
remember: wife: Pat Morgan
That's the fact.`
	mems := parseExtractionOutput(out)
	require.Len(t, mems, 1)
	require.Equal(t, "wife", mems[0].Subject)
}

func TestParseExtractionOutput_RespectsCap(t *testing.T) {
	out := `remember: a: 1
remember: b: 2
remember: c: 3
remember: d: 4`
	mems := parseExtractionOutput(out)
	require.Len(t, mems, llm.MaxMemoryCandidatesPerResponse)
}

func TestParseExtractionOutput_DropsMalformed(t *testing.T) {
	mems := parseExtractionOutput("remember: just-a-subject")
	require.Empty(t, mems, "no colon after subject → no candidate")
	mems = parseExtractionOutput("remember: : nopredicate")
	require.Empty(t, mems, "empty subject → no candidate")
}

// stubExtractServer returns a fake LLM endpoint that always responds with
// the given content, so the integration test can exercise ExtractFacts
// without a live LLM.
func stubExtractServer(t *testing.T, response string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		resp := map[string]any{
			"id":     "test",
			"object": "chat.completion",
			"model":  "test",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": response},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 5, "completion_tokens": 5, "total_tokens": 10},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestExtractFacts_HappyPath(t *testing.T) {
	srv := stubExtractServer(t, "remember: wife: Pat Morgan, +447700900222, pat.morgan@example.com")
	client := llm.NewClient(llm.ClientConfig{Endpoint: srv.URL, Model: "test"})

	logger := logrus.New()
	logger.Out = io.Discard
	mems := ExtractFacts(context.Background(), client, "My wife is Pat Morgan, +447700900222, pat.morgan@example.com", 0, logger.WithField("c", "test"))
	require.Len(t, mems, 1)
	require.Equal(t, "wife", mems[0].Subject)
	require.Equal(t, "Pat Morgan, +447700900222, pat.morgan@example.com", mems[0].Predicate)
}

func TestExtractFacts_NoneResponse(t *testing.T) {
	srv := stubExtractServer(t, "none")
	client := llm.NewClient(llm.ClientConfig{Endpoint: srv.URL, Model: "test"})

	logger := logrus.New()
	logger.Out = io.Discard
	mems := ExtractFacts(context.Background(), client, "What's the weather?", 0, logger.WithField("c", "test"))
	require.Empty(t, mems)
}

func TestExtractFacts_NilClientSafe(t *testing.T) {
	mems := ExtractFacts(context.Background(), nil, "anything", 0, nil)
	require.Nil(t, mems, "nil client must not panic and must return no candidates")
}

func TestExtractFacts_EmptyQuerySafe(t *testing.T) {
	srv := stubExtractServer(t, "remember: nope: nope")
	client := llm.NewClient(llm.ClientConfig{Endpoint: srv.URL, Model: "test"})

	mems := ExtractFacts(context.Background(), client, "", 0, nil)
	require.Nil(t, mems, "empty query must short-circuit before the LLM call")
}
