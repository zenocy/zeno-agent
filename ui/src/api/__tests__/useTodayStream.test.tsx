import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { useTodayStream } from "../useTodayStream";
import { MockEventSource } from "../../test/mockEventSource";
import {
  resetLiveBroker,
  subscribeLive,
  type LiveEvent,
} from "../liveBroker";
import type { Card, CardsResponse } from "../../types";

const morningCard: Card = {
  id: "morning-1",
  date: "2026-04-27",
  src: "cal",
  src_label: "Calendar",
  rel: "med",
  title: "Existing morning card",
  sub: "",
  meta: [],
  actions: [],
};

const injectCard: Card = {
  id: "inject-1",
  date: "2026-04-27",
  src: "mail",
  src_label: "Mail",
  rel: "high",
  title: "Saru — re: redline (urgent)",
  sub: "Inject card from SSE",
  meta: [],
  actions: [],
  origin: "inject",
};

function makeQueryClient(): QueryClient {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  qc.setQueryData<CardsResponse>(["cards"], { date: "2026-04-27", cards: [morningCard] });
  return qc;
}

function wrapperFor(qc: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
  };
}

describe("useTodayStream", () => {
  beforeEach(() => {
    MockEventSource.reset();
    resetLiveBroker();
    vi.stubGlobal("EventSource", MockEventSource);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("subscribes to /api/today/stream on mount", () => {
    const qc = makeQueryClient();
    renderHook(() => useTodayStream(), { wrapper: wrapperFor(qc) });

    expect(MockEventSource.instances).toHaveLength(1);
    expect(MockEventSource.last().url).toBe("/api/today/stream");
  });

  it("prepends a card.appended event into the cards cache (deduping by id)", () => {
    const qc = makeQueryClient();
    renderHook(() => useTodayStream(), { wrapper: wrapperFor(qc) });

    act(() => {
      MockEventSource.last().emit("card.appended", injectCard);
    });

    let snapshot = qc.getQueryData<CardsResponse>(["cards"])!;
    expect(snapshot.cards.map((c) => c.id)).toEqual(["inject-1", "morning-1"]);

    // Re-emitting the same card is a no-op (dedup by id).
    act(() => {
      MockEventSource.last().emit("card.appended", injectCard);
    });
    snapshot = qc.getQueryData<CardsResponse>(["cards"])!;
    expect(snapshot.cards.map((c) => c.id)).toEqual(["inject-1", "morning-1"]);
  });

  // V2.4 P3: each new live-trace SSE event type is forwarded to the
  // broker. The cards cache is unaffected.

  it("forwards synth.started to the live broker", () => {
    const qc = makeQueryClient();
    const seen: LiveEvent[] = [];
    const unsub = subscribeLive((ev) => seen.push(ev));
    renderHook(() => useTodayStream(), { wrapper: wrapperFor(qc) });

    act(() => {
      MockEventSource.last().emit("synth.started", {
        run_id: "r-1", stage: "morning", date: "2026-04-29",
      });
    });

    expect(seen).toEqual([
      { kind: "synth.started", run_id: "r-1", stage: "morning", date: "2026-04-29" },
    ]);
    unsub();
  });

  it("forwards trace.step to the live broker", () => {
    const qc = makeQueryClient();
    const seen: LiveEvent[] = [];
    const unsub = subscribeLive((ev) => seen.push(ev));
    renderHook(() => useTodayStream(), { wrapper: wrapperFor(qc) });

    const step = { kind: "thought" as const, t: "checking calendar" };
    act(() => {
      MockEventSource.last().emit("trace.step", {
        run_id: "r-1", stage: "cards", step,
      });
    });

    expect(seen).toEqual([
      { kind: "trace.step", run_id: "r-1", stage: "cards", step },
    ]);
    unsub();
  });

  it("forwards synth.delta to the live broker", () => {
    const qc = makeQueryClient();
    const seen: LiveEvent[] = [];
    const unsub = subscribeLive((ev) => seen.push(ev));
    renderHook(() => useTodayStream(), { wrapper: wrapperFor(qc) });

    act(() => {
      MockEventSource.last().emit("synth.delta", {
        run_id: "r-1", stage: "briefing", delta: "Quiet ",
      });
    });

    expect(seen).toEqual([
      { kind: "synth.delta", run_id: "r-1", stage: "briefing", delta: "Quiet " },
    ]);
    unsub();
  });

  it("forwards synth.completed to the live broker", () => {
    const qc = makeQueryClient();
    const seen: LiveEvent[] = [];
    const unsub = subscribeLive((ev) => seen.push(ev));
    renderHook(() => useTodayStream(), { wrapper: wrapperFor(qc) });

    act(() => {
      MockEventSource.last().emit("synth.completed", {
        run_id: "r-1", stage: "morning", stopped: "ok", total_ms: 12345,
      });
    });

    expect(seen).toEqual([
      { kind: "synth.completed", run_id: "r-1", stage: "morning", stopped: "ok", total_ms: 12345 },
    ]);
    unsub();
  });

  it("malformed JSON in a synth event is silently dropped; subsequent valid events still flow", () => {
    const qc = makeQueryClient();
    const seen: LiveEvent[] = [];
    const unsub = subscribeLive((ev) => seen.push(ev));
    renderHook(() => useTodayStream(), { wrapper: wrapperFor(qc) });

    // Bad JSON arrives — bypass MockEventSource.emit's JSON.stringify.
    act(() => {
      const bad = new MessageEvent("synth.delta", { data: "not json {{{" });
      // Reach inside the mock's listener registry to dispatch raw.
      const list = (MockEventSource.last() as unknown as {
        listeners: Map<string, Array<(ev: MessageEvent) => void>>;
      }).listeners.get("synth.delta") ?? [];
      list.forEach((l) => l(bad));
    });
    expect(seen).toEqual([]);

    // A valid event right after still flows.
    act(() => {
      MockEventSource.last().emit("synth.delta", {
        run_id: "r-1", stage: "briefing", delta: "ok",
      });
    });
    expect(seen).toHaveLength(1);
    expect((seen[0] as { delta: string }).delta).toBe("ok");

    unsub();
  });

  // V2.5.0 Phase 4: concern lifecycle events. Three of them invalidate
  // the ["concerns"] query so the review surface refreshes; retrospective
  // progress is broker-only.

  it("forwards concern.proposed to broker and invalidates concerns query", () => {
    const qc = makeQueryClient();
    qc.setQueryData(["concerns"], { concerns: [] });
    const seen: LiveEvent[] = [];
    const unsub = subscribeLive((ev) => seen.push(ev));
    const invalSpy = vi.spyOn(qc, "invalidateQueries");
    renderHook(() => useTodayStream(), { wrapper: wrapperFor(qc) });

    act(() => {
      MockEventSource.last().emit("concern.proposed", {
        concern_id: "c-1",
        name: "Frankfurt trip",
        description: "Mid-June review.",
        source: "model",
        confidence: 0.7,
      });
    });

    expect(seen).toHaveLength(1);
    expect(seen[0].kind).toBe("concern.proposed");
    expect(invalSpy).toHaveBeenCalledWith({ queryKey: ["concerns"] });
    unsub();
  });

  it("forwards concern.state_changed to broker and invalidates concerns query", () => {
    const qc = makeQueryClient();
    qc.setQueryData(["concerns"], { concerns: [] });
    const seen: LiveEvent[] = [];
    const unsub = subscribeLive((ev) => seen.push(ev));
    const invalSpy = vi.spyOn(qc, "invalidateQueries");
    renderHook(() => useTodayStream(), { wrapper: wrapperFor(qc) });

    act(() => {
      MockEventSource.last().emit("concern.state_changed", {
        concern_id: "c-1",
        prior_state: "proposed",
        new_state: "active",
      });
    });

    expect(seen).toHaveLength(1);
    expect(seen[0].kind).toBe("concern.state_changed");
    expect(invalSpy).toHaveBeenCalledWith({ queryKey: ["concerns"] });
    unsub();
  });

  it("forwards concern.tagged to broker and invalidates concerns query", () => {
    const qc = makeQueryClient();
    qc.setQueryData(["concerns"], { concerns: [] });
    const seen: LiveEvent[] = [];
    const unsub = subscribeLive((ev) => seen.push(ev));
    const invalSpy = vi.spyOn(qc, "invalidateQueries");
    renderHook(() => useTodayStream(), { wrapper: wrapperFor(qc) });

    act(() => {
      MockEventSource.last().emit("concern.tagged", {
        concern_id: "c-1",
        event_ids: ["e-1", "e-2"],
        source: "model",
        batch_origin: "retrospective",
      });
    });

    expect(seen).toHaveLength(1);
    expect(seen[0].kind).toBe("concern.tagged");
    expect(invalSpy).toHaveBeenCalledWith({ queryKey: ["concerns"] });
    unsub();
  });

  it("forwards concern.retirement_proposed to broker and invalidates concerns query", () => {
    const qc = makeQueryClient();
    qc.setQueryData(["concerns"], { concerns: [] });
    const seen: LiveEvent[] = [];
    const unsub = subscribeLive((ev) => seen.push(ev));
    const invalSpy = vi.spyOn(qc, "invalidateQueries");
    renderHook(() => useTodayStream(), { wrapper: wrapperFor(qc) });

    act(() => {
      MockEventSource.last().emit("concern.retirement_proposed", {
        concern_id: "c-1",
        days_inactive: 95,
      });
    });

    expect(seen).toHaveLength(1);
    expect(seen[0].kind).toBe("concern.retirement_proposed");
    expect(invalSpy).toHaveBeenCalledWith({ queryKey: ["concerns"] });
    unsub();
  });

  it("forwards concern.retrospective_progress to broker only (no query invalidation)", () => {
    const qc = makeQueryClient();
    qc.setQueryData(["concerns"], { concerns: [] });
    const seen: LiveEvent[] = [];
    const unsub = subscribeLive((ev) => seen.push(ev));
    const invalSpy = vi.spyOn(qc, "invalidateQueries");
    renderHook(() => useTodayStream(), { wrapper: wrapperFor(qc) });

    act(() => {
      MockEventSource.last().emit("concern.retrospective_progress", {
        concern_id: "c-1",
        processed: 47,
        total: 200,
        status: "running",
      });
    });

    expect(seen).toHaveLength(1);
    expect(seen[0].kind).toBe("concern.retrospective_progress");
    // Progress events do not trigger refetch — broker-only.
    expect(invalSpy).not.toHaveBeenCalled();
    unsub();
  });

  it("closes EventSource on unmount and does not reconnect after error", () => {
    vi.useFakeTimers();
    try {
      const qc = makeQueryClient();
      const { unmount } = renderHook(() => useTodayStream(), { wrapper: wrapperFor(qc) });
      const initial = MockEventSource.last();

      unmount();
      expect(initial.closed).toBe(true);

      // Simulate a post-unmount error (e.g. network blip arriving after teardown).
      // The hook must not schedule a reconnect.
      act(() => {
        initial.fail();
        vi.advanceTimersByTime(60_000);
      });
      expect(MockEventSource.instances).toHaveLength(1);
    } finally {
      vi.useRealTimers();
    }
  });
});
