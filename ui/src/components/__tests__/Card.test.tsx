import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Card } from "../Card";
import type { Card as CardData } from "../../types";

function wrapper({ children }: { children: React.ReactNode }) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
}

const base: CardData = {
  id: "card-1",
  date: "2026-04-25",
  src: "mail",
  src_label: "Mail · Saru",
  rel: "high",
  title: "Series B redline — terms attached",
  sub: "Saru replied this morning with the updated redline.",
  meta: ["high", "just now"],
  actions: [{ label: "Reply", primary: true }, { label: "Snooze" }],
};

describe("Card", () => {
  it("renders title and sub text", () => {
    render(<Card card={base} />, { wrapper });
    expect(screen.getByText("Series B redline — terms attached")).toBeInTheDocument();
    expect(screen.getByText("Saru replied this morning with the updated redline.")).toBeInTheDocument();
  });

  it("renders *foo* in title as <em>", () => {
    render(
      <Card card={{ ...base, title: "Series B *redline* — terms attached" }} />,
      { wrapper }
    );
    const em = screen.getByText("redline");
    expect(em.tagName).toBe("EM");
  });

  it("renders *foo* in sub as <em>", () => {
    render(
      <Card card={{ ...base, sub: "Saru replied with the *updated* redline." }} />,
      { wrapper }
    );
    const em = screen.getByText("updated");
    expect(em.tagName).toBe("EM");
  });

  it("adds amber border class for kind='personal'", () => {
    const { container } = render(
      <Card card={{ ...base, kind: "personal" }} />,
      { wrapper }
    );
    // The root div should have the amber border variant class
    expect(container.firstChild).toHaveClass("border-amber/30");
  });

  it("does not show 'Why?' button when expand is empty", () => {
    render(<Card card={{ ...base, expand: {} }} />, { wrapper });
    expect(screen.queryByText("Why?")).toBeNull();
  });

  it("shows 'Why?' button when expand has content, and expands on click", async () => {
    const user = userEvent.setup();
    render(
      <Card card={{ ...base, expand: { Context: "This is the context." } }} />,
      { wrapper }
    );
    const whyBtn = screen.getByText("Why?");
    expect(whyBtn).toBeInTheDocument();
    await user.click(whyBtn);
    expect(screen.getByText("This is the context.")).toBeInTheDocument();
  });

  it("does not show trace toggle when trace_id is absent", () => {
    render(<Card card={{ ...base, trace_id: undefined }} />, { wrapper });
    expect(screen.queryByText(/traced/i)).toBeNull();
  });

  it("shows trace toggle when trace_id is present", () => {
    render(<Card card={{ ...base, trace_id: "trace-abc" }} />, { wrapper });
    // Trace component owns its own collapsed-by-default disclosure; the
    // word "traced" is the button label.
    expect(screen.getByText(/traced/i)).toBeInTheDocument();
  });

  it("renders digest items as a list for kind='digest'", () => {
    render(
      <Card
        card={{
          ...base,
          kind: "digest",
          title: "Five low-signal threads",
          items: [
            { title: "Stratechery weekly", src: "mail" },
            { title: "GitHub digest", sub: "3 repos updated" },
          ],
        }}
      />,
      { wrapper }
    );
    expect(screen.getByText("Stratechery weekly")).toBeInTheDocument();
    expect(screen.getByText("GitHub digest")).toBeInTheDocument();
    expect(screen.getByText("— 3 repos updated")).toBeInTheDocument();
  });

  it("shows a live freshness dot when a live value is fresh", () => {
    render(
      <Card card={{ ...base, live: [{ slot: "meta", kind: "stock", text: "$210.50 +1.25%" }] }} />,
      { wrapper }
    );
    expect(screen.getByLabelText("Live value")).toBeInTheDocument();
    expect(screen.queryByText("stale")).toBeNull();
  });

  it("shows a 'stale' marker when the latest live reading is stale", () => {
    render(
      <Card
        card={{ ...base, live: [{ slot: "meta", kind: "stock", text: "$1.00 -3.4%", stale: true }] }}
      />,
      { wrapper }
    );
    expect(screen.getByText("stale")).toBeInTheDocument();
    expect(screen.queryByLabelText("Live value")).toBeNull();
  });
});
