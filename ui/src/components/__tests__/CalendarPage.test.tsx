import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import dayjs from "dayjs";
import { CalendarPage } from "../CalendarPage";
import type { CalendarEvent } from "../../types";

function withProviders(ui: React.ReactElement, queryClient: QueryClient) {
  return <QueryClientProvider client={queryClient}>{ui}</QueryClientProvider>;
}

function makeEvent(uid: string, hour: number, title: string, tag = ""): CalendarEvent {
  const start = dayjs().hour(hour).minute(0).second(0).millisecond(0);
  return {
    uid,
    title,
    start: start.toISOString(),
    end: start.add(1, "hour").toISOString(),
    tag,
  };
}

const fetchMock = vi.fn();

beforeEach(() => {
  fetchMock.mockReset();
  fetchMock.mockResolvedValue({ ok: true, status: 200, json: async () => [] });
  vi.stubGlobal("fetch", fetchMock);
  // Freeze time to a deterministic in-window moment so the "now"
  // marker assertion isn't sensitive to wall-clock.
  vi.useFakeTimers();
  vi.setSystemTime(new Date("2026-05-09T14:30:00.000Z"));
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("CalendarPage", () => {
  it("renders today's events on the hour-track and the tomorrow preview", async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const todayEvents = [makeEvent("a", 11, "Series B review")];
    const tomorrowEvents = [makeEvent("b", 9, "Standup")];
    qc.setQueryData<CalendarEvent[]>(["today"], todayEvents);
    qc.setQueryData<CalendarEvent[]>(["tomorrow"], tomorrowEvents);

    render(withProviders(<CalendarPage />, qc));

    // Today's event title is on the hour track
    expect(screen.getByText("Series B review")).toBeInTheDocument();
    // Tomorrow preview header + event row
    expect(screen.getByText("tomorrow")).toBeInTheDocument();
    expect(screen.getByText("Standup")).toBeInTheDocument();
    // Header date label uses the long form (e.g. "Saturday, May 9")
    const today = dayjs();
    expect(
      screen.getByText(today.format("dddd, MMMM D")),
    ).toBeInTheDocument();
    // Summary line picks up the count
    expect(screen.getByText(/1 event/)).toBeInTheDocument();
  });

  it("renders the tomorrow eyebrow and the now marker even when tomorrow is empty", async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    qc.setQueryData<CalendarEvent[]>(["today"], []);
    qc.setQueryData<CalendarEvent[]>(["tomorrow"], []);
    render(withProviders(<CalendarPage />, qc));

    expect(screen.getByText("now")).toBeInTheDocument();
    expect(screen.getByText("tomorrow")).toBeInTheDocument();
    // Day navigation has been removed in favor of the design's
    // "today + compact tomorrow" combined view.
    expect(screen.queryByRole("button", { name: "Previous day" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Next day" })).toBeNull();
  });

  it("renders the first attendee in the tomorrow trailing column", async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    qc.setQueryData<CalendarEvent[]>(["today"], []);
    const ev = makeEvent("b", 9, "Standup");
    ev.attendees = ["Aria", "Saru", "Mara"];
    qc.setQueryData<CalendarEvent[]>(["tomorrow"], [ev]);
    render(withProviders(<CalendarPage />, qc));

    expect(screen.getByText("Aria +2")).toBeInTheDocument();
  });
});
