// Package action handles user-initiated card actions (dismiss, snooze,
// draft a reply, block calendar time, ...). V2.8.0 lifted the V2.0–V2.7
// MVP restriction that actions were log-only: each Intent now has a
// typed Executor that can write to mail, calendar, concerns, memory,
// or trigger a reactive synth follow-up. Executors declare a Mode that
// controls whether the click commits immediately or runs preview-then-
// confirm through a UI modal.
package action

import (
	"context"
	"time"

	"github.com/sirupsen/logrus"

	logp "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/store"
	"github.com/zenocy/zeno-v2/internal/synth"
)

// Mode controls whether an action commits immediately or requires a
// confirmation step through the UI modal. The handler reads it off the
// Executor at dispatch time; the UI reads it off /api/actions/modes at
// boot so the modal/no-modal choice is consistent on both sides.
type Mode string

const (
	// Mode1Click: execute immediately on the first POST. Used for
	// reversible or in-DB-only operations (dismiss, snooze, mark read,
	// save draft, add concern/memory, ask follow-up).
	Mode1Click Mode = "one_click"

	// ModePreflight: first POST (confirm:false) builds a preview without
	// writing; second POST (confirm:true) commits. Used for ops where
	// the user benefits from inspecting the artifact before commit
	// (draft reply preview, add event details, send reply).
	ModePreflight Mode = "preflight"

	// ModeConfirm: single POST that requires confirm:true. Used for
	// destructive operations where a server-side preview is unhelpful
	// (e.g. delete event). The UI is responsible for collecting
	// confirmation client-side before the only POST.
	ModeConfirm Mode = "confirm"
)

// ExecCtx is what every Executor receives. Card may be nil for reactive
// cards that aren't persisted in the store; Executors that need a Card
// must check and fail gracefully.
type ExecCtx struct {
	Intent  string
	Card    *store.Card
	Now     time.Time
	TZ      *time.Location
	Today   string // YYYY-MM-DD in TZ; pre-formatted for snooze and date-relative intents
	Confirm bool
	Target  map[string]any

	EventLog logp.Writer
	Logger   *logrus.Entry
}

// Result is what every Executor returns. The handler writes the audit
// row (KindUserActionTaken) plus an optional EventKind/EventPayload
// outcome row, then serializes the rest of Result as the HTTP response.
type Result struct {
	OK             bool           `json:"ok"`
	EventKind      string         `json:"-"`
	EventPayload   map[string]any `json:"-"`
	OptimisticHide bool           `json:"hide,omitempty"`
	Preview        map[string]any `json:"preview,omitempty"`
	NeedsConfirm   bool           `json:"needs_confirm,omitempty"`
	Followup       *synth.Card    `json:"followup,omitempty"`
	Toast          string         `json:"toast,omitempty"`
}

// Executor is the per-intent unit. Mode is declared statically so the
// /api/actions/modes endpoint can build its table at boot without
// invoking Execute. Execute may consult ExecCtx.Confirm to differentiate
// preview vs commit when Mode is ModePreflight.
type Executor interface {
	Mode() Mode
	Execute(ctx context.Context, e ExecCtx) (Result, error)
}

// Registry is the intent → Executor map. Built once at boot in
// cmd/zeno/main.go. Lookup is read-only after boot.
type Registry struct {
	execs map[string]Executor
}

// NewRegistry returns an empty Registry. Call Register for each intent.
func NewRegistry() *Registry {
	return &Registry{execs: make(map[string]Executor)}
}

// Register attaches ex as the handler for intent. Last registration
// wins; intentional for tests overriding production wiring.
func (r *Registry) Register(intent string, ex Executor) {
	r.execs[intent] = ex
}

// Lookup returns the Executor for intent. ok=false signals "no executor
// registered" — handler falls back to a no-op log-only path so the
// legacy KindUserActionTaken audit trail keeps working for unknown
// verbs (e.g. a UI on a newer version emitting an intent the server
// doesn't yet implement).
func (r *Registry) Lookup(intent string) (ex Executor, ok bool) {
	ex, ok = r.execs[intent]
	return
}

// Modes returns the {intent → Mode} table the /api/actions/modes
// endpoint serves. Allocates a fresh map; safe to mutate by callers.
func (r *Registry) Modes() map[string]Mode {
	out := make(map[string]Mode, len(r.execs))
	for intent, ex := range r.execs {
		out[intent] = ex.Mode()
	}
	return out
}

// Intents returns the registered intent names. Stable order is not
// guaranteed; sort at the call site if needed.
func (r *Registry) Intents() []string {
	out := make([]string, 0, len(r.execs))
	for intent := range r.execs {
		out = append(out, intent)
	}
	return out
}
