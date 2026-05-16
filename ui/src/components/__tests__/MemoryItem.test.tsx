import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryItem } from "../MemoryItem";
import type { MemoryFact } from "../../types";

function wrapper({ children }: { children: React.ReactNode }) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
}

const base: MemoryFact = {
  id: "partner-aaaa",
  subject: "partner",
  fact: "Partner is Sam.",
  category: "relationship",
  confidence: "high",
  source: "user",
  evidence_count: 1,
  first_seen: "2026-04-20T08:00:00Z",
  last_reinforced: "2026-04-25T08:00:00Z",
  updated_at: "2026-04-25T08:00:00Z",
};

describe("MemoryItem", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ ...base, fact: "Partner is Sam Vega." }),
    });
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("renders fact text, subject, and a 'you' badge for user-source facts", () => {
    render(<MemoryItem fact={base} onDelete={() => {}} />, { wrapper });
    expect(screen.getByText("Partner is Sam.")).toBeInTheDocument();
    expect(screen.getByText("partner")).toBeInTheDocument();
    expect(screen.getByText("you")).toBeInTheDocument();
  });

  it("renders 'learned' badge for synth-source facts", () => {
    render(<MemoryItem fact={{ ...base, source: "synth" }} onDelete={() => {}} />, { wrapper });
    expect(screen.getByText("learned")).toBeInTheDocument();
  });

  it("Edit toggles a textarea, Save fires PATCH and exits editing", async () => {
    const user = userEvent.setup();
    render(<MemoryItem fact={base} onDelete={() => {}} />, { wrapper });

    await user.click(screen.getByLabelText("Edit fact"));
    const textarea = screen.getByRole("textbox");
    expect(textarea).toHaveValue("Partner is Sam.");

    await user.clear(textarea);
    await user.type(textarea, "Partner is Sam Vega.");
    await user.click(screen.getByLabelText("Save edit"));

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/memory/partner-aaaa");
    expect(init.method).toBe("PATCH");
    expect(JSON.parse(init.body as string)).toMatchObject({ fact: "Partner is Sam Vega." });

    // After success the textarea is gone.
    await waitFor(() => expect(screen.queryByRole("textbox")).toBeNull());
  });

  it("Delete invokes onDelete callback (panel owns the undo timer)", async () => {
    const user = userEvent.setup();
    const onDelete = vi.fn();
    render(<MemoryItem fact={base} onDelete={onDelete} />, { wrapper });

    await user.click(screen.getByLabelText("Delete fact"));
    expect(onDelete).toHaveBeenCalledTimes(1);
    expect(onDelete).toHaveBeenCalledWith(base);
    // No fetch fired by the item itself — panel calls useMemoryDelete.
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("disables Save when draft is unchanged", async () => {
    const user = userEvent.setup();
    render(<MemoryItem fact={base} onDelete={() => {}} />, { wrapper });
    await user.click(screen.getByLabelText("Edit fact"));
    const save = screen.getByLabelText("Save edit");
    expect(save).toBeDisabled();
  });
});
