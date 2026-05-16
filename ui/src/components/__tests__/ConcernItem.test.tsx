import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import { ConcernItem } from "../ConcernItem";
import type { Concern } from "../../types";

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
    last_active_at: new Date().toISOString(),
    observation_count: 3,
    created_at: "2026-04-15T08:00:00Z",
    updated_at: "2026-05-01T08:00:00Z",
    ...overrides,
  };
}

describe("ConcernItem", () => {
  beforeEach(() => {
    vi.stubGlobal("fetch", vi.fn(() =>
      Promise.resolve({ ok: true, status: 200, json: async () => ({}) } as Response)
    ));
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("proposed: shows Approve, Dismiss, Edit; not Pause/End/Merge/Split", () => {
    render(
      <ConcernItem
        concern={concern({ state: "proposed" })}
        onApprove={() => {}}
        onDismiss={() => {}}
      />,
      { wrapper }
    );
    expect(screen.getByLabelText("Approve")).toBeInTheDocument();
    expect(screen.getByLabelText("Dismiss")).toBeInTheDocument();
    expect(screen.getByLabelText("Edit")).toBeInTheDocument();
    expect(screen.queryByLabelText("Pause")).toBeNull();
    expect(screen.queryByLabelText("End")).toBeNull();
  });

  it("active: shows Pause, End, Merge, Split, Edit; not Approve/Resume", () => {
    render(
      <ConcernItem
        concern={concern({ state: "active" })}
        onPause={() => {}}
        onEnd={() => {}}
        onMerge={() => {}}
        onSplit={() => {}}
      />,
      { wrapper }
    );
    expect(screen.getByLabelText("Pause")).toBeInTheDocument();
    expect(screen.getByLabelText("End")).toBeInTheDocument();
    expect(screen.getByLabelText("Merge")).toBeInTheDocument();
    expect(screen.getByLabelText("Split")).toBeInTheDocument();
    expect(screen.getByLabelText("Edit")).toBeInTheDocument();
    expect(screen.queryByLabelText("Approve")).toBeNull();
    expect(screen.queryByLabelText("Resume")).toBeNull();
  });

  it("paused: shows Resume, End, Merge, Split, Edit", () => {
    render(
      <ConcernItem
        concern={concern({ state: "paused" })}
        onResume={() => {}}
        onEnd={() => {}}
        onMerge={() => {}}
        onSplit={() => {}}
      />,
      { wrapper }
    );
    expect(screen.getByLabelText("Resume")).toBeInTheDocument();
    expect(screen.getByLabelText("End")).toBeInTheDocument();
    expect(screen.getByLabelText("Merge")).toBeInTheDocument();
    expect(screen.getByLabelText("Split")).toBeInTheDocument();
    expect(screen.queryByLabelText("Pause")).toBeNull();
  });

  it("renders ready-to-retire note when concern.ready_to_retire is true", () => {
    render(
      <ConcernItem
        concern={concern({
          state: "active",
          ready_to_retire: true,
        })}
      />,
      { wrapper }
    );
    expect(screen.getByText(/ready to retire\?/)).toBeInTheDocument();
  });

  it("does not render the retirement note when ready_to_retire is undefined", () => {
    render(
      <ConcernItem concern={concern({ state: "active" })} />,
      { wrapper }
    );
    expect(screen.queryByText(/ready to retire\?/)).toBeNull();
  });

  it("ended/merged: archived, no action buttons except none rendered", () => {
    render(
      <ConcernItem concern={concern({ state: "ended" })} />,
      { wrapper }
    );
    // Archived row hides Edit (read-only). Verify no lifecycle buttons.
    expect(screen.queryByLabelText("Pause")).toBeNull();
    expect(screen.queryByLabelText("End")).toBeNull();
    expect(screen.queryByLabelText("Merge")).toBeNull();
    expect(screen.queryByLabelText("Split")).toBeNull();
    expect(screen.queryByLabelText("Edit")).toBeNull();
  });

  it("Edit toggle reveals inline form; Cancel restores", async () => {
    const user = userEvent.setup();
    render(<ConcernItem concern={concern({ state: "active" })} />, { wrapper });

    await user.click(screen.getByLabelText("Edit"));
    expect(screen.getByLabelText("Cancel")).toBeInTheDocument();
    expect(screen.getByLabelText("Save edit")).toBeInTheDocument();

    await user.click(screen.getByLabelText("Cancel"));
    expect(screen.queryByLabelText("Save edit")).toBeNull();
    expect(screen.getByLabelText("Edit")).toBeInTheDocument();
  });

  it("Save edit issues PATCH with changed fields only", async () => {
    const fetchMock = vi.fn((_url: string, init?: RequestInit) => {
      if (init?.method === "PATCH") {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () =>
            concern({
              id: "id",
              name: "Renamed",
              description: "Description.",
            }),
        } as Response);
      }
      return Promise.resolve({
        ok: true,
        status: 200,
        json: async () => ({}),
      } as Response);
    });
    vi.stubGlobal("fetch", fetchMock);

    const user = userEvent.setup();
    render(<ConcernItem concern={concern({ state: "active" })} />, { wrapper });

    await user.click(screen.getByLabelText("Edit"));
    const nameInput = screen
      .getAllByRole("textbox")
      .find((el) => (el as HTMLInputElement).value === "Concern") as HTMLInputElement;
    await user.clear(nameInput);
    await user.type(nameInput, "Renamed");

    await user.click(screen.getByLabelText("Save edit"));

    const patchCall = fetchMock.mock.calls.find((c) => {
      const [, i] = c as [string, RequestInit | undefined];
      return i?.method === "PATCH";
    });
    expect(patchCall).toBeDefined();
    if (patchCall) {
      const init = patchCall[1] as RequestInit;
      const body = JSON.parse(init.body as string);
      expect(body.name).toBe("Renamed");
      // Description was untouched, should not be in PATCH body.
      expect(body.description).toBeUndefined();
    }
  });
});
