package gemini

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/genai"

	"github.com/zenocy/zeno-v2/internal/llm"
)

func TestConvertMessages_SystemInstructionRouting(t *testing.T) {
	msgs := []llm.Message{
		{Role: "system", Content: "you are a helpful assistant"},
		{Role: "user", Content: "hi"},
	}
	sys, contents, _ := convertMessages(msgs)
	require.NotNil(t, sys, "first system message must become systemInstruction")
	require.Len(t, sys.Parts, 1)
	require.Equal(t, "you are a helpful assistant", sys.Parts[0].Text)
	require.Len(t, contents, 1)
	require.Equal(t, string(genai.RoleUser), contents[0].Role)
}

func TestConvertMessages_MidConversationSystemBecomesUser(t *testing.T) {
	// The loop appends a system "Iteration cap reached" reminder
	// mid-conversation (loop.go:348). Gemini drops mid-conversation
	// system parts; we re-route to user role with a prefix so they
	// remain authoritative.
	msgs := []llm.Message{
		{Role: "system", Content: "system one"},
		{Role: "user", Content: "first turn"},
		{Role: "assistant", Content: "ok"},
		{Role: "system", Content: "Iteration cap reached"},
	}
	sys, contents, _ := convertMessages(msgs)
	require.NotNil(t, sys, "first system instruction preserved")
	require.Len(t, contents, 3)
	require.Equal(t, string(genai.RoleUser), contents[2].Role,
		"mid-conversation system message must be re-mapped to user role")
	require.Equal(t, "[SYSTEM]: Iteration cap reached", contents[2].Parts[0].Text)
}

func TestConvertMessages_MultiSegmentSystemConcatenated(t *testing.T) {
	// Some briefings (e.g. via NoMerge) accumulate multiple leading
	// system messages. They should all land in systemInstruction.
	msgs := []llm.Message{
		{Role: "system", Content: "voice prefix"},
		{Role: "system", Content: "voice register"},
		{Role: "user", Content: "ok"},
	}
	sys, _, _ := convertMessages(msgs)
	require.NotNil(t, sys)
	require.Len(t, sys.Parts, 2,
		"every leading system message lands as its own systemInstruction part")
	require.Equal(t, "voice prefix", sys.Parts[0].Text)
	require.Equal(t, "voice register", sys.Parts[1].Text)
}

func TestConvertMessages_AssistantEmptyContentWithToolCalls(t *testing.T) {
	// loop.go:261 emits {Role:"assistant", ToolCalls:cr.ToolCalls} with
	// no Content. Gemini rejects content with zero parts, so the
	// converter must emit ONLY FunctionCall parts (no empty TextPart).
	msgs := []llm.Message{
		{Role: "user", Content: "query"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: "call_1", Name: "read_thread", Arguments: map[string]any{"id": "T1"}},
		}},
	}
	_, contents, _ := convertMessages(msgs)
	require.Len(t, contents, 2)
	asst := contents[1]
	require.Equal(t, string(genai.RoleModel), asst.Role)
	require.Len(t, asst.Parts, 1, "no empty text part should be emitted alongside the tool call")
	require.Equal(t, "", asst.Parts[0].Text)
	require.NotNil(t, asst.Parts[0].FunctionCall)
	require.Equal(t, "read_thread", asst.Parts[0].FunctionCall.Name)
}

func TestConvertMessages_AmbiguousToolCallWarning(t *testing.T) {
	// Two unresolved tool calls with the same name in one assistant
	// turn — Gemini's FunctionResponse matches by name + positional
	// order, so we warn the operator.
	msgs := []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: "call_1", Name: "read_thread", Arguments: map[string]any{"id": "T1"}},
			{ID: "call_2", Name: "read_thread", Arguments: map[string]any{"id": "T2"}},
		}},
	}
	_, _, warnings := convertMessages(msgs)
	require.NotEmpty(t, warnings)
	require.Contains(t, warnings[0], "read_thread")
}

func TestConvertMessages_ToolResultBecomesUserFunctionResponse(t *testing.T) {
	msgs := []llm.Message{
		{Role: "tool", ToolCallID: "call_read_thread_1", Name: "read_thread", Content: "thread body"},
	}
	_, contents, _ := convertMessages(msgs)
	require.Len(t, contents, 1)
	require.Equal(t, string(genai.RoleUser), contents[0].Role)
	fr := contents[0].Parts[0].FunctionResponse
	require.NotNil(t, fr)
	require.Equal(t, "read_thread", fr.Name,
		"FunctionResponse.Name must come from Message.Name verbatim — Gemini correlates by name")
	require.Equal(t, "call_read_thread_1", fr.ID,
		"ToolCallID should also be forwarded as FunctionResponse.ID")
}

func TestConvertMessages_ToolResultRoundTripsSameNameCorrelation(t *testing.T) {
	// Regression for the read_thread/read_tasks correlation bug: when the
	// model emits multiple FunctionCalls with the same name in one turn,
	// the FunctionResponse parts the next turn must also carry that
	// exact name. Earlier code derived the name from a synthesized
	// ToolCallID via prefix-stripping, which produced "read_thread_0"
	// from "call_read_thread_0" — a name Gemini doesn't recognize, so
	// the responses were dropped and the model re-issued the same calls.
	msgs := []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: "call_read_thread_0", Name: "read_thread", Arguments: map[string]any{"id": "T1"}},
			{ID: "call_read_thread_1", Name: "read_thread", Arguments: map[string]any{"id": "T2"}},
		}},
		{Role: "tool", ToolCallID: "call_read_thread_0", Name: "read_thread", Content: "body 1"},
		{Role: "tool", ToolCallID: "call_read_thread_1", Name: "read_thread", Content: "body 2"},
	}
	_, contents, _ := convertMessages(msgs)
	require.Len(t, contents, 3)

	asstParts := contents[0].Parts
	require.Len(t, asstParts, 2)
	require.Equal(t, "read_thread", asstParts[0].FunctionCall.Name)
	require.Equal(t, "read_thread", asstParts[1].FunctionCall.Name)

	resp1 := contents[1].Parts[0].FunctionResponse
	resp2 := contents[2].Parts[0].FunctionResponse
	require.Equal(t, "read_thread", resp1.Name)
	require.Equal(t, "read_thread", resp2.Name)
}

func TestConvertMessages_ToolResultFallsBackWhenNameMissing(t *testing.T) {
	// Backward compat: a tool message without Name still produces a
	// best-effort FunctionResponse via the toolNameFromID shim.
	msgs := []llm.Message{
		{Role: "tool", ToolCallID: "call_legacy_id", Content: "result"},
	}
	_, contents, _ := convertMessages(msgs)
	require.Len(t, contents, 1)
	fr := contents[0].Parts[0].FunctionResponse
	require.NotNil(t, fr)
	require.Equal(t, "legacy_id", fr.Name,
		"fallback must strip the call_ prefix when Name is absent")
}

func TestConvertResult_BasicResponse(t *testing.T) {
	resp := &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{
			FinishReason: genai.FinishReasonStop,
			Content: &genai.Content{
				Parts: []*genai.Part{
					{Text: "hello world"},
				},
			},
		}},
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     12,
			CandidatesTokenCount: 4,
		},
	}
	out := convertResult(resp)
	require.Equal(t, "hello world", out.Content)
	require.Equal(t, 12, out.PromptTokens)
	require.Equal(t, 4, out.CompletionTokens)
	require.False(t, out.Empty)
}

func TestConvertResult_ThoughtsRoutedSeparately(t *testing.T) {
	resp := &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{
			Content: &genai.Content{
				Parts: []*genai.Part{
					{Text: "thinking through this", Thought: true},
					{Text: "answer text"},
				},
			},
		}},
	}
	out := convertResult(resp)
	require.Equal(t, "thinking through this", out.ThinkingContent)
	require.Equal(t, "answer text", out.Content)
}

func TestThoughtSignatureRoundTrip(t *testing.T) {
	// The Gemini API rejects (HTTP 400 INVALID_ARGUMENT) function-call
	// echo-backs that drop the thought_signature emitted alongside the
	// original FunctionCall. The full round-trip must preserve those
	// bytes verbatim: extract response → store on ToolCall → re-emit on
	// the next turn's assistant message. This test exercises both legs.
	sig := []byte{0x01, 0x02, 0xde, 0xad, 0xbe, 0xef}

	// Leg 1: response with a FunctionCall part carrying a signature.
	resp := &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{
			Content: &genai.Content{
				Parts: []*genai.Part{
					{
						FunctionCall: &genai.FunctionCall{
							ID:   "call_search_web_0",
							Name: "search_web",
							Args: map[string]any{"q": "google announcements"},
						},
						ThoughtSignature: sig,
					},
				},
			},
		}},
	}
	out := convertResult(resp)
	require.Len(t, out.ToolCalls, 1)
	require.Equal(t, sig, out.ToolCalls[0].ProviderState,
		"thought signature must be preserved on the extracted ToolCall")

	// Leg 2: feed that ToolCall back through convertMessages and
	// confirm the signature lands on the outbound Part.
	msgs := []llm.Message{
		{Role: "assistant", ToolCalls: out.ToolCalls},
	}
	_, contents, _ := convertMessages(msgs)
	require.Len(t, contents, 1)
	require.Len(t, contents[0].Parts, 1)
	require.Equal(t, sig, contents[0].Parts[0].ThoughtSignature,
		"echo-back FunctionCall part must carry the original thought signature")
	require.NotNil(t, contents[0].Parts[0].FunctionCall)
	require.Equal(t, "search_web", contents[0].Parts[0].FunctionCall.Name)
}

func TestConvertResult_ToolCallsExtracted(t *testing.T) {
	resp := &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{
			Content: &genai.Content{
				Parts: []*genai.Part{
					{FunctionCall: &genai.FunctionCall{
						Name: "read_event",
						Args: map[string]any{"uid": "E1"},
					}},
				},
			},
		}},
	}
	out := convertResult(resp)
	require.Len(t, out.ToolCalls, 1)
	require.Equal(t, "read_event", out.ToolCalls[0].Name)
	require.Equal(t, "E1", out.ToolCalls[0].Arguments["uid"])
	require.NotEmpty(t, out.ToolCalls[0].ID, "missing ID must be synthesized")
}

func TestConvertResult_EmptyWhenNoContentNoToolCalls(t *testing.T) {
	resp := &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{
			FinishReason: genai.FinishReasonStop,
			Content:      &genai.Content{Parts: []*genai.Part{}},
		}},
	}
	out := convertResult(resp)
	require.True(t, out.Empty)
}

func TestConvertResult_SafetyFinishNotEmpty(t *testing.T) {
	// Safety refusal is deliberate, not transient — must NOT be flagged
	// Empty (which would justify a retry).
	resp := &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{
			FinishReason: genai.FinishReasonSafety,
			Content:      &genai.Content{Parts: []*genai.Part{}},
		}},
	}
	out := convertResult(resp)
	require.False(t, out.Empty, "SAFETY finish must not be flagged Empty")
	require.Equal(t, string(genai.FinishReasonSafety), out.FinishReason)
}

func TestSchemaToGemini_BasicObjectShape(t *testing.T) {
	in := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"value": map[string]any{"type": "integer", "minimum": 0.0, "maximum": 3.0},
			"notes": map[string]any{"type": "string"},
		},
		"required": []any{"value"},
	}
	s, err := schemaToGemini(in)
	require.NoError(t, err)
	require.Equal(t, genai.TypeObject, s.Type)
	require.Contains(t, s.Properties, "value")
	require.Equal(t, genai.TypeInteger, s.Properties["value"].Type)
	require.NotNil(t, s.Properties["value"].Minimum)
	require.Equal(t, []string{"value"}, s.Required)
}

func TestSchemaToGemini_RejectsRef(t *testing.T) {
	in := map[string]any{
		"$ref": "#/definitions/foo",
	}
	_, err := schemaToGemini(in)
	require.Error(t, err)
	require.Contains(t, err.Error(), "$ref")
}

func TestSchemaToGemini_RejectsAllOf(t *testing.T) {
	in := map[string]any{"allOf": []any{}}
	_, err := schemaToGemini(in)
	require.Error(t, err)
	require.Contains(t, err.Error(), "allOf")
}

func TestSchemaToGemini_EnumPreserved(t *testing.T) {
	in := map[string]any{
		"type": "string",
		"enum": []any{"low", "medium", "high"},
	}
	s, err := schemaToGemini(in)
	require.NoError(t, err)
	require.Equal(t, []string{"low", "medium", "high"}, s.Enum)
}

func TestIsSafetyFinish(t *testing.T) {
	cases := []struct {
		reason genai.FinishReason
		want   bool
	}{
		{genai.FinishReasonStop, false},
		{genai.FinishReasonMaxTokens, false},
		{genai.FinishReasonSafety, true},
		{genai.FinishReasonProhibitedContent, true},
		{genai.FinishReasonBlocklist, true},
		{genai.FinishReasonSPII, true},
		{genai.FinishReasonRecitation, true},
	}
	for _, tc := range cases {
		t.Run(string(tc.reason), func(t *testing.T) {
			require.Equal(t, tc.want, isSafetyFinish(tc.reason))
		})
	}
}

func TestExtractCitations_ChunksOnly(t *testing.T) {
	meta := &genai.GroundingMetadata{
		GroundingChunks: []*genai.GroundingChunk{
			{Web: &genai.GroundingChunkWeb{Title: "Title A", URI: "https://a.example"}},
			{Web: &genai.GroundingChunkWeb{Title: "Title B", URI: "https://b.example"}},
		},
	}
	cits := extractCitations(meta)
	require.Len(t, cits, 2)
	require.Equal(t, "Title A", cits[0].Title)
	require.Equal(t, "https://a.example", cits[0].URI)
	require.Equal(t, 0, cits[0].StartIndex)
}

func TestExtractCitations_WithSupports(t *testing.T) {
	meta := &genai.GroundingMetadata{
		GroundingChunks: []*genai.GroundingChunk{
			{Web: &genai.GroundingChunkWeb{Title: "T", URI: "https://e.example"}},
		},
		GroundingSupports: []*genai.GroundingSupport{
			{
				GroundingChunkIndices: []int32{0},
				Segment: &genai.Segment{
					StartIndex: 5,
					EndIndex:   42,
				},
			},
		},
	}
	cits := extractCitations(meta)
	require.Len(t, cits, 1)
	require.Equal(t, 5, cits[0].StartIndex)
	require.Equal(t, 42, cits[0].EndIndex)
}

func TestMapServiceTier(t *testing.T) {
	cases := []struct {
		in   string
		want genai.ServiceTier
		ok   bool
	}{
		{"flex", genai.ServiceTierFlex, true},
		{"standard", genai.ServiceTierStandard, true},
		{"priority", genai.ServiceTierPriority, true},
		{"FLEX", genai.ServiceTierFlex, true}, // case-insensitive
		{"", "", false},
		{"turbo", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := mapServiceTier(tc.in)
			require.Equal(t, tc.ok, ok)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestMapThinkingLevel(t *testing.T) {
	cases := []struct {
		in   string
		want genai.ThinkingLevel
		ok   bool
	}{
		{"minimal", genai.ThinkingLevelMinimal, true},
		{"low", genai.ThinkingLevelLow, true},
		{"medium", genai.ThinkingLevelMedium, true},
		{"high", genai.ThinkingLevelHigh, true},
		{"HIGH", genai.ThinkingLevelHigh, true}, // case-insensitive
		{"", "", false},
		{"turbo", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := mapThinkingLevel(tc.in)
			require.Equal(t, tc.ok, ok)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestJSONTypeToGenAI(t *testing.T) {
	cases := map[string]genai.Type{
		"string":  genai.TypeString,
		"integer": genai.TypeInteger,
		"number":  genai.TypeNumber,
		"boolean": genai.TypeBoolean,
		"object":  genai.TypeObject,
		"array":   genai.TypeArray,
		"null":    genai.TypeNULL,
		"":        genai.TypeUnspecified,
		"foo":     genai.TypeUnspecified,
	}
	for in, want := range cases {
		require.Equal(t, want, jsonTypeToGenAI(in), "input: %q", in)
	}
}
