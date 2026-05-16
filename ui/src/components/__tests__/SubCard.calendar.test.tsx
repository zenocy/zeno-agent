import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { SubCard } from "../SubCard";
import { ToastProvider } from "../Toast";
import type { SubCard as SubCardData } from "../../types";

function wrapper({ children }: { children: React.ReactNode }) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return (
    <QueryClientProvider client={qc}>
      <ToastProvider>{children}</ToastProvider>
    </QueryClientProvider>
  );
}

const fetchMock = vi.fn();

beforeEach(() => {
  fetchMock.mockReset();
  // Intent-modes lookup (used by SubCard.dispatch) — return empty list so
  // every action defaults to direct-dispatch in tests.
  fetchMock.mockResolvedValue({
    ok: true,
    status: 200,
    json: async () => ({ intents: [] }),
  });
  vi.stubGlobal("fetch", fetchMock);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

const richCal: SubCardData = {
  id: "sub-rich",
  kind: "calendar",
  eyebrow: "calendar · proposed",
  title: "Block 17:00 → 18:30 Thursday",
  cal: {
    title: "Lia's school recital",
    when: "Thu · 17:00 → 18:30",
    where: "Sutter Heights Elementary",
    who: "Sam, Lia",
    start: "17:00",
    end: "18:30",
    travel_before: 15,
    reminder: "leave by 16:40",
    attendees: [
      { name: "Sam", role: "partner", status: "accepted" },
      { name: "Lia", role: "daughter", status: "host" },
    ],
    conflict: { ok: true, text: "Range Ventures ends 15:45 — 75 min buffer." },
    reasoning: ["Sam is free from 16:30.", "No focus blocks after 17:00 Thursdays."],
    alternatives: [{ when: "Wed · 17:00 → 18:30", note: "lighter calendar" }],
    recurring: { label: "Hold every Thursday", default: false },
    daystrip: {
      label: "Thu, May 14",
      start_hr: 9,
      end_hr: 21,
      events: [
        { start: 11.0, end: 12.0, label: "Series B", kind: "muted" },
        { start: 16.75, end: 17.0, label: "travel", kind: "travel" },
        { start: 17.0, end: 18.5, label: "Recital", kind: "proposed" },
      ],
    },
  },
  actions: [{ label: "Confirm & send", primary: true }],
};

const minimalCal: SubCardData = {
  id: "sub-min",
  kind: "calendar",
  eyebrow: "calendar · proposed",
  title: "Block 12:30 → 13:30",
  cal: {
    title: "Run window",
    when: "Tue · 12:30 → 13:30",
    where: "Solo",
    who: "Solo",
  },
  actions: [{ label: "Confirm & send", primary: true }],
};

describe("SubCard calendar — rich rendering", () => {
  it("renders title + duration pill, attendee statuses, conflict, reasoning, alternatives", () => {
    render(<SubCard cardId="card-1" reply={richCal} />, { wrapper });

    expect(screen.getByText("Lia's school recital")).toBeInTheDocument();
    expect(screen.getByText("1h 30m")).toBeInTheDocument();
    // Attendee pills carry status symbols
    expect(screen.getByText("✦")).toBeInTheDocument(); // host
    expect(screen.getByText("●")).toBeInTheDocument(); // accepted
    // Travel row
    expect(
      screen.getByText("+15 min before · auto-blocked"),
    ).toBeInTheDocument();
    // Conflict box
    expect(
      screen.getByText(/Range Ventures ends 15:45/),
    ).toBeInTheDocument();
    // Why this slot — open by default
    expect(screen.getByText("why this slot")).toBeInTheDocument();
    expect(screen.getByText("Sam is free from 16:30.")).toBeInTheDocument();
    // Alternatives
    expect(screen.getByText("other windows")).toBeInTheDocument();
    expect(screen.getByText("Wed · 17:00 → 18:30")).toBeInTheDocument();
    // Recurring
    expect(screen.getByText("Hold every Thursday")).toBeInTheDocument();
    // Day-strip events show their labels via the title attribute (and text)
    expect(screen.getByText("Recital")).toBeInTheDocument();
  });

  it("alternatives picker toggles selected state", async () => {
    const user = userEvent.setup();
    render(<SubCard cardId="card-1" reply={richCal} />, { wrapper });

    const altBtn = screen.getByRole("button", { name: /Wed · 17:00 → 18:30/ });
    await user.click(altBtn);
    // Once picked the radio mark flips to ●
    expect(altBtn.textContent).toContain("●");
  });

  it("why-this-slot collapses on click", async () => {
    const user = userEvent.setup();
    render(<SubCard cardId="card-1" reply={richCal} />, { wrapper });

    expect(screen.getByText("Sam is free from 16:30.")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /why this slot/ }));
    expect(
      screen.queryByText("Sam is free from 16:30."),
    ).not.toBeInTheDocument();
  });
});

describe("SubCard calendar — legacy minimal rendering", () => {
  it("falls back to the minimal grid when only title/when/where/who are set", () => {
    render(<SubCard cardId="card-1" reply={minimalCal} />, { wrapper });

    // Minimal grid surfaces all four labels
    expect(screen.getByText("title")).toBeInTheDocument();
    expect(screen.getByText("when")).toBeInTheDocument();
    expect(screen.getByText("where")).toBeInTheDocument();
    expect(screen.getByText("who")).toBeInTheDocument();
    expect(screen.getByText("Run window")).toBeInTheDocument();
    // No rich-only blocks
    expect(screen.queryByText("why this slot")).not.toBeInTheDocument();
    expect(screen.queryByText("other windows")).not.toBeInTheDocument();
  });
});
