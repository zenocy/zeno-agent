import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import App from "./App";
import { MockEventSource } from "./test/mockEventSource";

function jsonResponse(body: unknown, status = 200) {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => body,
  } as unknown as Response;
}

function withProviders(ui: React.ReactElement) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return <QueryClientProvider client={qc}>{ui}</QueryClientProvider>;
}

function stubBrowser() {
  const store: Record<string, string> = {};
  vi.stubGlobal("localStorage", {
    getItem: (k: string) => (k in store ? store[k] : null),
    setItem: (k: string, v: string) => { store[k] = v; },
    removeItem: (k: string) => { delete store[k]; },
    clear: () => { for (const k of Object.keys(store)) delete store[k]; },
    key: (i: number) => Object.keys(store)[i] ?? null,
    get length() { return Object.keys(store).length; },
  });
  vi.stubGlobal("matchMedia", () => ({
    matches: false,
    addEventListener: () => {},
    removeEventListener: () => {},
    addListener: () => {},
    removeListener: () => {},
    dispatchEvent: () => false,
    media: "",
    onchange: null,
  }));
  MockEventSource.reset();
  vi.stubGlobal("EventSource", MockEventSource);
}

interface BriefingStub {
  state: string;
  tension: number;
}

function fetchStub(briefing: BriefingStub) {
  return vi.fn((input: RequestInfo | URL) => {
    const url = typeof input === "string" ? input : input.toString();
    if (url.startsWith("/api/briefing")) {
      return Promise.resolve(jsonResponse({
        date: "2026-04-27",
        eyebrow: "this morning",
        title: "Heads down.",
        summary: "Three protected hours.",
        tension: briefing.tension,
        state: briefing.state,
      }));
    }
    if (url.startsWith("/api/cards")) {
      return Promise.resolve(jsonResponse({ date: "2026-04-27", cards: [] }));
    }
    if (url.startsWith("/api/projections/calendar/")) {
      // today / tomorrow / week all return a CalendarEvent[]
      return Promise.resolve(jsonResponse([]));
    }
    return Promise.resolve(jsonResponse({}));
  });
}

describe("App — deep_work hides RightRail by default", () => {
  beforeEach(() => {
    stubBrowser();
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("hides the RightRail when briefing.state is deep_work and restores it via the Topbar button", async () => {
    vi.stubGlobal("fetch", fetchStub({ state: "deep_work", tension: 20 }));
    const user = userEvent.setup();
    render(withProviders(<App />));

    // Wait for the briefing query to land — pill renders once tension is known.
    await screen.findByText("deep work");

    // RightRail is suppressed: the "Today" sidebar header (rendered only inside RightRail) is absent.
    await waitFor(() => {
      expect(screen.queryByText("Today")).toBeNull();
    });

    // Topbar exposes the restore button while the rail is hidden.
    const restoreBtn = await screen.findByRole("button", { name: "Show side panel" });
    await user.click(restoreBtn);

    // After the click the rail re-renders and "Today" is back.
    await waitFor(() => {
      expect(screen.getByText("Today")).toBeInTheDocument();
    });
  });

  it("keeps the RightRail visible for non-deep_work states", async () => {
    vi.stubGlobal("fetch", fetchStub({ state: "morning_calm", tension: 32 }));
    render(withProviders(<App />));

    // The rail's "Today" header renders for morning_calm.
    await waitFor(() => {
      expect(screen.getByText("Today")).toBeInTheDocument();
    });
    expect(screen.queryByRole("button", { name: "Show side panel" })).toBeNull();
  });

  it("clicking the Settings nav button switches to the SettingsPanel", async () => {
    // Extend the briefing-only stub to also serve /api/settings (the
    // SettingsPanel hits it on mount).
    vi.stubGlobal(
      "fetch",
      vi.fn((input: RequestInfo | URL) => {
        const url = typeof input === "string" ? input : input.toString();
        if (url.startsWith("/api/settings")) {
          return Promise.resolve(jsonResponse({
            timezone: "Europe/Athens", city: "Athens", country: "Greece",
            latitude: 37.9838, longitude: 23.7275, set: true,
          }));
        }
        if (url.startsWith("/api/briefing")) {
          return Promise.resolve(jsonResponse({
            date: "2026-04-27", eyebrow: "now", title: "T", summary: "S",
            tension: 30, state: "morning_calm",
          }));
        }
        if (url.startsWith("/api/cards")) {
          return Promise.resolve(jsonResponse({ date: "2026-04-27", cards: [] }));
        }
        if (
          url.startsWith("/api/projections/calendar/") ||
          url.startsWith("/api/projections/run-window") ||
          url.startsWith("/api/projections/weather")
        ) {
          return Promise.resolve(jsonResponse([]));
        }
        return Promise.resolve(jsonResponse({}));
      })
    );

    const user = userEvent.setup();
    render(withProviders(<App />));

    // Wait for the briefing route to render first.
    await screen.findByText("Today");

    await user.click(screen.getByRole("button", { name: "Settings" }));

    // Settings page header is unique to SettingsPanel — confirms the route flipped.
    await waitFor(() =>
      expect(screen.getByRole("heading", { name: "Settings" })).toBeInTheDocument()
    );
    // And the form fields hydrated from the GET response.
    await waitFor(() =>
      expect((screen.getByLabelText(/^City$/) as HTMLInputElement).value).toBe("Athens")
    );
  });
});
