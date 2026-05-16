import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { InputBar } from "../InputBar";

const FALLBACK = "Draft my reply to Saru using the redline?";

describe("InputBar", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  function flushGhost(target: string) {
    // 18ms per char in the component; advance enough to fully reveal.
    act(() => {
      vi.advanceTimersByTime(target.length * 18 + 20);
    });
  }

  it("renders the suggestion prop as ghost text", () => {
    const suggestion = "Block 17:00 for Lia's recital?";
    render(<InputBar onSubmit={() => {}} suggestion={suggestion} />);
    flushGhost(suggestion);
    expect(screen.getByText(suggestion)).toBeInTheDocument();
  });

  it("falls back to the default suggestion when prop is missing", () => {
    render(<InputBar onSubmit={() => {}} />);
    flushGhost(FALLBACK);
    expect(screen.getByText(FALLBACK)).toBeInTheDocument();
  });

  it("re-runs the ghost animation when the suggestion prop changes", () => {
    const first = "First suggestion";
    const second = "Second";
    const { rerender } = render(<InputBar onSubmit={() => {}} suggestion={first} />);
    flushGhost(first);
    expect(screen.getByText(first)).toBeInTheDocument();

    rerender(<InputBar onSubmit={() => {}} suggestion={second} />);
    // Before advancing time, the new ghost should be empty / partial; after, full.
    flushGhost(second);
    expect(screen.getByText(second)).toBeInTheDocument();
    expect(screen.queryByText(first)).toBeNull();
  });

  it("Tab key fills the input with the suggestion when ghost is fully shown", async () => {
    vi.useRealTimers();
    const user = userEvent.setup();
    const suggestion = "What's on today?";
    render(<InputBar onSubmit={() => {}} suggestion={suggestion} />);
    // Wait for the real-timer ghost animation to finish.
    await act(async () => {
      await new Promise((r) => setTimeout(r, suggestion.length * 18 + 60));
    });

    const input = screen.getByLabelText("Ask Zeno") as HTMLInputElement;
    input.focus();
    await user.keyboard("{Tab}");
    expect(input.value).toBe(suggestion);
  });

  it("Enter submits the typed query and clears the input", async () => {
    vi.useRealTimers();
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    render(<InputBar onSubmit={onSubmit} />);
    const input = screen.getByLabelText("Ask Zeno") as HTMLInputElement;
    await user.type(input, "weather today");
    await user.keyboard("{Enter}");
    expect(onSubmit).toHaveBeenCalledWith("weather today");
    expect(input.value).toBe("");
  });

  it("clicking a chip submits its label", async () => {
    vi.useRealTimers();
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    render(<InputBar onSubmit={onSubmit} />);
    await user.click(screen.getByText("What should I focus on?"));
    expect(onSubmit).toHaveBeenCalledWith("What should I focus on?");
  });

  it("renders pre_meeting chips when state='pre_meeting'", () => {
    render(<InputBar onSubmit={() => {}} state="pre_meeting" />);
    expect(screen.getByText("What's on for the meeting?")).toBeInTheDocument();
    expect(screen.getByText("Draft a one-liner")).toBeInTheDocument();
    expect(screen.queryByText("What should I focus on?")).toBeNull();
  });

  it("renders deep_work chips when state='deep_work'", () => {
    render(<InputBar onSubmit={() => {}} state="deep_work" />);
    expect(screen.getByText("What's the one thing?")).toBeInTheDocument();
    expect(screen.getByText("Hold pings until 17:00")).toBeInTheDocument();
  });

  it("renders end_of_day chips when state='end_of_day'", () => {
    render(<InputBar onSubmit={() => {}} state="end_of_day" />);
    expect(screen.getByText("Tomorrow's first thing")).toBeInTheDocument();
    expect(screen.getByText("What closed today?")).toBeInTheDocument();
  });

  it("falls back to morning_calm chips when state is undefined", () => {
    render(<InputBar onSubmit={() => {}} />);
    expect(screen.getByText("What should I focus on?")).toBeInTheDocument();
    expect(screen.getByText("What's tonight?")).toBeInTheDocument();
  });

  it("state-specific chip click fires onSubmit with the chip label", async () => {
    vi.useRealTimers();
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    render(<InputBar onSubmit={onSubmit} state="deep_work" />);
    await user.click(screen.getByText("Hold pings until 17:00"));
    expect(onSubmit).toHaveBeenCalledWith("Hold pings until 17:00");
  });
});
