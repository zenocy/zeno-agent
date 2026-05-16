import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ContactsPanel } from "../ContactsPanel";
import type { ContactsResponse } from "../../api/useContacts";

function wrapper({ children }: { children: React.ReactNode }) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
}

const sampleResponse: ContactsResponse = {
  carddav_enabled: true,
  last_sync_at: "2026-05-10T11:55:00Z",
  contacts: [
    {
      uid: "vc-sam",
      display_name: "Sam Carter",
      given_name: "Sam",
      family_name: "Carter",
      nicknames: ["Sammy"],
      phones: [
        { value: "+44 7700 900111", types: ["CELL"], pref: 1 },
        { value: "+44 20 1234 5678", types: ["WORK"] },
      ],
      emails: [{ value: "partner@example.com", types: ["HOME"] }],
      aliases: [
        { id: "fact-wife", subject: "wife", fact: "Partner.", preferred_phone: "+44 7700 900111" },
      ],
    },
    {
      uid: "vc-bob",
      display_name: "Bob Smith",
      phones: [{ value: "+1 555 1234567" }],
    },
  ],
  groups: [
    { id: "fact-fam", subject: "family group", fact: "Living-room family chat.", jid: "120001@g.us" },
  ],
};

describe("ContactsPanel", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  function stubGet(resp: ContactsResponse) {
    fetchMock = vi.fn((url: string, init?: RequestInit) => {
      if (typeof url === "string" && url === "/api/contacts" && (!init || init.method === undefined)) {
        return Promise.resolve({ ok: true, status: 200, json: async () => resp } as Response);
      }
      return Promise.resolve({ ok: true, status: 204, json: async () => ({}) } as Response);
    });
    vi.stubGlobal("fetch", fetchMock);
  }

  it("renders every CardDAV contact with full details", async () => {
    stubGet(sampleResponse);
    render(<ContactsPanel />, { wrapper });

    await waitFor(() => expect(screen.getByText("Sam Carter")).toBeInTheDocument());
    expect(screen.getByText("+44 7700 900111")).toBeInTheDocument();
    expect(screen.getByText("+44 20 1234 5678")).toBeInTheDocument();
    expect(screen.getByText("partner@example.com")).toBeInTheDocument();
    expect(screen.getByText(/Sammy/)).toBeInTheDocument();
    // Bob has no aliases or extra detail but still surfaces.
    expect(screen.getByText("Bob Smith")).toBeInTheDocument();
    expect(screen.getByText("+1 555 1234567")).toBeInTheDocument();
  });

  it("nests aliases under their CardDAV row", async () => {
    stubGet(sampleResponse);
    render(<ContactsPanel />, { wrapper });

    await waitFor(() => expect(screen.getByText("wife")).toBeInTheDocument());
    // The alias' surrounding li sits inside Sam's card. Find Sam's card by
    // finding "Sam Carter" and walking up to the nearest <li>.
    const samHeading = screen.getByText("Sam Carter");
    const samCard = samHeading.closest("li");
    expect(samCard).not.toBeNull();
    expect(samCard!.textContent).toContain("wife");
  });

  it("Add alias on a contact fires POST with carddav_uid", async () => {
    stubGet(sampleResponse);
    fetchMock.mockImplementation((url: string, init?: RequestInit) => {
      if (typeof url === "string" && url === "/api/contacts" && init?.method === "POST") {
        const body = JSON.parse(String(init.body));
        // Assertion happens in-test via the captured body.
        return Promise.resolve({ ok: true, status: 201, json: async () => body } as Response);
      }
      if (typeof url === "string" && url === "/api/contacts") {
        return Promise.resolve({ ok: true, status: 200, json: async () => sampleResponse } as Response);
      }
      return Promise.resolve({ ok: true, status: 204, json: async () => ({}) } as Response);
    });

    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    render(<ContactsPanel />, { wrapper });

    // Bob has no aliases — click his "Add alias" specifically.
    await waitFor(() => expect(screen.getByText("Bob Smith")).toBeInTheDocument());
    const bobCard = screen.getByText("Bob Smith").closest("li")!;
    const addBtn = bobCard.querySelector("button");
    expect(addBtn).not.toBeNull();
    expect(addBtn!.textContent).toMatch(/Add alias/);
    await user.click(addBtn!);

    const subjInput = bobCard.querySelector('input[placeholder="Alias (e.g. wife)"]') as HTMLInputElement;
    expect(subjInput).not.toBeNull();
    await user.type(subjInput, "buddy");
    const saveBtn = Array.from(bobCard.querySelectorAll("button")).find((b) =>
      b.textContent?.includes("Save alias")
    )!;
    await user.click(saveBtn);

    await waitFor(() => {
      const calls = fetchMock.mock.calls.filter((c) => c[1]?.method === "POST");
      expect(calls.length).toBeGreaterThanOrEqual(1);
      const body = JSON.parse(String(calls[0][1].body));
      expect(body.carddav_uid).toBe("vc-bob");
      expect(body.subject).toBe("buddy");
    });
  });

  it("groups appear in their own section, not in the contacts list", async () => {
    stubGet(sampleResponse);
    render(<ContactsPanel />, { wrapper });

    await waitFor(() => expect(screen.getByText("family group")).toBeInTheDocument());
    // The Groups header should be present.
    expect(screen.getByText("Groups")).toBeInTheDocument();
    // The group's JID is shown in the group section.
    expect(screen.getByText("120001@g.us")).toBeInTheDocument();
  });

  it("renders empty-state when CardDAV is disabled", async () => {
    stubGet({ carddav_enabled: false, contacts: [], groups: [] });
    render(<ContactsPanel />, { wrapper });
    await waitFor(() => expect(screen.getByText(/CardDAV import is off/)).toBeInTheDocument());
  });

  it("filter input narrows the visible contacts", async () => {
    stubGet(sampleResponse);
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    render(<ContactsPanel />, { wrapper });

    await waitFor(() => expect(screen.getByText("Sam Carter")).toBeInTheDocument());
    const filter = screen.getByPlaceholderText("Filter contacts…");
    await user.type(filter, "bob");
    expect(screen.queryByText("Sam Carter")).not.toBeInTheDocument();
    expect(screen.getByText("Bob Smith")).toBeInTheDocument();
  });
});
