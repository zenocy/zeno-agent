import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";

import {
  publishLive,
  resetLiveBroker,
  subscriberCount,
  type LiveEvent,
} from "../liveBroker";
import { useLiveSynth, DissolveDelayMs } from "../useLiveSynth";
import type { TraceStep } from "../../types";

const startedA: LiveEvent = {
  kind: "synth.started",
  run_id: "run-A",
  stage: "morning",
  date: "2026-04-29",
};

const startedAsk: LiveEvent = {
  kind: "synth.started",
  run_id: "run-ask",
  stage: "ask",
  date: "2026-04-29",
};

const stepThought: TraceStep = { kind: "thought", t: "looking at calendar" };
const stepTool: TraceStep = { kind: "tool", op: "READ", target: "redline" };

describe("useLiveSynth", () => {
  beforeEach(() => {
    resetLiveBroker();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("starts in the idle state — inactive, no run, empty steps + body", () => {
    const { result } = renderHook(() => useLiveSynth());
    expect(result.current).toEqual({
      active: false,
      dissolving: false,
      runId: null,
      stage: null,
      steps: [],
      body: "",
    });
  });

  it("synth.started transitions to active and resets state for the new run", () => {
    const { result } = renderHook(() => useLiveSynth());

    act(() => {
      publishLive(startedA);
    });

    expect(result.current.active).toBe(true);
    expect(result.current.dissolving).toBe(false);
    expect(result.current.runId).toBe("run-A");
    expect(result.current.stage).toBe("morning");
    expect(result.current.steps).toEqual([]);
    expect(result.current.body).toBe("");
  });

  it("trace.step events append to steps in arrival order", () => {
    const { result } = renderHook(() => useLiveSynth());

    act(() => {
      publishLive(startedA);
      publishLive({ kind: "trace.step", run_id: "run-A", stage: "cards", step: stepThought });
      publishLive({ kind: "trace.step", run_id: "run-A", stage: "cards", step: stepTool });
      publishLive({ kind: "trace.step", run_id: "run-A", stage: "briefing", step: stepThought });
    });

    expect(result.current.steps).toEqual([stepThought, stepTool, stepThought]);
  });

  it("synth.delta events concatenate into the body", () => {
    const { result } = renderHook(() => useLiveSynth());

    act(() => {
      publishLive(startedA);
      publishLive({ kind: "synth.delta", run_id: "run-A", stage: "briefing", delta: "Quiet " });
      publishLive({ kind: "synth.delta", run_id: "run-A", stage: "briefing", delta: "morning. " });
      publishLive({ kind: "synth.delta", run_id: "run-A", stage: "briefing", delta: "Saru moved noon." });
    });

    expect(result.current.body).toBe("Quiet morning. Saru moved noon.");
  });

  it("synth.completed transitions to dissolving=true, active=false, but keeps state", () => {
    const { result } = renderHook(() => useLiveSynth());

    act(() => {
      publishLive(startedA);
      publishLive({ kind: "trace.step", run_id: "run-A", stage: "cards", step: stepThought });
      publishLive({
        kind: "synth.completed",
        run_id: "run-A",
        stage: "morning",
        stopped: "ok",
        total_ms: 12345,
      });
    });

    expect(result.current.active).toBe(false);
    expect(result.current.dissolving).toBe(true);
    expect(result.current.runId).toBe("run-A");
    expect(result.current.steps).toHaveLength(1);
  });

  it("after the dissolve delay, state resets to clean idle", () => {
    vi.useFakeTimers();
    const { result } = renderHook(() => useLiveSynth());

    act(() => {
      publishLive(startedA);
      publishLive({
        kind: "synth.completed",
        run_id: "run-A",
        stage: "morning",
        stopped: "ok",
        total_ms: 1,
      });
    });
    expect(result.current.dissolving).toBe(true);

    act(() => {
      vi.advanceTimersByTime(DissolveDelayMs);
    });

    expect(result.current).toEqual({
      active: false,
      dissolving: false,
      runId: null,
      stage: null,
      steps: [],
      body: "",
    });
  });

  it("stale events from a different run_id are ignored", () => {
    const { result } = renderHook(() => useLiveSynth());

    act(() => {
      publishLive(startedA);
      publishLive({ kind: "trace.step", run_id: "run-A", stage: "cards", step: stepThought });
      // Stranger event for a different run lands; should NOT mutate state.
      publishLive({ kind: "trace.step", run_id: "run-B", stage: "cards", step: stepTool });
      publishLive({ kind: "synth.delta", run_id: "run-B", stage: "briefing", delta: "noise" });
      publishLive({
        kind: "synth.completed",
        run_id: "run-B",
        stage: "morning",
        stopped: "ok",
        total_ms: 1,
      });
    });

    expect(result.current.runId).toBe("run-A");
    expect(result.current.active).toBe(true);
    expect(result.current.steps).toEqual([stepThought]);
    expect(result.current.body).toBe("");
  });

  it("a new synth.started during dissolve cancels the timer and resets to the new run", () => {
    vi.useFakeTimers();
    const { result } = renderHook(() => useLiveSynth());

    act(() => {
      publishLive(startedA);
      publishLive({
        kind: "synth.completed",
        run_id: "run-A",
        stage: "morning",
        stopped: "ok",
        total_ms: 1,
      });
      // Mid-dissolve, a new run kicks off.
      vi.advanceTimersByTime(100);
      publishLive(startedAsk);
    });

    expect(result.current.active).toBe(true);
    expect(result.current.dissolving).toBe(false);
    expect(result.current.runId).toBe("run-ask");
    expect(result.current.stage).toBe("ask");

    // Letting the original 600ms elapse must not now reset us — the
    // original timer was cancelled when run-ask started.
    act(() => {
      vi.advanceTimersByTime(DissolveDelayMs);
    });
    expect(result.current.runId).toBe("run-ask");
    expect(result.current.active).toBe(true);
  });

  it("unmount removes the broker subscription and cancels any pending dissolve timer", () => {
    vi.useFakeTimers();
    const { result, unmount } = renderHook(() => useLiveSynth());

    act(() => {
      publishLive(startedA);
      publishLive({
        kind: "synth.completed",
        run_id: "run-A",
        stage: "morning",
        stopped: "ok",
        total_ms: 1,
      });
    });
    expect(result.current.dissolving).toBe(true);
    expect(subscriberCount()).toBe(1);

    unmount();
    expect(subscriberCount()).toBe(0);

    // Advancing timers post-unmount must not throw nor schedule any
    // late state writes (which would trigger React's "can't update an
    // unmounted component" warning).
    expect(() => {
      act(() => {
        vi.advanceTimersByTime(DissolveDelayMs * 2);
      });
    }).not.toThrow();
  });
});
