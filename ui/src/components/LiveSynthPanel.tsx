// LiveSynthPanel — global Topbar-mounted live trace for V2.4.
//
// Rendered nothing-until-active; mounts on synth.started, animates
// through trace.step + synth.delta arrivals, dissolves on
// synth.completed. Subscribes via useLiveSynth (which sits on top of
// the broker fed by useTodayStream's SSE listeners).
//
// Class names match the prototype canon (Zeno V2/zeno-cards.jsx + Zeno.html)
// so the CSS port in LiveSynthPanel.css is a one-grep diff.

import type { TraceStep } from "../types";
import { useLiveSynth } from "../api/useLiveSynth";
import "./LiveSynthPanel.css";

interface TraceStepRowProps {
  step: TraceStep;
  state: "active" | "done";
}

function TraceStepRow({ step, state }: TraceStepRowProps) {
  if (step.kind === "thought") {
    return (
      <li className={`trace-step trace-thought ${state}`}>
        <span className="trace-rule" aria-hidden />
        <p>{step.t}</p>
      </li>
    );
  }
  return (
    <li className={`trace-step ${state}`}>
      <span className="trace-bullet" aria-hidden />
      {step.op && <span className="trace-op">{step.op}</span>}
      {step.target && <span className="trace-target">{step.target}</span>}
      {step.note && <span className="trace-note">· {step.note}</span>}
    </li>
  );
}

export function LiveSynthPanel() {
  const { active, dissolving, stage, steps, body } = useLiveSynth();

  if (!active && !dissolving) return null;

  // Eyebrow voice: "thinking" for ask (the user is waiting on a direct
  // answer), "working" for morning / inject (background synth surfacing).
  const eyebrow = stage === "ask" ? "thinking" : "working";

  return (
    <div
      className={`live-synth-panel ${dissolving ? "dissolving" : "active"}`}
      role="status"
      aria-live="polite"
      aria-label="Live synthesis in progress" // allow-pm-language
    >
      <div className="trace-head">
        <span className="trace-eb">{eyebrow}</span>
        <span className="trace-progress">
          {steps.length} {steps.length === 1 ? "step" : "steps"}
        </span>
      </div>
      <ol className="trace-steps">
        {steps.map((s, i) => (
          <TraceStepRow
            key={i}
            step={s}
            state={i === steps.length - 1 && active ? "active" : "done"}
          />
        ))}
      </ol>
      {body && (
        <div className="live-body">
          <p>{body}</p>
        </div>
      )}
    </div>
  );
}
