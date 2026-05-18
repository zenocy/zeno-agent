// Block-level markdown renderer used by the SubCard `document` kind to
// display lightly-restructured source content (emails, notices). The
// existing `markdown.tsx` is inline-only (`*emphasis*` for card titles);
// keeping block parsing in a separate module preserves that surface
// untouched.
//
// Supported blocks: `## heading`, `- `/`* ` unordered list, `<n>. `
// ordered list, `> blockquote`, `---` hr, and paragraphs separated by
// blank lines. Inline runs are passed through a small parser that
// handles `**bold**` and `*emphasis*`. Anything not recognized stays as
// a paragraph — no raw HTML is ever rendered.

import type { ReactNode } from "react";

type Block =
  | { kind: "heading"; level: 2 | 3; text: string }
  | { kind: "ul"; items: string[] }
  | { kind: "ol"; items: string[] }
  | { kind: "quote"; text: string }
  | { kind: "hr" }
  | { kind: "p"; text: string };

const HEADING_RE = /^(#{1,3})\s+(.*)$/;
const UL_RE = /^[-*]\s+(.*)$/;
const OL_RE = /^\d+\.\s+(.*)$/;
const QUOTE_RE = /^>\s?(.*)$/;
const HR_RE = /^---+$/;

function parseBlocks(raw: string): Block[] {
  const lines = raw.replace(/\r\n/g, "\n").split("\n");
  const out: Block[] = [];
  let i = 0;

  while (i < lines.length) {
    const line = lines[i];
    const trimmed = line.trim();

    if (trimmed === "") {
      i++;
      continue;
    }

    if (HR_RE.test(trimmed)) {
      out.push({ kind: "hr" });
      i++;
      continue;
    }

    const h = HEADING_RE.exec(trimmed);
    if (h) {
      // `#` and `###` collapse to the same visual level as `##` inside a
      // sub-card so the hierarchy stays tight.
      out.push({ kind: "heading", level: h[1].length === 2 ? 2 : 3, text: h[2] });
      i++;
      continue;
    }

    const ul = UL_RE.exec(trimmed);
    if (ul) {
      const items: string[] = [];
      while (i < lines.length) {
        const m = UL_RE.exec(lines[i].trim());
        if (!m || lines[i].trim() === "") break;
        items.push(m[1]);
        i++;
      }
      out.push({ kind: "ul", items });
      continue;
    }

    const ol = OL_RE.exec(trimmed);
    if (ol) {
      const items: string[] = [];
      while (i < lines.length) {
        const m = OL_RE.exec(lines[i].trim());
        if (!m || lines[i].trim() === "") break;
        items.push(m[1]);
        i++;
      }
      out.push({ kind: "ol", items });
      continue;
    }

    const q = QUOTE_RE.exec(trimmed);
    if (q) {
      const parts: string[] = [q[1]];
      i++;
      while (i < lines.length) {
        const m = QUOTE_RE.exec(lines[i].trim());
        if (!m) break;
        parts.push(m[1]);
        i++;
      }
      out.push({ kind: "quote", text: parts.join(" ") });
      continue;
    }

    const paraParts: string[] = [trimmed];
    i++;
    while (i < lines.length) {
      const t = lines[i].trim();
      if (
        t === "" ||
        HEADING_RE.test(t) ||
        UL_RE.test(t) ||
        OL_RE.test(t) ||
        QUOTE_RE.test(t) ||
        HR_RE.test(t)
      ) {
        break;
      }
      paraParts.push(t);
      i++;
    }
    out.push({ kind: "p", text: paraParts.join(" ") });
  }

  return out;
}

// renderInline handles `**bold**` then `*emphasis*` within a single line.
// Bold is parsed first so its inner asterisks don't get re-consumed as emphasis.
function renderInline(raw: string): ReactNode[] {
  const nodes: ReactNode[] = [];
  const boldRe = /\*\*([^*]+)\*\*/g;
  let last = 0;
  let m: RegExpExecArray | null;
  let key = 0;
  while ((m = boldRe.exec(raw)) !== null) {
    if (m.index > last) {
      nodes.push(...renderEmphasis(raw.slice(last, m.index), key));
      key += 100;
    }
    nodes.push(
      <strong key={`b${key++}`} className="font-[600] text-ink">
        {m[1]}
      </strong>,
    );
    last = m.index + m[0].length;
  }
  if (last < raw.length) {
    nodes.push(...renderEmphasis(raw.slice(last), key));
  }
  return nodes;
}

function renderEmphasis(raw: string, baseKey: number): ReactNode[] {
  const nodes: ReactNode[] = [];
  const re = /\*([^*]+)\*/g;
  let last = 0;
  let m: RegExpExecArray | null;
  let k = baseKey;
  while ((m = re.exec(raw)) !== null) {
    if (m.index > last) {
      nodes.push(<span key={`t${k++}`}>{raw.slice(last, m.index)}</span>);
    }
    nodes.push(
      <em key={`e${k++}`} className="not-italic font-[430] text-ink-3">
        {m[1]}
      </em>,
    );
    last = m.index + m[0].length;
  }
  if (last < raw.length) {
    nodes.push(<span key={`t${k++}`}>{raw.slice(last)}</span>);
  }
  return nodes;
}

export function renderMarkdownBlocks(raw: string): ReactNode {
  const blocks = parseBlocks(raw);
  return (
    <div className="flex flex-col gap-3 text-[14px] leading-[1.6] text-ink">
      {blocks.map((b, i) => renderBlock(b, i))}
    </div>
  );
}

function renderBlock(b: Block, i: number): ReactNode {
  switch (b.kind) {
    case "heading":
      return (
        <h4
          key={i}
          className="text-[13px] font-[600] uppercase tracking-[.04em] text-ink-2 m-0 mt-1"
        >
          {renderInline(b.text)}
        </h4>
      );
    case "ul":
      return (
        <ul key={i} className="m-0 pl-5 list-disc marker:text-ink-4 flex flex-col gap-1">
          {b.items.map((it, j) => (
            <li key={j} className="text-[14px] leading-[1.55] text-ink">
              {renderInline(it)}
            </li>
          ))}
        </ul>
      );
    case "ol":
      return (
        <ol key={i} className="m-0 pl-5 list-decimal marker:text-ink-4 flex flex-col gap-1">
          {b.items.map((it, j) => (
            <li key={j} className="text-[14px] leading-[1.55] text-ink">
              {renderInline(it)}
            </li>
          ))}
        </ol>
      );
    case "quote":
      return (
        <blockquote
          key={i}
          className="m-0 px-3 py-1.5 border-l-2 border-ink-5 text-ink-2 text-[13.5px] leading-[1.55]"
        >
          {renderInline(b.text)}
        </blockquote>
      );
    case "hr":
      return <hr key={i} className="m-0 border-0 border-t border-line" />;
    case "p":
      return (
        <p key={i} className="m-0 text-[14px] leading-[1.6] text-ink">
          {renderInline(b.text)}
        </p>
      );
  }
}
