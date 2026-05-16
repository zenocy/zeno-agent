import { describe, it, expect, beforeEach, vi } from "vitest";
import {
  publishLive,
  subscribeLive,
  resetLiveBroker,
  subscriberCount,
  type LiveEvent,
} from "../liveBroker";

const startedEvent: LiveEvent = {
  kind: "synth.started",
  run_id: "run-1",
  stage: "morning",
  date: "2026-04-29",
};

describe("liveBroker", () => {
  beforeEach(() => {
    resetLiveBroker();
  });

  it("delivers a published event to a subscribed listener", () => {
    const seen: LiveEvent[] = [];
    const unsub = subscribeLive((ev) => seen.push(ev));

    publishLive(startedEvent);

    expect(seen).toEqual([startedEvent]);
    unsub();
  });

  it("delivers to multiple subscribers in registration order", () => {
    const order: number[] = [];
    const unsub1 = subscribeLive(() => order.push(1));
    const unsub2 = subscribeLive(() => order.push(2));
    const unsub3 = subscribeLive(() => order.push(3));

    publishLive(startedEvent);

    expect(order).toEqual([1, 2, 3]);
    unsub1();
    unsub2();
    unsub3();
  });

  it("unsubscribe stops delivery for that listener; siblings keep receiving", () => {
    const seenA: LiveEvent[] = [];
    const seenB: LiveEvent[] = [];
    const unsubA = subscribeLive((ev) => seenA.push(ev));
    const unsubB = subscribeLive((ev) => seenB.push(ev));

    publishLive(startedEvent);
    unsubA();
    publishLive({ ...startedEvent, run_id: "run-2" });

    expect(seenA).toHaveLength(1);
    expect(seenB).toHaveLength(2);
    unsubB();
  });

  it("resetLiveBroker clears every subscriber", () => {
    subscribeLive(() => {
      throw new Error("should not run after reset");
    });
    expect(subscriberCount()).toBe(1);

    resetLiveBroker();

    expect(subscriberCount()).toBe(0);
    // No throw — broker is empty.
    publishLive(startedEvent);
  });

  it("a listener that throws does not break the fan-out to its siblings", () => {
    const seenAfter: LiveEvent[] = [];
    subscribeLive(() => {
      throw new Error("misbehaving listener");
    });
    subscribeLive((ev) => seenAfter.push(ev));

    expect(() => publishLive(startedEvent)).not.toThrow();
    expect(seenAfter).toEqual([startedEvent]);
  });

  it("publish without subscribers is a no-op (no throw)", () => {
    const spy = vi.fn();
    expect(() => publishLive(startedEvent)).not.toThrow();
    expect(spy).not.toHaveBeenCalled();
  });
});
