import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { act, render, screen } from "@testing-library/react";

import { LiveSynthPanel } from "../LiveSynthPanel";
import {
  publishLive,
  resetLiveBroker,
  type LiveEvent,
} from "../../api/liveBroker";
import { DissolveDelayMs } from "../../api/useLiveSynth";
import type { TraceStep } from "../../types";

const startMorning: LiveEvent = {
  kind: "synth.started",
  run_id: "run-M",
  stage: "morning",
  date: "2026-04-29",
};
const startAsk: LiveEvent = {
  kind: "synth.started",
  run_id: "run-A",
  stage: "ask",
  date: "2026-04-29",
};
const completeMorning: LiveEvent = {
  kind: "synth.completed",
  run_id: "run-M",
  stage: "morning",
  stopped: "ok",
  total_ms: 12345,
};
const stepThought: TraceStep = { kind: "thought", t: "looking at calendar" };
const stepTool: TraceStep = { kind: "tool", op: "READ", target: "redline" };

describe("LiveSynthPanel", () => {
  beforeEach(() => {
    resetLiveBroker();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("renders nothing when no run is active", () => {
    const { container } = render(<LiveSynthPanel />);
    expect(container).toBeEmptyDOMElement();
  });

  it("mounts on synth.started; eyebrow says 'working' for stage=morning", () => {
    render(<LiveSynthPanel />);
    act(() => {
      publishLive(startMorning);
    });

    const panel = screen.getByRole("status");
    expect(panel).toBeInTheDocument();
    expect(panel.className).toContain("active");
    expect(panel.className).not.toContain("dissolving");
    expect(panel.textContent).toContain("working");
  });

  it("eyebrow says 'thinking' when stage=ask", () => {
    render(<LiveSynthPanel />);
    act(() => {
      publishLive(startAsk);
    });

    const panel = screen.getByRole("status");
    expect(panel.textContent).toContain("thinking");
    expect(panel.textContent).not.toContain("working");
  });

  it("steps stream in order; the last step has the active class while active", () => {
    render(<LiveSynthPanel />);
    act(() => {
      publishLive(startMorning);
      publishLive({ kind: "trace.step", run_id: "run-M", stage: "cards", step: stepThought });
      publishLive({ kind: "trace.step", run_id: "run-M", stage: "cards", step: stepTool });
    });

    const items = screen.getAllByRole("listitem");
    expect(items).toHaveLength(2);
    // First step (thought) is now "done" (a later step exists).
    expect(items[0].className).toContain("done");
    expect(items[0].className).not.toContain("active");
    // Last step (tool) is "active" while the run is still active.
    expect(items[1].className).toContain("active");
  });

  it("body paragraph appears with the first synth.delta and accumulates", () => {
    render(<LiveSynthPanel />);
    act(() => {
      publishLive(startMorning);
      publishLive({ kind: "synth.delta", run_id: "run-M", stage: "briefing", delta: "Quiet " });
      publishLive({ kind: "synth.delta", run_id: "run-M", stage: "briefing", delta: "morning." });
    });

    const body = screen.getByRole("status").querySelector(".live-body p");
    expect(body).not.toBeNull();
    expect(body!.textContent).toBe("Quiet morning.");
  });

  it("on synth.completed: panel stays mounted but switches to dissolving class", () => {
    render(<LiveSynthPanel />);
    act(() => {
      publishLive(startMorning);
      publishLive({ kind: "trace.step", run_id: "run-M", stage: "cards", step: stepThought });
      publishLive(completeMorning);
    });

    const panel = screen.getByRole("status");
    expect(panel).toBeInTheDocument();
    expect(panel.className).toContain("dissolving");
    expect(panel.className).not.toContain("active");
    // Steps are still rendered during the dissolve animation. None are
    // "active" anymore because the run is done.
    const items = screen.getAllByRole("listitem");
    expect(items).toHaveLength(1);
    expect(items[0].className).toContain("done");
  });

  it("after the dissolve delay, the panel unmounts cleanly", () => {
    vi.useFakeTimers();
    const { container } = render(<LiveSynthPanel />);
    act(() => {
      publishLive(startMorning);
      publishLive(completeMorning);
    });
    expect(container.querySelector(".live-synth-panel")).not.toBeNull();

    act(() => {
      vi.advanceTimersByTime(DissolveDelayMs);
    });

    expect(container.querySelector(".live-synth-panel")).toBeNull();
    expect(container).toBeEmptyDOMElement();
  });

  it("a tool step renders bullet + op + target; a thought step renders rule + italic prose", () => {
    render(<LiveSynthPanel />);
    act(() => {
      publishLive(startMorning);
      publishLive({ kind: "trace.step", run_id: "run-M", stage: "cards", step: stepTool });
      publishLive({ kind: "trace.step", run_id: "run-M", stage: "cards", step: stepThought });
    });

    const panel = screen.getByRole("status");
    // Tool step DOM shape.
    const toolBullet = panel.querySelector(".trace-bullet");
    expect(toolBullet).not.toBeNull();
    expect(panel.textContent).toContain("READ");
    expect(panel.textContent).toContain("redline");
    // Thought step DOM shape.
    const thoughtRule = panel.querySelector(".trace-thought .trace-rule");
    expect(thoughtRule).not.toBeNull();
    expect(panel.querySelector(".trace-thought p")?.textContent).toBe("looking at calendar");
  });
});
