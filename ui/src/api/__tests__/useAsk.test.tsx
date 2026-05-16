import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { useAsk } from "../useAsk";

function wrapperFor(qc: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
  };
}

describe("useAsk", () => {
  beforeEach(() => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        new Response(
          JSON.stringify({
            card: {
              id: "ask-1",
              date: "2026-04-29",
              src: "ask",
              src_label: "Generated",
              rel: "med",
              title: "ok",
              sub: "ok",
              meta: [],
              actions: [{ label: "Dismiss" }],
            },
            trace_id: "trace-x",
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      ),
    );
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  // V2.4 P3: the cards cache is mutated via SSE (`card.appended`),
  // not by the mutation. Pin that no invalidation happens — otherwise
  // the `[cards]` query would refetch on every Ask, causing a visible
  // flash of the existing morning cards.
  it("does not invalidate the cards cache on success", async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    qc.setQueryData(["cards"], { date: "2026-04-29", cards: [] });

    const invalidateSpy = vi.spyOn(qc, "invalidateQueries");

    const { result } = renderHook(() => useAsk(), { wrapper: wrapperFor(qc) });
    act(() => {
      result.current.mutate("hello");
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));

    expect(invalidateSpy).not.toHaveBeenCalled();
  });

  it("resolves successfully with the parsed response on a 200", async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const { result } = renderHook(() => useAsk(), { wrapper: wrapperFor(qc) });

    act(() => {
      result.current.mutate("hello");
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));

    expect(result.current.data?.card.id).toBe("ask-1");
    expect(result.current.data?.trace_id).toBe("trace-x");
  });

  it("rejects with the existing /api/ask <status> error message on a non-2xx", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response("upstream", { status: 503 })),
    );

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const { result } = renderHook(() => useAsk(), { wrapper: wrapperFor(qc) });

    act(() => {
      result.current.mutate("hello");
    });
    await waitFor(() => expect(result.current.isError).toBe(true));

    expect(result.current.error?.message).toBe("/api/ask 503");
  });
});
