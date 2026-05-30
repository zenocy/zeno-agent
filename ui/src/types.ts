export type State =
  | "morning_calm"
  | "pre_meeting"
  | "deep_work"
  | "message_inject"
  | "end_of_day";

export interface Briefing {
  date: string;
  eyebrow: string;
  title: string;
  summary: string;
  tension: number;
  suggested_followup?: string;
  state?: State;
}

// V2.8.0: structured action verbs. `intent` is the canonical verb the
// backend dispatches on (see internal/action/intent_table.go); `target`
// carries intent-specific parameters. Both are optional at the type
// level for forward compatibility — pre-V2.8 cards from the store may
// still arrive without them, in which case the server-side
// postProcessIntent has already inferred the intent before serializing.
export type ActionIntent =
  | "dismiss"
  | "snooze"
  | "mark_read"
  | "move_mail"
  | "draft_reply"
  | "send_reply"
  | "forward"
  | "add_event"
  | "block_calendar"
  | "rsvp_yes"
  | "rsvp_no"
  | "rsvp_maybe"
  | "add_concern"
  | "add_memory"
  | "ask_followup"
  | "open_url"
  | "set_reminder"
  | "add_task"
  | "complete_task"
  | "delete_task";

export type ActionMode = "one_click" | "preflight" | "confirm";

export interface CardAction {
  label: string;
  primary?: boolean;
  intent?: ActionIntent | string;
  target?: Record<string, unknown>;
}

// V2.8.0: GET /api/actions/modes payload — the catalog of supported
// intents, their default Mode, a one-line description, and whether
// the running server has an Executor wired for it. The UI fetches this
// once at boot and uses Mode to decide modal-vs-immediate dispatch.
export interface IntentEntry {
  intent: ActionIntent | string;
  mode: ActionMode;
  description: string;
  wired: boolean;
}

export interface IntentModesResponse {
  intents: IntentEntry[];
}

// V2.8.0: POST /api/cards/:id/action response shape. Replaces the
// pre-V2.8 204 No Content. Followup is the reactive Card returned for
// ask_followup intents; Preview carries the modal preview body when
// NeedsConfirm is true.
export interface ActionResult {
  ok: boolean;
  hide?: boolean;
  preview?: Record<string, unknown>;
  needs_confirm?: boolean;
  followup?: Card;
  toast?: string;
}

// CardOrigin is the documented set of values the server stamps on a Card's
// `origin` field. V2.4 P3 adds "ask" for reactive Ask cards delivered via
// SSE. The Card.origin field stays typed as a plain string for forward
// compatibility (the server is the source of truth for new origins).
export type CardOrigin = "" | "morning" | "inject" | "ask";

export interface CardSource {
  t: string;
  u: string;
}

// One rolled-up child of a kind="digest" card (V2.x). The server collapses
// many low-signal items (newsletters, minor threads) into one card to cut
// visual repetition; `ref` carries the item's entity key for promotion.
export interface DigestItem {
  title: string;
  sub?: string;
  src?: string;
  ref?: string;
}

// A serve-time resolved live value (V2.x). The server substitutes the value
// into meta/sub in place; this carries the freshness metadata so the UI can
// grey out a stale reading or show an "as of" hint.
export interface LiveChip {
  slot: string;
  kind: string;
  text: string;
  stale?: boolean;
  as_of?: string;
}

export interface Card {
  id: string;
  date: string;
  src: string;
  src_label: string;
  rel: "high" | "med" | "low";
  kind?: string;
  title: string;
  sub: string;
  // Multi-paragraph elaboration populated by the reactive Ask flow only
  // when the request came from the in-app text surface. Absent on
  // morning cards and WhatsApp-origin cards. Paragraphs are separated
  // by blank lines (`\n\n`).
  body?: string;
  // Web citations the model used when answering. Populated only when
  // `search_web` or `read_url` was called this turn. Rendered as
  // clickable links below the body in CardFocus.
  sources?: CardSource[];
  meta: string[];
  actions: CardAction[];
  expand?: Record<string, string>;
  // Rolled-up children, present only on kind="digest" cards (V2.x).
  items?: DigestItem[];
  // Serve-time resolved live values with freshness metadata (V2.x). The
  // value text is already substituted into `meta`/`sub`; this is for
  // rendering a freshness affordance (e.g. greying a stale reading).
  live?: LiveChip[];
  trace_id?: string;
  origin?: string;
  pinned?: boolean; // V2.8.1
  // RFC3339 timestamp; populated by the server only for reactive ask cards.
  // When the server's clock passes expires_at the card is dropped from
  // the main rail by CardRepo.ListByDate, but the row remains in the
  // archive view forever.
  expires_at?: string;
}

export interface CardsResponse {
  date: string;
  cards: Card[];
}

export interface TraceStep {
  kind: "thought" | "tool";
  t?: string;
  op?: string;
  target?: string;
  note?: string;
}

export interface Trace {
  trace_id: string;
  run_id: string;
  date: string;
  stopped: string;
  total_ms: number;
  steps: TraceStep[];
}

export interface CalendarEvent {
  uid: string;
  title: string;
  location?: string;
  tag?: string;
  start: string;
  end: string;
  attendees?: string[];
  last_modified?: string;
}

export interface RunWindow {
  start: string;
  end: string;
  condition: string;
}

export interface WeatherCurrent {
  time: string;
  temp_c: number;
  label: string;
  wind_kmh: number;
  precip_mm: number;
}

export interface WeatherHourPoint {
  time: string;
  temp_c: number;
}

export interface WeatherDayPoint {
  date: string;
  temp_max_c: number;
  temp_min_c: number;
  label?: string;
  code: number;
}

export interface WeatherView {
  captured_at: string;
  timezone: string;
  location?: string;
  current: WeatherCurrent;
  hourly: WeatherHourPoint[];
  now_index: number;
  daily?: WeatherDayPoint[];
}

// Stock projection — one quote per configured ticker, surfaced via
// /api/projections/stock. Null is returned when no tickers are
// configured (the widget renders an empty state).
export interface StockTick {
  as_of: string;
  price: number;
}

export interface StockQuote {
  ticker: string;
  price: number;
  prev_close: number;
  currency?: string;
  change_pct: number;
  as_of: string;
  stale?: boolean;
  open?: number;
  day_high?: number;
  day_low?: number;
  volume?: number;
  post_price?: number;
  post_change_pct?: number;
  market_state?: string;
  series?: StockTick[];
}

export interface StockView {
  as_of: string;
  quotes: StockQuote[];
}

// V2.6: open-task projection rows surfaced via /api/projections/tasks/open.
export type TaskPriority = "low" | "med" | "high";

export interface OpenTask {
  uid: string;
  title: string;
  body?: string;
  completed: boolean;
  due_date?: string;
  done_date?: string;
  priority: TaskPriority;
  tags?: string[];
  // V2.11: a task with fire_at set is what used to be a "reminder".
  // The sweeper fires the alarm when fire_at <= now and fired_at IS NULL,
  // then sets fired_at so it doesn't repeat. UI renders ⏰ + relative
  // time when fire_at is set and fired_at is null.
  fire_at?: string; // RFC3339
  fired_at?: string;
  source_card_id?: string;
}

// V2.2.0: derived-memory facts surfaced via /api/memory.
export interface MemoryFact {
  id: string;
  subject: string;
  fact: string;
  category: string;
  confidence: "high" | "med" | "low";
  source: "user" | "synth";
  evidence_count: number;
  first_seen: string;
  last_reinforced: string;
  updated_at: string;
}

export interface MemoryListResponse {
  facts: MemoryFact[];
}

// V2.5.0: Concerns — long-running threads of attention.
export type ConcernState = "proposed" | "active" | "paused" | "ended" | "merged";
export type ConcernSource = "user" | "model";

export interface Concern {
  id: string;
  name: string;
  description: string;
  state: ConcernState;
  source: ConcernSource;
  confidence: number;
  merged_into_id?: string | null;
  split_from_id?: string | null;
  last_active_at: string;
  ended_at?: string | null;
  observation_count: number;
  created_at: string;
  updated_at: string;
  ready_to_retire?: boolean;
}

export interface ConcernObservation {
  event_id: string;
  source: ConcernSource;
  confidence: number;
  tagged_at: string;
}

export interface ConcernListResponse {
  concerns: Concern[];
}

export interface ConcernObservationsResponse {
  observations: ConcernObservation[];
}

export interface ConcernApproveResponse {
  concern: Concern;
  retrospective_dispatched: boolean;
}

export interface ConcernSplitInput {
  name: string;
  description: string;
  observation_ids: string[];
}

export interface ConcernSplitResponse {
  source: Concern;
  new: Concern[];
}

export interface RetrospectiveProgress {
  concern_id: string;
  processed: number;
  total: number;
  status: "running" | "completed" | "cancelled" | "failed";
  error?: string;
}

// V2.10: card-conversation surface — clicking a card opens a modal where
// the user has a multi-turn conversation against that card. Each turn
// resolves to a typed SubCard reply, persisted on a per-card thread.

export type SubCardKind = "calendar" | "draft" | "research" | "answer" | "document";

// SubCardCalendar mirrors internal/synth/schema.go SubCalendar.
// Title/when/where/who is the legacy minimal shape preserved for
// backwards compatibility — old persisted SubCards decode with only
// these four fields set. Everything below is the V2.x rich-detail
// extension and stays optional so the renderer can fall back to the
// minimal grid when the LLM hasn't populated them.
export interface SubCardCalendar {
  title: string;
  when: string;
  where?: string;
  who?: string;

  start?: string;
  end?: string;
  attendees?: SubCardAttendee[];
  travel_before?: number;
  travel_after?: number;
  reminder?: string;
  conflict?: SubCardCalendarConflict;
  reasoning?: string[];
  alternatives?: SubCardCalendarAlternative[];
  recurring?: SubCardCalendarRecurring;
  daystrip?: SubCardCalendarDaystrip;
}

export type SubCardAttendeeStatus =
  | "host"
  | "accepted"
  | "pending"
  | "declined";

export interface SubCardAttendee {
  name: string;
  role?: string;
  status?: SubCardAttendeeStatus | "";
}

export interface SubCardCalendarConflict {
  ok: boolean;
  text: string;
}

export interface SubCardCalendarAlternative {
  when: string;
  note?: string;
}

export interface SubCardCalendarRecurring {
  label: string;
  default?: boolean;
}

export type SubCardDaystripEventKind = "muted" | "travel" | "proposed";

export interface SubCardCalendarDaystripEvent {
  start: number;
  end: number;
  label?: string;
  kind?: SubCardDaystripEventKind | "";
}

export interface SubCardCalendarDaystrip {
  label?: string;
  start_hr?: number;
  end_hr?: number;
  events?: SubCardCalendarDaystripEvent[];
}

export interface SubCardSource {
  i: number;
  t: string;
  w?: string;
}

export interface SubCard {
  id: string;
  kind: SubCardKind;
  eyebrow: string;
  title: string;
  actions?: CardAction[];

  // kind=calendar
  cal?: SubCardCalendar;
  conflict?: string;

  // kind=draft
  draft?: string;
  draft_meta?: string;

  // kind=research / answer / document
  body?: string;
  sources?: SubCardSource[];

  // kind=document — "{sender} · {short date}" header and the subject
  // substring the model passed to read_thread. The UI uses thread_hint
  // to fetch the verbatim original body for the "view original" toggle.
  from?: string;
  thread_hint?: string;
}

export interface ConversationTurn {
  id: string;
  position: number;
  prompt: string;
  reply: SubCard;
  trace_id: string;
  created_at: string;
}

export interface ConversationThread {
  thread_id: string;
  card_id: string;
  turns: ConversationTurn[];
}
