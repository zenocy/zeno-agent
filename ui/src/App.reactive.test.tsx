import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import App from "./App";
import { MockEventSource } from "./test/mockEventSource";
import {
  publishLive,
  resetLiveBroker,
  type LiveEvent,
} from "./api/liveBroker";
import { DissolveDelayMs } from "./api/useLiveSynth";
import type { Card as CardData } from "./types";

function jsonResponse(body: unknown, status = 200) {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => body,
  } as unknown as Response;
}

function withProviders(ui: React.ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return <QueryClientProvider client={qc}>{ui}</QueryClientProvider>;
}

const askCard: CardData = {
  id: "ask-weather-1",
  date: "2026-04-29",
  src: "ask",
  src_label: "Generated",
  rel: "med",
  title: "Light rain easing by 14:00",
  sub: "Window opens for a 5K loop after lunch.",
  meta: ["13°"],
  actions: [{ label: "Dismiss" }],
  origin: "ask",
  trace_id: "trace-abc",
};

const morningCard: CardData = {
  id: "morn-1",
  date: "2026-04-29",
  src: "calendar",
  src_label: "Today",
  rel: "high",
  title: "Sync at 11.",
  sub: "Series B prep with Saru.",
  meta: [],
  actions: [{ label: "Dismiss" }],
  origin: "morning",
};

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

interface FetchOpts {
  initialCards?: CardData[];
  askResponse?: { card: CardData; trace_id: string };
  askPending?: boolean; // hold the ask mutation open
}

function makeFetchStub(opts: FetchOpts = {}): {
  fetch: ReturnType<typeof vi.fn>;
  resolveAsk: ((res: Response) => void) | null;
} {
  const ref: { resolveAsk: ((res: Response) => void) | null } = { resolveAsk: null };
  const fetchMock = vi.fn((input: RequestInfo | URL) => {
    const url = typeof input === "string" ? input : input.toString();
    if (url.startsWith("/api/ask")) {
      if (opts.askPending) {
        return new Promise<Response>((resolve) => { ref.resolveAsk = resolve; });
      }
      return Promise.resolve(jsonResponse(opts.askResponse ?? { card: askCard, trace_id: "trace-abc" }));
    }
    if (url.startsWith("/api/briefing")) {
      return Promise.resolve(jsonResponse({
        date: "2026-04-29",
        eyebrow: "this morning",
        title: "A *quiet* start.",
        summary: "Nothing pressing.",
        tension: 20,
      }));
    }
    if (url.startsWith("/api/cards/") && url.includes("/action")) {
      return Promise.resolve(jsonResponse({}, 204));
    }
    if (url.startsWith("/api/cards")) {
      return Promise.resolve(jsonResponse({ date: "2026-04-29", cards: opts.initialCards ?? [] }));
    }
    if (url.startsWith("/api/projections/calendar/")) {
      return Promise.resolve(jsonResponse([]));
    }
    if (url.startsWith("/api/projections/run-window")) {
      return Promise.resolve(jsonResponse({ start: "", end: "", condition: "" }));
    }
    return Promise.resolve(jsonResponse({}));
  });
  vi.stubGlobal("fetch", fetchMock);
  return { fetch: fetchMock, resolveAsk: ref.resolveAsk! ?? null } as unknown as {
    fetch: ReturnType<typeof vi.fn>;
    resolveAsk: ((res: Response) => void) | null;
  };
}

describe("App reactive submit flow (Phase 3 — SSE-delivered)", () => {
  beforeEach(() => {
    stubBrowser();
    resetLiveBroker();
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
    resetLiveBroker();
  });

  it("submitting an Ask clears the InputBar and shows the pending placeholder", async () => {
    makeFetchStub({ askPending: true });
    const user = userEvent.setup();
    render(withProviders(<App />));

    const input = (await screen.findByLabelText("Ask Zeno")) as HTMLInputElement;
    await user.type(input, "weather today");
    await user.keyboard("{Enter}");

    expect(input.value).toBe("");
    await waitFor(() => {
      expect(screen.getByText("Generated")).toBeInTheDocument();
      expect(screen.getByText("Generated · just now")).toBeInTheDocument();
    });
  });

  it("an Ask card arriving via SSE lands in the Generated section, not Context cards", async () => {
    makeFetchStub({ initialCards: [morningCard] });
    render(withProviders(<App />));

    // Wait for the initial cards fetch + render.
    await waitFor(() => {
      expect(screen.getByText("Sync at 11.")).toBeInTheDocument();
    });

    // SSE delivers the ask card.
    act(() => {
      MockEventSource.last().emit("card.appended", askCard);
    });

    // After the design pass the card's meta row leads with src_label
    // ("Generated") to match the Zeno V2 reference. So "Generated" now
    // appears twice — once as the section H3, once as the card's meta
    // entry — which is the intended layout.
    await waitFor(() => {
      const matches = screen.getAllByText("Generated");
      expect(matches.length).toBeGreaterThanOrEqual(2);
      expect(screen.getByText("Light rain easing by 14:00")).toBeInTheDocument();
    });

    // Sanity: the Context cards counter still reads "1 card" (morning only).
    expect(screen.getByText(/1 card$/)).toBeInTheDocument();
  });

  it("a full morning run sequence renders the LiveSynthPanel then dissolves it", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    makeFetchStub({});
    render(withProviders(<App />));

    // Drive the broker through the full timeline a real Runner would
    // produce. No HTTP needed — the panel listens directly.
    const startedEvent: LiveEvent = {
      kind: "synth.started", run_id: "run-M", stage: "morning", date: "2026-04-29",
    };
    const completed: LiveEvent = {
      kind: "synth.completed", run_id: "run-M", stage: "morning", stopped: "ok", total_ms: 1234,
    };

    act(() => {
      publishLive(startedEvent);
      publishLive({ kind: "trace.step", run_id: "run-M", stage: "cards", step: { kind: "thought", t: "looking at calendar" } });
    });

    expect(screen.getByRole("status").textContent).toContain("working");
    expect(screen.getByRole("status").textContent).toContain("looking at calendar");

    act(() => {
      publishLive(completed);
    });

    // Panel is dissolving but still mounted right after completion.
    expect(screen.getByRole("status").className).toContain("dissolving");

    act(() => {
      vi.advanceTimersByTime(DissolveDelayMs);
    });
    await waitFor(() => {
      expect(screen.queryByRole("status")).toBeNull();
    });
    vi.useRealTimers();
  });

  it("an Ask while a morning run is in flight switches the panel to the new run", async () => {
    makeFetchStub({});
    render(withProviders(<App />));

    act(() => {
      publishLive({ kind: "synth.started", run_id: "run-M", stage: "morning", date: "2026-04-29" });
      publishLive({ kind: "trace.step", run_id: "run-M", stage: "cards", step: { kind: "thought", t: "morning" } });
    });
    expect(screen.getByRole("status").textContent).toContain("working");

    act(() => {
      publishLive({ kind: "synth.started", run_id: "run-A", stage: "ask", date: "2026-04-29" });
    });

    // Eyebrow flips to "thinking" and steps reset (the new run owns the panel now).
    expect(screen.getByRole("status").textContent).toContain("thinking");
    expect(screen.getByRole("status").textContent).not.toContain("looking at calendar");
  });

  it("morning cards arriving via SSE prepend to the Context cards section, not Generated", async () => {
    makeFetchStub({ initialCards: [] });
    render(withProviders(<App />));
    await screen.findByText("Context cards");

    act(() => {
      MockEventSource.last().emit("card.appended", morningCard);
    });

    await waitFor(() => {
      expect(screen.getByText("Sync at 11.")).toBeInTheDocument();
    });
    // No Generated section yet — there's no ask card.
    expect(screen.queryByText("Generated")).toBeNull();
  });
});
