import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { WorldClockWidget } from "../WorldClockWidget";

function wrapperFor(qc: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
  };
}

function stubSettingsFetch(worldClocks: string) {
  return vi.fn(async (input: RequestInfo | URL) => {
    const url = typeof input === "string" ? input : input.toString();
    if (url === "/api/settings") {
      return new Response(
        JSON.stringify({
          timezone: "UTC",
          city: "",
          country: "",
          latitude: 0,
          longitude: 0,
          stock_tickers: "",
          stock_threshold_pct: 0,
          stock_always_poll: false,
          world_clocks: worldClocks,
          set: true,
        }),
        { status: 200, headers: { "Content-Type": "application/json" } }
      );
    }
    return new Response("{}", { status: 200 });
  });
}

describe("WorldClockWidget", () => {
  beforeEach(() => {
    // Pin Date only — leaving setTimeout/queueMicrotask real so React
    // Query's fetch + state updates can complete during the test.
    // 2026-05-09T12:34:00Z → LA: 05:34, London: 13:34, Kolkata: 18:04.
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-05-09T12:34:00Z"));
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it("shows the empty-state copy when no clocks are configured", async () => {
    vi.stubGlobal("fetch", stubSettingsFetch(""));
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<WorldClockWidget />, { wrapper: wrapperFor(qc) });
    await waitFor(() =>
      expect(screen.getByText(/add timezones in Settings/i)).toBeInTheDocument()
    );
  });

  it("renders one row per configured tz with derived city + HH:MM", async () => {
    vi.stubGlobal(
      "fetch",
      stubSettingsFetch("America/Los_Angeles,Europe/London,Asia/Kolkata")
    );
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<WorldClockWidget />, { wrapper: wrapperFor(qc) });

    await waitFor(() => {
      expect(screen.getByText("Los Angeles")).toBeInTheDocument();
      expect(screen.getByText("London")).toBeInTheDocument();
      expect(screen.getByText("Kolkata")).toBeInTheDocument();
    });

    // Concrete time strings derived from the pinned UTC instant. These
    // aren't sensitive to DST drift in 2026 because the offsets for
    // 2026-05-09 are fixed: PDT -7, BST +1, IST +5:30.
    expect(screen.getByText("05:34")).toBeInTheDocument();
    expect(screen.getByText("13:34")).toBeInTheDocument();
    expect(screen.getByText("18:04")).toBeInTheDocument();
  });

  it("invokes onUnpin when the pin icon is clicked", async () => {
    vi.stubGlobal("fetch", stubSettingsFetch("Europe/London"));
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const onUnpin = vi.fn();
    render(<WorldClockWidget onUnpin={onUnpin} />, { wrapper: wrapperFor(qc) });

    const pin = await screen.findByRole("button", { name: /unpin/i });
    pin.click();
    expect(onUnpin).toHaveBeenCalledTimes(1);
  });
});
