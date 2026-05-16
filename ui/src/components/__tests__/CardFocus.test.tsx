import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { CardFocus } from "../CardFocus";
import { ToastProvider } from "../Toast";
import type {
  Card as CardData,
  ConversationThread,
  ConversationTurn,
} from "../../types";

const baseCard: CardData = {
  id: "saru-1",
  date: "2026-05-09",
  src: "mail",
  src_label: "Email · Acuity",
  rel: "high",
  title: "Saru Patel · re: redline",
  sub: "Walked the redline with Lin. Two questions remain.",
  meta: ["06:14"],
  actions: [
    {
      label: "Pull option-pool from Aria's deck",
      intent: "ask_followup",
      target: { seed: "Pull option-pool from Aria's deck" },
    },
    {
      label: "What did we say about 1× preferred?",
      intent: "ask_followup",
      target: { seed: "What did we say about 1× preferred last call?" },
    },
    { label: "Reply" },
  ],
};

function wrapper({ children }: { children: React.ReactNode }) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return (
    <QueryClientProvider client={qc}>
      <ToastProvider>{children}</ToastProvider>
    </QueryClientProvider>
  );
}

const fetchMock = vi.fn();

beforeEach(() => {
  fetchMock.mockReset();
  vi.stubGlobal("fetch", fetchMock);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

function jsonResponse(body: unknown) {
  return {
    ok: true,
    status: 200,
    json: () => Promise.resolve(body),
  } as unknown as Response;
}

const emptyThread: ConversationThread = {
  thread_id: "t-1",
  card_id: "saru-1",
  turns: [],
};

describe("CardFocus", () => {
  it("renders the pinned card title and src label", async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(emptyThread));
    fetchMock.mockResolvedValueOnce(jsonResponse({ intents: [] })); // intent modes
    render(<CardFocus card={baseCard} onClose={() => {}} />, { wrapper });
    expect(
      await screen.findByText("Saru Patel · re: redline"),
    ).toBeInTheDocument();
    expect(screen.getByText("Email · Acuity")).toBeInTheDocument();
  });

  it("derives suggested chips from ask_followup actions", async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(emptyThread));
    fetchMock.mockResolvedValueOnce(jsonResponse({ intents: [] }));
    render(<CardFocus card={baseCard} onClose={() => {}} />, { wrapper });
    expect(
      await screen.findByRole("button", {
        name: "Pull option-pool from Aria's deck",
      }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", {
        name: "What did we say about 1× preferred last call?",
      }),
    ).toBeInTheDocument();
  });

  it("falls back to generic chips when no ask_followup actions exist", async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(emptyThread));
    fetchMock.mockResolvedValueOnce(jsonResponse({ intents: [] }));
    const cardNoFollowup: CardData = {
      ...baseCard,
      actions: [{ label: "Dismiss", intent: "dismiss" }],
    };
    render(<CardFocus card={cardNoFollowup} onClose={() => {}} />, { wrapper });
    expect(
      await screen.findByRole("button", { name: "Tell me more" }),
    ).toBeInTheDocument();
  });

  it("calls onClose on Escape", async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(emptyThread));
    fetchMock.mockResolvedValueOnce(jsonResponse({ intents: [] }));
    const onClose = vi.fn();
    const user = userEvent.setup();
    render(<CardFocus card={baseCard} onClose={onClose} />, { wrapper });
    await screen.findByText("Saru Patel · re: redline");
    await user.keyboard("{Escape}");
    expect(onClose).toHaveBeenCalled();
  });

  it("submits a turn and renders the returned SubCard", async () => {
    const newTurn: ConversationTurn = {
      id: "turn-1",
      position: 0,
      prompt: "What did we say about 1× preferred last call?",
      reply: {
        id: "sub-1",
        kind: "answer",
        eyebrow: "answer",
        title: "What we said last call",
        body: "Aria flagged it as the headline trade-off.",
      },
      trace_id: "trace-1",
      created_at: new Date().toISOString(),
    };
    // Ordered fetches: thread on mount → sends on mount (V2.13.2)
    // → converse POST after click → intent modes when the SubCard
    // renders. Mocked in the order they fire.
    fetchMock.mockResolvedValueOnce(jsonResponse(emptyThread));
    fetchMock.mockResolvedValueOnce(jsonResponse({ sends: [] }));
    fetchMock.mockResolvedValueOnce(jsonResponse(newTurn));
    fetchMock.mockResolvedValueOnce(jsonResponse({ intents: [] }));

    const user = userEvent.setup();
    render(<CardFocus card={baseCard} onClose={() => {}} />, { wrapper });

    const chip = await screen.findByRole("button", {
      name: "What did we say about 1× preferred last call?",
    });
    await user.click(chip);

    expect(
      await screen.findByText("What we said last call"),
    ).toBeInTheDocument();
    expect(
      screen.getByText("Aria flagged it as the headline trade-off."),
    ).toBeInTheDocument();
  });

  it("renders prior turns from the loaded thread", async () => {
    const populated: ConversationThread = {
      thread_id: "t-1",
      card_id: "saru-1",
      turns: [
        {
          id: "turn-0",
          position: 0,
          prompt: "What did Aria say about 1× preferred?",
          reply: {
            id: "sub-0",
            kind: "answer",
            eyebrow: "answer",
            title: "Aria's stance",
            body: "She flagged it as the headline.",
          },
          trace_id: "trace-0",
          created_at: new Date().toISOString(),
        },
      ],
    };
    fetchMock.mockResolvedValueOnce(jsonResponse(populated));
    fetchMock.mockResolvedValueOnce(jsonResponse({ intents: [] }));
    render(<CardFocus card={baseCard} onClose={() => {}} />, { wrapper });
    expect(await screen.findByText("Aria's stance")).toBeInTheDocument();
    expect(
      screen.getByText("What did Aria say about 1× preferred?"),
    ).toBeInTheDocument();
  });
});

describe("CardFocus anchor mode", () => {
  it("renders the calendar anchor when card.kind is calendar_day", async () => {
    // Calendar fetch + thread fetch + intent modes
    fetchMock.mockImplementation((url: string) => {
      if (url.includes("/api/projections/calendar")) return Promise.resolve(jsonResponse([]));
      if (url.includes("/thread")) return Promise.resolve(jsonResponse({ thread_id: "t-cal", card_id: "calendar_day", turns: [] }));
      return Promise.resolve(jsonResponse({ intents: [] }));
    });

    const anchor: CardData = {
      id: "calendar_day",
      date: "",
      src: "calendar",
      src_label: "Calendar · today",
      rel: "med",
      kind: "calendar_day",
      title: "Today's calendar",
      sub: "",
      meta: [],
      actions: [],
    };
    render(<CardFocus card={anchor} onClose={() => {}} />, { wrapper });

    // Eyebrow flips to "in calendar"
    expect(await screen.findByText("in calendar")).toBeInTheDocument();
    // Calendar anchor renders the "tomorrow" eyebrow from CalendarPage
    expect(await screen.findByText("tomorrow")).toBeInTheDocument();
    // Suggested chip from FOCUS_PROMPTS_BY_KIND.calendar_day
    expect(
      await screen.findByRole("button", { name: "Find a 30-min slot for Sam call" }),
    ).toBeInTheDocument();
  });

  it("renders the tasks anchor when card.kind is tasks_view", async () => {
    fetchMock.mockImplementation((url: string) => {
      if (url.includes("/api/tasks")) return Promise.resolve(jsonResponse([]));
      if (url.includes("/thread")) return Promise.resolve(jsonResponse({ thread_id: "t-task", card_id: "tasks_view", turns: [] }));
      return Promise.resolve(jsonResponse({ intents: [] }));
    });

    const anchor: CardData = {
      id: "tasks_view",
      date: "",
      src: "tasks",
      src_label: "Tasks · all",
      rel: "med",
      kind: "tasks_view",
      title: "Tasks",
      sub: "",
      meta: [],
      actions: [],
    };
    render(<CardFocus card={anchor} onClose={() => {}} />, { wrapper });

    expect(await screen.findByText("in tasks")).toBeInTheDocument();
    // Tasks anchor's capture row placeholder
    expect(
      await screen.findByPlaceholderText(/Capture a task/),
    ).toBeInTheDocument();
    // Suggested chip from FOCUS_PROMPTS_BY_KIND.tasks_view
    expect(
      await screen.findByRole("button", { name: "What's actually due today?" }),
    ).toBeInTheDocument();
  });
});

describe("Card with onOpen", () => {
  it("clicking the body fires onOpen, but clicking action buttons does not", async () => {
    const { Card } = await import("../Card");
    const onOpen = vi.fn();
    const user = userEvent.setup();
    render(<Card card={baseCard} onOpen={onOpen} />, { wrapper });

    // Click on the title text — should fire onOpen
    await user.click(screen.getByText("Saru Patel · re: redline"));
    expect(onOpen).toHaveBeenCalledTimes(1);

    // Click on the Reply button — should NOT fire onOpen
    onOpen.mockClear();
    await user.click(screen.getByRole("button", { name: "Reply" }));
    expect(onOpen).not.toHaveBeenCalled();
  });
});
