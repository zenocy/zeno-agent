// liveBroker — singleton in-memory pub/sub for V2.4 live-trace events.
//
// `useTodayStream`'s SSE listener calls `publishLive` on every
// synth.started / trace.step / synth.delta / synth.completed event.
// `useLiveSynth` (the panel's data source) calls `subscribeLive` to
// receive them. The broker is intentionally module-scoped: there is one
// EventSource per app instance and one panel that consumes its events,
// so a context provider would be ceremony.
//
// Tests should call `resetLiveBroker()` in `beforeEach` to clear stale
// subscribers between cases.

import type {
  CalendarEvent,
  ConcernSource,
  ConcernState,
  OpenTask,
  StockView,
  TraceStep,
  WeatherView,
} from "../types";
import type { Health } from "./useHealth";
import type { Settings } from "./useSettings";
import type { StatsSnapshot } from "./useStats";
import type { WhatsAppStatus } from "./useWhatsApp";

export type LiveEvent =
  | { kind: "synth.started"; run_id: string; stage: string; date: string }
  | { kind: "trace.step"; run_id: string; stage: string; step: TraceStep }
  | { kind: "synth.delta"; run_id: string; stage: string; delta: string }
  | {
      kind: "synth.completed";
      run_id: string;
      stage: string;
      stopped: string;
      total_ms: number;
    }
  | {
      kind: "concern.proposed";
      concern_id: string;
      name: string;
      description: string;
      source: ConcernSource;
      confidence: number;
    }
  | {
      kind: "concern.state_changed";
      concern_id: string;
      prior_state: ConcernState;
      new_state: ConcernState;
      merged_into_id?: string;
    }
  | {
      kind: "concern.tagged";
      concern_id: string;
      event_ids: string[];
      source: ConcernSource;
      batch_origin: string;
    }
  | {
      kind: "concern.retrospective_progress";
      concern_id: string;
      processed: number;
      total: number;
      status: "running" | "completed" | "cancelled" | "failed";
      error?: string;
    }
  | {
      kind: "concern.retirement_proposed";
      concern_id: string;
      days_inactive: number;
    }
  // SSE-driven UI updates (replaces React Query polling). Each event
  // carries the full new state so consuming hooks can drop it straight
  // into setQueryData without a follow-up fetch.
  | ({ kind: "whatsapp.status_changed" } & WhatsAppStatus)
  | { kind: "task.created"; task: OpenTask }
  | { kind: "task.completed"; task: OpenTask }
  | { kind: "task.deleted"; uid: string }
  | { kind: "task.reminder_set"; task: OpenTask }
  | { kind: "task.edited"; task: OpenTask }
  | { kind: "settings.changed"; settings: Settings }
  | { kind: "weather.updated"; weather: WeatherView | null }
  | { kind: "stock.updated"; stock: StockView | null }
  | { kind: "calendar.today_changed"; events: CalendarEvent[] }
  | { kind: "calendar.tomorrow_changed"; events: CalendarEvent[] }
  | { kind: "calendar.week_changed"; events: CalendarEvent[] }
  | { kind: "memory.changed"; memory: { facts: unknown[] } }
  | { kind: "stats.snapshot"; stats: StatsSnapshot }
  | ({ kind: "health.changed" } & Health)
  | {
      kind: "concern.observations_changed";
      concern_id: string;
      observations: unknown;
    };

export type LiveListener = (ev: LiveEvent) => void;

let listeners: LiveListener[] = [];

/** publishLive fans out an event to every current subscriber, in
 * registration order. Errors thrown by listeners are swallowed so one
 * misbehaving consumer can't blackhole the rest. */
export function publishLive(ev: LiveEvent): void {
  for (const fn of listeners) {
    try {
      fn(ev);
    } catch {
      // Listeners are best-effort; the durable record is the SSE
      // stream and the database. Drop and continue.
    }
  }
}

/** subscribeLive registers a listener and returns an unsubscribe func.
 * Always pair with the returned cleanup (e.g. via React's useEffect
 * cleanup) so the listener list doesn't grow on repeated mounts. */
export function subscribeLive(fn: LiveListener): () => void {
  listeners.push(fn);
  return () => {
    listeners = listeners.filter((l) => l !== fn);
  };
}

/** resetLiveBroker clears every subscriber. Test-only: tests call this
 * in `beforeEach` so a previous test's React hook tear-down race
 * doesn't leak a listener into the next case. */
export function resetLiveBroker(): void {
  listeners = [];
}

/** subscriberCount is exposed for tests that want to assert clean
 * subscription state (no leaks after unmount). Not used in production
 * code. */
export function subscriberCount(): number {
  return listeners.length;
}
