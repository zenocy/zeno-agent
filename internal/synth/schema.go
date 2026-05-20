// Package synth holds the morning synthesizer pipeline: schema definitions
// for Card and Briefing, the reflect-based JSON-Schema generator, the
// tool-using cards loop, the literary briefing call, and the runner that
// orchestrates them.
package synth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// Action is one button on a Card.
//
// V2.8.0 added Intent + Target. Intent is the structured verb (enum)
// the action handler dispatches on; Target carries intent-specific
// parameters (thread_id for draft_reply, start/end for block_calendar,
// url for open_url, etc). Both are optional at the schema level so
// pre-V2.8 fixtures and legacy stored cards continue to validate;
// `postProcessIntent` in cards.go backfills Intent from Label before
// persistence so the dispatch table is always exhaustive on read.
type Action struct {
	Label   string `json:"label"            zen:"required,maxlen=40"`
	Primary bool   `json:"primary,omitempty"`
	Intent  string `json:"intent,omitempty" zen:"enum=|dismiss|snooze|mark_read|move_mail|draft_reply|send_reply|forward|add_event|block_calendar|rsvp_yes|rsvp_no|rsvp_maybe|add_concern|add_memory|ask_followup|open_url|flag_mail|reschedule_event|cancel_event|pin_card|unpin_card|set_reminder|add_task|complete_task|delete_task|edit_task|send_whatsapp"`
	// Target carries the per-intent arguments the action executor reads
	// (recipient, subject, url, etc). The Go type stays `map[string]any`
	// so executors keep using `stringFromTarget(ec.Target, "key")`, but
	// the JSON-Schema we send to the LLM is the hand-rolled
	// actionTargetSchema() with every known key declared. This is the
	// shape Gemini's constrained decoder needs to actually populate
	// target args — a bare {type:object} or empty-properties object
	// produces `target: {}` because the model can't see which keys
	// matter. `zen:"schema=action_target"` switches the field to the
	// custom schema; see schemaForType.
	Target map[string]any `json:"target,omitempty" zen:"schema=action_target"`
}

// Card is one synthesized item on the morning surface.
//
// V2.5.0 Phase 3 added ConcernID — populated server-side after the
// model's response by `postProcessCards`, NOT by the model itself.
// The schema deliberately omits it from the LLM-facing JSON Schema
// (zen tag is empty) so the model never tries to set it.
type Card struct {
	ID       string   `json:"id"                  zen:"required"`
	Date     string   `json:"date"                zen:"required,format=date"`
	Source   string   `json:"src"                 zen:"required,enum=mail|calendar|personal|tasks|markets|ask"`
	SrcLabel string   `json:"src_label"           zen:"required,maxlen=80"`
	Rel      string   `json:"rel"                 zen:"required,enum=high|med|low"`
	Kind     string   `json:"kind"                zen:"enum=|personal|reply_received"`
	Title    string   `json:"title"               zen:"required,minlen=4,maxlen=120"`
	Sub      string   `json:"sub"                 zen:"required,minlen=20"`
	Meta     []string `json:"meta"                zen:"maxitems=4"`
	Actions  []Action `json:"actions"             zen:"required,minitems=1,maxitems=3"`
	// Expand is a free-form key→string map the LLM may populate with
	// expanded-card metadata (e.g. {"thread tail":"..."}). The keys
	// aren't fixed, so we declare an OBJECT with empty `properties`
	// and a typed additionalProperties — Gemini accepts the shape and
	// the strict validator allows arbitrary string-valued entries.
	// `zen:"schema=expand_map"` selects the custom schema in
	// schemaForType.
	Expand    map[string]string `json:"expand,omitempty" zen:"schema=expand_map"`
	TraceID   string            `json:"trace_id,omitempty"`
	ConcernID *string           `json:"concern_id,omitempty"`

	// Speech is V2.7's WhatsApp register. The reactive prompt populates
	// it with a 1–3 sentence plain-text reply ready to send verbatim
	// over WhatsApp; the in-app surface continues to render Title/Sub
	// untouched. Empty in non-WhatsApp contexts; the Card schema accepts
	// it as optional everywhere so the eval corpus and morning cards
	// continue to validate without changes.
	Speech string `json:"speech,omitempty"`

	// Body is the in-app text-chat elaboration: 2–4 short paragraphs of
	// literary prose populated by the reactive Ask flow only when the
	// request originates from the in-app surface (Conversation == nil).
	// Title and Sub stay voice-styled (skim headline); Body carries the
	// detail a reader who typed into the chat wants without a second
	// turn. Empty on WhatsApp-origin cards and on morning cards. No
	// zen constraint — soft length lives in the prompt; the schema
	// accepts any string so historical fixtures keep validating.
	Body string `json:"body,omitempty"`

	// Sources are the web citations the model used when answering the
	// query. Populated by the reactive Ask flow when search_web or
	// read_url was called this turn; empty on morning cards and on ask
	// cards that didn't touch the web. Trust-but-verify is left to a
	// future URL-grounding pass — for now the model self-reports which
	// URLs informed its answer.
	Sources []Source `json:"sources,omitempty"`
}

// Source is one web citation under an ask card. Title is the
// article/page title (short, capped). URL is the canonical link the
// UI renders as a clickable anchor. Kept distinct from ResearchSource
// (used by SubCard) because that type intentionally omits the URL.
type Source struct {
	T string `json:"t" zen:"required,maxlen=200"`
	U string `json:"u" zen:"required,maxlen=600"`
}

// CardSet is the LLM's required output shape for the cards loop.
//
// minitems is 1 (not 2) so quiet mornings — days where there's
// genuinely one thing worth surfacing — produce a single-card
// briefing cleanly. Forcing minitems=2 made the model invent filler
// or, on Gemini Flash 3 preview, burn its full token budget reasoning
// through how to fabricate a second card and return truncated JSON.
// maxitems caps surface density at 6.
type CardSet struct {
	Cards []Card `json:"cards" zen:"required,minitems=1,maxitems=6"`
}

// SubCard is the typed reply shape for the card-conversation surface
// (CardFocus modal). Each turn the user makes against a pinned card
// resolves to one SubCard; Kind selects which body block the renderer
// shows. The renderer ignores blocks that don't belong to the chosen
// kind, so the LLM only needs to populate the fields that match.
//
//	calendar → Cal + optional Conflict
//	draft    → Draft + optional DraftMeta
//	research → Body + Sources
//	answer   → Body
//	document → Body (markdown) + optional From + optional ThreadHint
//
// Actions reuse the morning Card.Action vocabulary so the existing V2.8
// action handler dispatches sub-card buttons unchanged.
type SubCard struct {
	ID      string   `json:"id"      zen:"required"`
	Kind    string   `json:"kind"    zen:"required,enum=calendar|draft|research|answer|document"`
	Eyebrow string   `json:"eyebrow" zen:"required,maxlen=40"`
	Title   string   `json:"title"   zen:"required,minlen=4,maxlen=120"`
	Actions []Action `json:"actions,omitempty" zen:"maxitems=3"`

	// kind=calendar
	Cal      *SubCalendar `json:"cal,omitempty"`
	Conflict string       `json:"conflict,omitempty"`

	// kind=draft
	Draft     string `json:"draft,omitempty"`
	DraftMeta string `json:"draft_meta,omitempty"`

	// kind=research / answer / document
	Body    string           `json:"body,omitempty"`
	Sources []ResearchSource `json:"sources,omitempty"`

	// kind=document — From is a "{sender} · {short date}" header shown
	// above the rendered markdown. ThreadHint is the same subject
	// substring the model passed to read_thread; the UI uses it to
	// refetch the verbatim body for the "view original" toggle.
	From       string `json:"from,omitempty"        zen:"maxlen=80"`
	ThreadHint string `json:"thread_hint,omitempty" zen:"maxlen=120"`
}

// SubCalendar is the calendar-flavored reply payload.
//
// Title/When/Where/Who is the legacy minimal shape preserved verbatim
// for backward compatibility — old persisted SubCards in the
// conversation thread store decode unchanged, and the LLM can still
// emit just these four fields when no richer evidence is at hand.
//
// Everything below is the V2.x rich-detail extension that lets the
// model populate the day-strip, attendee statuses, conflict, "why this
// slot" reasoning, alternatives, and recurring suggestion the design
// shows in Zeno V2/zeno-focus.jsx. Every new field is opt-in: nil /
// empty decodes as "not provided" and the renderer falls back to the
// minimal grid.
type SubCalendar struct {
	Title string `json:"title" zen:"required,maxlen=120"`
	When  string `json:"when"  zen:"required,maxlen=80"`
	Where string `json:"where,omitempty" zen:"maxlen=80"`
	Who   string `json:"who,omitempty"   zen:"maxlen=80"`

	Start        string                   `json:"start,omitempty"          zen:"maxlen=10"`
	End          string                   `json:"end,omitempty"            zen:"maxlen=10"`
	Attendees    []SubCalendarAttendee    `json:"attendees,omitempty"      zen:"maxitems=8"`
	TravelBefore int                      `json:"travel_before,omitempty"  zen:"min=0,max=240"`
	TravelAfter  int                      `json:"travel_after,omitempty"   zen:"min=0,max=240"`
	Reminder     string                   `json:"reminder,omitempty"       zen:"maxlen=60"`
	Conflict     *SubCalendarConflict     `json:"conflict,omitempty"`
	Reasoning    []string                 `json:"reasoning,omitempty"      zen:"maxitems=5"`
	Alternatives []SubCalendarAlternative `json:"alternatives,omitempty"   zen:"maxitems=3"`
	Recurring    *SubCalendarRecurring    `json:"recurring,omitempty"`
	Daystrip     *SubCalendarDaystrip     `json:"daystrip,omitempty"`
}

// SubCalendarAttendee is one row in the attendee strip — name + role
// + RSVP status. Status maps to the visual symbol in the UI:
// host=✦ accepted=● pending=◌ declined=✕.
type SubCalendarAttendee struct {
	Name   string `json:"name"             zen:"required,maxlen=60"`
	Role   string `json:"role,omitempty"   zen:"maxlen=40"`
	Status string `json:"status,omitempty" zen:"enum=|host|accepted|pending|declined"`
}

// SubCalendarConflict is the proposed-slot status box. Ok=true renders
// as a calm green confirmation; Ok=false renders as an amber warning.
// Text is the one-line explanation shown alongside.
type SubCalendarConflict struct {
	Ok   bool   `json:"ok"`
	Text string `json:"text" zen:"required,maxlen=160"`
}

// SubCalendarAlternative is one row in the "other windows" picker. When
// is the human-formatted slot string (e.g. "Wed · 17:00 → 18:30"),
// Note is the optional rationale.
type SubCalendarAlternative struct {
	When string `json:"when"           zen:"required,maxlen=60"`
	Note string `json:"note,omitempty" zen:"maxlen=120"`
}

// SubCalendarRecurring is the optional "hold this slot every X" prompt
// that appears below the alternatives. Default is the initial checkbox
// state shown to the user.
type SubCalendarRecurring struct {
	Label   string `json:"label"             zen:"required,maxlen=80"`
	Default bool   `json:"default,omitempty"`
}

// SubCalendarDaystrip is the mini timeline that places the proposed
// slot in the context of the day's other events. StartHr/EndHr define
// the window the strip spans (e.g. 9..21 for a workday view).
type SubCalendarDaystrip struct {
	Label   string                     `json:"label,omitempty"     zen:"maxlen=40"`
	StartHr int                        `json:"start_hr,omitempty"  zen:"min=0,max=24"`
	EndHr   int                        `json:"end_hr,omitempty"    zen:"min=0,max=24"`
	Events  []SubCalendarDaystripEvent `json:"events,omitempty"    zen:"maxitems=12"`
}

// SubCalendarDaystripEvent is one event block inside the day-strip.
// Start/End are decimal hours (14.5 == 14:30). Kind drives styling:
// muted=existing event, travel=auto-blocked travel time, proposed=the
// new slot the user is being asked to confirm.
type SubCalendarDaystripEvent struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Label string  `json:"label,omitempty" zen:"maxlen=40"`
	Kind  string  `json:"kind,omitempty"  zen:"enum=|muted|travel|proposed"`
}

// ResearchSource is one citation row under a research-flavored reply.
type ResearchSource struct {
	I int    `json:"i"`
	T string `json:"t" zen:"required,maxlen=120"`
	W string `json:"w,omitempty" zen:"maxlen=80"`
}

// InjectCardSet is the V2.3.0 P3 inject pipeline's required output: a
// 1-card array under the same `cards` key the morning loop uses, so the
// model carries forward the same shape it already knows. minitems=1
// allows a single card; maxitems=3 leaves a small slop band for noisy
// local 7B/8B models — SynthesizeInject takes the first and drops the
// rest.
type InjectCardSet struct {
	Cards []Card `json:"cards" zen:"required,minitems=1,maxitems=3"`
}

// Briefing is the literary morning paragraph + tension meter. Title is
// markdown-flavored prose: one *italicized* word per beat. SuggestedFollowup
// is an optional one-line query the InputBar surfaces as ghost text.
type Briefing struct {
	Date              string `json:"date"                          zen:"required,format=date"`
	Eyebrow           string `json:"eyebrow"                       zen:"required,maxlen=80"`
	Title             string `json:"title"                         zen:"required,minlen=4,maxlen=160"`
	Summary           string `json:"summary"                       zen:"required,minlen=20,maxlen=400"`
	Tension           int    `json:"tension"                       zen:"required,min=0,max=100"`
	SuggestedFollowup string `json:"suggested_followup,omitempty"  zen:"maxlen=120"`
}

// ---------------------------------------------------------------------------
// Reflect-based JSON-Schema generator
// ---------------------------------------------------------------------------

// GenerateSchema walks a Go struct via reflection and emits a JSON-Schema map
// suitable for serializing to an OpenAI tool's function.parameters or for
// compiling with santhosh-tekuri/jsonschema. Tag conventions:
//
//	json:"name,omitempty"   — property name + optional flag
//	zen:"required,enum=a|b,maxlen=120,maxitems=4,format=date,min=0,max=100"
//
// The empty-string variant of an enum is supported via leading/trailing or
// adjacent pipes, e.g. enum=|personal → ["", "personal"].
func GenerateSchema(t reflect.Type) map[string]any {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return schemaForType(t, "")
}

func schemaForType(t reflect.Type, zenTag string) map[string]any {
	// `zen:"schema=<name>"` overrides reflection entirely with a
	// hand-rolled schema looked up by name. Used for fields whose Go
	// type doesn't translate to a Gemini-compatible JSON-Schema —
	// notably free-form maps. See customSchemas.
	if name, ok := customSchemaName(zenTag); ok {
		if fn, found := customSchemas[name]; found {
			return fn()
		}
	}
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		return schemaForStruct(t)
	case reflect.Slice, reflect.Array:
		s := map[string]any{
			"type":  "array",
			"items": schemaForType(t.Elem(), ""),
		}
		applyArrayConstraints(s, parseZen(zenTag))
		return s
	case reflect.Map:
		return map[string]any{
			"type":                 "object",
			"additionalProperties": schemaForType(t.Elem(), ""),
		}
	case reflect.String:
		s := map[string]any{"type": "string"}
		applyStringConstraints(s, parseZen(zenTag))
		return s
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		s := map[string]any{"type": "integer"}
		applyNumericConstraints(s, parseZen(zenTag))
		return s
	case reflect.Float32, reflect.Float64:
		s := map[string]any{"type": "number"}
		applyNumericConstraints(s, parseZen(zenTag))
		return s
	default:
		// Fallback for interface{} and friends — accept anything.
		return map[string]any{}
	}
}

func schemaForStruct(t reflect.Type) map[string]any {
	props := map[string]any{}
	required := []string{}

	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		jsonTag := f.Tag.Get("json")
		if jsonTag == "-" {
			continue
		}
		zenTag := f.Tag.Get("zen")
		// `zen:"-"` excludes the field from BOTH the LLM-facing schema
		// and the post-parse strict validator. Use this for fields that
		// are server-managed and never emitted by the model — including
		// them in the schema as map[string]any / map[string]string
		// produces an OBJECT shape (or an empty {} after relaxation)
		// that Gemini's OpenAPI 3.0 subset rejects with
		// 400 INVALID_ARGUMENT. Marshaling and runtime parsing of
		// server-set values stay unchanged because the Go struct
		// keeps the field; only the JSON-Schema serialization drops it.
		if zenTag == "-" {
			continue
		}
		name, omitempty := parseJSON(jsonTag, f.Name)
		zen := parseZen(zenTag)

		props[name] = schemaForType(f.Type, zenTag)

		if zen.required && !omitempty {
			required = append(required, name)
		}
	}

	out := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

// customSchemaName extracts the `<name>` from a `zen:"schema=<name>"`
// directive, when present. Returns ("", false) for any other tag shape.
// Kept narrow on purpose: this directive is the only one that fully
// replaces the reflection-based schema for a field.
func customSchemaName(zenTag string) (string, bool) {
	if zenTag == "" {
		return "", false
	}
	for _, part := range splitOutsidePipe(zenTag, ',') {
		part = strings.TrimSpace(part)
		if name, ok := strings.CutPrefix(part, "schema="); ok {
			return name, true
		}
	}
	return "", false
}

// customSchemas maps `zen:"schema=<name>"` directives to the
// hand-rolled schema each one returns. Add an entry here when a Go
// field needs a JSON-Schema shape the reflection generator can't
// produce — typically free-form maps that need declared properties to
// satisfy Gemini's structured output constraints.
var customSchemas = map[string]func() map[string]any{
	"action_target": actionTargetSchema,
	"expand_map":    expandMapSchema,
}

// actionTargetSchema declares every key that any action executor reads
// from an Action.Target value. Gemini's constrained decoder won't
// populate undeclared object properties, so listing them here is what
// makes "send_whatsapp" actually carry a `recipient`, "open_url"
// carry a `url`, etc. Keep this in sync with
// internal/action/executors_*.go — when an executor starts reading a
// new key, declare it here too. The schema deliberately omits
// `additionalProperties:false` so the strict validator tolerates new
// executor keys before the schema catches up.
//
// Types:
//   - boolean: `on` (mail flag toggle)
//   - string array: `recipients`, `tags`
//   - everything else: string (RFC3339 / ISO date / URL / etc are all
//     transmitted as strings; executors parse as needed)
func actionTargetSchema() map[string]any {
	str := map[string]any{"type": "string"}
	strArr := map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"at":             str, // generic time anchor (HH:MM or RFC3339)
			"body":           str,
			"category":       str,
			"context_id":     str,
			"context_kind":   str,
			"date":           str, // YYYY-MM-DD pairs with start/end (HH:MM)
			"description":    str,
			"due":            str,
			"due_date":       str,
			"end":            str, // HH:MM wall-clock — calendar add/move
			"end_iso":        str, // full RFC3339 — calendar add/move
			"event_uid":      str,
			"fact":           str,
			"fire_at":        str,
			"folder":         str,
			"location":       str,
			"message":        str,
			"name":           str,
			"note":           str,
			"on":             map[string]any{"type": "boolean"},
			"priority":       str,
			"query":          str,
			"recipient":      str,
			"recipients":     strArr,
			"seed":           str,
			"source_card_id": str,
			"start":          str, // HH:MM wall-clock — calendar add/move
			"start_iso":      str, // full RFC3339 — calendar add/move
			"steer":          str,
			"subject":        str,
			"tags":           strArr,
			"task_uid":       str,
			"title":          str,
			"to":             str,
			"uid":            str,
			"url":            str,
			"when":           str,
		},
	}
}

// expandMapSchema declares Card.Expand's shape: a free-form map whose
// keys are chosen by the LLM at synthesis time and whose values are
// strings. Empty `properties` is required because Gemini rejects
// OBJECT schemas without it; `additionalProperties` describes the
// value shape and is preserved by the strict validator (the relax
// pass strips it on the way to Gemini, leaving a clean
// `{type:object, properties:{}}` Gemini accepts).
func expandMapSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{},
		"additionalProperties": map[string]any{"type": "string"},
	}
}

// zenTag is the parsed form of one struct field's `zen:"..."` tag.
type zenTag struct {
	required bool
	enum     []string
	minLen   *int
	maxLen   *int
	min      *int
	max      *int
	minItems *int
	maxItems *int
	format   string
}

func parseZen(s string) zenTag {
	out := zenTag{}
	if s == "" {
		return out
	}
	for _, part := range splitOutsidePipe(s, ',') {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if part == "required" {
			out.required = true
			continue
		}
		key, val, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		switch key {
		case "enum":
			out.enum = strings.Split(val, "|")
		case "minlen":
			n, _ := strconv.Atoi(val)
			out.minLen = &n
		case "maxlen":
			n, _ := strconv.Atoi(val)
			out.maxLen = &n
		case "min":
			n, _ := strconv.Atoi(val)
			out.min = &n
		case "max":
			n, _ := strconv.Atoi(val)
			out.max = &n
		case "minitems":
			n, _ := strconv.Atoi(val)
			out.minItems = &n
		case "maxitems":
			n, _ := strconv.Atoi(val)
			out.maxItems = &n
		case "format":
			out.format = val
		}
	}
	return out
}

// splitOutsidePipe splits s on sep but treats `enum=a|b|c` as a single token
// (i.e. pipes inside a value never split). Simple parser: the only nesting
// we care about is `enum=...` whose value runs until the next comma.
func splitOutsidePipe(s string, sep byte) []string {
	out := []string{}
	last := 0
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			out = append(out, s[last:i])
			last = i + 1
		}
	}
	out = append(out, s[last:])
	return out
}

func parseJSON(tag, fallback string) (name string, omitempty bool) {
	if tag == "" {
		return fallback, false
	}
	parts := strings.Split(tag, ",")
	name = parts[0]
	if name == "" {
		name = fallback
	}
	for _, p := range parts[1:] {
		if p == "omitempty" {
			omitempty = true
		}
	}
	return
}

func applyStringConstraints(s map[string]any, z zenTag) {
	if z.minLen != nil {
		s["minLength"] = *z.minLen
	}
	if z.maxLen != nil {
		s["maxLength"] = *z.maxLen
	}
	if z.format != "" {
		s["format"] = z.format
	}
	if len(z.enum) > 0 {
		// Enum values stay as raw strings; an empty token means the empty string.
		vals := make([]any, 0, len(z.enum))
		for _, v := range z.enum {
			vals = append(vals, v)
		}
		s["enum"] = vals
	}
}

func applyNumericConstraints(s map[string]any, z zenTag) {
	if z.min != nil {
		s["minimum"] = *z.min
	}
	if z.max != nil {
		s["maximum"] = *z.max
	}
}

func applyArrayConstraints(s map[string]any, z zenTag) {
	if z.minItems != nil {
		s["minItems"] = *z.minItems
	}
	if z.maxItems != nil {
		s["maxItems"] = *z.maxItems
	}
}

// ---------------------------------------------------------------------------
// Compiled schema cache
// ---------------------------------------------------------------------------

var (
	cardSetSchemaOnce       sync.Once
	cardSetSchema           *jsonschema.Schema
	cardSchemaOnce          sync.Once
	cardSchema              *jsonschema.Schema
	subCardSchemaOnce       sync.Once
	subCardSchema           *jsonschema.Schema
	briefingSchemaOnce      sync.Once
	briefingSchema          *jsonschema.Schema
	injectCardSetSchemaOnce sync.Once
	injectCardSetSchema     *jsonschema.Schema
)

// CardSetSchema returns the compiled CardSet validator. Compiled once.
func CardSetSchema() *jsonschema.Schema {
	cardSetSchemaOnce.Do(func() {
		cardSetSchema = mustCompile("cards.json", GenerateSchema(reflect.TypeOf(CardSet{})))
	})
	return cardSetSchema
}

// InjectCardSetSchema returns the compiled InjectCardSet validator. Used
// by V2.3.0 P3's inject pipeline.
func InjectCardSetSchema() *jsonschema.Schema {
	injectCardSetSchemaOnce.Do(func() {
		injectCardSetSchema = mustCompile("inject_cards.json", GenerateSchema(reflect.TypeOf(InjectCardSet{})))
	})
	return injectCardSetSchema
}

// InjectCardSetSchemaMap returns the relaxed JSON-Schema map for
// constrained-decoding of the inject pipeline.
func InjectCardSetSchemaMap() map[string]any {
	return relaxSchemaForLLM(GenerateSchema(reflect.TypeOf(InjectCardSet{})))
}

// CardSchema returns the compiled single-Card validator. Used by reactive Ask.
func CardSchema() *jsonschema.Schema {
	cardSchemaOnce.Do(func() {
		cardSchema = mustCompile("card.json", GenerateSchema(reflect.TypeOf(Card{})))
	})
	return cardSchema
}

// BriefingSchema returns the compiled Briefing validator. Compiled once.
func BriefingSchema() *jsonschema.Schema {
	briefingSchemaOnce.Do(func() {
		briefingSchema = mustCompile("briefing.json", GenerateSchema(reflect.TypeOf(Briefing{})))
	})
	return briefingSchema
}

// CardSetSchemaMap returns the raw JSON-Schema map (not compiled). Used by
// the LLM client to populate response_format.json_schema.schema.
//
// The schema is relaxed for LLM-side constrained decoding: we keep the
// type + properties + required + enum shape, but strip length/count/format
// constraints and additionalProperties:false. Most decoders (LM Studio's
// included) bail to empty output when asked to satisfy minLength/format/
// additionalProperties simultaneously. We still validate against the strict
// schema post-parse, so quality holds — we just don't ask the decoder to
// enforce length floors mid-generation.
func CardSetSchemaMap() map[string]any {
	return relaxSchemaForLLM(GenerateSchema(reflect.TypeOf(CardSet{})))
}

// CardSchemaMap returns the raw JSON-Schema map for a single Card.
func CardSchemaMap() map[string]any {
	return relaxSchemaForLLM(GenerateSchema(reflect.TypeOf(Card{})))
}

// SubCardSchema returns the compiled single-SubCard validator. Used by
// the card-conversation surface (synth.Converse).
func SubCardSchema() *jsonschema.Schema {
	subCardSchemaOnce.Do(func() {
		subCardSchema = mustCompile("sub_card.json", GenerateSchema(reflect.TypeOf(SubCard{})))
	})
	return subCardSchema
}

// SubCardSchemaMap returns the relaxed JSON-Schema map for constrained
// decoding of a SubCard reply.
func SubCardSchemaMap() map[string]any {
	return relaxSchemaForLLM(GenerateSchema(reflect.TypeOf(SubCard{})))
}

// BriefingSchemaMap returns the raw JSON-Schema map for a Briefing.
func BriefingSchemaMap() map[string]any {
	return relaxSchemaForLLM(GenerateSchema(reflect.TypeOf(Briefing{})))
}

// relaxSchemaForLLM walks a JSON-Schema map and strips constraints that
// reliably break constrained-decoding backends (LM Studio's schema engine,
// llama.cpp grammars, vLLM guided JSON) or are rejected by remote
// providers like Gemini. Preserves: type, properties, required, items,
// enum, minItems, minimum, maximum. Strips: additionalProperties (when
// false), minLength, maxLength, format, maxItems.
//
// Rationale: integer-range constraints decode fine; minItems gates "at
// least one" semantics that the LLM should respect. The kinds we strip
// are the brittle ones that push decoders into emitting empty output
// or trigger upstream provider rejections. The strict versions of
// every stripped constraint still run post-parse via ValidateJSON
// (against CardSetSchema, etc.) so hollow-content protection holds.
//
// Why maxItems is stripped: Gemini 3 Flash preview rejects the cards
// schema with 400 INVALID_ARGUMENT when maxItems is present alongside
// the rest of the constraint mix, even on schemas the linter
// considers clean. Reduced repros narrow the trigger to maxItems
// specifically — removing every maxItems makes the same schema accepted
// across 5/5 runs. Other backends are fine with maxItems but don't
// strictly need it at decode time, so dropping it is behavior-neutral
// for them.
//
// Gemini compatibility note: free-form map fields (e.g. map[string]any)
// must NOT reach this pass — relaxation can't produce a shape Gemini
// accepts (bare {type:object} is rejected; bare {} is rejected). The
// canonical fix is `zen:"-"` on the struct field so the schema
// generator omits it entirely. See Action.Target / Card.Expand.
func relaxSchemaForLLM(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	stripped := map[string]bool{
		"minLength":            true,
		"maxLength":            true,
		"format":               true,
		"maxItems":             true, // Gemini 3 Flash preview rejects schemas with maxItems present
		"additionalProperties": true, // always strip — false is the only value we emit and it's the troublemaker
	}
	for k, v := range in {
		if stripped[k] {
			continue
		}
		out[k] = relaxValue(v)
	}
	return out
}

func relaxValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return relaxSchemaForLLM(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = relaxValue(e)
		}
		return out
	default:
		return v
	}
}

func mustCompile(name string, schema map[string]any) *jsonschema.Schema {
	b, err := json.Marshal(schema)
	if err != nil {
		panic(fmt.Sprintf("synth: marshal schema %s: %v", name, err))
	}
	c := jsonschema.NewCompiler()
	c.Draft = jsonschema.Draft7
	if err := c.AddResource(name, bytes.NewReader(b)); err != nil {
		panic(fmt.Sprintf("synth: add schema %s: %v", name, err))
	}
	s, err := c.Compile(name)
	if err != nil {
		panic(fmt.Sprintf("synth: compile schema %s: %v", name, err))
	}
	return s
}

// ValidateJSON validates raw JSON bytes against the compiled schema. Returns
// nil on success, an error describing the first failure on mismatch.
func ValidateJSON(s *jsonschema.Schema, raw []byte) error {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse json: %w", err)
	}
	if err := s.Validate(doc); err != nil {
		return err
	}
	return nil
}
