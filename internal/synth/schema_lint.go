package synth

import (
	"fmt"
	"sort"
)

// LintForGemini walks a JSON-Schema map and returns a list of human-readable
// findings for patterns Gemini's structured-output validator (Google's
// OpenAPI 3.0 subset) is known to reject with 400 INVALID_ARGUMENT or
// to silently mishandle (returning empty content).
//
// The intent is to fail fast at test time when a schema would be rejected
// in production. Add a new check here whenever Gemini surfaces a fresh
// failure mode — the production error messages are wrapped by OpenRouter
// and rarely point at the offending field, so the linter is the cheaper
// place to catch this class of bug.
//
// Returns an empty slice when the schema is Gemini-clean. Findings are
// sorted by JSON-Pointer path so test output is deterministic.
//
// Rules currently enforced:
//
//   - Every schema node must declare a `type` field. Bare `{}` (no type)
//     is rejected — Gemini requires every property to have a type. Bare
//     `enum`-only schemas (`{"enum": [...]}` with no type) likewise.
//
//   - `type: "object"` schemas must declare a `properties` map (which
//     may be empty). Bare `{"type":"object"}` from a Go map field
//     trips Gemini's "OBJECT must declare properties" check.
//
//   - `additionalProperties` set to a non-false value (i.e. a schema)
//     means a free-form map. Gemini's structured output drops this and
//     the resulting bare object then fails the rule above. Use
//     `zen:"-"` on the Go struct field to keep maps out of the schema.
//
//   - Unsupported keywords (`$ref`, `$schema`, `$id`, `oneOf`,
//     `anyOf`, `allOf`, `not`, `patternProperties`) — Google's subset
//     does not implement them and silently mishandles or rejects
//     schemas that include them.
//
//   - `maxItems` on any node. Gemini 3 Flash preview rejects schemas
//     containing maxItems with 400 INVALID_ARGUMENT once the rest of
//     the constraint mix passes a (poorly-defined) complexity threshold.
//     Reduced repros against the cards schema confirmed this is the
//     specific trigger. relaxSchemaForLLM strips maxItems for this
//     reason; the rule here ensures any new schema author who forgets
//     to relax catches it in CI rather than in production.
//
// Rules deliberately NOT enforced (yet):
//
//   - Empty-string entries in `enum` arrays (e.g. `["", "personal"]`).
//     Real-world Gemini accepts these for STRING type; flagging would
//     produce false positives against today's Card.Kind / Action.Intent.
//
//   - Large enums (>16 entries). Some Gemini versions cap enums
//     around this size, but the limit is undocumented and varies;
//     warning-only would be more useful than a hard fail.
func LintForGemini(schema map[string]any) []string {
	var findings []string
	lintNode(schema, "", &findings)
	sort.Strings(findings)
	return findings
}

// LintForGeminiNamed is the multi-schema entry point used by tests:
// runs LintForGemini against each named schema and returns
// "<name>: <finding>" lines for any failure. Empty slice = all clean.
func LintForGeminiNamed(named map[string]map[string]any) []string {
	var out []string
	names := make([]string, 0, len(named))
	for n := range named {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, f := range LintForGemini(named[name]) {
			out = append(out, fmt.Sprintf("%s: %s", name, f))
		}
	}
	return out
}

// lintNode is the recursive walker. path is a JSON-Pointer-ish address
// of the current node ("" at the root, "/properties/cards" deeper) so
// findings can name the offending field.
func lintNode(node map[string]any, path string, out *[]string) {
	if node == nil {
		return
	}

	// Rule: unsupported keywords. Surface immediately and continue
	// walking — multiple findings on one node are useful.
	for _, k := range unsupportedGeminiKeywords {
		if _, ok := node[k]; ok {
			*out = append(*out, fmt.Sprintf("%s uses unsupported keyword %q", displayPath(path), k))
		}
	}

	// Rule: every schema must declare `type`. The exception is a
	// non-schema container — `properties`, `items` of an array's items,
	// etc. — but the recursion below dispatches on those, so by the
	// time we reach lintNode, this map IS a schema.
	t, hasType := node["type"].(string)
	if !hasType {
		// `properties` map without `type` could still be a top-level
		// schema fragment in some authoring styles, but our generator
		// always emits type. Anything missing it here is wrong.
		*out = append(*out, fmt.Sprintf("%s has no `type` field — Gemini rejects schemas without a declared type", displayPath(path)))
	}

	// Rule: `additionalProperties` with a schema (not `false`) means
	// "free-form map" — Gemini's subset drops this and we end up with a
	// bare object below. Flag both shapes; the false case is fine for
	// strict validators but we strip it on the way out anyway.
	if ap, ok := node["additionalProperties"]; ok {
		if _, isFalse := ap.(bool); !isFalse {
			*out = append(*out, fmt.Sprintf("%s sets `additionalProperties` to a schema — free-form maps aren't supported; mark the Go field `zen:\"-\"`", displayPath(path)))
		}
	}

	// Rule: `maxItems` triggers Gemini 3 Flash preview's 400
	// INVALID_ARGUMENT on the cards schema. relaxSchemaForLLM strips
	// it; the rule here catches any caller that bypasses the relax
	// pass (or any future relax-pass regression).
	if _, ok := node["maxItems"]; ok {
		*out = append(*out, fmt.Sprintf("%s uses `maxItems` — Gemini 3 Flash preview rejects schemas containing it; relaxSchemaForLLM strips this", displayPath(path)))
	}

	// Type-specific checks.
	if hasType {
		switch t {
		case "object":
			props, hasProps := node["properties"]
			if !hasProps {
				*out = append(*out, fmt.Sprintf("%s is `type:object` with no `properties` map — Gemini requires OBJECT to declare properties", displayPath(path)))
			} else if propsMap, ok := props.(map[string]any); ok {
				// Recurse into each property — sorted for stable output.
				keys := make([]string, 0, len(propsMap))
				for k := range propsMap {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					if child, ok := propsMap[k].(map[string]any); ok {
						lintNode(child, path+"/properties/"+k, out)
					}
				}
			}
		case "array":
			if items, ok := node["items"].(map[string]any); ok {
				lintNode(items, path+"/items", out)
			} else {
				*out = append(*out, fmt.Sprintf("%s is `type:array` with no `items` schema — Gemini requires ARRAY to declare items", displayPath(path)))
			}
		}
	}
}

// displayPath formats a JSON-Pointer-ish path for human-readable
// findings. The root node renders as "(root)" so "(root) is type:object
// with no properties" reads cleanly.
func displayPath(p string) string {
	if p == "" {
		return "(root)"
	}
	return p
}

// unsupportedGeminiKeywords is the list of JSON-Schema keywords known to
// be either silently dropped or hard-rejected by Google's
// structured-output validator. See Google AI Studio docs for the
// supported subset; absence of a keyword from the supported list is the
// signal here.
var unsupportedGeminiKeywords = []string{
	"$id",
	"$ref",
	"$schema",
	"allOf",
	"anyOf",
	"not",
	"oneOf",
	"patternProperties",
}

// init pin: keep unsupportedGeminiKeywords sorted so test diffs against
// it stay readable when entries are added.
func init() {
	if !sort.StringsAreSorted(unsupportedGeminiKeywords) {
		panic("synth: unsupportedGeminiKeywords must be sorted — see schema_lint.go")
	}
}

// productionSchemasForLint returns the named map of every JSON-Schema
// the synth package sends to an LLM provider. Used by the lint test as
// the source of truth — when a new schema is added, register it here so
// the Gemini gate runs against it too.
//
// Kept in this file (rather than the test) so the registry is shipped
// with the linter — anyone reading schema_lint.go sees both the rules
// and the call sites they protect.
func productionSchemasForLint() map[string]map[string]any {
	return map[string]map[string]any{
		"cards":        CardSetSchemaMap(),
		"card":         CardSchemaMap(),
		"inject_cards": InjectCardSetSchemaMap(),
		"sub_card":     SubCardSchemaMap(),
		"briefing":     BriefingSchemaMap(),
	}
}
