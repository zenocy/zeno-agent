package action

// CanonicalIntents is the V2.8.0 source of truth for the supported
// action verbs. Each entry pairs the wire-level intent name with its
// Mode and a short description used by /api/actions/modes for UI
// hydration. The Action.Intent enum in internal/synth/schema.go must
// stay in sync with this list — the schema's empty-string alternative
// keeps legacy fixtures parsing while postProcessIntent backfills.
//
// Phase 0 only registers Executors for dismiss/snooze/open_url. The
// rest carry an entry here so /api/actions/modes returns the full
// vocabulary at boot, and the handler's "no executor" branch can
// gracefully degrade when a UI emits a verb its backend doesn't yet
// implement (cross-version compatibility during the V2.8 phase rollout).
type CanonicalIntent struct {
	Intent      string
	Mode        Mode
	Description string
}

var CanonicalIntents = []CanonicalIntent{
	{"dismiss", Mode1Click, "Hide the card permanently."},
	{"snooze", Mode1Click, "Hide the card for the rest of today."},
	{"mark_read", Mode1Click, "Set the IMAP \\Seen flag on the source thread (target.subject required — copy the source thread's subject verbatim)."},
	{"move_mail", Mode1Click, "Move the source thread to a folder (target.folder; target.subject required — copy the source thread's subject verbatim)."},
	{"draft_reply", ModePreflight, "Generate and save a draft reply to the source thread (target.subject required — copy the source thread's subject verbatim; target.steer optional)."},
	{"send_reply", ModePreflight, "Generate, preview, and send a reply via SMTP (target.subject required — copy the source thread's subject verbatim; target.steer optional)."},
	{"forward", ModePreflight, "Generate, preview, and forward the source thread (target.subject required — copy the source thread's subject verbatim; target.to required; target.note optional)."},
	{"add_event", ModePreflight, "Create a new calendar event (target.start, target.end, target.title)."},
	{"block_calendar", Mode1Click, "Block a time window on the user's calendar."},
	{"rsvp_yes", Mode1Click, "Set the user's PARTSTAT=ACCEPTED on the source event."},
	{"rsvp_no", Mode1Click, "Set the user's PARTSTAT=DECLINED on the source event."},
	{"rsvp_maybe", Mode1Click, "Set the user's PARTSTAT=TENTATIVE on the source event."},
	{"add_concern", Mode1Click, "Persist a new concern (target.name required, e.g. \"Series B raise\"; target.description optional — falls back to the card's sub when omitted)."},
	{"add_memory", Mode1Click, "Persist a new derived-memory fact (target.subject required, lowercased noun phrase like \"dana lopez\" or \"morning routine\"; target.fact required — the actual sentence to remember; target.category optional, e.g. \"people\" / \"work\" / \"personal\")."},
	{"ask_followup", Mode1Click, "Run a reactive synth pass seeded with this card's context."},
	{"open_url", Mode1Click, "Open target.url in a new tab; logs the click."},

	// V2.8.1 expansion.
	{"flag_mail", Mode1Click, "Toggle the IMAP \\Flagged star on the source thread (target.subject; target.on=true|false, default true)."},
	{"reschedule_event", ModePreflight, "Move an existing calendar event to a new time (target.uid, target.start, target.end, target.date)."},
	{"cancel_event", ModeConfirm, "Delete an existing calendar event (target.uid). Destructive; user must confirm."},
	{"pin_card", Mode1Click, "Pin the card so it survives across days until unpinned."},
	{"unpin_card", Mode1Click, "Unpin a previously pinned card."},
	{"set_reminder", Mode1Click, "Schedule a reminder card for a future time (target.when as RFC3339 or +<N>(m|h|d), target.title, target.body)."},
	{"add_task", Mode1Click, "Append a new task to the local Markdown tasks file (target.title, target.due, target.tags, target.priority)."},
	{"complete_task", Mode1Click, "Mark a task complete in the local Markdown tasks file (target.uid)."},
	{"delete_task", Mode1Click, "Remove a task line from the local Markdown tasks file (target.uid)."},
	{"edit_task", Mode1Click, "Update fields on an existing task (target.uid, target.title?, target.due_date?)."},

	// V2.12: outbound WhatsApp send. Distinct from `send_reply`, which is
	// SMTP/email — never use `send_reply` for chat messages.
	{"send_whatsapp", ModePreflight, "Send a WhatsApp message to a contact (target.recipient required; target.message OR target.steer for the body). Use this — NOT send_reply — for any WhatsApp/chat message."},
}

// CanonicalIntentMap returns CanonicalIntents indexed by Intent name.
// Allocated each call; cheap (16 entries).
func CanonicalIntentMap() map[string]CanonicalIntent {
	out := make(map[string]CanonicalIntent, len(CanonicalIntents))
	for _, c := range CanonicalIntents {
		out[c.Intent] = c
	}
	return out
}

// IsCanonical reports whether intent is one of the V2.8.0 vocabulary
// entries. Used by the handler to distinguish "client emitted a known
// verb whose Executor isn't wired yet (degrade gracefully)" from
// "client emitted garbage (reject)".
func IsCanonical(intent string) bool {
	_, ok := CanonicalIntentMap()[intent]
	return ok
}
