import { useEffect, useState } from "react";
import dayjs from "dayjs";
import clsx from "clsx";
import { Phone, FileText } from "lucide-react";

import type { Briefing as BriefingData, CalendarEvent, State } from "../types";
import { renderMarkdown } from "../lib/markdown";

interface Props {
  data?: BriefingData | null;
  isLoading: boolean;
  // Optional pre-meeting context. App.tsx derives `nextEvent` from
  // useTodayCalendar() when state === "pre_meeting" and passes onBrief
  // as a callback so Briefing.tsx stays test-isolated (no need for a
  // QueryClientProvider wrapper around the legacy unit tests).
  nextEvent?: CalendarEvent | null;
  onBrief?: (event: CalendarEvent) => void;
}

const STATE_EYEBROW: Record<State, string> = {
  morning_calm: "morning brief",
  pre_meeting: "pre-meeting",
  deep_work: "deep work",
  message_inject: "signal",
  end_of_day: "end of day",
};

function eyebrowDotColor(t: number) {
  if (t >= 70) return "bg-crit";
  if (t >= 40) return "bg-amber";
  return "bg-accent";
}

// initials returns up to two upper-case letters from a person's name —
// used in the pinned-event attendee strip.
function initials(name: string): string {
  const trimmed = name.trim();
  if (!trimmed) return "·";
  const parts = trimmed.split(/\s+/);
  const first = parts[0]?.[0] ?? "";
  const last = parts.length > 1 ? parts[parts.length - 1][0] : "";
  return (first + last).toUpperCase() || trimmed.slice(0, 2).toUpperCase();
}

// PinnedNextMeeting renders the pre-meeting countdown card the design
// shows above the eyebrow when a meeting is imminent. The countdown
// re-renders every second and tones from accent → amber (≤15 min) →
// crit (≤5 min).
function PinnedNextMeeting({
  event,
  onBrief,
}: {
  event: CalendarEvent;
  onBrief?: (event: CalendarEvent) => void;
}) {
  const [now, setNow] = useState(() => dayjs());
  useEffect(() => {
    const id = window.setInterval(() => setNow(dayjs()), 1000);
    return () => window.clearInterval(id);
  }, []);

  const start = dayjs(event.start);
  const end = event.end ? dayjs(event.end) : null;
  const totalSeconds = Math.max(0, start.diff(now, "second"));
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  const tone =
    totalSeconds <= 5 * 60
      ? "text-crit"
      : totalSeconds <= 15 * 60
        ? "text-amber"
        : "text-accent";

  const durMin = end ? Math.max(0, end.diff(start, "minute")) : 0;
  const attendees = event.attendees ?? [];
  const isLink =
    !!event.location && /^https?:\/\//i.test(event.location);

  function handleJoin() {
    if (isLink && event.location) {
      window.open(event.location, "_blank", "noopener,noreferrer");
    }
  }

  return (
    <div
      className="mb-6 rounded-z-md border border-line bg-bg-card animate-fade-up overflow-hidden"
      data-testid="pinned-next-meeting"
    >
      <div className="grid grid-cols-[110px_1fr_auto] gap-4 px-4 py-3.5 items-center">
        {/* Countdown clock */}
        <div className="flex flex-col">
          <span className="font-display font-[500] text-[28px] leading-none text-ink tracking-[-0.01em]">
            {start.format("HH:mm")}
          </span>
          <span className="mt-1 font-mono text-[11px] text-ink-3">
            in {minutes}:{String(seconds).padStart(2, "0")}
          </span>
          <span className={clsx("font-mono text-[11px] mt-0.5", tone)}>
            · {totalSeconds <= 0 ? "now" : "locked"}
          </span>
        </div>

        {/* Meeting details */}
        <div className="min-w-0">
          <h4 className="text-[15px] font-[500] leading-[1.35] text-ink m-0 truncate">
            {event.title}
          </h4>
          <div className="mt-1 font-mono text-[11px] text-ink-3 flex flex-wrap items-center gap-1.5">
            {durMin > 0 && <span>{durMin} min</span>}
            {durMin > 0 && attendees.length > 0 && <span>·</span>}
            {attendees.length > 0 && (
              <span>
                {attendees.length}{" "}
                {attendees.length === 1 ? "attendee" : "attendees"}
              </span>
            )}
            {event.location && !isLink && (
              <>
                <span>·</span>
                <span className="truncate max-w-[180px]">{event.location}</span>
              </>
            )}
          </div>
          {attendees.length > 0 && (
            <div className="mt-2 flex items-center gap-1">
              {attendees.slice(0, 5).map((a, i) => (
                <span
                  key={i}
                  title={a}
                  className="h-5 px-1.5 rounded-[4px] border border-line bg-bg-elev font-mono text-[10px] text-ink-3 inline-flex items-center"
                >
                  {initials(a)}
                </span>
              ))}
              {attendees.length > 5 && (
                <span className="font-mono text-[10px] text-ink-4 ml-0.5">
                  +{attendees.length - 5}
                </span>
              )}
            </div>
          )}
        </div>

        {/* Actions */}
        <div className="flex items-center gap-1.5">
          <button
            type="button"
            onClick={handleJoin}
            disabled={!isLink}
            className="h-7 px-2.5 rounded-[7px] border border-line text-ink-2 bg-transparent hover:bg-bg-elev hover:border-ink-5 transition disabled:opacity-40 disabled:cursor-not-allowed inline-flex items-center gap-1.5 text-[12px] font-[500]"
          >
            <Phone className="h-3 w-3" />
            Join
          </button>
          <button
            type="button"
            onClick={() => onBrief?.(event)}
            disabled={!onBrief}
            className="h-7 px-2.5 rounded-[7px] bg-ink text-bg border border-ink hover:opacity-90 transition disabled:opacity-40 inline-flex items-center gap-1.5 text-[12px] font-[500]"
          >
            <FileText className="h-3 w-3" />
            Brief
          </button>
        </div>
      </div>
    </div>
  );
}

export function Briefing({ data, isLoading, nextEvent, onBrief }: Props) {
  if (isLoading) {
    return (
      <section className="mb-8 animate-fade-in">
        <div className="h-4 w-24 rounded bg-ink-5 opacity-30 mb-4" />
        <div className="h-7 w-3/4 rounded bg-ink-5 opacity-20 mb-3" />
        <div className="h-4 w-full rounded bg-ink-5 opacity-15 mb-2" />
        <div className="h-4 w-5/6 rounded bg-ink-5 opacity-15" />
      </section>
    );
  }

  if (!data) {
    return (
      <section className="mb-8">
        <p className="font-mono text-[11px] text-ink-5">No briefing yet — synthesis hasn't run today.</p>
      </section>
    );
  }

  const tension = Math.max(0, Math.min(100, data.tension));
  const showPinned = data.state === "pre_meeting" && !!nextEvent;

  return (
    <section className="mb-8 animate-fade-up">
      {showPinned && nextEvent && (
        <PinnedNextMeeting event={nextEvent} onBrief={onBrief} />
      )}

      {/* Eyebrow */}
      <div className="mb-3">
        <span className="inline-flex items-center gap-1.5 font-mono text-[11px] uppercase tracking-wide text-ink-4 border border-line rounded-full px-2.5 py-0.5">
          <span className={`h-1.5 w-1.5 rounded-full ${eyebrowDotColor(tension)}`} />
          {data.eyebrow || (data.state && STATE_EYEBROW[data.state]) || STATE_EYEBROW.morning_calm}
        </span>
      </div>

      {/* Title */}
      <h1
        className="font-display font-[400] text-[22px] leading-[1.3] tracking-[-0.012em] text-ink mb-3 max-w-[36ch]"
        style={{ fontVariationSettings: '"opsz" 36' }}
      >
        {renderMarkdown(data.title)}
      </h1>

      {/* Summary */}
      <p className="text-[15px] leading-[1.55] text-ink-2 max-w-[56ch]">
        {renderMarkdown(data.summary)}
      </p>
    </section>
  );
}
