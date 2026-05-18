import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import dayjs from "dayjs";
import { ArchivePanel } from "../ArchivePanel";
import type { CardsResponse, Card as CardData } from "../../types";

function wrapperFor() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
  };
}

// A populated card. The renderer pulls everything from `card` so the
// fixture just needs the required fields.
function makeCard(overrides: Partial<CardData> = {}): CardData {
  return {
    id: "iran-war-ab12",
    date: "2026-05-18",
    src: "ask",
    src_label: "Generated",
    rel: "med",
    title: "Iran war: latest",
    sub: "Brief summary of the situation.",
    meta: [],
    actions: [],
    origin: "ask",
    ...overrides,
  };
}

describe("ArchivePanel", () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  const today = dayjs().format("YYYY-MM-DD");

  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("renders empty state for a day with no cards", async () => {
    fetchMock.mockResolvedValue({
      ok: true,
      status: 200,
      json: async (): Promise<CardsResponse> => ({ date: today, cards: [] }),
    });

    render(<ArchivePanel />, { wrapper: wrapperFor() });

    await waitFor(() => {
      expect(screen.getByText(new RegExp(`No cards for ${today}`))).toBeInTheDocument();
    });
  });

  it("renders cards returned by the archive endpoint", async () => {
    fetchMock.mockResolvedValue({
      ok: true,
      status: 200,
      json: async (): Promise<CardsResponse> => ({
        date: today,
        cards: [makeCard({ title: "First card" }), makeCard({ id: "c2", title: "Second card" })],
      }),
    });

    render(<ArchivePanel />, { wrapper: wrapperFor() });

    await waitFor(() => {
      expect(screen.getByText("First card")).toBeInTheDocument();
    });
    expect(screen.getByText("Second card")).toBeInTheDocument();
  });

  it("refetches with a new date when the user clicks the previous-day arrow", async () => {
    fetchMock.mockResolvedValue({
      ok: true,
      status: 200,
      json: async (): Promise<CardsResponse> => ({ date: today, cards: [] }),
    });

    render(<ArchivePanel />, { wrapper: wrapperFor() });

    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith(`/api/cards/archive?date=${today}`);
    });

    fireEvent.click(screen.getByLabelText("Previous day"));

    const yesterday = dayjs(today).subtract(1, "day").format("YYYY-MM-DD");
    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith(
        `/api/cards/archive?date=${yesterday}`,
      );
    });
  });

  it("disables the next-day arrow when viewing today", async () => {
    fetchMock.mockResolvedValue({
      ok: true,
      status: 200,
      json: async (): Promise<CardsResponse> => ({ date: today, cards: [] }),
    });

    render(<ArchivePanel />, { wrapper: wrapperFor() });

    await waitFor(() => {
      expect(screen.getByLabelText("Next day")).toBeDisabled();
    });
  });
});
