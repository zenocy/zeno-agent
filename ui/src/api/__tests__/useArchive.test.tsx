import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { useArchive } from "../useArchive";
import type { CardsResponse } from "../../types";

function wrapperFor() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
  };
}

describe("useArchive", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("hits /api/cards/archive?date= with the requested date and returns parsed cards", async () => {
    const fixture: CardsResponse = {
      date: "2026-05-18",
      cards: [
        {
          id: "iran-war-ab12",
          date: "2026-05-18",
          src: "ask",
          src_label: "Generated",
          rel: "med",
          title: "Iran war: latest",
          sub: "...",
          meta: [],
          actions: [],
          origin: "ask",
        },
      ],
    };
    fetchMock.mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => fixture,
    });

    const { result } = renderHook(() => useArchive("2026-05-18"), {
      wrapper: wrapperFor(),
    });

    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(fetchMock).toHaveBeenCalledWith("/api/cards/archive?date=2026-05-18");
    expect(result.current.data).toEqual(fixture);
  });

  it("propagates non-2xx responses as errors", async () => {
    fetchMock.mockResolvedValue({
      ok: false,
      status: 500,
      json: async () => ({}),
    });

    const { result } = renderHook(() => useArchive("2026-05-18"), {
      wrapper: wrapperFor(),
    });

    await waitFor(() => expect(result.current.isError).toBe(true));
    expect((result.current.error as Error).message).toMatch(/500/);
  });
});
