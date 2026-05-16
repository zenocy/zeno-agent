package llm

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStripThinkTags(t *testing.T) {
	cases := map[string]string{
		"":                                      "",
		"plain text":                            "plain text",
		"<think>reasoning</think>answer":        "answer",
		"<think>a</think>{\"x\":1}":             `{"x":1}`,
		"prefix<think>r</think>suffix":          "prefixsuffix",
		"<think>x</think><think>y</think>final": "final",
		"  <think>x</think>  json  ":            "json",
		"no tags here":                          "no tags here",
		"<think>only thinking, no answer":       "only thinking, no answer",
		"answer<think>x</think>more answer":     "answermore answer",
	}
	for input, want := range cases {
		t.Run(input, func(t *testing.T) {
			got := stripThinkTags(input)
			require.Equal(t, want, got)
		})
	}
}

func TestExtractThinkBlocks(t *testing.T) {
	cases := map[string]string{
		"":                                  "",
		"plain text":                        "",
		"<think>just thinking</think>":      "just thinking",
		"<think>a</think><think>b</think>":  "a\nb",
		"prefix<think>middle</think>suffix": "middle",
		"<think>unclosed":                   "unclosed",
		"<think>  spaced  </think>":         "spaced",
	}
	for input, want := range cases {
		t.Run(input, func(t *testing.T) {
			require.Equal(t, want, extractThinkBlocks(input))
		})
	}
}

func TestCollectThinking(t *testing.T) {
	cases := []struct {
		name string
		in   rawMessage
		want string
	}{
		{
			"think_in_content",
			rawMessage{Content: "<think>cyprus</think>{\"x\":1}"},
			"cyprus",
		},
		{
			"think_in_reasoning_content",
			rawMessage{ReasoningContent: "<think>asia</think>{\"x\":1}"},
			"asia",
		},
		{
			"reasoning_content_with_no_think_tags",
			rawMessage{ReasoningContent: "The user wants foo."},
			"The user wants foo.",
		},
		{
			"both_have_think_first_wins",
			rawMessage{Content: "<think>first</think>x", ReasoningContent: "<think>second</think>y"},
			"first",
		},
		{
			"empty_when_no_thinking",
			rawMessage{Content: "{\"x\":1}"},
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, collectThinking(tc.in))
		})
	}
}

func TestSummarizeThinking(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"single_sentence", "The user is asking about Cyprus.", "The user is asking about Cyprus."},
		{"multi_sentence", "The user is asking about Cyprus. The president is X. Done.", "The user is asking about Cyprus."},
		{"newline_break", "First line.\nSecond line.", "First line."},
		{"long_text_truncates", strings.Repeat("a ", 200), strings.Repeat("a ", 100) + "…"},
		{"trim_whitespace", "   leading and trailing.   ", "leading and trailing."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, summarizeThinking(tc.in))
		})
	}
}

func TestPickContentField(t *testing.T) {
	cases := []struct {
		name string
		in   rawMessage
		want string
	}{
		{
			"content_only",
			rawMessage{Content: `{"x":1}`},
			`{"x":1}`,
		},
		{
			"reasoning_content_fallback",
			rawMessage{Content: "", ReasoningContent: `{"y":2}`},
			`{"y":2}`,
		},
		{
			"reasoning_content_with_think_block",
			rawMessage{Content: "", ReasoningContent: `<think>plan</think>{"y":2}`},
			`{"y":2}`,
		},
		{
			"reasoning_field_fallback",
			rawMessage{Content: "", ReasoningContent: "", Reasoning: `{"z":3}`},
			`{"z":3}`,
		},
		{
			"content_takes_precedence_when_both_set",
			rawMessage{Content: `{"a":1}`, ReasoningContent: `{"b":2}`},
			`{"a":1}`,
		},
		{
			"empty_when_all_blank",
			rawMessage{},
			"",
		},
		{
			"think_block_in_content",
			rawMessage{Content: `<think>thinking</think>final answer`},
			"final answer",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, pickContentField(tc.in))
		})
	}
}
