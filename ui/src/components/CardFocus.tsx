import { useEffect, useMemo, useRef, useState } from "react";
import clsx from "clsx";
import { Send, X } from "lucide-react";
import type { Card as CardData } from "../types";
import { useCardThread } from "../api/useCardThread";
import { useCardConverse } from "../api/useCardConverse";
import { useSends, type Send as SendItem } from "../api/useSends";
import { SubCard } from "./SubCard";
import { CalendarPage } from "./CalendarPage";
import { TasksPanel } from "./TasksPanel";
import { renderMarkdown } from "../lib/markdown";

interface Props {
  card: CardData;
  onClose: () => void;
}

const FALLBACK_PROMPTS = [
  "Tell me more",
  "What changed since yesterday?",
  "Draft something based on this",
];

// FOCUS_PROMPTS_BY_KIND mirrors the design's hardcoded prompt sets in
// Zeno V2/zeno-focus.jsx:5–70 — used when the focused "card" is one of
// the synthetic anchor kinds (calendar_day / tasks_view).
const FOCUS_PROMPTS_BY_KIND: Record<string, string[]> = {
  calendar_day: [
    "Find a 30-min slot for Sam call",
    "Move my 15:00 to Wednesday",
    "What does tomorrow look like?",
    "Block 12:30–13:30 for the run",
    "Show me this week's load",
  ],
  tasks_view: [
    "What's actually due today?",
    "Anything I'm forgetting before tomorrow's board prep?",
    "Move 'Brief Mara' to Friday",
    "Remind me at 16:30 to leave for Lia's recital",
    "What can I delegate to Mara?",
  ],
};

// eyebrowFor matches the design's per-kind header copy
// (Zeno V2/zeno-focus.jsx:650).
function eyebrowFor(kind?: string): string {
  if (kind === "calendar_day") return "in calendar";
  if (kind === "tasks_view") return "in tasks";
  return "in conversation about";
}

// CardFocus is the right-side panel opened by clicking a card. The pinned
// card sits at the top as immutable context (left-accent rule, no box);
// the body is a thread of (prompt → SubCard) turns that persists across
// opens for this card. Suggested prompts come from the card's own
// ask_followup actions when present.
export function CardFocus({ card, onClose }: Props) {
  const thread = useCardThread(card.id);
  const converse = useCardConverse(card.id);
  // V2.13.2: assistant-mode sends anchored to this card's calendar
  // event. Empty when the card has no anchor or no sends were made.
  const sendsQuery = useSends({ cardId: card.id });
  const [val, setVal] = useState("");
  const inputRef = useRef<HTMLInputElement | null>(null);
  const scrollRef = useRef<HTMLDivElement | null>(null);

  const turns = thread.data?.turns ?? [];
  const pending = converse.isPending;
  const isAnchor = card.kind === "calendar_day" || card.kind === "tasks_view";
  const cardSends = sendsQuery.data ?? [];

  const suggestedPrompts = useMemo(() => {
    if (isAnchor && card.kind && FOCUS_PROMPTS_BY_KIND[card.kind]) {
      return FOCUS_PROMPTS_BY_KIND[card.kind];
    }
    const fromActions = (card.actions ?? [])
      .filter((a) => (a.intent ?? "") === "ask_followup")
      .map((a) => {
        const seed = (a.target ?? {})["seed"];
        return typeof seed === "string" && seed.length > 0 ? seed : a.label;
      });
    return fromActions.length > 0 ? fromActions : FALLBACK_PROMPTS;
  }, [card.actions, card.kind, isAnchor]);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    const focusTimer = setTimeout(() => inputRef.current?.focus(), 80);
    return () => {
      window.removeEventListener("keydown", onKey);
      clearTimeout(focusTimer);
    };
  }, [onClose]);

  useEffect(() => {
    if (scrollRef.current) {
      scrollRef.current.scrollTop = scrollRef.current.scrollHeight;
    }
  }, [turns.length, pending]);

  function ask(text: string) {
    const trimmed = text.trim();
    if (!trimmed || pending) return;
    setVal("");
    converse.mutate(trimmed);
  }

  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-labelledby="card-focus-title"
      className="fixed inset-0 z-50 flex justify-end bg-bg/70 backdrop-blur-md backdrop-saturate-150 animate-fade-in"
      onClick={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div
        className="w-[min(720px,92vw)] h-full bg-bg border-l border-line flex flex-col animate-slide-in-right"
        style={{ boxShadow: "-24px 0 64px -32px rgba(14,14,12,0.18)" }}
      >
        {/* Header */}
        <div className="flex items-center justify-between px-7 pt-[18px] pb-[14px] border-b border-line shrink-0">
          <div className="flex flex-col gap-[2px]">
            <span className="font-mono text-[10.5px] uppercase tracking-[.08em] text-ink-4">
              {eyebrowFor(card.kind)}
            </span>
            <span className="text-[12px] text-ink-2">{card.src_label}</span>
          </div>
          <button
            type="button"
            onClick={onClose}
            aria-label="Close"
            className="h-7 w-7 rounded-[6px] border border-line grid place-items-center text-ink-3 hover:bg-bg-elev hover:text-ink"
          >
            <X className="h-3.5 w-3.5" />
          </button>
        </div>

        {/* Body */}
        <div ref={scrollRef} className="flex-1 overflow-y-auto px-7 pt-6 pb-4">
          {/* Anchor — left accent rule, no surrounding box. For the
              calendar / tasks rail-anchors the body itself is the full
              CalendarPage / TasksPanel surface (Zeno V2/zeno-focus.jsx
              :662–673). Note: the rail-click also stamps a hidden
              h3#card-focus-title for aria-labelledby. */}
          <div className="border-l-2 border-accent pl-[18px] py-1 mb-7">
            {isAnchor ? (
              <>
                <h3 id="card-focus-title" className="sr-only">
                  {card.title}
                </h3>
                {card.kind === "calendar_day" && <CalendarPage />}
                {card.kind === "tasks_view" && <TasksPanel />}
              </>
            ) : (
              <>
                <h3
                  id="card-focus-title"
                  className="font-display text-[22px] font-[400] leading-[1.25] tracking-[-0.005em] text-ink m-0 mb-2"
                >
                  {renderMarkdown(card.title, {
                    emClassName: "not-italic font-[600] text-ink",
                  })}
                </h3>
                {card.sub && (
                  <p className="text-[14px] leading-[1.55] text-ink-2 m-0">
                    {renderMarkdown(card.sub, {
                      emClassName: "not-italic font-[600] text-ink",
                    })}
                  </p>
                )}
                {card.body && (
                  <div className="mt-4 text-[14px] leading-[1.65] text-ink-2 space-y-3 max-w-prose">
                    {card.body.split(/\n{2,}/).map((para, i) => (
                      <p key={i} className="m-0">
                        {renderMarkdown(para, {
                          emClassName: "not-italic font-[600] text-ink",
                        })}
                      </p>
                    ))}
                  </div>
                )}
                {card.sources && card.sources.length > 0 && (
                  <div className="mt-5 pt-4 border-t border-line">
                    <h6 className="font-mono text-[10.5px] uppercase tracking-[.08em] text-ink-4 mb-2">
                      Sources
                    </h6>
                    <ul className="space-y-1.5 m-0 p-0 list-none">
                      {card.sources.map((s, i) => (
                        <li key={i} className="text-[13px] leading-[1.45]">
                          <a
                            href={s.u}
                            target="_blank"
                            rel="noreferrer noopener"
                            className="text-accent hover:underline break-words"
                            title={s.u}
                          >
                            {s.t || s.u}
                          </a>
                        </li>
                      ))}
                    </ul>
                  </div>
                )}
              </>
            )}
          </div>

          {/* V2.13.2: assistant-mode send banner — surfaces "you texted
              X earlier" status for any sends anchored on this card. */}
          {cardSends.length > 0 && (
            <div className="mb-6 flex flex-col gap-2">
              {cardSends.map((s) => (
                <SendBanner key={s.id} send={s} />
              ))}
            </div>
          )}

          {/* Thread */}
          <div className="flex flex-col gap-[26px]">
            {thread.isLoading && (
              <div className="font-mono text-[11px] text-ink-4">
                Loading thread…
              </div>
            )}
            {turns.map((turn) => (
              <div key={turn.id} className="flex flex-col gap-3">
                <div className="flex gap-2.5 items-start text-[14px] text-ink-2">
                  <span className="text-accent font-[500]">›</span>
                  <span>{turn.prompt}</span>
                </div>
                <SubCard cardId={card.id} reply={turn.reply} />
              </div>
            ))}
            {pending && (
              <div className="flex flex-col gap-3 animate-fade-in">
                <div className="flex gap-2.5 items-start text-[14px] text-ink-2">
                  <span className="text-accent font-[500]">›</span>
                  <span>{converse.variables}</span>
                </div>
                <div className="flex gap-2 items-center text-[12px] text-ink-3 py-1.5">
                  <DotsInline />
                  <span>Zeno is working…</span>
                </div>
              </div>
            )}
            {converse.isError && (
              <div className="font-mono text-[11px] text-crit">
                {converse.error.message}
              </div>
            )}
          </div>
        </div>

        {/* Footer — chips (only on first open) + bordered input row */}
        <div className="px-7 pt-[14px] pb-[18px] border-t border-line bg-bg shrink-0">
          {turns.length === 0 && !pending && !thread.isLoading && (
            <div className="mb-3">
              <span className="block font-mono text-[10.5px] uppercase tracking-[.08em] text-ink-4 mb-2">
                try
              </span>
              <div className="flex flex-wrap gap-1.5">
                {suggestedPrompts.map((p, i) => (
                  <button
                    key={i}
                    type="button"
                    onClick={() => ask(p)}
                    className="border border-line rounded-full px-3 py-1.5 text-[12.5px] text-ink-2 hover:border-accent hover:text-accent hover:bg-accent-soft transition-all"
                  >
                    {p}
                  </button>
                ))}
              </div>
            </div>
          )}
          <div className="flex gap-2 items-center border border-line rounded-[8px] pl-3.5 pr-1 py-1 transition-colors focus-within:border-accent">
            <input
              ref={inputRef}
              type="text"
              value={val}
              onChange={(e) => setVal(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") ask(val);
              }}
              placeholder={
                turns.length === 0
                  ? "Ask, draft, or schedule…"
                  : "Ask a follow-up…"
              }
              spellCheck={false}
              autoComplete="off"
              aria-label="Ask Zeno about this card"
              className="flex-1 bg-transparent text-[14px] text-ink placeholder:text-ink-4 py-2 focus:outline-none border-0"
            />
            <button
              type="button"
              onClick={() => ask(val)}
              disabled={!val.trim() || pending}
              aria-label="Send"
              className="h-8 w-8 rounded-[6px] bg-accent text-white grid place-items-center disabled:opacity-25"
            >
              <Send className="h-3.5 w-3.5" />
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}

// SendBanner shows the status of an assistant-mode send anchored on
// this card. Recipient + relative sent time + status pill; on resolved
// rows the inbound reply is quoted inline.
function SendBanner({ send }: { send: SendItem }) {
  const sent = new Date(send.sent_at);
  const minsAgo = Math.max(1, Math.round((Date.now() - sent.getTime()) / 60_000));
  const ago =
    minsAgo < 60
      ? `${minsAgo}m ago`
      : minsAgo < 60 * 24
        ? `${Math.round(minsAgo / 60)}h ago`
        : sent.toLocaleString();
  const tone =
    send.status === "replied"
      ? "border-accent/40 bg-accent/5"
      : send.status === "expired"
        ? "border-line bg-bg-2"
        : "border-amber/40 bg-amber-soft";
  const label =
    send.status === "replied"
      ? `${send.recipient_name} replied`
      : send.status === "expired"
        ? `Sent to ${send.recipient_name} — no reply (expired)`
        : `Texted ${send.recipient_name} ${ago} — awaiting reply`;
  return (
    <div
      className={clsx(
        "rounded-md border px-3 py-2 text-[12.5px]",
        tone
      )}
    >
      <div className="flex items-baseline gap-2">
        <span className="text-ink font-medium">{label}</span>
        {send.status !== "awaiting_reply" && (
          <span className="font-mono text-[10.5px] text-ink-4">{ago}</span>
        )}
      </div>
      {send.status === "replied" && send.reply_body && (
        <pre className="mt-1.5 whitespace-pre-wrap rounded-z-sm border-l-2 border-accent pl-2 py-0.5 text-[12px] text-ink-2 font-sans m-0">
          {send.reply_body}
        </pre>
      )}
    </div>
  );
}

// DotsInline renders three accent dots staggered for the "Zeno is
// working…" pulse. Mirrors the design's `.dots-inline` keyframe.
function DotsInline() {
  return (
    <span className="inline-flex gap-[3px]" aria-hidden>
      {[0, 1, 2].map((i) => (
        <i
          key={i}
          className="h-1 w-1 rounded-full bg-accent animate-pulse"
          style={{ animationDelay: `${i * 150}ms` }}
        />
      ))}
    </span>
  );
}
