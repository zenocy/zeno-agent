import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { RightRail } from "../RightRail";

function wrapperFor(qc: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
  };
}

const tasksPayload = [
  { uid: "a", title: "Ship V2.6 plan", completed: false, priority: "high", due_date: "2026-05-10" },
];

const calendarPayload = [
  { uid: "evt-1", title: "Series B sync", start: "2026-05-09T11:00:00Z", end: "2026-05-09T11:30:00Z", tag: "work" },
];

function stubFetch(byPath: Record<string, unknown>) {
  return vi.fn(async (input: RequestInfo | URL) => {
    const url = typeof input === "string" ? input : input.toString();
    for (const [prefix, body] of Object.entries(byPath)) {
      if (url.startsWith(prefix)) {
        return new Response(JSON.stringify(body), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }
    }
    return new Response("[]", { status: 200, headers: { "Content-Type": "application/json" } });
  });
}

function stubLocalStorage() {
  // Pre-seed an empty pinned-widget list so the rail doesn't try to
  // render the default Weather widget (which would need its own fetch
  // stubbing — orthogonal to what this test cares about).
  const store: Record<string, string> = { "zeno.pinnedWidgets": "[]" };
  vi.stubGlobal("localStorage", {
    getItem: (k: string) => (k in store ? store[k] : null),
    setItem: (k: string, v: string) => { store[k] = v; },
    removeItem: (k: string) => { delete store[k]; },
    clear: () => { for (const k of Object.keys(store)) delete store[k]; },
    key: (i: number) => Object.keys(store)[i] ?? null,
    get length() { return Object.keys(store).length; },
  });
}

describe("RightRail", () => {
  beforeEach(() => {
    stubLocalStorage();
    vi.stubGlobal("fetch", stubFetch({
      "/api/projections/calendar/today": [],
      "/api/projections/tasks/open": [],
    }));
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("renders nothing when hidden", () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const { container } = render(<RightRail hidden />, { wrapper: wrapperFor(qc) });
    expect(container.firstChild).toBeNull();
  });

  it("renders the Pinned heading and the Today attention horizon", async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<RightRail />, { wrapper: wrapperFor(qc) });
    await waitFor(() => {
      expect(screen.getByText("Pinned")).toBeInTheDocument();
      expect(screen.getByText("Today")).toBeInTheDocument();
    });
  });

  it("does NOT render an Open tasks strip — tasks moved out of the rail", async () => {
    vi.stubGlobal("fetch", stubFetch({
      "/api/projections/calendar/today": [],
      "/api/projections/tasks/open": tasksPayload,
    }));
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<RightRail />, { wrapper: wrapperFor(qc) });
    // Rail no longer surfaces tasks; the Tasks page is the canonical surface.
    await waitFor(() => {
      expect(screen.getByText("Today")).toBeInTheDocument();
    });
    expect(screen.queryByText("Open tasks")).toBeNull();
    expect(screen.queryByText("Ship V2.6 plan")).toBeNull();
  });

  it("renders Today's events as attention-stream items", async () => {
    vi.stubGlobal("fetch", stubFetch({
      "/api/projections/calendar/today": calendarPayload,
      "/api/projections/tasks/open": [],
    }));
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<RightRail />, { wrapper: wrapperFor(qc) });
    await waitFor(() => {
      expect(screen.getByText("Series B sync")).toBeInTheDocument();
    });
  });
});
