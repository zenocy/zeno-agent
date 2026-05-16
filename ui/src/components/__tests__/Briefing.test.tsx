import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, render, screen } from "@testing-library/react";
import dayjs from "dayjs";
import { Briefing } from "../Briefing";
import type { Briefing as BriefingData, CalendarEvent } from "../../types";

const sample: BriefingData = {
  date: "2026-04-25",
  eyebrow: "morning brief",
  title: "A *calm* start.",
  summary: "Three things surface today.",
  tension: 30,
};

describe("Briefing", () => {
  it("renders loading skeletons when isLoading is true", () => {
    const { container } = render(<Briefing isLoading={true} />);
    // Loading state renders divs with opacity classes, no summary text
    expect(screen.queryByText("Three things surface today.")).toBeNull();
    expect(container.querySelector("section")).toBeTruthy();
  });

  it("renders the no-briefing message when data is null", () => {
    render(<Briefing isLoading={false} data={null} />);
    expect(
      screen.getByText(/synthesis hasn't run today/i)
    ).toBeInTheDocument();
  });

  it("renders eyebrow, title, and summary when data is present", () => {
    render(<Briefing isLoading={false} data={sample} />);
    expect(screen.getByText("morning brief")).toBeInTheDocument();
    expect(screen.getByText("calm")).toBeInTheDocument(); // inside <em>
    expect(screen.getByText("Three things surface today.")).toBeInTheDocument();
  });

  it("applies accent class for tension below 40", () => {
    const { container } = render(
      <Briefing isLoading={false} data={{ ...sample, tension: 25 }} />
    );
    const bar = container.querySelector(".bg-accent");
    expect(bar).toBeTruthy();
  });

  it("applies amber class for tension between 40 and 69", () => {
    const { container } = render(
      <Briefing isLoading={false} data={{ ...sample, tension: 55 }} />
    );
    const bar = container.querySelector(".bg-amber");
    expect(bar).toBeTruthy();
  });

  it("applies crit class for tension 70 or above", () => {
    const { container } = render(
      <Briefing isLoading={false} data={{ ...sample, tension: 80 }} />
    );
    const bar = container.querySelector(".bg-crit");
    expect(bar).toBeTruthy();
  });

  it("falls back to state-derived eyebrow when eyebrow is empty", () => {
    render(
      <Briefing
        isLoading={false}
        data={{ ...sample, eyebrow: "", state: "end_of_day" }}
      />,
    );
    expect(screen.getByText("end of day")).toBeInTheDocument();
  });

  it("falls back to morning brief when both eyebrow and state are empty", () => {
    render(
      <Briefing
        isLoading={false}
        data={{ ...sample, eyebrow: "" }}
      />,
    );
    expect(screen.getByText("morning brief")).toBeInTheDocument();
  });
});

describe("Briefing — pinned next meeting", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-05-09T10:52:00.000Z"));
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  function buildEvent(): CalendarEvent {
    // 8 minutes from frozen now.
    const start = dayjs("2026-05-09T11:00:00.000Z");
    return {
      uid: "ev-1",
      title: "Series B narrative review",
      start: start.toISOString(),
      end: start.add(45, "minute").toISOString(),
      attendees: ["Saru Patel", "Lin Vega", "Park Choi"],
      location: "https://meet.example/sb-review",
    };
  }

  it("renders the pinned card when state=pre_meeting and a nextEvent is in range", () => {
    render(
      <Briefing
        isLoading={false}
        data={{ ...sample, state: "pre_meeting" }}
        nextEvent={buildEvent()}
      />,
    );
    expect(screen.getByTestId("pinned-next-meeting")).toBeInTheDocument();
    expect(screen.getByText("Series B narrative review")).toBeInTheDocument();
    // Attendee initials surface
    expect(screen.getByText("SP")).toBeInTheDocument();
    expect(screen.getByText("LV")).toBeInTheDocument();
    // Join + Brief buttons render
    expect(screen.getByRole("button", { name: /Join/ })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Brief/ })).toBeInTheDocument();
  });

  it("does NOT render the pinned card when state is morning_calm", () => {
    render(
      <Briefing
        isLoading={false}
        data={{ ...sample, state: "morning_calm" }}
        nextEvent={buildEvent()}
      />,
    );
    expect(screen.queryByTestId("pinned-next-meeting")).not.toBeInTheDocument();
  });

  it("does NOT render the pinned card when nextEvent is null", () => {
    render(
      <Briefing
        isLoading={false}
        data={{ ...sample, state: "pre_meeting" }}
        nextEvent={null}
      />,
    );
    expect(screen.queryByTestId("pinned-next-meeting")).not.toBeInTheDocument();
  });

  it("countdown advances when the clock ticks", () => {
    render(
      <Briefing
        isLoading={false}
        data={{ ...sample, state: "pre_meeting" }}
        nextEvent={buildEvent()}
      />,
    );
    // Initial: 8 minutes till start.
    expect(screen.getByText(/in 8:00/)).toBeInTheDocument();
    // Advance the clock by 30 seconds; countdown re-renders.
    act(() => {
      vi.advanceTimersByTime(30_000);
    });
    expect(screen.getByText(/in 7:30/)).toBeInTheDocument();
  });
});
