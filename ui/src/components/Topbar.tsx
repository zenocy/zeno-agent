import { Sun, Moon, RefreshCw, PanelRightOpen } from "lucide-react";
import { useTheme } from "../theme";
import { useNow } from "../hooks/useNow";
import { useSynthNow } from "../api/useSynthNow";
import { LiveSynthPanel } from "./LiveSynthPanel";
import { StatePill } from "./StatePill";
import type { State } from "../types";

interface TopbarProps {
  state?: State;
  tension?: number;
  rightRailHidden?: boolean;
  onShowRightRail?: () => void;
}

export function Topbar({ state, tension, rightRailHidden, onShowRightRail }: TopbarProps) {
  const { theme, toggle } = useTheme();
  const now = useNow();
  const synth = useSynthNow();

  const conflict = (synth.error as (Error & { conflict?: boolean }) | null)?.conflict;
  const status = synth.isPending
    ? "synthesizing…"
    : conflict
    ? "already running"
    : synth.isError
    ? "failed"
    : null;

  const showPill = typeof tension === "number";

  // Ghost-tiny button class shared by every right-side affordance so the
  // topbar reads as a single row of equivalents rather than a mixed bag.
  const ghostBtn =
    "h-7 px-2.5 rounded-z-sm border border-line text-ink-3 hover:bg-bg-elev hover:text-ink inline-flex items-center gap-1.5 text-[12px] font-mono transition disabled:opacity-50 disabled:cursor-not-allowed";

  return (
    <div className="border-b border-line shrink-0">
      <div className="flex items-center justify-between px-6 h-12">
        <div className="flex items-center gap-3">
          {/* Time pill */}
          <span className="inline-flex items-center gap-1.5 px-2 py-0.5 rounded-full border border-line bg-bg-card font-mono text-[11px] text-ink-3 lowercase">
            <span className="h-1.5 w-1.5 rounded-full bg-accent animate-pulse" />
            {now.format("HH:mm")} · {now.format("ddd")} · {now.format("MMM D")}
          </span>
          {showPill && <StatePill state={state} tension={tension as number} />}
        </div>
        <div className="flex items-center gap-2">
          {status && (
            <span className="font-mono text-[11px] text-ink-4 lowercase">{status}</span>
          )}
          {rightRailHidden && onShowRightRail && (
            <button
              type="button"
              onClick={onShowRightRail}
              title="Show side panel"
              aria-label="Show side panel"
              className={ghostBtn}
            >
              <PanelRightOpen className="h-3 w-3" />
              side panel
            </button>
          )}
          <button
            type="button"
            onClick={() => synth.mutate()}
            disabled={synth.isPending}
            title="Force re-run morning synth (cards + briefing)"
            className={ghostBtn}
          >
            <RefreshCw className={`h-3 w-3 ${synth.isPending ? "animate-spin" : ""}`} />
            refresh
          </button>
          <button type="button" onClick={toggle} className={ghostBtn}>
            {theme === "dark" ? <Sun className="h-3 w-3" /> : <Moon className="h-3 w-3" />}
            {theme}
          </button>
        </div>
      </div>
      <LiveSynthPanel />
    </div>
  );
}
