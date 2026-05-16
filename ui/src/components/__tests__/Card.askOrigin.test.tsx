// Pin the V2.4 P3 invariants on Card rendering for ask-origin cards:
//   - Ask cards still render their title/sub normally.
//   - Inject-origin cards still receive the visual signal (animate-inject-in
//     animation; the textual "inject" chip was removed in the design refit).

import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { Card } from "../Card";
import type { Card as CardData } from "../../types";

function wrapper({ children }: { children: ReactNode }) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
}

const askCard: CardData = {
  id: "ask-1",
  date: "2026-04-29",
  src: "ask",
  src_label: "Generated",
  rel: "med",
  title: "A *quiet* day.",
  sub: "Calendar is empty and threads are settled. Spend it on deep work.",
  meta: [],
  actions: [{ label: "Dismiss" }],
  origin: "ask",
};

describe("Card with origin=ask", () => {
  it("does NOT carry the inject animation class", () => {
    const { container } = render(<Card card={askCard} />, { wrapper });
    expect(container.firstChild).not.toHaveClass("animate-inject-in");
  });

  it("still renders title and sub normally", () => {
    const { container } = render(<Card card={askCard} />, { wrapper });
    expect(container.textContent).toContain("quiet");
    expect(container.textContent).toContain("day");
    expect(
      screen.getByText("Calendar is empty and threads are settled. Spend it on deep work."),
    ).toBeInTheDocument();
  });

  it("inject-origin cards animate in with the inject keyframe (regression)", () => {
    const injectCard: CardData = { ...askCard, id: "i-1", origin: "inject" };
    const { container } = render(<Card card={injectCard} />, { wrapper });
    expect(container.firstChild).toHaveClass("animate-inject-in");
  });
});
