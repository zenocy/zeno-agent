import { useEffect, useState } from "react";
import { Sparkles, ArrowRight, Loader2 } from "lucide-react";
import clsx from "clsx";
import type { State } from "../types";

const FALLBACK_SUGGESTION = "Draft my reply to Saru using the redline?";

interface Chip {
  id: string;
  label: string;
}

const MORNING_CALM_CHIPS: Chip[] = [
  { id: "focus", label: "What should I focus on?" },
  { id: "tonight", label: "What's tonight?" },
  { id: "draft-saru", label: "Draft a reply to Saru" },
];

const CHIPS_BY_STATE: Record<State, Chip[]> = {
  morning_calm: MORNING_CALM_CHIPS,
  pre_meeting: [
    { id: "meeting-prep", label: "What's on for the meeting?" },
    { id: "one-liner", label: "Draft a one-liner" },
    { id: "open-with", label: "What should I open with?" },
  ],
  deep_work: [
    { id: "one-thing", label: "What's the one thing?" },
    { id: "hold-pings", label: "Hold pings until 17:00" },
    { id: "summarize", label: "Summarize the redline thread" },
  ],
  end_of_day: [
    { id: "tomorrow-first", label: "Tomorrow's first thing" },
    { id: "draft-tonight", label: "Draft tonight or hold?" },
    { id: "closed-today", label: "What closed today?" },
  ],
  message_inject: MORNING_CALM_CHIPS,
};

function chipsForState(state?: State): Chip[] {
  if (!state || !(state in CHIPS_BY_STATE)) return MORNING_CALM_CHIPS;
  return CHIPS_BY_STATE[state];
}

interface Props {
  onSubmit: (query: string) => void;
  loading?: boolean;
  suggestion?: string;
  state?: State;
}

export function InputBar({ onSubmit, loading = false, suggestion, state }: Props) {
  const target = suggestion?.trim() || FALLBACK_SUGGESTION;
  const [val, setVal] = useState("");
  const [ghostLen, setGhostLen] = useState(0);

  // Animate ghost text character by character whenever the target changes.
  useEffect(() => {
    setGhostLen(0);
    let i = 0;
    const id = setInterval(() => {
      i += 1;
      setGhostLen(i);
      if (i >= target.length) clearInterval(id);
    }, 18);
    return () => clearInterval(id);
  }, [target]);

  const showGhost = !val && ghostLen > 0;
  const ghost = target.slice(0, ghostLen);
  const ghostComplete = ghostLen >= target.length;

  function submit() {
    const q = val.trim();
    if (!q || loading) return;
    setVal("");
    onSubmit(q);
  }

  function onKeyDown(e: React.KeyboardEvent<HTMLInputElement>) {
    if (e.key === "Tab" && !val && ghostComplete) {
      e.preventDefault();
      setVal(target);
      return;
    }
    if (e.key === "Enter") submit();
  }

  return (
    <div className="flex flex-col items-center gap-2">
      {/* Chips row */}
      <div className="flex items-center justify-center gap-1.5 flex-wrap">
        {chipsForState(state).map((c) => (
          <button
            key={c.id}
            type="button"
            disabled={loading}
            onClick={() => { setVal(""); onSubmit(c.label); }}
            className="inline-flex items-center gap-1.5 h-6 px-2.5 rounded-full border border-line bg-bg-card text-[11px] font-mono text-ink-4 hover:bg-bg-elev hover:text-ink-3 transition disabled:opacity-40 disabled:cursor-not-allowed"
          >
            <span className="h-1 w-1 rounded-full bg-accent" />
            {c.label}
          </button>
        ))}
      </div>

      {/* Input shell — the visible card. */}
      <div
        className={clsx(
          "w-full bg-bg-card border border-line-strong rounded-[14px] px-3.5 py-2.5",
          "flex items-center gap-2.5",
          "shadow-[0_4px_24px_rgba(14,14,12,0.06)] dark:shadow-[0_4px_30px_rgba(0,0,0,0.35)]",
        )}
      >
        <div className="flex items-center gap-2 shrink-0 text-ink-4">
          <Sparkles className="h-3.5 w-3.5" />
          <span className="font-mono text-[11px] text-ink-5 hidden sm:block">ask · or wait for Zeno</span>
        </div>

        <div className="relative flex-1">
          <input
            type="text"
            value={val}
            onChange={(e) => setVal(e.target.value)}
            onKeyDown={onKeyDown}
            spellCheck={false}
            autoComplete="off"
            aria-label="Ask Zeno"
            disabled={loading}
            className="w-full h-7 bg-transparent text-[14px] text-ink outline-none placeholder-transparent disabled:opacity-50"
          />
          {showGhost && (
            <div className="absolute inset-0 flex items-center pointer-events-none">
              <span className="text-[14px] text-ink-5 select-none">{ghost}</span>
              {ghostComplete && (
                <span className="ml-2 font-mono text-[10px] text-ink-5 border border-line-strong rounded px-1 py-px bg-bg-elev">tab</span>
              )}
            </div>
          )}
        </div>

        <button
          type="button"
          onClick={submit}
          disabled={loading || !val.trim()}
          className={clsx(
            "h-7 w-7 rounded-[7px] grid place-items-center bg-ink text-bg shrink-0 transition",
            loading || !val.trim() ? "opacity-35 cursor-not-allowed" : "hover:-translate-y-px",
          )}
          aria-label="Send"
        >
          {loading ? (
            <Loader2 className="h-3.5 w-3.5 animate-spin" />
          ) : (
            <ArrowRight className="h-3.5 w-3.5" />
          )}
        </button>
      </div>
    </div>
  );
}
