import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { StatePill } from "../StatePill";

describe("StatePill", () => {
  it("renders the human-readable state label", () => {
    render(<StatePill state="pre_meeting" tension={80} />);
    expect(screen.getByText("pre-meeting")).toBeInTheDocument();
  });

  it("falls back to calm morning when state is undefined", () => {
    render(<StatePill state={undefined} tension={35} />);
    expect(screen.getByText("calm morning")).toBeInTheDocument();
  });

  it("color class tracks tension band", () => {
    const cases: Array<{ tension: number; cls: string }> = [
      { tension: 10, cls: "bg-blue-500/20" },
      { tension: 35, cls: "bg-emerald-500/20" },
      { tension: 50, cls: "bg-stone-500/20" },
      { tension: 65, cls: "bg-amber-500/20" },
      { tension: 90, cls: "bg-rose-500/20" },
    ];
    for (const { tension, cls } of cases) {
      const { unmount } = render(<StatePill state="morning_calm" tension={tension} />);
      const el = screen.getByRole("status");
      expect(el.className).toContain(cls);
      unmount();
    }
  });

  it("ARIA label includes state and tension", () => {
    render(<StatePill state="deep_work" tension={20} />);
    const el = screen.getByRole("status");
    expect(el.getAttribute("aria-label")).toBe(
      "Today's register: deep work, tension 20",
    );
  });

  it("title tooltip shows state and tension", () => {
    render(<StatePill state="message_inject" tension={88} />);
    const el = screen.getByRole("status");
    expect(el.getAttribute("title")).toBe("signal · tension 88");
  });
});
