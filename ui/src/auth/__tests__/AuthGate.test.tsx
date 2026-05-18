import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import AuthGate from "../AuthGate";

function jsonResponse(body: unknown, status = 200) {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => body,
  } as unknown as Response;
}

describe("AuthGate", () => {
  beforeEach(() => {
    vi.useRealTimers();
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("renders children when /api/auth/me returns 200", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(jsonResponse({ username: "alice" }, 200))
    );

    render(
      <AuthGate>
        <div>app shell</div>
      </AuthGate>
    );

    await waitFor(() => {
      expect(screen.getByText("app shell")).toBeInTheDocument();
    });
  });

  it("renders LoginPage when /api/auth/me returns 401", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(jsonResponse({ error: "unauthorized" }, 401))
    );

    render(
      <AuthGate>
        <div>app shell</div>
      </AuthGate>
    );

    await waitFor(() => {
      expect(screen.getByRole("heading", { name: /sign in/i })).toBeInTheDocument();
    });
    expect(screen.queryByText("app shell")).toBeNull();
  });

  it("shows an error with retry on network failure, then succeeds after retry", async () => {
    const fetchMock = vi
      .fn()
      // First call rejects → AuthGate enters error state.
      .mockRejectedValueOnce(new Error("network down"))
      // Retry click → second call returns 200 and renders children.
      .mockResolvedValueOnce(jsonResponse({ username: "alice" }, 200));
    vi.stubGlobal("fetch", fetchMock);

    render(
      <AuthGate>
        <div>app shell</div>
      </AuthGate>
    );

    await waitFor(() => {
      expect(screen.getByText(/connection problem/i)).toBeInTheDocument();
    });

    await userEvent.click(screen.getByRole("button", { name: /retry/i }));

    await waitFor(() => {
      expect(screen.getByText("app shell")).toBeInTheDocument();
    });
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });
});
