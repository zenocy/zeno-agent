package synth

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCardSchema_LiveOptional verifies the V2.x serve-time live-binding
// field: cards without it validate (every card pre-V2.x), cards with a
// valid declaration validate, a bad enum value fails, and exceeding the
// 3-entry cap fails.
func TestCardSchema_LiveOptional(t *testing.T) {
	base := func(extra string) string {
		return `{
		  "id": "weather-1234",
		  "date": "2026-04-25",
		  "src": "personal",
		  "src_label": "Run window",
		  "rel": "med",
		  "title": "Run window holds at noon",
		  "sub": "Clear and dry through the early afternoon.",
		  "meta": ["12:00"],
		  "actions": [{"label":"Dismiss"}]` + extra + `
		}`
	}

	// Live absent — must validate.
	require.NoError(t, ValidateJSON(CardSchema(), []byte(base(""))))

	// Live populated with valid entries — must validate.
	good := base(`,"live":[{"slot":"meta","kind":"weather","ref":"current"},{"slot":"sub_suffix","kind":"countdown","ref":"evt-uid-1"}]`)
	require.NoError(t, ValidateJSON(CardSchema(), []byte(good)))

	// Bad slot enum — must fail.
	badSlot := base(`,"live":[{"slot":"footer","kind":"weather","ref":"current"}]`)
	require.Error(t, ValidateJSON(CardSchema(), []byte(badSlot)))

	// Bad kind enum — must fail.
	badKind := base(`,"live":[{"slot":"meta","kind":"humidity","ref":"current"}]`)
	require.Error(t, ValidateJSON(CardSchema(), []byte(badKind)))

	// Missing required ref — must fail.
	missingRef := base(`,"live":[{"slot":"meta","kind":"stock"}]`)
	require.Error(t, ValidateJSON(CardSchema(), []byte(missingRef)))

	// Over the 3-entry cap — must fail.
	tooMany := base(`,"live":[` +
		`{"slot":"meta","kind":"weather","ref":"current"},` +
		`{"slot":"meta","kind":"stock","ref":"AAPL"},` +
		`{"slot":"meta","kind":"stock","ref":"TSLA"},` +
		`{"slot":"meta","kind":"countdown","ref":"e"}]`)
	require.Error(t, ValidateJSON(CardSchema(), []byte(tooMany)))
}

// TestCardSchema_ItemsAndDigestKind verifies the V2.x digest support:
// kind="digest" validates, items[] children validate, the legacy kinds
// still validate (enum-expansion regression), a bad item src fails, and
// the 8-item cap holds.
func TestCardSchema_ItemsAndDigestKind(t *testing.T) {
	digest := `{
	  "id": "digest:2026-04-25",
	  "date": "2026-04-25",
	  "src": "mail",
	  "src_label": "Digest",
	  "rel": "low",
	  "kind": "digest",
	  "title": "Five low-signal threads",
	  "sub": "Newsletters and minor notifications you can skim later.",
	  "meta": [],
	  "actions": [{"label":"Dismiss"}],
	  "items": [
	    {"title":"Stratechery weekly","src":"mail","ref":"thread:stratechery"},
	    {"title":"GitHub digest","sub":"3 repos updated","src":"mail"}
	  ]
	}`
	require.NoError(t, ValidateJSON(CardSchema(), []byte(digest)))

	// Legacy kinds still validate (regression guard for the enum expansion).
	for _, kind := range []string{"", "personal", "reply_received"} {
		legacy := `{"id":"a","date":"2026-04-25","src":"mail","src_label":"x","rel":"low",` +
			`"kind":"` + kind + `","title":"Title here","sub":"Sub long enough to clear the floor.",` +
			`"meta":[],"actions":[{"label":"Dismiss"}]}`
		require.NoError(t, ValidateJSON(CardSchema(), []byte(legacy)), "kind=%q must still validate", kind)
	}

	// Bad item src enum — must fail.
	badSrc := `{"id":"d","date":"2026-04-25","src":"mail","src_label":"x","rel":"low","kind":"digest",` +
		`"title":"Title here","sub":"Sub long enough to clear the floor.","meta":[],"actions":[{"label":"Dismiss"}],` +
		`"items":[{"title":"x","src":"weather"}]}`
	require.Error(t, ValidateJSON(CardSchema(), []byte(badSrc)))

	// Item missing required title — must fail.
	missingTitle := `{"id":"d","date":"2026-04-25","src":"mail","src_label":"x","rel":"low","kind":"digest",` +
		`"title":"Title here","sub":"Sub long enough to clear the floor.","meta":[],"actions":[{"label":"Dismiss"}],` +
		`"items":[{"sub":"no title"}]}`
	require.Error(t, ValidateJSON(CardSchema(), []byte(missingTitle)))

	// Over the 8-item cap — must fail.
	var items []string
	for i := 0; i < 9; i++ {
		items = append(items, `{"title":"item"}`)
	}
	tooMany := `{"id":"d","date":"2026-04-25","src":"mail","src_label":"x","rel":"low","kind":"digest",` +
		`"title":"Title here","sub":"Sub long enough to clear the floor.","meta":[],"actions":[{"label":"Dismiss"}],` +
		`"items":[` + strings.Join(items, ",") + `]}`
	require.Error(t, ValidateJSON(CardSchema(), []byte(tooMany)))
}

// TestCardSchemaMap_LiveAndItemsAreGeminiSafe asserts the relaxed,
// LLM-facing schema for the new fields stays inside Gemini's OpenAPI
// subset: no maxItems anywhere, no additionalProperties on the nested
// item objects, the DigestItem.src enum (which contains "") is dropped,
// and the LiveField enums (no empty member) are preserved.
func TestCardSchemaMap_LiveAndItemsAreGeminiSafe(t *testing.T) {
	m := relaxSchemaForLLM(GenerateSchema(reflect.TypeOf(Card{})))
	props := m["properties"].(map[string]any)

	// live: array of objects, no maxItems, item object has no
	// additionalProperties, slot/kind enums preserved (no empty member).
	live := props["live"].(map[string]any)
	require.Equal(t, "array", live["type"])
	require.NotContains(t, live, "maxItems")
	liveItem := live["items"].(map[string]any)
	require.NotContains(t, liveItem, "additionalProperties")
	liveItemProps := liveItem["properties"].(map[string]any)
	slot := liveItemProps["slot"].(map[string]any)
	require.Contains(t, slot, "enum", "slot enum (no empty member) must survive relaxation")
	kind := liveItemProps["kind"].(map[string]any)
	require.Contains(t, kind, "enum")

	// items: array of objects, no maxItems, no additionalProperties, and
	// the src enum (contains "") is dropped on the LLM side.
	items := props["items"].(map[string]any)
	require.Equal(t, "array", items["type"])
	require.NotContains(t, items, "maxItems")
	digestItem := items["items"].(map[string]any)
	require.NotContains(t, digestItem, "additionalProperties")
	digestItemProps := digestItem["properties"].(map[string]any)
	src := digestItemProps["src"].(map[string]any)
	require.NotContains(t, src, "enum", "src enum contains \"\" and must be dropped for Gemini")
	// title's maxLength must be stripped too.
	title := digestItemProps["title"].(map[string]any)
	require.NotContains(t, title, "maxLength")
}

// TestGenerateSchema_EntityKeyExcluded confirms the server-managed
// EntityKey field is omitted from the generated schema (zen:"-"), exactly
// like ConcernID — so the model never tries to set it and json_schema mode
// stays Gemini-safe.
func TestGenerateSchema_EntityKeyExcluded(t *testing.T) {
	s := GenerateSchema(reflect.TypeOf(Card{}))
	props := s["properties"].(map[string]any)
	require.NotContains(t, props, "entity_key")
	require.NotContains(t, props, "EntityKey")
	required, _ := s["required"].([]string)
	require.NotContains(t, required, "entity_key")
}
