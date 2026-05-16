package eventbus

import (
	"encoding/json"
	"time"

	"github.com/zenocy/zeno-v2/internal/llm"
	"github.com/zenocy/zeno-v2/internal/metrics"
	"github.com/zenocy/zeno-v2/internal/projection"
	"github.com/zenocy/zeno-v2/internal/store"
)

// Event is the bus's typed payload. Each concrete event returns a stable
// kind string used by the SSE handler to pick an `event:` name and by
// observability to filter without reflection.
//
// V2.4 introduces this interface as a refactor of V2.3's `Bus[store.Card]`.
// V2.3's only payload was `card.appended`; V2.4 adds streaming trace +
// body deltas + run-lifecycle events + sensor observations.
type Event interface {
	Kind() string
}

// CardAppendedEvent fires whenever a fully-synthesized Card lands in the
// store and should be reflected in the live UI. V2.3 callers use the
// PublishCard shim to emit this without naming the type. SSE event name:
// `card.appended` — byte-equal to the V2.3 wire shape (the data payload
// is the marshaled Card itself, not a wrapper object).
type CardAppendedEvent struct {
	Card store.Card
}

func (CardAppendedEvent) Kind() string { return "card.appended" }

// TraceStepEvent fires during an active synth run when the LLM tool loop
// records a thought or tool step. RunID lets the UI scope steps to a
// single LiveSynthPanel instance; Stage names which leg of the run the
// step came from ("cards" | "briefing" | "ask" | "inject").
type TraceStepEvent struct {
	RunID string        `json:"run_id"`
	Stage string        `json:"stage"`
	Step  llm.TraceStep `json:"step"`
}

func (TraceStepEvent) Kind() string { return "trace.step" }

// SynthDeltaEvent fires for each coalesced chunk of body text streamed
// from the model during the briefing or Ask phases. Coalescing is the
// publisher's responsibility — see internal/synth/publisher.go in P2.
type SynthDeltaEvent struct {
	RunID string `json:"run_id"`
	Stage string `json:"stage"`
	Delta string `json:"delta"`
}

func (SynthDeltaEvent) Kind() string { return "synth.delta" }

// SynthStartedEvent marks the beginning of a synth run. The UI mounts
// the LiveSynthPanel on this event. Date is the briefing's logical date
// (matters for replay and for distinguishing morning runs from late
// reactive Asks against yesterday's projections).
type SynthStartedEvent struct {
	RunID string `json:"run_id"`
	Stage string `json:"stage"`
	Date  string `json:"date"`
}

func (SynthStartedEvent) Kind() string { return "synth.started" }

// SynthCompletedEvent marks the end of a synth run. The UI dissolves the
// LiveSynthPanel on this event after a 600ms settle delay. Stopped is
// the loop's terminal reason ("ok", "iteration_cap", "deadline", ...).
type SynthCompletedEvent struct {
	RunID   string `json:"run_id"`
	Stage   string `json:"stage"`
	Stopped string `json:"stopped"`
	TotalMs int64  `json:"total_ms"`
}

func (SynthCompletedEvent) Kind() string { return "synth.completed" }

// SensorEventObservedEvent fires whenever a sensor observes something
// new (mail.received, cal.event_changed, etc.) and has just appended it
// to the durable log. V2.4's inject subscriber consumes these to drive
// reactive synthesis without waiting for the cron tick.
//
// **Bus-internal**: this event does NOT flow over SSE. The SSE handler
// recognizes it in the type-switch and skips it. Only Go subscribers
// (the inject subscriber goroutine) read it.
//
// The trailing underscore on the field name avoids colliding with the
// Kind() method while keeping the JSON tag clean for any future
// consumers that decide to surface the field.
type SensorEventObservedEvent struct {
	Kind_      string         `json:"kind"`
	EvidenceID string         `json:"evidence_id"`
	Payload    map[string]any `json:"payload,omitempty"`
}

func (SensorEventObservedEvent) Kind() string { return "sensor.event_observed" }

// V2.5.0: concerns events. Published as state changes happen so the
// review surface can update without polling. Phase 1 publishes
// ConcernStateChangedEvent for transitions; Phase 2 adds ConcernProposedEvent
// from recognition and RetrospectiveProgressEvent during retrospective
// tagging. Phase 4 wires the SSE consumer.

// ConcernProposedEvent fires when the recognition pass produces a new
// proposal. Source distinguishes model-proposed from user-declared (the
// latter auto-promotes to active and skips the review surface). The UI
// uses this to highlight the "Pending review" section.
type ConcernProposedEvent struct {
	ConcernID   string  `json:"concern_id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Source      string  `json:"source"`
	Confidence  float64 `json:"confidence"`
}

func (ConcernProposedEvent) Kind() string { return "concern.proposed" }

// ConcernStateChangedEvent fires on every lifecycle transition. Includes
// the prior state so the UI can animate the row's section change without
// re-fetching the full list.
type ConcernStateChangedEvent struct {
	ConcernID    string  `json:"concern_id"`
	PriorState   string  `json:"prior_state"`
	NewState     string  `json:"new_state"`
	MergedIntoID *string `json:"merged_into_id,omitempty"`
}

func (ConcernStateChangedEvent) Kind() string { return "concern.state_changed" }

// ConcernTaggedEvent fires when an observation is joined to a concern.
// During retrospective tagging this event fires per batch (not per row)
// to keep the bus quiet; live tagging from the recognition post-pass
// fires per row.
type ConcernTaggedEvent struct {
	ConcernID   string   `json:"concern_id"`
	EventIDs    []string `json:"event_ids"`
	Source      string   `json:"source"`
	BatchOrigin string   `json:"batch_origin"` // "retrospective" | "post_tag" | "user"
}

func (ConcernTaggedEvent) Kind() string { return "concern.tagged" }

// RetrospectiveProgressEvent fires per batch during retrospective tagging
// so the review surface can render *"tagging history... 47 of ~200"*.
// Status is one of "running" | "completed" | "cancelled" | "failed".
type RetrospectiveProgressEvent struct {
	ConcernID string `json:"concern_id"`
	Processed int    `json:"processed"`
	Total     int    `json:"total"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
}

func (RetrospectiveProgressEvent) Kind() string { return "concern.retrospective_progress" }

// ConcernRetirementProposedEvent fires when the daily recognition pass
// surveys active concerns for inactivity and finds one whose
// last_active_at is older than auto_retire_days. The event fires once
// per concern per crossing — the recognition pass guards against
// re-emission via an audit-log idempotency check. The UI surfaces the
// state via the projection-derived `ready_to_retire` field on the
// concern list; this event triggers a cache refresh so the badge
// appears live.
type ConcernRetirementProposedEvent struct {
	ConcernID    string `json:"concern_id"`
	DaysInactive int    `json:"days_inactive"`
}

func (ConcernRetirementProposedEvent) Kind() string { return "concern.retirement_proposed" }

// V2.6.0 (or wherever this lands): replace UI polling with SSE-driven
// updates. The events below carry full state-change payloads so the React
// hooks can update React Query caches via setQueryData and stop polling
// entirely. See doc plan: vivid-sauteeing-cherny.md.
//
// Cycle-avoidance note: events whose payload type lives in a package that
// itself publishes (whatsapp.Service, settings via the api package's
// private settingsDTO, memory via the api package's private memoryFactDTO)
// use either inline fields or json.RawMessage to keep eventbus from
// importing the publisher's package. Events whose payload type lives in
// a non-publishing package (projection.*, metrics.Snapshot) embed the
// concrete type directly.

// WhatsAppStatusEvent fires whenever the WhatsApp service's Status
// transitions: connected/disconnected, paired, logged out, runtime
// config updated, or an error captured. Wire shape matches GET
// /api/whatsapp/status (statusDTO + nested configDTO) so the UI's
// useWhatsAppStatus hook can drop the data straight into its cache via
// setQueryData. Fields are inlined here rather than typed against
// internal/whatsapp to avoid an eventbus → whatsapp → eventbus cycle.
type WhatsAppStatusEvent struct {
	Enabled     bool      `json:"enabled"`
	HasSession  bool      `json:"has_session"`
	Connected   bool      `json:"connected"`
	LoggedIn    bool      `json:"logged_in"`
	OwnJID      string    `json:"own_jid,omitempty"`
	OwnPushName string    `json:"own_push_name,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	LastSeenAt  time.Time `json:"last_seen_at,omitempty"`
	PairedAt    time.Time `json:"paired_at,omitempty"`

	Config WhatsAppConfigPayload `json:"config"`
}

// WhatsAppConfigPayload mirrors api.configDTO. Inlining the fields keeps
// eventbus decoupled from the api/whatsapp packages.
type WhatsAppConfigPayload struct {
	MentionName        string   `json:"mention_name"`
	AllowedDMs         []string `json:"allowed_dms"`
	MinChatIntervalMs  int      `json:"min_chat_interval_ms"`
	MaxConcurrentSynth int      `json:"max_concurrent_synth"`
	PerChatBuffer      int      `json:"per_chat_buffer"`
}

func (WhatsAppStatusEvent) Kind() string { return "whatsapp.status_changed" }

// TaskCreatedEvent fires after a successful task create through the API
// or action handler. Payload is the full new task in the OpenTasksTask
// shape so the React Query cache for ["tasks","all"] can splice it in.
type TaskCreatedEvent struct {
	Task projection.OpenTasksTask `json:"task"`
}

func (TaskCreatedEvent) Kind() string { return "task.created" }

// TaskCompletedEvent fires after a successful task completion. Payload
// is the post-completion task so the cache can replace the row by UID.
type TaskCompletedEvent struct {
	Task projection.OpenTasksTask `json:"task"`
}

func (TaskCompletedEvent) Kind() string { return "task.completed" }

// TaskDeletedEvent fires after a successful task deletion. Only the UID
// is needed to drop the row from the client cache.
type TaskDeletedEvent struct {
	UID string `json:"uid"`
}

func (TaskDeletedEvent) Kind() string { return "task.deleted" }

// TaskReminderSetEvent fires after a reminder is attached to a task.
// Payload is the post-update task; reminder metadata travels on the
// task row itself when the projection includes it.
type TaskReminderSetEvent struct {
	Task projection.OpenTasksTask `json:"task"`
}

func (TaskReminderSetEvent) Kind() string { return "task.reminder_set" }

// TaskEditedEvent fires after a successful PATCH /api/tasks/:uid edit.
// Payload is the post-edit task so the React Query cache for ["tasks"]
// can replace the row by UID without a refetch.
type TaskEditedEvent struct {
	Task projection.OpenTasksTask `json:"task"`
}

func (TaskEditedEvent) Kind() string { return "task.edited" }

// SettingsChangedEvent fires after PUT /api/settings successfully
// reloads. Payload is the wire-DTO from the api package as raw JSON to
// avoid an import cycle (eventbus → api → eventbus).
type SettingsChangedEvent struct {
	Settings json.RawMessage `json:"settings"`
}

func (SettingsChangedEvent) Kind() string { return "settings.changed" }

// WeatherUpdatedEvent fires when the weather sensor commits a fresh
// snapshot. Payload is the full WeatherView the UI's right-rail widget
// expects (same shape returned by GET /api/projections/weather).
type WeatherUpdatedEvent struct {
	Weather *projection.WeatherView `json:"weather"`
}

func (WeatherUpdatedEvent) Kind() string { return "weather.updated" }

// StockUpdatedEvent fires when the stock sensor commits a fresh
// snapshot. Payload is the full StockView (matches the GET /api/projections/stock
// shape). Nil StockView is valid when the user cleared their tickers.
type StockUpdatedEvent struct {
	Stock *projection.StockView `json:"stock"`
}

func (StockUpdatedEvent) Kind() string { return "stock.updated" }

// CalendarTodayChangedEvent fires when the caldav poller sees an
// add/change/remove that affects today's calendar list. Payload is the
// full filtered list (matches GET /api/projections/calendar/today).
type CalendarTodayChangedEvent struct {
	Events []projection.CalendarEvent `json:"events"`
}

func (CalendarTodayChangedEvent) Kind() string { return "calendar.today_changed" }

// CalendarTomorrowChangedEvent fires alongside CalendarTodayChangedEvent
// from the same poll cycle so the right-rail Tomorrow horizon stays in
// sync with Today (otherwise a change that affects both windows would
// produce visible drift). Payload mirrors GET /api/projections/calendar/tomorrow.
type CalendarTomorrowChangedEvent struct {
	Events []projection.CalendarEvent `json:"events"`
}

func (CalendarTomorrowChangedEvent) Kind() string { return "calendar.tomorrow_changed" }

// CalendarWeekChangedEvent fires alongside the today/tomorrow events
// from the same poll cycle. Payload mirrors GET /api/projections/calendar/week.
type CalendarWeekChangedEvent struct {
	Events []projection.CalendarEvent `json:"events"`
}

func (CalendarWeekChangedEvent) Kind() string { return "calendar.week_changed" }

// MemoryChangedEvent fires after a memory mutation (insert/update/
// soft-delete) commits. Payload is the full list response wire shape
// (api.listResponse) as raw JSON to avoid an api → eventbus → api cycle.
type MemoryChangedEvent struct {
	Memory json.RawMessage `json:"memory"`
}

func (MemoryChangedEvent) Kind() string { return "memory.changed" }

// StatsSnapshotEvent fires from the server-side metrics ticker (see
// internal/http/api/metrics_publisher.go) on a fixed cadence so the UI
// stats panel can stop polling /api/metrics/snapshot. Payload is the
// canonical metrics.Snapshot.
type StatsSnapshotEvent struct {
	Stats metrics.Snapshot `json:"stats"`
}

func (StatsSnapshotEvent) Kind() string { return "stats.snapshot" }

// HealthChangedEvent fires from the metrics ticker only when the health
// state transitions (or every 60s as a heartbeat for first-mount
// clients). Fields mirror the GET /api/health HealthResponse wire shape.
type HealthChangedEvent struct {
	OK           bool       `json:"ok"`
	Version      string     `json:"version"`
	Uptime       string     `json:"uptime"`
	DBOK         bool       `json:"db_ok"`
	LLMReachable bool       `json:"llm_reachable"`
	LLMError     string     `json:"llm_error,omitempty"`
	LastSynthAt  *time.Time `json:"last_synth_at,omitempty"`
	LastSyncAt   *time.Time `json:"last_sync_at,omitempty"`
}

func (HealthChangedEvent) Kind() string { return "health.changed" }

// ConcernObservationsChangedEvent fires when an observation is joined
// to or detached from an expanded concern panel. The UI scopes the
// update to the matching concern's observations cache.
type ConcernObservationsChangedEvent struct {
	ConcernID    string          `json:"concern_id"`
	Observations json.RawMessage `json:"observations"`
}

func (ConcernObservationsChangedEvent) Kind() string { return "concern.observations_changed" }
