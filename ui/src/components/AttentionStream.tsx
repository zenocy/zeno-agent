import dayjs from "dayjs";
import clsx from "clsx";
import type { CalendarEvent } from "../types";
import { useTodayCalendar } from "../api/useTodayCalendar";
import { useTomorrowCalendar } from "../api/useTomorrowCalendar";
import { useWeekCalendar } from "../api/useWeekCalendar";

const ARC_START_HOUR = 6;
const ARC_END_HOUR = 22;

function isPersonal(ev: CalendarEvent): boolean {
  return ev.tag === "personal" || (ev.tag ?? "").startsWith("personal");
}

// DayArc — tiny SVG sparkline of today's events laid out 6am→10pm.
// Ports zeno-rail.jsx's DayArc: dashed midline + per-event dot
// (work=accent, personal=amber) + a vertical "now" marker.
function DayArc({ events }: { events: CalendarEvent[] }) {
  const W = 280;
  const H = 36;
  const span = ARC_END_HOUR - ARC_START_HOUR;

  const now = dayjs();
  const nowHours = now.hour() + now.minute() / 60;
  const nowInWindow = nowHours >= ARC_START_HOUR && nowHours <= ARC_END_HOUR;
  const nowX = ((nowHours - ARC_START_HOUR) / span) * W;

  const dots = events
    .map((ev) => {
      const start = dayjs(ev.start);
      const hours = start.hour() + start.minute() / 60;
      if (hours < ARC_START_HOUR || hours > ARC_END_HOUR) return null;
      const x = ((hours - ARC_START_HOUR) / span) * W;
      return { x, personal: isPersonal(ev), uid: ev.uid };
    })
    .filter((d): d is { x: number; personal: boolean; uid: string } => d !== null);

  return (
    <div className="px-5 pb-2 pt-1">
      <svg
        viewBox={`0 0 ${W} ${H}`}
        preserveAspectRatio="none"
        className="block w-full h-9"
        aria-hidden
      >
        <line
          x1="0"
          y1={H / 2}
          x2={W}
          y2={H / 2}
          stroke="var(--line-strong)"
          strokeDasharray="2 3"
        />
        {dots.map((d) => (
          <circle
            key={d.uid}
            cx={d.x}
            cy={H / 2}
            r="4"
            fill={d.personal ? "var(--amber)" : "var(--accent)"}
          />
        ))}
        {nowInWindow && (
          <>
            <line
              x1={nowX}
              y1={2}
              x2={nowX}
              y2={H - 2}
              stroke="var(--ink)"
              strokeWidth="1.2"
            />
            <circle cx={nowX} cy={H / 2} r="2" fill="var(--ink)" />
          </>
        )}
      </svg>
      <div className="flex justify-between font-mono text-[10px] text-ink-4 mt-1 px-[2px]">
        <span>6a</span>
        <span>noon</span>
        <span>6p</span>
        <span>10p</span>
      </div>
    </div>
  );
}

// Single horizon row. The 56px time column and 1fr body match the design's
// .stream-item grid (zeno-rail.jsx). Personal vs work tag pills get
// distinct soft fills so the rail reads at a glance.
function StreamItem({ ev }: { ev: CalendarEvent }) {
  const start = dayjs(ev.start);
  const end = ev.end ? dayjs(ev.end) : null;
  const durMin = end ? Math.max(0, end.diff(start, "minute")) : 0;
  const personal = isPersonal(ev);

  return (
    <div className="grid grid-cols-[56px_1fr] gap-2.5 px-1 py-3 border-t border-line first:border-t-0 hover:bg-bg-elev rounded-[6px] transition-colors">
      <div className="flex flex-col gap-0.5 font-mono text-[11px] text-ink-3">
        <span>{start.format("HH:mm")}</span>
        {durMin > 0 && (
          <span className="text-[10px] text-ink-4">{durMin}m</span>
        )}
      </div>
      <div className="min-w-0">
        <h5 className="text-[13px] font-[500] text-ink leading-snug tracking-[-0.005em] truncate">
          {ev.title}
        </h5>
        {ev.location && (
          <p className="font-mono text-[11px] text-ink-4 mt-0.5 truncate">{ev.location}</p>
        )}
        {ev.tag && (
          <div className="mt-1.5 flex items-center gap-1.5 flex-wrap">
            <span
              className={clsx(
                "font-mono text-[10px] px-1.5 py-0.5 rounded-[4px] border",
                personal
                  ? "bg-amber-soft border-amber/30 text-amber"
                  : "bg-accent-soft border-accent/30 text-accent",
              )}
            >
              {ev.tag}
            </span>
          </div>
        )}
      </div>
    </div>
  );
}

interface HorizonProps {
  title: string;
  when: string;
  events: CalendarEvent[];
  emptyMessage?: string;
}

function Horizon({ title, when, events, emptyMessage }: HorizonProps) {
  return (
    <section className="px-5 pb-3">
      <header className="sticky top-0 z-[5] bg-bg flex justify-between items-baseline py-2.5 mb-1">
        <h3 className="font-mono text-[11px] uppercase tracking-[0.04em] text-ink-3 font-[500]">
          {title}
        </h3>
        <span className="font-mono text-[11px] text-ink-4">{when}</span>
      </header>
      {events.length === 0 ? (
        <p className="font-mono text-[11px] text-ink-5 py-1">{emptyMessage ?? "Nothing scheduled"}</p>
      ) : (
        <div>{events.map((ev) => <StreamItem key={ev.uid} ev={ev} />)}</div>
      )}
    </section>
  );
}

export function AttentionStream() {
  const { data: today = [], isLoading: loadingToday } = useTodayCalendar();
  const { data: tomorrow = [] } = useTomorrowCalendar();
  const { data: week = [] } = useWeekCalendar();
  const now = dayjs();

  if (loadingToday) {
    return (
      <div className="px-5 py-3 space-y-2">
        {[...Array(3)].map((_, i) => (
          <div key={i} className="h-10 rounded bg-ink-5 opacity-15" />
        ))}
      </div>
    );
  }

  return (
    <div className="flex flex-col">
      <DayArc events={today} />
      <Horizon
        title="Today"
        when={now.format("ddd, MMM D")}
        events={today}
      />
      <Horizon
        title="Tomorrow"
        when={now.add(1, "day").format("ddd, MMM D")}
        events={tomorrow}
      />
      <Horizon
        title="This week"
        when={`${now.add(2, "day").format("MMM D")} – ${now.add(8, "day").format("MMM D")}`}
        events={week}
      />
    </div>
  );
}
