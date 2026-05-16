package synth

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLintForGemini_AllProductionSchemasClean is the CI gate that catches
// Gemini-incompatible schema patterns before they hit production. Every
// schema we send to the LLM provider goes through this check; new
// entries should be registered in productionSchemasForLint().
//
// When this test fails, the message names the offending schema and the
// JSON-Pointer path to the bad node — fix the corresponding Go field's
// `zen:"..."` tag, then re-run.
func TestLintForGemini_AllProductionSchemasClean(t *testing.T) {
	findings := LintForGeminiNamed(productionSchemasForLint())
	if len(findings) > 0 {
		t.Fatalf("Gemini schema lint found %d issues:\n  %s",
			len(findings), strings.Join(findings, "\n  "))
	}
}

// Direct rule coverage: each rule has a small fixture proving the
// linter actually fires on it. Without these, the gate above could
// silently drift to a no-op.

func TestLintForGemini_FlagsBareObject(t *testing.T) {
	bad := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"hidden": map[string]any{"type": "object"}, // missing properties
		},
	}
	findings := LintForGemini(bad)
	require.Contains(t, joined(findings), "/properties/hidden",
		"linter must name the offending field")
	require.Contains(t, joined(findings), "Gemini requires OBJECT to declare properties")
}

func TestLintForGemini_FlagsEmptySchema(t *testing.T) {
	bad := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"target": map[string]any{}, // empty {} — no type
		},
	}
	findings := LintForGemini(bad)
	require.Contains(t, joined(findings), "/properties/target")
	require.Contains(t, joined(findings), "no `type` field")
}

func TestLintForGemini_FlagsAdditionalPropertiesSchema(t *testing.T) {
	bad := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"freeform": map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": map[string]any{"type": "string"},
			},
		},
	}
	findings := LintForGemini(bad)
	require.Contains(t, joined(findings), "/properties/freeform")
	require.Contains(t, joined(findings), "free-form maps")
}

func TestLintForGemini_FlagsUnsupportedKeywords(t *testing.T) {
	bad := map[string]any{
		"type":  "object",
		"oneOf": []any{},
		"properties": map[string]any{
			"x": map[string]any{
				"type": "string",
				"$ref": "#/defs/foo",
			},
		},
	}
	findings := LintForGemini(bad)
	joined := joined(findings)
	require.Contains(t, joined, `unsupported keyword "oneOf"`)
	require.Contains(t, joined, `unsupported keyword "$ref"`)
}

func TestLintForGemini_FlagsMaxItems(t *testing.T) {
	bad := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"tags": map[string]any{
				"type":     "array",
				"items":    map[string]any{"type": "string"},
				"maxItems": 4,
			},
		},
	}
	findings := LintForGemini(bad)
	require.Contains(t, joined(findings), "/properties/tags")
	require.Contains(t, joined(findings), "maxItems")
}

func TestLintForGemini_FlagsArrayWithoutItems(t *testing.T) {
	bad := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"arr": map[string]any{"type": "array"}, // missing items
		},
	}
	findings := LintForGemini(bad)
	require.Contains(t, joined(findings), "/properties/arr")
	require.Contains(t, joined(findings), "ARRAY to declare items")
}

func TestLintForGemini_AcceptsCleanSchema(t *testing.T) {
	good := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
			"tags": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
			},
		},
	}
	require.Empty(t, LintForGemini(good),
		"a well-formed object schema must produce no findings")
}

func joined(ss []string) string { return strings.Join(ss, " | ") }
