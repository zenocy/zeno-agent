import { useEffect, useState } from "react";
import dayjs, { type Dayjs } from "dayjs";
import clsx from "clsx";

import { useTodayCalendar } from "../api/useTodayCalendar";
import { useTomorrowCalendar } from "../api/useTomorrowCalendar";
import type { CalendarEvent } from "../types";

// Reference: Zeno V2/zeno-focus.jsx CalendarDayAnchor + Zeno V2/Zeno.html
// .cal-day-anchor (line 1147) and .focus-anchor-* (line 1132). The page is
// the standalone surface; the styling mirrors the focus-modal anchor so
// the two views feel like the same thing.

const TRACK_START_HOUR = 6;
const TRACK_END_HOUR = 22;
const TRACK_HOUR_MARKS = [8, 12, 16, 20];

function isPersonal(ev: CalendarEvent): boolean {
  return ev.tag === "personal" || (ev.tag ?? "").startsWith("personal");
}

function eventHours(ev: CalendarEvent): { start: number; end: number } {
  const start = dayjs(ev.start);
  const end = dayjs(ev.end);
  const startH = start.hour() + start.minute() / 60;
  const endH = end.hour() + end.minute() / 60;
  return { start: startH, end: endH };
}

function useNowMinute(): Dayjs {
  const [now, setNow] = useState(() => dayjs());
  useEffect(() => {
    const id = window.setInterval(() => setNow(dayjs()), 60_000);
    return () => window.clearInterval(id);
  }, []);
  return now;
}

function summary(events: CalendarEvent[]): string {
  if (events.length === 0) return "Nothing scheduled.";
  const personal = events.filter(isPersonal).length;
  const evWord = events.length === 1 ? "event" : "events";
  if (personal > 0) return `${events.length} ${evWord} · ${personal} personal.`;
  return `${events.length} ${evWord}.`;
}

function HourTrack({
  events,
  now,
  showNow,
}: {
  events: CalendarEvent[];
  now: Dayjs;
  showNow: boolean;
}) {
  const span = TRACK_END_HOUR - TRACK_START_HOUR;
  const toPct = (h: number) =>
    Math.max(0, Math.min(100, ((h - TRACK_START_HOUR) / span) * 100));

  const nowH = now.hour() + now.minute() / 60;
  const nowInWindow = nowH >= TRACK_START_HOUR && nowH <= TRACK_END_HOUR;

  return (
    <div className="relative pt-5">
      {/* the track itself: 132px tall with hairline borders top + bottom */}
      <div className="relative h-[132px] border-y border-line">
        {/* hour labels positioned ABOVE the track (negative offset) */}
        {TRACK_HOUR_MARKS.map((h) => (
          <span
            key={`lbl-${h}`}
            className="absolute -translate-x-1/2 -translate-y-full font-mono text-[10px] text-ink-4 tracking-[0.04em]"
            style={{ left: `${toPct(h)}%`, top: "-2px" }}
          >
            {String(h).padStart(2, "0")}:00
          </span>
        ))}

        {/* hour grid lines (subtle) */}
        {TRACK_HOUR_MARKS.map((h) => (
          <span
            key={`tick-${h}`}
            className="absolute top-0 bottom-0 w-px bg-line"
            style={{ left: `${toPct(h)}%` }}
            aria-hidden
          />
        ))}

        {/* event boxes — top:22px, h:86px to match .cal-day-evt */}
        {events.map((ev) => {
          const { start, end } = eventHours(ev);
          if (end <= TRACK_START_HOUR || start >= TRACK_END_HOUR) return null;
          const left = toPct(start);
          const right = toPct(end);
          const width = Math.max(2.5, right - left);
          const personal = isPersonal(ev);
          return (
            <div
              key={ev.uid}
              title={`${dayjs(ev.start).format("HH:mm")} · ${ev.title}`}
              className={clsx(
                "absolute h-[86px] min-w-[88px] rounded-[4px] bg-bg-elev border border-line border-l-2",
                "px-2.5 py-2 flex flex-col gap-1 overflow-hidden cursor-pointer",
                "transition-[border-color,box-shadow] hover:border-ink-3 hover:shadow-sm",
                personal ? "border-l-amber" : "border-l-accent",
              )}
              style={{ left: `${left}%`, width: `${width}%`, top: "22px" }}
            >
              <span className="font-mono text-[10px] text-ink-3 tracking-[0.04em]">
                {dayjs(ev.start).format("HH:mm")}
              </span>
              <span className="text-[12px] text-ink leading-[1.3] truncate">
                {ev.title}
              </span>
            </div>
          );
        })}

        {/* now marker — vertical accent line, label above */}
        {showNow && nowInWindow && (
          <>
            <span
              className="absolute -translate-x-1/2 -translate-y-full font-mono text-[10px] text-accent tracking-[0.04em]"
              style={{ left: `${toPct(nowH)}%`, top: "-2px" }}
            >
              now
            </span>
            <span
              className="absolute top-0 bottom-0 w-px bg-accent z-[2]"
              style={{ left: `${toPct(nowH)}%` }}
              aria-hidden
            />
          </>
        )}
      </div>
    </div>
  );
}

function tomorrowTrailing(ev: CalendarEvent): string | null {
  // Design row is `time | title | who` (Zeno V2/zeno-focus.jsx:127–131).
  // Prefer attendees when the API supplies them; fall back to location so
  // the column is never empty for an event that has *some* context.
  const who = (ev.attendees ?? []).filter(Boolean);
  if (who.length === 1) return who[0];
  if (who.length > 1) return `${who[0]} +${who.length - 1}`;
  return ev.location ?? null;
}

function TomorrowPreview({ events }: { events: CalendarEvent[] }) {
  return (
    <section className="mt-7 flex flex-col gap-2.5">
      <span className="font-mono text-[10.5px] uppercase tracking-[0.08em] text-ink-4">
        tomorrow
      </span>
      {events.length === 0 ? (
        <p className="font-mono text-[11px] text-ink-5 py-1">Nothing scheduled.</p>
      ) : (
        <ul className="m-0 p-0 list-none flex flex-col gap-1.5">
          {events.map((ev) => {
            const trailing = tomorrowTrailing(ev);
            return (
              <li
                key={ev.uid}
                className="grid grid-cols-[56px_1fr_auto] gap-3 py-1.5 border-b border-line last:border-b-0 items-baseline text-[13px]"
              >
                <span className="font-mono text-[11px] text-ink-3 tracking-[0.04em]">
                  {dayjs(ev.start).format("HH:mm")}
                </span>
                <span className="text-ink truncate">{ev.title}</span>
                {trailing && (
                  <span className="text-[11px] text-ink-3 truncate max-w-[180px]">
                    {trailing}
                  </span>
                )}
              </li>
            );
          })}
        </ul>
      )}
    </section>
  );
}

// CalendarPage is the design's CalendarDayAnchor (Zeno V2/zeno-focus.jsx
// :72–137) — not a standalone "page". It's rendered inside CardFocus
// when the user clicks the Calendar icon in the left rail; the modal's
// own body provides the surrounding chrome.
export function CalendarPage() {
  const { data: today = [], isLoading: loadingToday } = useTodayCalendar();
  const { data: tomorrow = [] } = useTomorrowCalendar();
  const now = useNowMinute();

  return (
    <div className="flex flex-col gap-[18px]">
      <header>
        <h2 className="font-display text-[22px] font-[400] leading-[1.25] tracking-[-0.005em] text-ink m-0">
          {now.format("dddd, MMMM D")}
        </h2>
        <p className="mt-2 text-[14px] leading-[1.55] text-ink-2 m-0">
          {loadingToday ? "Loading…" : summary(today)}
        </p>
      </header>

      <HourTrack events={today} now={now} showNow />

      <TomorrowPreview events={tomorrow} />
    </div>
  );
}
