import { useState } from "react";
import clsx from "clsx";
import { ChevronRight, ChevronDown } from "lucide-react";
import { useTrace } from "../api/useTrace";
import type { TraceStep } from "../types";

interface Props {
  traceId: string;
}

function ToolStep({ step }: { step: TraceStep }) {
  return (
    <li className="flex items-baseline gap-2 py-1">
      <span className="h-1 w-1 rounded-full bg-accent shrink-0 mt-1.5" />
      {step.op && (
        <span className="font-mono text-[10px] uppercase tracking-widest text-ink-4 shrink-0">
          {step.op}
        </span>
      )}
      {step.target && (
        <span className="font-display italic text-[13px] text-ink-2">{step.target}</span>
      )}
      {step.note && (
        <span className="font-mono text-[10px] text-ink-5">· {step.note}</span>
      )}
    </li>
  );
}

function ThoughtStep({ step }: { step: TraceStep }) {
  return (
    <li className="flex gap-2 py-1.5">
      <span className="w-0.5 self-stretch bg-accent rounded-full shrink-0" />
      <p className="font-display italic text-[13px] leading-relaxed text-ink-3">{step.t}</p>
    </li>
  );
}

export function Trace({ traceId }: Props) {
  const [open, setOpen] = useState(false);
  const { data: trace, isLoading, error } = useTrace(open ? traceId : undefined);

  const toolCount = trace?.steps.filter((s) => s.kind === "tool").length ?? 0;
  const stepCount = trace?.steps.length ?? 0;

  return (
    <div className="mt-2 border-t border-line pt-2">
      <button
        onClick={() => setOpen((v) => !v)}
        className="flex items-center gap-1.5 font-mono text-[10px] uppercase tracking-wide text-ink-4 hover:text-ink-3 transition"
      >
        {open ? <ChevronDown className="h-3 w-3" /> : <ChevronRight className="h-3 w-3" />}
        <span>traced</span>
        {trace && (
          <span className={clsx("text-ink-5 normal-case tracking-normal not-italic")}>
            · {stepCount} steps · {toolCount} reads
          </span>
        )}
      </button>

      {open && (
        <div className="mt-2 animate-fade-in">
          {isLoading && (
            <p className="font-mono text-[11px] text-ink-5 py-1">Loading trace…</p>
          )}
          {error && (
            <p className="font-mono text-[11px] text-crit py-1">Failed to load trace</p>
          )}
          {trace && (
            <ol className="space-y-0.5 pl-1">
              {trace.steps.map((s, i) =>
                s.kind === "thought" ? (
                  <ThoughtStep key={i} step={s} />
                ) : (
                  <ToolStep key={i} step={s} />
                )
              )}
            </ol>
          )}
        </div>
      )}
    </div>
  );
}
