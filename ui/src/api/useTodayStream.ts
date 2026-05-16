import { useEffect, useRef } from "react";
import { useQueryClient } from "@tanstack/react-query";

import type {
  CalendarEvent,
  Card,
  CardsResponse,
  OpenTask,
  StockView,
  WeatherView,
} from "../types";
import type { Settings } from "./useSettings";
import type { StatsSnapshot } from "./useStats";
import type { Health } from "./useHealth";
import type { WhatsAppStatus } from "./useWhatsApp";
import { publishLive, type LiveEvent } from "./liveBroker";

const ENDPOINT = "/api/today/stream";
const RECONNECT_BASE_MS = 1000;
const RECONNECT_CAP_MS = 30000;

// dispatchLive turns a raw SSE message into a typed LiveEvent and
// forwards it to the broker. Malformed payloads are silently dropped —
// the server is the audit trail, the broker is best-effort.
function dispatchLive<K extends LiveEvent["kind"]>(kind: K) {
  return (ev: MessageEvent) => {
    let payload: object;
    try {
      payload = JSON.parse(ev.data);
    } catch {
      return;
    }
    publishLive({ kind, ...payload } as LiveEvent);
  };
}

// SSE-driven query keys that may have missed events while the
// EventSource was disconnected. On reconnect we invalidate all of them
// so the cache resyncs from server state — without this, a publish that
// landed while the network was down would leave the UI stuck on stale
// data forever (event-driven hooks don't poll to recover on their own).
const SSE_DRIVEN_QUERY_KEYS: ReadonlyArray<readonly unknown[]> = [
  ["cards"],
  ["concerns"],
  ["whatsapp-status"],
  ["tasks", "all"],
  ["tasks", "open"],
  ["settings"],
  ["weather-snapshot"],
  ["stock-snapshot"],
  ["today"],
  ["tomorrow"],
  ["week"],
  ["memory"],
  ["stats", "snapshot"],
  ["health"],
];

// Subscribes to /api/today/stream and prepends inject cards to the ["cards"]
// React Query cache. Mount once at the App root. Returns nothing — it is a
// side-effect hook.
export function useTodayStream() {
  const queryClient = useQueryClient();
  const sourceRef = useRef<EventSource | null>(null);
  const reconnectAttemptsRef = useRef(0);
  // True after the first successful reconnect so we don't fire a
  // resync invalidate on the initial mount (the hooks' own initial
  // queryFn already handles cold-start).
  const hadDisconnectRef = useRef(false);

  useEffect(() => {
    if (typeof EventSource === "undefined") return;

    let cancelled = false;
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null;

    const resyncAfterReconnect = () => {
      if (!hadDisconnectRef.current) return;
      hadDisconnectRef.current = false;
      for (const key of SSE_DRIVEN_QUERY_KEYS) {
        queryClient.invalidateQueries({ queryKey: key });
      }
    };

    const connect = () => {
      if (cancelled) return;
      const source = new EventSource(ENDPOINT);
      sourceRef.current = source;
      // EventSource fires `open` once the stream is established; the
      // first open is the cold-start mount and skipped — only flag
      // resyncs that follow a prior error/disconnect.
      source.addEventListener("open", () => {
        resyncAfterReconnect();
        reconnectAttemptsRef.current = 0;
      });

      source.addEventListener("card.appended", (ev: MessageEvent) => {
        let card: Card;
        try {
          card = JSON.parse(ev.data) as Card;
        } catch {
          return;
        }
        queryClient.setQueryData<CardsResponse>(["cards"], (prev) => {
          if (!prev) return prev;
          if (prev.cards.some((c) => c.id === card.id)) return prev;
          return { ...prev, cards: [card, ...prev.cards] };
        });
        reconnectAttemptsRef.current = 0;
      });

      // V2.4 P3: forward live-trace events to the in-memory broker.
      // useLiveSynth (consumed by LiveSynthPanel) subscribes there.
      // The cards cache is unchanged for these event types.
      source.addEventListener("synth.started", dispatchLive("synth.started"));
      source.addEventListener("trace.step", dispatchLive("trace.step"));
      source.addEventListener("synth.delta", dispatchLive("synth.delta"));
      source.addEventListener("synth.completed", dispatchLive("synth.completed"));

      // V2.5.0 Phase 4: concern lifecycle events. Three of them invalidate
      // the concerns query so the review surface refreshes; retrospective
      // progress is broker-only — the panel subscribes to the live stream
      // for that one because it's high-frequency and per-row state, not
      // suitable for a query refetch.
      const invalidateConcerns = () => {
        queryClient.invalidateQueries({ queryKey: ["concerns"] });
      };
      source.addEventListener("concern.proposed", (ev: MessageEvent) => {
        dispatchLive("concern.proposed")(ev);
        invalidateConcerns();
      });
      source.addEventListener("concern.state_changed", (ev: MessageEvent) => {
        dispatchLive("concern.state_changed")(ev);
        invalidateConcerns();
      });
      source.addEventListener("concern.tagged", (ev: MessageEvent) => {
        dispatchLive("concern.tagged")(ev);
        invalidateConcerns();
      });
      source.addEventListener(
        "concern.retrospective_progress",
        dispatchLive("concern.retrospective_progress")
      );

      // V2.5.0 Phase 5: retirement-proposed events from the daily
      // recognition retirement survey. Forward to broker for tests +
      // invalidate the concerns query so the badge appears live.
      source.addEventListener("concern.retirement_proposed", (ev: MessageEvent) => {
        dispatchLive("concern.retirement_proposed")(ev);
        invalidateConcerns();
      });

      // SSE-driven UI updates (replaces polling). Each handler parses the
      // event payload, updates the corresponding React Query cache key
      // via setQueryData, and forwards to the broker (so tests / panels
      // that listen via subscribeLive still see them).

      const onJSON = <T>(handler: (data: T) => void) => (ev: MessageEvent) => {
        let parsed: T;
        try {
          parsed = JSON.parse(ev.data) as T;
        } catch {
          return;
        }
        handler(parsed);
      };

      source.addEventListener(
        "whatsapp.status_changed",
        onJSON<WhatsAppStatus>((status) => {
          queryClient.setQueryData<WhatsAppStatus>(["whatsapp-status"], status);
          publishLive({ kind: "whatsapp.status_changed", ...status });
        }),
      );

      // task.* events: mutate the ["tasks","all"] cache directly (full
      // file shape, no sort dependency) and invalidate ["tasks","open"]
      // so the projection re-fetches — the sort order there is
      // server-derived and not safe to splice client-side.
      const invalidateOpenTasks = () => {
        queryClient.invalidateQueries({ queryKey: ["tasks", "open"] });
      };

      source.addEventListener(
        "task.created",
        onJSON<{ task: OpenTask }>(({ task }) => {
          queryClient.setQueryData<OpenTask[]>(["tasks", "all"], (prev) => {
            if (!prev) return [task];
            if (prev.some((t) => t.uid === task.uid)) return prev;
            return [task, ...prev];
          });
          invalidateOpenTasks();
          publishLive({ kind: "task.created", task });
        }),
      );

      source.addEventListener(
        "task.completed",
        onJSON<{ task: OpenTask }>(({ task }) => {
          queryClient.setQueryData<OpenTask[]>(["tasks", "all"], (prev) =>
            prev ? prev.map((t) => (t.uid === task.uid ? task : t)) : prev,
          );
          invalidateOpenTasks();
          publishLive({ kind: "task.completed", task });
        }),
      );

      source.addEventListener(
        "task.deleted",
        onJSON<{ uid: string }>(({ uid }) => {
          queryClient.setQueryData<OpenTask[]>(["tasks", "all"], (prev) =>
            prev ? prev.filter((t) => t.uid !== uid) : prev,
          );
          invalidateOpenTasks();
          publishLive({ kind: "task.deleted", uid });
        }),
      );

      source.addEventListener(
        "task.reminder_set",
        onJSON<{ task: OpenTask }>(({ task }) => {
          queryClient.setQueryData<OpenTask[]>(["tasks", "all"], (prev) =>
            prev ? prev.map((t) => (t.uid === task.uid ? task : t)) : prev,
          );
          invalidateOpenTasks();
          publishLive({ kind: "task.reminder_set", task });
        }),
      );

      source.addEventListener(
        "task.edited",
        onJSON<{ task: OpenTask }>(({ task }) => {
          queryClient.setQueryData<OpenTask[]>(["tasks", "all"], (prev) =>
            prev ? prev.map((t) => (t.uid === task.uid ? task : t)) : prev,
          );
          invalidateOpenTasks();
          publishLive({ kind: "task.edited", task });
        }),
      );

      source.addEventListener(
        "settings.changed",
        onJSON<{ settings: Settings }>(({ settings }) => {
          queryClient.setQueryData<Settings>(["settings"], settings);
          publishLive({ kind: "settings.changed", settings });
        }),
      );

      source.addEventListener(
        "weather.updated",
        onJSON<{ weather: WeatherView | null }>(({ weather }) => {
          queryClient.setQueryData<WeatherView | null>(
            ["weather-snapshot"],
            weather,
          );
          publishLive({ kind: "weather.updated", weather });
        }),
      );

      source.addEventListener(
        "stock.updated",
        onJSON<{ stock: StockView | null }>(({ stock }) => {
          queryClient.setQueryData<StockView | null>(["stock-snapshot"], stock);
          publishLive({ kind: "stock.updated", stock });
        }),
      );

      source.addEventListener(
        "calendar.today_changed",
        onJSON<{ events: CalendarEvent[] }>(({ events }) => {
          queryClient.setQueryData<CalendarEvent[]>(["today"], events);
          publishLive({ kind: "calendar.today_changed", events });
        }),
      );

      source.addEventListener(
        "calendar.tomorrow_changed",
        onJSON<{ events: CalendarEvent[] }>(({ events }) => {
          queryClient.setQueryData<CalendarEvent[]>(["tomorrow"], events);
          publishLive({ kind: "calendar.tomorrow_changed", events });
        }),
      );

      source.addEventListener(
        "calendar.week_changed",
        onJSON<{ events: CalendarEvent[] }>(({ events }) => {
          queryClient.setQueryData<CalendarEvent[]>(["week"], events);
          publishLive({ kind: "calendar.week_changed", events });
        }),
      );

      source.addEventListener(
        "memory.changed",
        onJSON<{ memory: { facts: unknown[] } }>(({ memory }) => {
          queryClient.setQueryData(["memory"], memory);
          publishLive({ kind: "memory.changed", memory });
        }),
      );

      source.addEventListener(
        "stats.snapshot",
        onJSON<{ stats: StatsSnapshot }>(({ stats }) => {
          queryClient.setQueryData<StatsSnapshot>(["stats", "snapshot"], stats);
          publishLive({ kind: "stats.snapshot", stats });
        }),
      );

      source.addEventListener(
        "health.changed",
        onJSON<Health>((data) => {
          queryClient.setQueryData<Health>(["health"], data);
          publishLive({ kind: "health.changed", ...data });
        }),
      );

      source.addEventListener(
        "concern.observations_changed",
        onJSON<{ concern_id: string; observations: unknown }>(({ concern_id, observations }) => {
          queryClient.setQueryData(
            ["concern-observations", concern_id],
            observations,
          );
          publishLive({ kind: "concern.observations_changed", concern_id, observations });
        }),
      );

      source.onerror = () => {
        source.close();
        sourceRef.current = null;
        if (cancelled) return;
        // Mark that we've now experienced a disconnect; the next
        // successful `open` event triggers a resync invalidate of every
        // event-driven query key.
        hadDisconnectRef.current = true;
        const attempt = reconnectAttemptsRef.current;
        const delay = Math.min(RECONNECT_CAP_MS, RECONNECT_BASE_MS * 2 ** attempt);
        reconnectAttemptsRef.current = attempt + 1;
        reconnectTimer = setTimeout(connect, delay);
      };
    };

    connect();

    return () => {
      cancelled = true;
      if (reconnectTimer) clearTimeout(reconnectTimer);
      sourceRef.current?.close();
      sourceRef.current = null;
    };
  }, [queryClient]);
}
