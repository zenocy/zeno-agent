import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { renderHook, act, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import {
  useConcernAction,
  ConcernStateError,
} from "../useConcernAction";

function wrapper({ children }: { children: React.ReactNode }) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
}

function jsonOk(body: unknown) {
  return {
    ok: true,
    status: 200,
    json: async () => body,
  } as Response;
}

function json409(body: unknown) {
  return {
    ok: false,
    status: 409,
    json: async () => body,
  } as Response;
}

describe("useConcernAction", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn(() => Promise.resolve(jsonOk({})));
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("approve hits /:id/approve", async () => {
    const { result } = renderHook(() => useConcernAction(), { wrapper });
    await act(async () => {
      await result.current.mutateAsync({ id: "abc", action: "approve" });
    });
    const call = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(call[0]).toBe("/api/concerns/abc/approve");
    expect(call[1].method).toBe("POST");
  });

  it("dismiss hits /:id/dismiss", async () => {
    const { result } = renderHook(() => useConcernAction(), { wrapper });
    await act(async () => {
      await result.current.mutateAsync({ id: "abc", action: "dismiss" });
    });
    const call = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(call[0]).toBe("/api/concerns/abc/dismiss");
  });

  it("pause hits /:id/state with state=paused", async () => {
    const { result } = renderHook(() => useConcernAction(), { wrapper });
    await act(async () => {
      await result.current.mutateAsync({ id: "abc", action: "pause" });
    });
    const call = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(call[0]).toBe("/api/concerns/abc/state");
    expect(JSON.parse(call[1].body as string)).toEqual({ state: "paused" });
  });

  it("resume hits /:id/state with state=active", async () => {
    const { result } = renderHook(() => useConcernAction(), { wrapper });
    await act(async () => {
      await result.current.mutateAsync({ id: "abc", action: "resume" });
    });
    const call = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(JSON.parse(call[1].body as string)).toEqual({ state: "active" });
  });

  it("end hits /:id/state with state=ended", async () => {
    const { result } = renderHook(() => useConcernAction(), { wrapper });
    await act(async () => {
      await result.current.mutateAsync({ id: "abc", action: "end" });
    });
    const call = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(JSON.parse(call[1].body as string)).toEqual({ state: "ended" });
  });

  it("409 throws ConcernStateError", async () => {
    fetchMock.mockResolvedValueOnce(
      json409({ error: "lifecycle precondition failed" })
    );
    const { result } = renderHook(() => useConcernAction(), { wrapper });
    let caught: unknown = null;
    await act(async () => {
      try {
        await result.current.mutateAsync({ id: "abc", action: "approve" });
      } catch (e) {
        caught = e;
      }
    });
    expect(caught).toBeInstanceOf(ConcernStateError);
    await waitFor(() => expect(result.current.isError).toBe(true));
  });
});
