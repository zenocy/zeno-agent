// Block-level markdown renderer tests. The `document` SubCard kind
// depends on this for day-of-week headings, bullet lists, blockquotes,
// and paragraph separation — the homework-email scenario was the
// motivating fixture, so the day headings + bullet structure are pinned
// as load-bearing.

import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";

import { renderMarkdownBlocks } from "../markdownBlocks";

describe("renderMarkdownBlocks", () => {
  it("renders ## headings as h4 with the heading text", () => {
    render(<>{renderMarkdownBlocks("## Monday\nGreek: page 57")}</>);
    const heading = screen.getByRole("heading", { name: /Monday/i });
    expect(heading.tagName.toLowerCase()).toBe("h4");
  });

  it("groups consecutive '- ' lines into a single <ul>", () => {
    render(
      <>
        {renderMarkdownBlocks(
          "- Greek: page 57\n- Spelling words in homework notebook\n- Reading 10 min",
        )}
      </>,
    );
    const items = screen.getAllByRole("listitem");
    expect(items).toHaveLength(3);
    // All three siblings must share the same parent <ul> — proves the
    // tokenizer didn't restart the list between lines.
    const ul = items[0].parentElement;
    expect(ul?.tagName.toLowerCase()).toBe("ul");
    expect(items[1].parentElement).toBe(ul);
    expect(items[2].parentElement).toBe(ul);
  });

  it("renders '1.' / '2.' lines as an <ol>", () => {
    render(<>{renderMarkdownBlocks("1. First\n2. Second")}</>);
    const items = screen.getAllByRole("listitem");
    expect(items).toHaveLength(2);
    expect(items[0].parentElement?.tagName.toLowerCase()).toBe("ol");
  });

  it("renders '> ' as <blockquote> joining consecutive quoted lines", () => {
    const { container } = render(
      <>{renderMarkdownBlocks("> remember to use features\n> address, stamp, two events")}</>,
    );
    const quote = container.querySelector("blockquote");
    expect(quote).not.toBeNull();
    expect(quote?.textContent).toContain("remember to use features");
    expect(quote?.textContent).toContain("address, stamp, two events");
  });

  it("renders '---' as <hr>", () => {
    const { container } = render(<>{renderMarkdownBlocks("first\n\n---\n\nsecond")}</>);
    expect(container.querySelector("hr")).not.toBeNull();
  });

  it("renders **bold** and *emphasis* inside paragraphs", () => {
    const { container } = render(
      <>{renderMarkdownBlocks("This is **important** and this is *italic*.")}</>,
    );
    expect(container.querySelector("strong")?.textContent).toBe("important");
    expect(container.querySelector("em")?.textContent).toBe("italic");
  });

  it("separates blocks by blank lines and never spills HTML through", () => {
    const { container } = render(
      <>{renderMarkdownBlocks('first paragraph\n\nsecond paragraph<script>alert("x")</script>')}</>,
    );
    // No <script> should escape into the DOM — the renderer never
    // injects raw HTML, only structured React elements.
    expect(container.querySelector("script")).toBeNull();
    // The literal text including <script>...</script> survives as
    // plain text inside the paragraph.
    const paras = container.querySelectorAll("p");
    expect(paras.length).toBeGreaterThanOrEqual(2);
  });

  it("renders the homework email's day-by-day shape end-to-end", () => {
    const body = [
      "## Monday",
      "- Greek: page 57",
      "- Spelling words",
      "",
      "## Tuesday",
      "- Maths: halves of amounts",
      "- Reading 10 min",
    ].join("\n");

    render(<>{renderMarkdownBlocks(body)}</>);

    // Both day headings must appear as h4s.
    expect(screen.getByRole("heading", { name: /Monday/i }).tagName.toLowerCase()).toBe("h4");
    expect(screen.getByRole("heading", { name: /Tuesday/i }).tagName.toLowerCase()).toBe("h4");

    // Four bullets total, distributed across two <ul>s.
    const items = screen.getAllByRole("listitem");
    expect(items).toHaveLength(4);
    expect(items[0].parentElement).not.toBe(items[2].parentElement);
  });
});
