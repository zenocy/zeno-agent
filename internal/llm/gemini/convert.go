package gemini

import (
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/genai"

	"github.com/zenocy/zeno-v2/internal/llm"
)

// convertMessages translates the provider-agnostic []llm.Message into
// Gemini's two-channel shape: a single systemInstruction Content (when
// the first message is a system message) and a list of conversation
// contents alternating between "user" and "model" roles.
//
// Edge cases the converter handles explicitly:
//
//   - The first system message becomes systemInstruction.
//   - Any subsequent system message (e.g. the loop's "Iteration cap
//     reached" reminder at loop.go:348) is re-mapped to a user-role
//     content prefixed with "[SYSTEM]: " — Gemini drops mid-conversation
//     system parts silently otherwise.
//   - An assistant message carrying tool_calls but empty content emits
//     only FunctionCall parts (no empty TextPart), since Gemini
//     rejects content with zero parts.
//   - A tool result message (Role=="tool") becomes a user-role content
//     with FunctionResponse parts. ToolCallID is discarded — Gemini
//     correlates by Name + positional order within the turn.
func convertMessages(in []llm.Message) (system *genai.Content, contents []*genai.Content, warnings []string) {
	contents = make([]*genai.Content, 0, len(in))

	sysParts := []*genai.Part{}
	sawNonSystem := false

	for _, m := range in {
		switch m.Role {
		case "system":
			if !sawNonSystem {
				// Pre-conversation system block — accumulate into
				// systemInstruction.
				if t := strings.TrimSpace(m.Content); t != "" {
					sysParts = append(sysParts, genai.NewPartFromText(t))
				}
				continue
			}
			// Mid-conversation system message: route to user role with
			// a [SYSTEM]: prefix so Gemini honors it as authoritative
			// guidance instead of silently dropping it.
			if t := strings.TrimSpace(m.Content); t != "" {
				contents = append(contents, &genai.Content{
					Role:  string(genai.RoleUser),
					Parts: []*genai.Part{genai.NewPartFromText("[SYSTEM]: " + t)},
				})
			}
		case "user":
			sawNonSystem = true
			parts := []*genai.Part{}
			if m.Content != "" {
				parts = append(parts, genai.NewPartFromText(m.Content))
			}
			if len(parts) > 0 {
				contents = append(contents, &genai.Content{
					Role:  string(genai.RoleUser),
					Parts: parts,
				})
			}
		case "assistant":
			sawNonSystem = true
			parts := []*genai.Part{}
			if m.Content != "" {
				parts = append(parts, genai.NewPartFromText(m.Content))
			}
			// Track tool-call names per turn to detect ambiguous
			// correlation (multiple unresolved calls with the same name
			// in one assistant turn — Gemini matches FunctionResponse by
			// Name, not ID, so this is potentially lossy).
			seenNames := map[string]int{}
			for _, tc := range m.ToolCalls {
				args := tc.Arguments
				if args == nil {
					args = map[string]any{}
				}
				// Build the Part manually instead of using
				// NewPartFromFunctionCall so we can reattach the
				// thought signature the model emitted on the previous
				// turn. Gemini rejects FunctionCall echo-backs that
				// drop the signature when thinking is enabled.
				parts = append(parts, &genai.Part{
					FunctionCall: &genai.FunctionCall{
						ID:   tc.ID,
						Name: tc.Name,
						Args: args,
					},
					ThoughtSignature: tc.ProviderState,
				})
				seenNames[tc.Name]++
			}
			for name, n := range seenNames {
				if n > 1 {
					warnings = append(warnings,
						fmt.Sprintf("multiple tool calls with name %q in one assistant turn; Gemini correlates by name+position", name))
				}
			}
			if len(parts) == 0 {
				continue
			}
			contents = append(contents, &genai.Content{
				Role:  string(genai.RoleModel),
				Parts: parts,
			})
		case "tool":
			sawNonSystem = true
			// Gemini expects FunctionResponse parts carrying a JSON-
			// shaped response. The internal Message.Content carries the
			// raw stringified tool output, which we wrap into {"result":
			// ...} so downstream models can read it without grappling
			// with shape ambiguity. Lossy on ToolCallID — Gemini
			// correlates by name + position.
			resp := map[string]any{"result": m.Content}
			parts := []*genai.Part{
				genai.NewPartFromFunctionResponse(toolNameFromID(m.ToolCallID), resp),
			}
			contents = append(contents, &genai.Content{
				Role:  string(genai.RoleUser),
				Parts: parts,
			})
		}
	}

	if len(sysParts) > 0 {
		system = &genai.Content{Parts: sysParts}
	}
	return system, contents, warnings
}

// toolNameFromID extracts a tool name when the loop synthesizes a tool
// message. The loop's tool-result messages set ToolCallID; the name
// itself isn't on the message because the OpenAI wire format ties
// responses to calls via ID. For Gemini we lift the name back out of
// the call site: the loop sets ToolCallID to a deterministic prefix
// (e.g. "call_abc"), but the actual function name is what Gemini
// needs. The caller wires this via the FunctionResponse part name; in
// our case we fall back to "tool_response" when the ID alone is
// unparseable. This is a known limitation — callers can mitigate by
// passing the tool name in ToolCallID itself, but the cleaner long-
// term fix is to add a ToolName field to llm.Message.
func toolNameFromID(id string) string {
	if id == "" {
		return "tool_response"
	}
	// IDs from sashabaranov often look like "call_<random>". Strip the
	// "call_" prefix when present so the function name reads naturally
	// in trace surfaces; otherwise return the ID verbatim.
	if strings.HasPrefix(id, "call_") {
		return id[len("call_"):]
	}
	return id
}

// convertTools translates []llm.ToolDefinition into the Gemini Tool
// shape (a single Tool with multiple FunctionDeclarations).
//
// When the chatOpts request Google Search grounding, an additional
// Tool with GoogleSearch set is appended. Gemini supports mixing
// google_search with function declarations within the same request.
func convertTools(defs []llm.ToolDefinition, enableGoogleSearch bool) []*genai.Tool {
	out := []*genai.Tool{}

	if len(defs) > 0 {
		funcs := make([]*genai.FunctionDeclaration, 0, len(defs))
		for _, d := range defs {
			funcs = append(funcs, &genai.FunctionDeclaration{
				Name:        d.Name,
				Description: d.Description,
				Parameters:  paramsToGeminiSchema(d.Parameters),
			})
		}
		out = append(out, &genai.Tool{FunctionDeclarations: funcs})
	}

	if enableGoogleSearch {
		out = append(out, &genai.Tool{GoogleSearch: &genai.GoogleSearch{}})
	}
	return out
}

// paramsToGeminiSchema builds the Schema object the
// FunctionDeclaration.Parameters slot expects from a flat list of
// ToolParamSpec. Mirrors the OpenAI client's paramsToSchema but
// produces *genai.Schema instead of map[string]any.
func paramsToGeminiSchema(params []llm.ToolParamSpec) *genai.Schema {
	props := map[string]*genai.Schema{}
	required := []string{}
	for _, p := range params {
		s := &genai.Schema{}
		if p.Type != "" {
			s.Type = jsonTypeToGenAI(p.Type)
		}
		if p.Description != "" {
			s.Description = p.Description
		}
		if len(p.Enum) > 0 {
			s.Enum = append([]string{}, p.Enum...)
		}
		if p.Items != nil {
			s.Items = &genai.Schema{Type: jsonTypeToGenAI(p.Items.Type)}
		}
		props[p.Name] = s
		if p.Required {
			required = append(required, p.Name)
		}
	}
	out := &genai.Schema{
		Type:       genai.TypeObject,
		Properties: props,
	}
	if len(required) > 0 {
		out.Required = required
	}
	return out
}

// jsonTypeToGenAI maps the lowercase JSON-Schema type strings the
// internal types use into the uppercase Gemini enum. An unrecognized
// type maps to TypeUnspecified so the SDK can surface a clearer error
// at request time than a silent rejection would.
func jsonTypeToGenAI(t string) genai.Type {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "string":
		return genai.TypeString
	case "integer":
		return genai.TypeInteger
	case "number":
		return genai.TypeNumber
	case "boolean":
		return genai.TypeBoolean
	case "object":
		return genai.TypeObject
	case "array":
		return genai.TypeArray
	case "null":
		return genai.TypeNULL
	default:
		return genai.TypeUnspecified
	}
}

// convertResult assembles the provider-agnostic ChatResult from a
// non-streaming Gemini GenerateContentResponse. Streaming follows a
// different path in stream.go but the per-candidate extraction logic
// (text/thoughts/tool-calls/citations/finish-reason) is shared via
// extractCandidate.
func convertResult(resp *genai.GenerateContentResponse) llm.ChatResult {
	out := llm.ChatResult{}
	if resp == nil {
		out.Empty = true
		return out
	}
	if resp.UsageMetadata != nil {
		out.PromptTokens = int(resp.UsageMetadata.PromptTokenCount)
		out.CachedTokens = int(resp.UsageMetadata.CachedContentTokenCount)
		out.CompletionTokens = int(resp.UsageMetadata.CandidatesTokenCount)
	}
	if len(resp.Candidates) == 0 {
		out.Empty = true
		return out
	}
	cand := resp.Candidates[0]
	out.FinishReason = string(cand.FinishReason)
	extractCandidate(cand, &out)
	if out.Content == "" && len(out.ToolCalls) == 0 && len(out.ToolArgsErrors) == 0 && !isSafetyFinish(cand.FinishReason) {
		out.Empty = true
	}
	return out
}

// extractCandidate populates Content / ThinkingContent / ToolCalls /
// Citations from a single Candidate. Shared by the streaming and
// non-streaming paths.
func extractCandidate(cand *genai.Candidate, out *llm.ChatResult) {
	if cand == nil || cand.Content == nil {
		return
	}
	var contentBuilder, thinkBuilder strings.Builder
	for _, p := range cand.Content.Parts {
		if p == nil {
			continue
		}
		if p.FunctionCall != nil {
			args := p.FunctionCall.Args
			if args == nil {
				args = map[string]any{}
			}
			id := p.FunctionCall.ID
			if id == "" {
				// Gemini doesn't always populate ID; synthesize one
				// from the function name + ordinal so the tool loop's
				// repair tracker has a unique key.
				id = fmt.Sprintf("call_%s_%d", p.FunctionCall.Name, len(out.ToolCalls))
			}
			out.ToolCalls = append(out.ToolCalls, llm.ToolCall{
				ID:        id,
				Name:      p.FunctionCall.Name,
				Arguments: args,
				// Preserve the thought signature so the next turn's
				// echo-back can reattach it. Gemini rejects FunctionCall
				// parts that are missing their signature when thinking
				// is on (https://ai.google.dev/gemini-api/docs/thought-signatures).
				ProviderState: p.ThoughtSignature,
			})
			continue
		}
		if p.Thought {
			if p.Text != "" {
				if thinkBuilder.Len() > 0 {
					thinkBuilder.WriteByte('\n')
				}
				thinkBuilder.WriteString(p.Text)
			}
			continue
		}
		if p.Text != "" {
			contentBuilder.WriteString(p.Text)
		}
	}
	out.Content = contentBuilder.String()
	out.ThinkingContent = thinkBuilder.String()

	if cand.GroundingMetadata != nil {
		out.Citations = extractCitations(cand.GroundingMetadata)
	}
}

// schemaToGemini converts a JSON-Schema-shaped map[string]any (the
// shape produced by GenerateSchema and accepted by WithJSONSchema)
// into a *genai.Schema. Returns an error when the input uses
// constructs Gemini's schema dialect doesn't support. Used both at
// request time for WithJSONSchema and at startup for schema
// pre-flighting.
func schemaToGemini(in map[string]any) (*genai.Schema, error) {
	if in == nil {
		return nil, nil
	}
	// Reject unsupported combinators up-front. anyOf/oneOf would
	// require us to flatten/lift; $ref needs a registry. The schemas
	// the codebase emits today (judge, cards, briefing, ...) don't use
	// any of these.
	for _, k := range []string{"$ref", "$id", "allOf"} {
		if _, ok := in[k]; ok {
			return nil, fmt.Errorf("gemini schema: unsupported %q construct", k)
		}
	}

	s := &genai.Schema{}
	if t, ok := in["type"].(string); ok && t != "" {
		s.Type = jsonTypeToGenAI(t)
	}
	if d, ok := in["description"].(string); ok {
		s.Description = d
	}
	if enum, ok := in["enum"].([]any); ok {
		for _, v := range enum {
			if str, ok := v.(string); ok {
				s.Enum = append(s.Enum, str)
			}
		}
	}
	if min, ok := numericField(in["minimum"]); ok {
		s.Minimum = &min
	}
	if max, ok := numericField(in["maximum"]); ok {
		s.Maximum = &max
	}
	if items, ok := in["items"].(map[string]any); ok {
		sub, err := schemaToGemini(items)
		if err != nil {
			return nil, fmt.Errorf("items: %w", err)
		}
		s.Items = sub
	}
	if props, ok := in["properties"].(map[string]any); ok {
		s.Properties = map[string]*genai.Schema{}
		for k, v := range props {
			sub, ok := v.(map[string]any)
			if !ok {
				continue
			}
			converted, err := schemaToGemini(sub)
			if err != nil {
				return nil, fmt.Errorf("properties.%s: %w", k, err)
			}
			s.Properties[k] = converted
		}
	}
	if req, ok := in["required"].([]any); ok {
		for _, v := range req {
			if str, ok := v.(string); ok {
				s.Required = append(s.Required, str)
			}
		}
	}
	if anyOf, ok := in["anyOf"].([]any); ok {
		for _, v := range anyOf {
			sub, ok := v.(map[string]any)
			if !ok {
				continue
			}
			converted, err := schemaToGemini(sub)
			if err != nil {
				return nil, fmt.Errorf("anyOf: %w", err)
			}
			s.AnyOf = append(s.AnyOf, converted)
		}
	}
	return s, nil
}

// numericField extracts a float from JSON-decoded numeric values which
// may arrive as float64 or json.Number.
func numericField(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}
