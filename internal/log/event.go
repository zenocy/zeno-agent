package log

import (
	"time"

	"gorm.io/datatypes"
)

// Event is one row in the observation log. The log is append-only and is the
// spine the projections fold over.
type Event struct {
	ID      string         `gorm:"primaryKey;type:text"`
	TS      time.Time      `gorm:"index;not null"`
	Kind    string         `gorm:"index;not null;type:text"`
	Source  string         `gorm:"type:text"`
	Payload datatypes.JSON `gorm:"type:text"`
}

// Event kinds. Phase 0 only declares the system-level ones; sensors and synth
// add to this list as they come online.
const (
	KindSynthRunStarted             = "synth.run_started"
	KindSynthRunCompleted           = "synth.run_completed"
	KindSynthFailed                 = "synth.failed"
	KindSynthBriefingRetryScheduled = "synth.briefing_retry_scheduled"
	KindSynthBriefingRetryCompleted = "synth.briefing_retry_completed"
	KindSyncCompleted               = "sync.completed"
	KindUserActionTaken             = "user.action_taken"
	KindUserAsk                     = "user.ask"

	// V2.2.0: derived-memory event kinds.
	KindMemoryAdded             = "memory.added"              // user-typed manual add (Phase 3)
	KindMemoryEdited            = "memory.edited"             // Phase 3
	KindMemoryDeleted           = "memory.deleted"            // soft delete (Phase 3)
	KindMemoryCandidates        = "memory.candidates"         // raw `remember:` lines from a synth run (Phase 2)
	KindMemoryConsolidated      = "memory.consolidated"       // post-consolidator deltas (Phase 2)
	KindMemoryConsolidateFailed = "memory.consolidate.failed" // best-effort consolidator failure audit (Phase 2)

	// V2.3.0: adaptive-state event kinds.
	KindSynthStateChanged        = "synth.state_changed"         // detected state differs from prior briefing for the same date (Phase 1)
	KindSynthMessageInject       = "synth.message_inject"        // inject pipeline produced a card + fragment (Phase 3)
	KindSynthMessageInjectFailed = "synth.message_inject.failed" // inject pipeline errored (Phase 3)

	// V2.4.0: live-trace audit kinds. Emitted at run start/end ONLY — per-step
	// trace events and per-token body deltas are bus-only (ephemeral). These
	// give us a durable time-series of "the user saw a live trace at T".
	KindSynthLiveStarted   = "synth.live_started"
	KindSynthLiveCompleted = "synth.live_completed"

	// V2.5.0: concerns event kinds. Every state-changing operation on a
	// concern emits exactly one of these to the durable log so the
	// review surface can render an audit trail and the eval harness can
	// score recognition + tagging behavior post-hoc.
	KindConcernCreated      = "concern.created"       // Phase 1: model-proposed or user-declared
	KindConcernApproved     = "concern.approved"      // Phase 2: proposed → active
	KindConcernDismissed    = "concern.dismissed"     // Phase 2: proposed → soft-deleted (denylist)
	KindConcernStateChanged = "concern.state_changed" // any lifecycle move (active → paused, etc)
	KindConcernRenamed      = "concern.renamed"       // PATCH /api/concerns/:id with name change
	KindConcernTagged       = "concern.tagged"        // observation joined to concern
	KindConcernUntagged     = "concern.untagged"      // observation removed from concern
	KindConcernMerged       = "concern.merged"        // source → merged into target
	KindConcernSplit        = "concern.split"         // source ended; new concerns at active

	// V2.5.0 Phase 2 — retrospective tagging audit kinds.
	KindConcernRetrospectiveStarted   = "concern.retrospective_started"
	KindConcernRetrospectiveCompleted = "concern.retrospective_completed"
	KindConcernRetrospectiveCancelled = "concern.retrospective_cancelled"
	KindConcernRetrospectiveFailed    = "concern.retrospective_failed"
	// V2.5.0 Phase 2 — recognition daily pass.
	KindConcernRecognitionRun = "concern.recognition_run"
	// V2.5.0 Phase 5 — auto-retire survey. One row per concern per
	// crossing of the auto_retire_days threshold; the survey is
	// idempotent (a concern already crossed will not re-emit until it
	// re-engages and crosses again).
	KindConcernRetirementProposed = "concern.retirement_proposed"

	// Operational: periodic metrics snapshot written by internal/metrics.Emitter.
	KindStatsSnapshot = "stats.snapshot"

	// Phase 1: sensor event kinds.
	KindMailReceived      = "mail.received"
	KindMailThreadUpdated = "mail.thread_updated"
	KindCalEventSeen      = "cal.event_seen"
	KindCalEventChanged   = "cal.event_changed"

	// Full UID set per CalDAV Sync, scoped to the listing window
	// (now-1h … now+lookahead). Latest is authoritative — projections
	// use it to drop events that vanished between polls (user deleted
	// in their calendar UI), since cal.event_seen alone never retracts.
	KindCalListSnapshot = "cal.list_snapshot"
	KindWeatherSnapshot = "weather.snapshot"
	KindIMAPCursor      = "imap.cursor"

	// Full UID set per IMAP folder per poll. The latest event per folder
	// is authoritative — projections use it to filter mail.received down
	// to messages still present in the folder, so externally
	// archived/deleted mail drops out within one poll cycle.
	KindIMAPInboxSnapshot = "imap.inbox_snapshot"

	// V2.6: Jina saved-search sensor. One row per saved search per
	// refresh; payload carries the rendered top-N results so the inject
	// detector / cards loop can reach them via the read tools.
	KindWebSearchResult = "web.search.result"

	// V2.6: Tasks sensor. One snapshot row per parsed task per file
	// scan, plus a status_changed audit row when an existing task's
	// completed/due fields shift, plus a single cursor row per scan
	// recording the file mtime so the next tick can short-circuit.
	KindTaskSnapshot      = "task.snapshot"
	KindTaskStatusChanged = "task.status_changed"
	KindTaskCursor        = "task.cursor"

	// Full UID set parsed from the tasks file on each successful pass.
	// Latest is authoritative — projections drop tasks the user
	// deleted from the file, since task.snapshot alone never retracts.
	KindTaskListSnapshot = "task.list_snapshot"

	// V2.7: WhatsApp integration. Lifecycle events for the whatsmeow
	// socket plus inbound/outbound message audit. Group messages that
	// do not @-mention Zeno and DMs from non-allowlisted senders are
	// dropped at the receive boundary and emit NO events — that is the
	// privacy contract documented in docs/whatsapp.md and asserted in
	// internal/whatsapp/service_test.go.
	KindWhatsAppConnected         = "whatsapp.connected"
	KindWhatsAppDisconnected      = "whatsapp.disconnected"
	KindWhatsAppLoggedOut         = "whatsapp.logged_out"
	KindWhatsAppMessageRecv       = "whatsapp.message.received"
	KindWhatsAppMessageSent       = "whatsapp.message.sent"
	KindWhatsAppMessageFailed     = "whatsapp.message.failed"
	KindWhatsAppMessageSuppressed = "whatsapp.message.suppressed" // V2.13.0: inbound matched an open ExpectedReply; reactive auto-reply was skipped.

	// Stock sensor. One snapshot row per ticker per poll (only when the
	// price or as_of changes vs the previous snapshot), plus an alert
	// row on the leading edge of a configurable threshold breach. The
	// alert is the trigger the inject detector subscribes to.
	KindStockSnapshot = "stock.snapshot"
	KindStockAlert    = "stock.alert"

	// V2.8.0: action surface outcome events. Each Executor in
	// internal/action emits its kind-specific outcome event in
	// addition to the legacy KindUserActionTaken row, so audit
	// queries can ask "what mail did Zeno draft today?" without
	// joining payload-keyed dispatch tables. Only the verbs that
	// produce a state change emit an outcome row; dismiss/snooze
	// keep firing user.action_taken alone (they are themselves the
	// state change and adding a parallel row would double-count).
	KindMailDraftSaved        = "mail.draft_saved"
	KindMailMarkedRead        = "mail.marked_read"
	KindMailMoved             = "mail.moved"
	KindMailSent              = "mail.sent"
	KindCalEventCreated       = "cal.event_created"
	KindCalEventBlocked       = "cal.event_blocked"
	KindCalRSVPSent           = "cal.rsvp_sent"
	KindConcernAddedViaAction = "concern.added_via_action"
	KindMemoryAddedViaAction  = "memory.added_via_action"
	KindAskFollowup           = "ask.followup"
	KindCardConverse          = "card.converse"    // V2.10: card-conversation turn appended
	KindActionPreflight       = "action.preflight" // preview built, not committed
	KindActionFailed          = "action.failed"    // executor returned an error or 412/etc

	// V2.8.1: dropped at synth-time post-process because the action's
	// final intent isn't in the live registry's wired set. Payload is
	// {card_id, intent, label}; the row gives us a feedback loop on
	// prompt drift over time.
	KindActionDropped = "action.dropped"

	// V2.8.1: action surface expansion outcome events.
	KindMailFlagged         = "mail.flagged"
	KindCalEventRescheduled = "cal.event_rescheduled"
	KindCalEventCancelled   = "cal.event_cancelled"
	KindCardPinned          = "card.pinned"
	KindCardUnpinned        = "card.unpinned"
	KindReminderSet         = "reminder.set"   // emitted by SetReminderExec; payload carries task_uid + fire_at
	KindReminderFired       = "reminder.fired" // deprecated as of V2.11 — use KindTaskAlarmFired (kept registered so historical logs parse)
	KindTaskAddedViaAction  = "task.added_via_action"
	KindTaskDeleted         = "task.deleted"
	KindTaskEdited          = "task.edited" // emitted by EditTaskExec when title or due_date changes

	// V2.11: tasks/reminders unification. The sweeper writes this row
	// each time a task's fire_at trips (task.status_changed already
	// covers the [ ]→[x] transition). Replaces KindReminderFired.
	KindTaskAlarmFired = "task.alarm_fired"
)
