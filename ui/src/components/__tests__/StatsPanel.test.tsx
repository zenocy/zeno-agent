import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { StatsPanel } from "../StatsPanel";
import type { StatsSnapshot } from "../../api/useStats";

function wrapper({ children }: { children: React.ReactNode }) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
}

const fixture: StatsSnapshot = {
  ts: "2026-05-05T09:00:00Z",
  uptime_sec: 1234,
  memory_facts: 42,
  llm: {
    cards: { count: 12, ok: 11, err: 1, last_ms: 2400, max_ms: 5800, avg_ms: 2100 },
  },
  llm_retries: { succeeded: 3, exhausted: 1 },
  synth: {
    cards: { runs: 5, ok: 4, degraded: 1, failed: 0, last_outcome: "ok", last_dur_ms: 28000, last_at: "2026-05-05T07:00:30Z" },
    briefing: { runs: 5, ok: 5, degraded: 0, failed: 0, last_outcome: "ok", last_dur_ms: 32000, last_at: "2026-05-05T07:01:02Z" },
  },
  sensors: {
    imap: { runs: 144, ok: 143, err: 1, last_outcome: "ok", last_dur_ms: 800, last_at: "2026-05-05T08:50:00Z", records: { mail: 17 } },
  },
  cron: {
    morning_synth: { runs: 1, ok: 1, err: 0, last_outcome: "ok", last_at: "2026-05-05T07:00:00Z" },
  },
  http: { requests: 482, slow: 4, status_2xx: 470, status_4xx: 8, status_5xx: 4 },
  sse: { subscribers: 1, dropped_total: 0 },
  db: {
    select: { count: 6, ok: 6, err: 0, last_ms: 280, max_ms: 720, avg_ms: 320 },
  },
};

describe("StatsPanel", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => fixture,
    });
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("renders all sections with values from the fixture", async () => {
    render(<StatsPanel />, { wrapper });

    await waitFor(() => {
      expect(screen.getByText("Operations")).toBeInTheDocument();
    });
    await waitFor(() => {
      // memory_facts gauge
      expect(screen.getByText("42")).toBeInTheDocument();
    });

    // Synth section
    expect(screen.getByText("Synth")).toBeInTheDocument();
    // "cards" appears as a label in both Synth and LLM sections.
    expect(screen.getAllByText("cards").length).toBeGreaterThanOrEqual(2);
    expect(screen.getByText("briefing")).toBeInTheDocument();
    expect(
      screen.getByText(/ok · 28\.0s · ok=4 degraded=1 failed=0/)
    ).toBeInTheDocument();

    // Sensors
    expect(screen.getByText("Sensors")).toBeInTheDocument();
    expect(screen.getByText(/ok · 800ms · ok=143 err=1 · mail=17/)).toBeInTheDocument();

    // LLM
    expect(screen.getByText("LLM")).toBeInTheDocument();
    expect(screen.getByText(/calls=12 avg=2\.1s max=5\.8s err=1/)).toBeInTheDocument();
    expect(screen.getByText(/succeeded=3 · exhausted=1/)).toBeInTheDocument();

    // HTTP
    expect(
      screen.getByText(
        /total=482 2xx=470 4xx=8 5xx=4 slow=4/
      )
    ).toBeInTheDocument();

    // Cron
    expect(screen.getByText("morning_synth")).toBeInTheDocument();

    // DB slow queries
    expect(screen.getByText("DB (slow queries)")).toBeInTheDocument();
    expect(screen.getByText(/count=6 avg=320ms max=720ms/)).toBeInTheDocument();
  });

  it("handles missing optional sections gracefully", async () => {
    fetchMock.mockReset();
    fetchMock.mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({
        ts: "2026-05-05T09:00:00Z",
        uptime_sec: 60,
        memory_facts: 0,
        http: { requests: 0, slow: 0, status_2xx: 0, status_4xx: 0, status_5xx: 0 },
        sse: { subscribers: 0, dropped_total: 0 },
      }),
    });

    render(<StatsPanel />, { wrapper });

    await waitFor(() => {
      expect(screen.getByText("HTTP")).toBeInTheDocument();
    });
    expect(screen.queryByText("Synth")).toBeNull();
    expect(screen.queryByText("Sensors")).toBeNull();
    expect(screen.queryByText("LLM")).toBeNull();
  });

  it("surfaces fetch errors", async () => {
    fetchMock.mockReset();
    fetchMock.mockResolvedValue({
      ok: false,
      status: 500,
      json: async () => ({}),
    });

    render(<StatsPanel />, { wrapper });

    await waitFor(() => {
      expect(
        screen.getByText(/\/api\/metrics\/snapshot returned 500/)
      ).toBeInTheDocument();
    });
  });
});
