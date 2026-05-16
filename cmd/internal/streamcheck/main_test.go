package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestParsesJSON pins the smoke-test driver's mid-stream-validity
// classifier. Whether each accumulating delta parses determines what
// gets recorded in the smoke-test report's "mid-stream JSON validity"
// field — the load-bearing input to the V2.4 streaming-fallback
// decision in P0.
func TestParsesJSON(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"whitespace_only", "  \n\t ", false},
		{"valid_object", `{"a":1}`, true},
		{"valid_array", `[1,2,3]`, true},
		{"valid_string", `"hello"`, true},
		{"valid_number", `42`, true},
		{"truncated_object_braces", `{"a":1`, false},
		{"truncated_string", `{"a":"hel`, false},
		{"trailing_comma", `{"a":1,}`, false},
		{"with_outer_whitespace", "  {\"a\":1}\n  ", true},
		{"plain_text_not_json", "Hello, world.", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, parsesJSON(tc.in))
		})
	}
}

// TestTruncate pins the report-output truncation behavior. Long bodies
// — especially Config B's text streams — would clutter the report; the
// truncation keeps the doc readable while preserving the load-bearing
// header bytes.
func TestTruncate(t *testing.T) {
	t.Run("under_limit_unchanged", func(t *testing.T) {
		require.Equal(t, "short", truncate("short", 100))
	})
	t.Run("at_limit_unchanged", func(t *testing.T) {
		s := strings.Repeat("a", 100)
		require.Equal(t, s, truncate(s, 100))
	})
	t.Run("over_limit_truncates", func(t *testing.T) {
		s := strings.Repeat("a", 200)
		got := truncate(s, 50)
		require.True(t, strings.HasSuffix(got, "[truncated]"),
			"truncation marker must be visible to readers of the report")
		require.LessOrEqual(t, len(s[:50]), len(got),
			"the kept prefix must be at least the limit")
	})
}

// TestEnvOrDefault pins the BASE_URL / MODEL fallback behavior. The
// driver is run from the CLI by setting these in the environment; an
// unset env var must produce the documented default rather than
// hitting an empty URL.
func TestEnvOrDefault(t *testing.T) {
	const key = "STREAMCHECK_TEST_KEY"

	t.Run("unset_uses_default", func(t *testing.T) {
		t.Setenv(key, "")
		require.Equal(t, "fallback", envOrDefault(key, "fallback"))
	})
	t.Run("set_uses_value", func(t *testing.T) {
		t.Setenv(key, "from-env")
		require.Equal(t, "from-env", envOrDefault(key, "fallback"))
	})
}

// TestRawSchema_MarshalsAsObject pins the rawSchema adapter shape: the
// json.Marshaler interface must produce the underlying schema object,
// not the Go type wrapper. The OpenAI SDK marshals the json_schema
// `Schema` field through this interface.
func TestRawSchema_MarshalsAsObject(t *testing.T) {
	s := rawSchema{
		"type":     "object",
		"required": []string{"title"},
	}
	body, err := s.MarshalJSON()
	require.NoError(t, err)
	require.Contains(t, string(body), `"type":"object"`)
	require.Contains(t, string(body), `"required":["title"]`)
}

// TestReport_StructureMatchesTemplate pins the Markdown report shape so
// the operator can pipe driver output directly into
// doc/v2.4/streaming_smoke_test.md. The header section + per-config
// section + decision-input section are all expected.
func TestReport_StructureMatchesTemplate(t *testing.T) {
	results := []result{
		{
			name:             "A: non-stream + json_schema",
			schema:           true,
			ttfbMs:           1200,
			totalMs:          1200,
			promptTokens:     150,
			completionTokens: 80,
			body:             `{"title":"x","sub":"y"}`,
			finalParsesJSON:  true,
			midStreamJSON:    "n/a",
		},
		{
			name:             "B: stream + no schema",
			stream:           true,
			ttfbMs:           300,
			ttftMs:           450,
			totalMs:          2300,
			promptTokens:     50,
			completionTokens: 120,
			body:             "Streaming text body.",
			midStreamJSON:    "n/a",
		},
		{
			name:             "C: stream + json_schema",
			stream:           true,
			schema:           true,
			ttfbMs:           400,
			ttftMs:           700,
			totalMs:          2700,
			promptTokens:     150,
			completionTokens: 90,
			body:             `{"title":"x","sub":"y"}`,
			finalParsesJSON:  true,
			midStreamJSON:    "yes (every sample parsed)",
		},
	}

	var buf bytes.Buffer
	report(&buf, "http://endpoint", "test-model", results)
	out := buf.String()

	// Header block.
	require.Contains(t, out, "# V2.4.0 — Streaming Smoke Test")
	require.Contains(t, out, "**Endpoint:** http://endpoint")
	require.Contains(t, out, "**Model:** test-model")

	// One section per config.
	require.Contains(t, out, "## A: non-stream + json_schema")
	require.Contains(t, out, "## B: stream + no schema")
	require.Contains(t, out, "## C: stream + json_schema")

	// Streaming TTFT line absent for the non-streaming config; present
	// for the two streaming configs.
	configA := sliceBetween(t, out, "## A: non-stream + json_schema", "## B: stream + no schema")
	require.NotContains(t, configA, "TTFT")
	configB := sliceBetween(t, out, "## B: stream + no schema", "## C: stream + json_schema")
	require.Contains(t, configB, "TTFT (first non-empty delta): 450 ms")
	configC := sliceBetween(t, out, "## C: stream + json_schema", "## Decision input")
	require.Contains(t, configC, "Mid-stream JSON validity: yes (every sample parsed)")

	// Decision-input section is the load-bearing prose for the operator
	// who pastes this into the smoke-test doc. Pin its presence.
	require.Contains(t, out, "## Decision input")
	require.Contains(t, out, "stream_schema = true")
	require.Contains(t, out, "stream_schema = false")
}

// TestReport_HandlesError pins that a config with err != "" still
// renders cleanly — the operator should see the error in the report
// rather than a corrupt document.
func TestReport_HandlesError(t *testing.T) {
	results := []result{{
		name: "C: stream + json_schema",
		err:  "model rejected stream + json_schema",
	}}
	var buf bytes.Buffer
	report(&buf, "x", "y", results)
	out := buf.String()
	require.Contains(t, out, "**ERROR:** model rejected stream + json_schema")
	require.Contains(t, out, "## C: stream + json_schema")
}

// sliceBetween returns the substring of haystack between two markers
// (exclusive of the markers themselves). Used to pin per-section
// content without coupling to the exact byte offsets.
func sliceBetween(t *testing.T, haystack, start, end string) string {
	t.Helper()
	si := strings.Index(haystack, start)
	require.GreaterOrEqualf(t, si, 0, "start marker %q not found", start)
	rest := haystack[si+len(start):]
	ei := strings.Index(rest, end)
	require.GreaterOrEqualf(t, ei, 0, "end marker %q not found", end)
	return rest[:ei]
}
