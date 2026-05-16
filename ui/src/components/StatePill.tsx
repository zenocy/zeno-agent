import type { State } from "../types";

interface StatePillProps {
  state?: State;
  tension: number;
}

const STATE_LABELS: Record<State, string> = {
  morning_calm: "calm morning",
  pre_meeting: "pre-meeting",
  deep_work: "deep work",
  message_inject: "signal",
  end_of_day: "winding down",
};

interface BandStyle {
  className: string;
}

function bandStyle(tension: number): BandStyle {
  if (tension <= 25) return { className: "bg-blue-500/20 text-blue-200" };
  if (tension <= 45) return { className: "bg-emerald-500/20 text-emerald-200" };
  if (tension <= 55) return { className: "bg-stone-500/20 text-stone-200" };
  if (tension <= 75) return { className: "bg-amber-500/20 text-amber-200" };
  return { className: "bg-rose-500/20 text-rose-200" };
}

export function StatePill({ state, tension }: StatePillProps) {
  const label = (state && STATE_LABELS[state]) || STATE_LABELS.morning_calm;
  const { className } = bandStyle(tension);
  const tooltip = `${label} · tension ${tension}`;

  return (
    <div
      className={`px-2 py-0.5 rounded-full text-[11px] font-mono lowercase ${className}`}
      role="status"
      aria-label={`Today's register: ${label}, tension ${tension}`}
      title={tooltip}
    >
      {label}
    </div>
  );
}
