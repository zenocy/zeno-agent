import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import { ConcernMergeModal } from "../ConcernMergeModal";
import type { Concern } from "../../types";

function wrapper({ children }: { children: React.ReactNode }) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
}

function concern(o: Partial<Concern>): Concern {
  return {
    id: "id",
    name: "Concern",
    description: "Desc",
    state: "active",
    source: "user",
    confidence: 1,
    last_active_at: "2026-05-01T08:00:00Z",
    observation_count: 0,
    created_at: "2026-04-15T08:00:00Z",
    updated_at: "2026-05-01T08:00:00Z",
    ...o,
  };
}

describe("ConcernMergeModal", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn(() =>
      Promise.resolve({
        ok: true,
        status: 200,
        json: async () => concern({ id: "src", state: "merged" }),
      } as Response)
    );
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("renders dropdown with candidate names", () => {
    const source = concern({ id: "src", name: "Frankfurt trip" });
    const candidates = [
      concern({ id: "t1", name: "Travel — Q3 2026" }),
      concern({ id: "t2", name: "Family schedule" }),
    ];
    render(
      <ConcernMergeModal source={source} candidates={candidates} onClose={() => {}} />,
      { wrapper }
    );
    expect(screen.getByText("Travel — Q3 2026")).toBeInTheDocument();
    expect(screen.getByText("Family schedule")).toBeInTheDocument();
  });

  it("issues POST /:id/merge with into_id on submit", async () => {
    const source = concern({ id: "src", name: "Frankfurt trip" });
    const candidates = [concern({ id: "t1", name: "Travel" })];
    const onClose = vi.fn();
    const user = userEvent.setup();

    render(
      <ConcernMergeModal source={source} candidates={candidates} onClose={onClose} />,
      { wrapper }
    );
    await user.click(screen.getByRole("button", { name: "Merge" }));

    await waitFor(() => {
      const call = fetchMock.mock.calls.find((c) => {
        const [u, i] = c as [string, RequestInit | undefined];
        return u === "/api/concerns/src/merge" && i?.method === "POST";
      });
      expect(call).toBeDefined();
      if (call) {
        const init = call[1] as RequestInit;
        expect(JSON.parse(init.body as string)).toEqual({ into_id: "t1" });
      }
    });
    await waitFor(() => expect(onClose).toHaveBeenCalled());
  });

  it("handles empty candidates gracefully", () => {
    const source = concern({ id: "src", name: "Frankfurt trip" });
    render(
      <ConcernMergeModal source={source} candidates={[]} onClose={() => {}} />,
      { wrapper }
    );
    expect(
      screen.getByText(/No other active or paused threads/)
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Merge" })).toBeDisabled();
  });

  it("ESC key triggers onClose", async () => {
    const onClose = vi.fn();
    const source = concern({ id: "src" });
    render(
      <ConcernMergeModal
        source={source}
        candidates={[concern({ id: "t1", name: "T1" })]}
        onClose={onClose}
      />,
      { wrapper }
    );
    await userEvent.keyboard("{Escape}");
    expect(onClose).toHaveBeenCalled();
  });
});
