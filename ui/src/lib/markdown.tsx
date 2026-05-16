// Tiny inline markdown renderer for `*emphasis*`. The synth side
// canonicalizes `**bold**` and `<em>x</em>` to `*x*` (see
// internal/synth/cards.go canonicalizeMarkdown), so the UI only needs to
// handle a single `*…*` shape. Anything else passes through as plain text.

import type { ReactNode } from "react";

export type MarkdownSegment = { text: string } | { em: string };

// splitMarkdownSegments parses `*emphasis*` runs out of a string. Backslash
// escapes are intentionally not supported — the synth canonicalizer already
// balances stray asterisks (balanceMarkdown), so by the time text reaches the
// UI a lone `*` is rare.
export function splitMarkdownSegments(raw: string): MarkdownSegment[] {
  const segments: MarkdownSegment[] = [];
  const re = /\*([^*]+)\*/g;
  let last = 0;
  let m: RegExpExecArray | null;
  while ((m = re.exec(raw)) !== null) {
    if (m.index > last) segments.push({ text: raw.slice(last, m.index) });
    segments.push({ em: m[1] });
    last = m.index + m[0].length;
  }
  if (last < raw.length) segments.push({ text: raw.slice(last) });
  return segments;
}

interface RenderOpts {
  // CSS class applied to <em> spans. Different surfaces use different
  // weight/color treatments (briefing title vs. card title vs. card sub).
  emClassName?: string;
}

// renderMarkdown turns a `*foo*`-flavored string into a flat ReactNode list.
// Plain strings without `*` collapse to a single span, so existing
// getByText(full) tests keep working.
export function renderMarkdown(raw: string, opts: RenderOpts = {}): ReactNode[] {
  const emClass = opts.emClassName ?? "not-italic font-[430] text-ink-3";
  return splitMarkdownSegments(raw).map((seg, i) =>
    "em" in seg
      ? <em key={i} className={emClass}>{seg.em}</em>
      : <span key={i}>{seg.text}</span>
  );
}
