import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryPanel } from "../MemoryPanel";
import type { MemoryFact } from "../../types";

function wrapper({ children }: { children: React.ReactNode }) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
}

function fact(overrides: Partial<MemoryFact>): MemoryFact {
  return {
    id: "id",
    subject: "subj",
    fact: "fact",
    category: "misc",
    confidence: "low",
    source: "synth",
    evidence_count: 1,
    first_seen: "2026-04-20T00:00:00Z",
    last_reinforced: "2026-04-25T00:00:00Z",
    updated_at: "2026-04-25T00:00:00Z",
    ...overrides,
  };
}

describe("MemoryPanel", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  function mockListResponse(facts: MemoryFact[]) {
    fetchMock = vi.fn((url: string, init?: RequestInit) => {
      if (typeof url === "string" && url === "/api/memory" && (!init || init.method === undefined)) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () => ({ facts }),
        } as Response);
      }
      // Default OK for mutations; per-test overrides may shadow this.
      return Promise.resolve({ ok: true, status: 204, json: async () => ({}) } as Response);
    });
    vi.stubGlobal("fetch", fetchMock);
  }

  it("empty state renders explainer", async () => {
    mockListResponse([]);
    render(<MemoryPanel />, { wrapper });
    await waitFor(() => expect(screen.getByText("No facts yet.")).toBeInTheDocument());
  });

  it("groups facts by category in stable order", async () => {
    mockListResponse([
      fact({ id: "p", subject: "partner", fact: "Partner is Sam.", category: "relationship", confidence: "high", source: "user" }),
      fact({ id: "a", subject: "anniversary", fact: "Anniversary May 7.", category: "identity", confidence: "high", source: "user" }),
      fact({ id: "r", subject: "runs", fact: "Tue/Thu mornings.", category: "routine", confidence: "med" }),
      fact({ id: "d", subject: "dinner", fact: "Otto's is the spot.", category: "preference", confidence: "low" }),
    ]);
    render(<MemoryPanel />, { wrapper });
    await waitFor(() => expect(screen.getByText("Partner is Sam.")).toBeInTheDocument());

    // Section headers appear in CATEGORY_ORDER: identity → relationship →
    // preference → routine.
    const headers = ["Identity", "Relationship", "Preference", "Routine"].map((label) =>
      screen.getByText(label)
    );
    for (let i = 1; i < headers.length; i++) {
      // Each later header lives below its predecessor in DOM order.
      expect(
        headers[i - 1].compareDocumentPosition(headers[i]) & Node.DOCUMENT_POSITION_FOLLOWING
      ).toBeTruthy();
    }
  });

  it("Add form: subject normalization, submit disabled state, success collapses form", async () => {
    mockListResponse([]);
    fetchMock.mockImplementation((url: string, init?: RequestInit) => {
      if (url === "/api/memory" && init?.method === "POST") {
        return Promise.resolve({
          ok: true,
          status: 201,
          json: async () => fact({ id: "p", subject: "partner", fact: "Partner is Sam.", source: "user", confidence: "high" }),
        } as Response);
      }
      return Promise.resolve({
        ok: true,
        status: 200,
        json: async () => ({ facts: [] }),
      } as Response);
    });
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    render(<MemoryPanel />, { wrapper });

    await user.click(await screen.findByText("Add a fact"));

    const subjectInput = screen.getByLabelText(/Subject/i, { selector: "input" });
    const factTextarea = screen.getByLabelText(/Fact/i, { selector: "textarea" });
    const submitBtn = screen.getByRole("button", { name: /Add fact/i });

    expect(submitBtn).toBeDisabled();

    await user.type(subjectInput, "My Partner");
    fireEvent.blur(subjectInput);
    expect((subjectInput as HTMLInputElement).value).toBe("my-partner");

    await user.type(factTextarea, "Partner is Sam.");
    expect(submitBtn).not.toBeDisabled();

    await user.click(submitBtn);
    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    const postCall = fetchMock.mock.calls.find((call) => {
      const [u, i] = call as [string, RequestInit | undefined];
      return u === "/api/memory" && i?.method === "POST";
    });
    expect(postCall).toBeDefined();
    if (postCall) {
      const init = postCall[1] as RequestInit;
      expect(JSON.parse(init.body as string)).toMatchObject({
        subject: "my-partner",
        fact: "Partner is Sam.",
        category: "relationship",
      });
    }
    // Form collapses after success.
    await waitFor(() => expect(screen.queryByLabelText(/^Fact$/)).toBeNull());
  });

  it("Delete shows undo toast; clicking undo cancels the DELETE", async () => {
    const facts = [
      fact({ id: "p", subject: "partner", fact: "Partner is Sam.", category: "relationship", confidence: "high", source: "user" }),
    ];
    mockListResponse(facts);
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    render(<MemoryPanel />, { wrapper });

    await waitFor(() => expect(screen.getByText("Partner is Sam.")).toBeInTheDocument());

    await user.click(screen.getByLabelText("Delete fact"));

    // Optimistic removal: row gone, toast visible.
    expect(screen.queryByText("Partner is Sam.")).toBeNull();
    expect(screen.getByText(/^Deleted/)).toBeInTheDocument();
    expect(screen.getByText(/Undo/)).toBeInTheDocument();

    await user.click(screen.getByText(/Undo/));
    // Toast dismissed; no DELETE fired.
    expect(screen.queryByText(/Undo \(/)).toBeNull();
    const deleteCall = fetchMock.mock.calls.find((call) => {
      const init = (call as [string, RequestInit | undefined])[1];
      return init?.method === "DELETE";
    });
    expect(deleteCall).toBeUndefined();
  });
});
