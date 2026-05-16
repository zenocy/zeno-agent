import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Card } from "../Card";
import { ToastProvider } from "../Toast";
import type { Card as CardData } from "../../types";

function wrapper({ children }: { children: React.ReactNode }) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return (
    <QueryClientProvider client={qc}>
      <ToastProvider>{children}</ToastProvider>
    </QueryClientProvider>
  );
}

const base: CardData = {
  id: "card-xyz",
  date: "2026-04-25",
  src: "mail",
  src_label: "Mail · Saru",
  rel: "high",
  title: "Series B redline",
  sub: "Saru replied with the updated redline.",
  meta: ["just now"],
  actions: [
    { label: "Dismiss", intent: "dismiss" },
    { label: "Snooze", intent: "snooze" },
    { label: "Archive", intent: "move_mail", target: { folder: "Archive" } },
  ],
};

// V2.8.0 added /api/actions/modes which Card pulls in via useIntentModes
// at first mount. The mock returns a catalog where dismiss/snooze are
// 1-click and move_mail is also 1-click (matches the canonical default
// in internal/action/intent_table.go).
function mockFetch() {
  return vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === "string" ? input : input.toString();
    if (url === "/api/actions/modes") {
      return {
        ok: true,
        status: 200,
        json: async () => ({
          intents: [
            { intent: "dismiss", mode: "one_click", description: "", wired: true },
            { intent: "snooze", mode: "one_click", description: "", wired: true },
            { intent: "move_mail", mode: "one_click", description: "", wired: true },
          ],
        }),
      } as Response;
    }
    void init;
    return { ok: true, status: 200, json: async () => ({ ok: true, hide: true }) } as Response;
  });
}

function postCalls(fetchMock: ReturnType<typeof mockFetch>): { url: string; body: any }[] {
  const out: { url: string; body: any }[] = [];
  for (const call of fetchMock.mock.calls) {
    const [url, init] = call as [string, RequestInit | undefined];
    if (init?.method === "POST") {
      out.push({ url, body: init.body ? JSON.parse(init.body as string) : undefined });
    }
  }
  return out;
}

describe("Card action wiring (Phase 4)", () => {
  let fetchMock: ReturnType<typeof mockFetch>;

  beforeEach(() => {
    fetchMock = mockFetch();
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("Dismiss click POSTs to /api/cards/:id/action and instantly hides the card", async () => {
    const user = userEvent.setup();
    render(<Card card={base} />, { wrapper });
    expect(screen.getByText("Series B redline")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Dismiss" }));

    // Card unmounts immediately via the `removing` state path. The
    // ToastProvider in the wrapper still renders a (currently empty)
    // toast container, so we check the card content is gone rather
    // than the wrapper subtree.
    expect(screen.queryByText("Series B redline")).toBeNull();

    await waitFor(() => expect(postCalls(fetchMock).length).toBe(1));
    const post = postCalls(fetchMock)[0];
    expect(post.url).toBe("/api/cards/card-xyz/action");
    expect(post.body).toMatchObject({ intent: "dismiss" });
  });

  it("Snooze click POSTs and instantly hides the card", async () => {
    const user = userEvent.setup();
    render(<Card card={base} />, { wrapper });
    await user.click(screen.getByRole("button", { name: "Snooze" }));

    expect(screen.queryByText("Series B redline")).toBeNull();

    await waitFor(() => expect(postCalls(fetchMock).length).toBe(1));
    expect(postCalls(fetchMock)[0].body).toMatchObject({ intent: "snooze" });
  });

  it("a non-dismiss/non-snooze action POSTs but leaves the card mounted", async () => {
    const user = userEvent.setup();
    render(<Card card={base} />, { wrapper });
    await user.click(screen.getByText("Archive"));

    // Card stays in DOM for non-dismiss/non-snooze actions.
    expect(screen.getByText("Series B redline")).toBeInTheDocument();

    await waitFor(() => expect(postCalls(fetchMock).length).toBe(1));
    expect(postCalls(fetchMock)[0].body).toMatchObject({ intent: "move_mail" });
  });
});
