import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import { ConcernsPanel } from "../ConcernsPanel";
import type { Concern } from "../../types";
import { resetLiveBroker, publishLive } from "../../api/liveBroker";

function wrapper({ children }: { children: React.ReactNode }) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
}

function concern(overrides: Partial<Concern>): Concern {
  return {
    id: "id",
    name: "Concern",
    description: "Description.",
    state: "active",
    source: "model",
    confidence: 0.7,
    last_active_at: "2026-05-01T08:00:00Z",
    observation_count: 3,
    created_at: "2026-04-15T08:00:00Z",
    updated_at: "2026-05-01T08:00:00Z",
    ...overrides,
  };
}

describe("ConcernsPanel", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    resetLiveBroker();
    vi.useFakeTimers({ shouldAdvanceTime: true });
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  function mockList(concerns: Concern[]) {
    fetchMock = vi.fn((url: string, init?: RequestInit) => {
      const u = String(url);
      if (u === "/api/concerns" && (!init || init.method === undefined)) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () => ({ concerns }),
        } as Response);
      }
      return Promise.resolve({
        ok: true,
        status: 200,
        json: async () => ({}),
      } as Response);
    });
    vi.stubGlobal("fetch", fetchMock);
  }

  it("renders empty state when no concerns", async () => {
    mockList([]);
    render(<ConcernsPanel />, { wrapper });
    await waitFor(() =>
      expect(screen.getByText("Nothing yet.")).toBeInTheDocument()
    );
  });

  it("groups concerns into Pending review / Active / Paused / Archived", async () => {
    mockList([
      concern({ id: "p1", name: "Proposed thread", state: "proposed" }),
      concern({ id: "a1", name: "Active thread", state: "active" }),
      concern({ id: "pa1", name: "Paused thread", state: "paused" }),
      concern({ id: "e1", name: "Archived thread", state: "ended" }),
    ]);
    render(<ConcernsPanel />, { wrapper });

    await waitFor(() =>
      expect(screen.getByText("Pending review")).toBeInTheDocument()
    );
    expect(screen.getByText("Active")).toBeInTheDocument();
    expect(screen.getByText("Paused")).toBeInTheDocument();
    expect(screen.getByText("Archived")).toBeInTheDocument();

    // Paused and Archived collapsed by default — names not rendered.
    expect(screen.getByText("Proposed thread")).toBeInTheDocument();
    expect(screen.getByText("Active thread")).toBeInTheDocument();
    expect(screen.queryByText("Paused thread")).toBeNull();
    expect(screen.queryByText("Archived thread")).toBeNull();
  });

  it("expands the Paused section on click", async () => {
    mockList([
      concern({ id: "pa1", name: "Paused thread", state: "paused" }),
    ]);
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    render(<ConcernsPanel />, { wrapper });

    await waitFor(() =>
      expect(screen.getByText("Paused")).toBeInTheDocument()
    );
    expect(screen.queryByText("Paused thread")).toBeNull();

    await user.click(screen.getByText("Paused"));
    expect(screen.getByText("Paused thread")).toBeInTheDocument();
  });

  it("Approve fires POST /:id/approve immediately (no undo)", async () => {
    mockList([
      concern({ id: "p1", name: "Proposed", state: "proposed" }),
    ]);
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    render(<ConcernsPanel />, { wrapper });

    await waitFor(() =>
      expect(screen.getByText("Proposed")).toBeInTheDocument()
    );

    await user.click(screen.getByLabelText("Approve"));

    await waitFor(() => {
      const call = fetchMock.mock.calls.find((c) => {
        const [u, i] = c as [string, RequestInit | undefined];
        return u === "/api/concerns/p1/approve" && i?.method === "POST";
      });
      expect(call).toBeDefined();
    });
  });

  it("Dismiss shows undo toast; undo cancels the POST", async () => {
    mockList([
      concern({ id: "p1", name: "Proposed thread", state: "proposed" }),
    ]);
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    render(<ConcernsPanel />, { wrapper });

    await waitFor(() =>
      expect(screen.getByText("Proposed thread")).toBeInTheDocument()
    );

    await user.click(screen.getByLabelText("Dismiss"));

    // Row hidden, toast visible.
    expect(screen.queryByText("Proposed thread")).toBeNull();
    expect(screen.getByText(/^Dismissed/)).toBeInTheDocument();

    await user.click(screen.getByText(/Undo/));

    expect(screen.queryByText(/^Dismissed/)).toBeNull();
    const dismissCall = fetchMock.mock.calls.find((c) => {
      const [u, i] = c as [string, RequestInit | undefined];
      return u === "/api/concerns/p1/dismiss" && i?.method === "POST";
    });
    expect(dismissCall).toBeUndefined();
  });

  it("Pause fires POST /:id/state with paused (immediate)", async () => {
    mockList([
      concern({ id: "a1", name: "Active thread", state: "active" }),
    ]);
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    render(<ConcernsPanel />, { wrapper });

    await waitFor(() =>
      expect(screen.getByText("Active thread")).toBeInTheDocument()
    );

    await user.click(screen.getByLabelText("Pause"));

    await waitFor(() => {
      const call = fetchMock.mock.calls.find((c) => {
        const [u, i] = c as [string, RequestInit | undefined];
        return (
          u === "/api/concerns/a1/state" &&
          i?.method === "POST" &&
          typeof i?.body === "string" &&
          (i.body as string).includes("paused")
        );
      });
      expect(call).toBeDefined();
    });
  });

  it("End shows undo toast (deferred)", async () => {
    mockList([
      concern({ id: "a1", name: "Active thread", state: "active" }),
    ]);
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    render(<ConcernsPanel />, { wrapper });

    await waitFor(() =>
      expect(screen.getByText("Active thread")).toBeInTheDocument()
    );

    await user.click(screen.getByLabelText("End"));
    expect(screen.getByText(/^Ended/)).toBeInTheDocument();
  });

  it("renders retrospective progress text from broker events", async () => {
    mockList([
      concern({ id: "a1", name: "Active thread", state: "active" }),
    ]);
    render(<ConcernsPanel />, { wrapper });

    await waitFor(() =>
      expect(screen.getByText("Active thread")).toBeInTheDocument()
    );

    publishLive({
      kind: "concern.retrospective_progress",
      concern_id: "a1",
      processed: 47,
      total: 200,
      status: "running",
    });

    await waitFor(() =>
      expect(screen.getByText(/tagging history… 47 of ~200/)).toBeInTheDocument()
    );
  });
});
