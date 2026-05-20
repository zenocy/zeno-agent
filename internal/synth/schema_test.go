package synth

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGenerateSchema_PrimitiveConstraints(t *testing.T) {
	type S struct {
		Name  string `json:"name" zen:"required,maxlen=10"`
		N     int    `json:"n"    zen:"required,min=0,max=100"`
		Maybe string `json:"maybe,omitempty"`
	}
	s := GenerateSchema(reflect.TypeOf(S{}))
	props := s["properties"].(map[string]any)
	name := props["name"].(map[string]any)
	require.Equal(t, "string", name["type"])
	require.Equal(t, 10, name["maxLength"])
	n := props["n"].(map[string]any)
	require.Equal(t, "integer", n["type"])
	require.Equal(t, 0, n["minimum"])
	require.Equal(t, 100, n["maximum"])
	required := s["required"].([]string)
	require.Contains(t, required, "name")
	require.Contains(t, required, "n")
	require.NotContains(t, required, "maybe")
}

func TestGenerateSchema_EnumWithEmpty(t *testing.T) {
	type S struct {
		Kind string `json:"kind" zen:"enum=|personal"`
	}
	s := GenerateSchema(reflect.TypeOf(S{}))
	props := s["properties"].(map[string]any)
	kind := props["kind"].(map[string]any)
	enum := kind["enum"].([]any)
	require.Contains(t, enum, "")
	require.Contains(t, enum, "personal")
}

// `zen:"-"` excludes a field from the generated JSON-Schema entirely,
// in BOTH the strict (post-parse) and relaxed (LLM-facing) variants.
// We use this for server-managed fields whose Go shape doesn't
// translate to Gemini's OpenAPI 3.0 subset — specifically free-form
// maps. Pre-fix, Action.Target's `map[string]any` and Card.Expand's
// `map[string]string` produced a bare {"type":"object"} property that
// Gemini rejected with 400 INVALID_ARGUMENT on the cards-repair call.
// Marking those fields zen:"-" drops them from the schema so Gemini
// never sees the offending shape.
func TestGenerateSchema_ZenDashExcludesField(t *testing.T) {
	type S struct {
		Keep   string         `json:"keep"             zen:"required"`
		Hidden map[string]any `json:"hidden,omitempty" zen:"-"`
	}
	s := GenerateSchema(reflect.TypeOf(S{}))
	props := s["properties"].(map[string]any)
	require.Contains(t, props, "keep")
	require.NotContains(t, props, "hidden",
		"zen:\"-\" must drop the field from the schema entirely")
	required := s["required"].([]string)
	require.NotContains(t, required, "hidden")
}

// Cards regression: Action.Target must use actionTargetSchema's
// declared properties (NOT a bare map), and Card.Expand must use
// expandMapSchema's empty-properties shape — both replace the
// reflection default for `map[string]any`/`map[string]string` with
// shapes Gemini accepts. The earlier band-aid that excluded these
// fields entirely (zen:"-") broke runtime behavior because the LLM
// genuinely emits them and the executors read them downstream.
func TestCardSetSchemaMap_TargetAndExpandAreDeclaredObjects(t *testing.T) {
	s := CardSetSchemaMap()
	cards := s["properties"].(map[string]any)["cards"].(map[string]any)
	cardItem := cards["items"].(map[string]any)
	cardProps := cardItem["properties"].(map[string]any)

	expand, ok := cardProps["expand"].(map[string]any)
	require.True(t, ok, "Card.Expand must appear in the cards schema")
	require.Equal(t, "object", expand["type"])
	require.Contains(t, expand, "properties",
		"Card.Expand must declare `properties` (even empty) so Gemini accepts it")

	actions := cardProps["actions"].(map[string]any)
	actionItem := actions["items"].(map[string]any)
	actionProps := actionItem["properties"].(map[string]any)

	target, ok := actionProps["target"].(map[string]any)
	require.True(t, ok, "Action.Target must appear in the action schema")
	require.Equal(t, "object", target["type"])
	tprops, ok := target["properties"].(map[string]any)
	require.True(t, ok, "Action.Target must declare `properties`")

	// Per-intent key spot-checks. When a new executor key is added in
	// internal/action/executors_*.go, declare it in actionTargetSchema
	// and add a row here. Missing declarations cause the model's
	// emitted target args to get stripped by the strict validator's
	// additionalProperties check, breaking the action at click time.
	for _, key := range []string{
		"recipient",  // send_whatsapp
		"url",        // open_url
		"subject",    // mail (draft_reply / send_reply / forward)
		"start",      // add_event / reschedule_event (HH:MM)
		"end",        // add_event / reschedule_event (HH:MM)
		"start_iso",  // add_event (RFC3339)
		"end_iso",    // add_event (RFC3339)
		"date",       // add_event (YYYY-MM-DD pairs with start/end)
		"title",      // add_event
		"location",   // add_event
		"uid",        // cancel_event / reschedule_event
		"event_uid",  // alias for uid
		"folder",     // move_mail
		"recipients", // forward (string array)
		"steer",      // mail compose nudge
		"on",         // flag_mail (boolean)
		"fire_at",    // add_task with reminder
	} {
		require.Contains(t, tprops, key,
			"actionTargetSchema must declare %q — at least one executor reads it", key)
	}
}

func TestGenerateSchema_SliceConstraints(t *testing.T) {
	type S struct {
		Cards []string `json:"cards" zen:"required,minitems=2,maxitems=6"`
	}
	s := GenerateSchema(reflect.TypeOf(S{}))
	props := s["properties"].(map[string]any)
	cards := props["cards"].(map[string]any)
	require.Equal(t, "array", cards["type"])
	require.Equal(t, 2, cards["minItems"])
	require.Equal(t, 6, cards["maxItems"])
}

func TestCardSetSchema_Validates(t *testing.T) {
	good := `{
	  "cards": [
	    {
	      "id": "saru-1234",
	      "date": "2026-04-25",
	      "src": "mail",
	      "src_label": "Email · Acuity",
	      "rel": "high",
	      "kind": "",
	      "title": "Saru Patel · re: redline",
	      "sub": "Walked the redline with Lin.",
	      "meta": ["06:14"],
	      "actions": [{"label":"Draft","primary":true}]
	    },
	    {
	      "id": "lia-5678",
	      "date": "2026-04-25",
	      "src": "personal",
	      "src_label": "Family · Sam",
	      "rel": "high",
	      "kind": "personal",
	      "title": "Lia's school recital is Thursday",
	      "sub": "Sam asked if you can be there.",
	      "meta": ["Thu"],
	      "actions": [{"label":"Block 17:00–18:30"}]
	    }
	  ]
	}`
	require.NoError(t, ValidateJSON(CardSetSchema(), []byte(good)))

	// Missing required field (rel).
	bad := `{"cards":[{"id":"a","date":"2026-04-25","src":"mail","src_label":"x","title":"t","sub":"s","actions":[{"label":"l"}]}]}`
	require.Error(t, ValidateJSON(CardSetSchema(), []byte(bad)))

	// Too few cards.
	tooFew := `{"cards":[{"id":"a","date":"2026-04-25","src":"mail","src_label":"x","rel":"high","title":"t","sub":"s","actions":[{"label":"l"}]}]}`
	require.Error(t, ValidateJSON(CardSetSchema(), []byte(tooFew)))
}

func TestBriefingSchema_Validates(t *testing.T) {
	// Voice-shaped fixtures: title/summary meet the V2.1 minlen=4/20 floors
	// that block hollow briefings (passed schema in V2.0 but rendered empty).
	good := `{"date":"2026-04-25","eyebrow":"e","title":"calm start","summary":"Series B at 11; otherwise the day breathes.","tension":38}`
	require.NoError(t, ValidateJSON(BriefingSchema(), []byte(good)))

	// suggested_followup is optional but must validate when present.
	withSuggestion := `{"date":"2026-04-25","eyebrow":"e","title":"calm start","summary":"Series B at 11; otherwise the day breathes.","tension":38,"suggested_followup":"Block lunch?"}`
	require.NoError(t, ValidateJSON(BriefingSchema(), []byte(withSuggestion)))

	// suggested_followup over the 120-char limit must fail.
	long := strings.Repeat("x", 121)
	tooLong := `{"date":"2026-04-25","eyebrow":"e","title":"calm start","summary":"Series B at 11; otherwise the day breathes.","tension":38,"suggested_followup":"` + long + `"}`
	require.Error(t, ValidateJSON(BriefingSchema(), []byte(tooLong)))

	// Tension out of range.
	bad := `{"date":"2026-04-25","eyebrow":"e","title":"calm start","summary":"Series B at 11; otherwise the day breathes.","tension":150}`
	require.Error(t, ValidateJSON(BriefingSchema(), []byte(bad)))

	// Missing tension.
	missing := `{"date":"2026-04-25","eyebrow":"e","title":"calm start","summary":"Series B at 11; otherwise the day breathes."}`
	require.Error(t, ValidateJSON(BriefingSchema(), []byte(missing)))

	// V2.1 minlen floors: hollow summary (<20 chars) must fail.
	hollow := `{"date":"2026-04-25","eyebrow":"e","title":"calm start","summary":"short","tension":38}`
	require.Error(t, ValidateJSON(BriefingSchema(), []byte(hollow)))

	// V2.1 minlen floors: too-short title (<4 chars) must fail.
	stubTitle := `{"date":"2026-04-25","eyebrow":"e","title":"hi","summary":"Series B at 11; otherwise the day breathes.","tension":38}`
	require.Error(t, ValidateJSON(BriefingSchema(), []byte(stubTitle)))
}

// TestSubCardSchema_DocumentKind verifies the V2.x document kind: a
// SubCard whose `kind=document` carries a markdown body, optional From
// header, and optional ThreadHint pointer that the UI's "view original"
// toggle uses to refetch the verbatim email body. The schema accepts
// the new kind alongside the legacy four.
func TestSubCardSchema_DocumentKind(t *testing.T) {
	good := `{
	  "id": "sub-doc1",
	  "kind": "document",
	  "eyebrow": "document · miss despoina",
	  "title": "Homework — week of 18 May",
	  "body": "## Monday\n- Greek: page 57\n- Spelling words\n",
	  "from": "Miss Despoina · Sun 18 May",
	  "thread_hint": "homework"
	}`
	require.NoError(t, ValidateJSON(SubCardSchema(), []byte(good)))

	// from over the 80-char ceiling must fail.
	tooLongFrom := `{"id":"sub-doc2","kind":"document","eyebrow":"document","title":"Reply","from":"` + strings.Repeat("x", 81) + `"}`
	require.Error(t, ValidateJSON(SubCardSchema(), []byte(tooLongFrom)))

	// thread_hint over the 120-char ceiling must fail.
	tooLongHint := `{"id":"sub-doc3","kind":"document","eyebrow":"document","title":"Reply","thread_hint":"` + strings.Repeat("y", 121) + `"}`
	require.Error(t, ValidateJSON(SubCardSchema(), []byte(tooLongHint)))

	// An unknown kind must still be rejected — the union is closed.
	badKind := `{"id":"sub-x","kind":"diagram","eyebrow":"e","title":"Reply"}`
	require.Error(t, ValidateJSON(SubCardSchema(), []byte(badKind)))

	// The legacy four kinds still validate (regression guard for the enum
	// expansion).
	for _, kind := range []string{"calendar", "draft", "research", "answer"} {
		legacy := `{"id":"sub-l","kind":"` + kind + `","eyebrow":"e","title":"Reply"}`
		require.NoError(t, ValidateJSON(SubCardSchema(), []byte(legacy)), "kind=%s must still validate", kind)
	}
}

// TestCardSchema_BodyOptional verifies the reactive Ask body field is
// schema-optional: cards without it validate (morning fixtures, WhatsApp
// replies), cards with multi-paragraph prose validate (in-app text
// chat), and a non-string value still fails the type check.
func TestCardSchema_BodyOptional(t *testing.T) {
	// Body absent — morning + WhatsApp shape. Must validate.
	absent := `{
	  "id": "saru-1234",
	  "date": "2026-04-25",
	  "src": "mail",
	  "src_label": "Email · Acuity",
	  "rel": "high",
	  "title": "Saru Patel · re: redline",
	  "sub": "Walked the redline with Lin.",
	  "meta": ["06:14"],
	  "actions": [{"label":"Draft","primary":true}]
	}`
	require.NoError(t, ValidateJSON(CardSchema(), []byte(absent)))

	// Body populated with multi-paragraph prose — in-app text-chat shape.
	populated := `{
	  "id": "ask-iran",
	  "date": "2026-04-25",
	  "src": "ask",
	  "src_label": "Generated",
	  "rel": "med",
	  "title": "A fragile pause in the Iran conflict",
	  "sub": "Vance keeps military action *locked and loaded* if talks stall.",
	  "body": "Paragraph one with concrete detail.\n\nParagraph two adding *context*.\n\nThird beat ending decisively.",
	  "meta": [],
	  "actions": [{"label":"Dismiss"}]
	}`
	require.NoError(t, ValidateJSON(CardSchema(), []byte(populated)))

	// Body non-string — must fail (type mismatch).
	badType := `{
	  "id": "ask-bad",
	  "date": "2026-04-25",
	  "src": "ask",
	  "src_label": "Generated",
	  "rel": "med",
	  "title": "Title here",
	  "sub": "Sub long enough to clear the floor.",
	  "body": 42,
	  "meta": [],
	  "actions": [{"label":"Dismiss"}]
	}`
	require.Error(t, ValidateJSON(CardSchema(), []byte(badType)))
}

// TestCardSchema_SourcesOptional verifies the reactive Ask sources
// field is schema-optional: cards without it validate (no web tools
// used), cards with a list of {t, u} entries validate (model cited
// web evidence), and an empty title fails the minimum (sources need
// at least a title to render).
func TestCardSchema_SourcesOptional(t *testing.T) {
	// Sources absent — most cards. Must validate.
	absent := `{
	  "id": "ask-no-web",
	  "date": "2026-04-25",
	  "src": "ask",
	  "src_label": "Generated",
	  "rel": "med",
	  "title": "Title here",
	  "sub": "Sub long enough to clear the floor.",
	  "meta": [],
	  "actions": [{"label":"Dismiss"}]
	}`
	require.NoError(t, ValidateJSON(CardSchema(), []byte(absent)))

	// Sources populated — model cited web evidence.
	populated := `{
	  "id": "ask-iran",
	  "date": "2026-04-25",
	  "src": "ask",
	  "src_label": "Generated",
	  "rel": "med",
	  "title": "A fragile pause in the Iran conflict",
	  "sub": "Vance keeps military action locked and loaded if talks stall.",
	  "sources": [
	    {"t": "Reuters: Trump delays strike", "u": "https://www.reuters.com/world/middle-east/iran-pause-2026"},
	    {"t": "Bloomberg: Gulf pressure", "u": "https://www.bloomberg.com/news/iran-gulf-allies"}
	  ],
	  "meta": [],
	  "actions": [{"label":"Dismiss"}]
	}`
	require.NoError(t, ValidateJSON(CardSchema(), []byte(populated)))

	// Source missing required title — fails.
	missingT := `{
	  "id": "ask-bad",
	  "date": "2026-04-25",
	  "src": "ask",
	  "src_label": "Generated",
	  "rel": "med",
	  "title": "Title here",
	  "sub": "Sub long enough to clear the floor.",
	  "sources": [{"u": "https://example.com"}],
	  "meta": [],
	  "actions": [{"label":"Dismiss"}]
	}`
	require.Error(t, ValidateJSON(CardSchema(), []byte(missingT)))

	// Source missing required URL — fails.
	missingU := `{
	  "id": "ask-bad",
	  "date": "2026-04-25",
	  "src": "ask",
	  "src_label": "Generated",
	  "rel": "med",
	  "title": "Title here",
	  "sub": "Sub long enough to clear the floor.",
	  "sources": [{"t": "Title only"}],
	  "meta": [],
	  "actions": [{"label":"Dismiss"}]
	}`
	require.Error(t, ValidateJSON(CardSchema(), []byte(missingU)))
}

// TestCardSchemaMap_BodyHasNoLengthConstraints verifies the LLM-facing
// schema lists `body` as an unconstrained optional string so Gemini's
// decoder doesn't refuse longer elaborations. The strict validator
// already enforces no minLength/maxLength (covered above); this guards
// the relaxed shape sent to the model.
func TestCardSchemaMap_BodyHasNoLengthConstraints(t *testing.T) {
	s := CardSchemaMap()
	props, ok := s["properties"].(map[string]any)
	require.True(t, ok, "card schema must expose properties")
	body, ok := props["body"].(map[string]any)
	require.True(t, ok, "body must appear in the card schema")
	require.Equal(t, "string", body["type"])
	require.NotContains(t, body, "minLength", "body must not be length-floored — soft length lives in the prompt")
	require.NotContains(t, body, "maxLength", "body must not be length-capped — multi-paragraph elaboration is intentional")
	required, _ := s["required"].([]string)
	require.NotContains(t, required, "body", "body must be optional — empty on morning + WhatsApp cards")
}

func TestSchema_Marshalable(t *testing.T) {
	// Both schemas must be JSON-marshalable so they can be sent as the
	// OpenAI tool's function.parameters.
	cs := GenerateSchema(reflect.TypeOf(CardSet{}))
	_, err := json.Marshal(cs)
	require.NoError(t, err)
	br := GenerateSchema(reflect.TypeOf(Briefing{}))
	_, err = json.Marshal(br)
	require.NoError(t, err)
}
