import { useEffect, useMemo, useState } from "react";
import { useSettings } from "../../api/useSettings";
import { PinIcon } from "./PinIcon";

interface Props {
  onUnpin?: () => void;
}

interface ClockRow {
  tz: string;
  city: string;
  abbr: string;
  time: string;
}

// cityFromIANA derives a display label from an IANA tz: take the last
// segment and replace underscores with spaces. "America/Los_Angeles" →
// "Los Angeles". For zones without a "/" (e.g. "UTC"), use the whole
// string.
function cityFromIANA(tz: string): string {
  const last = tz.includes("/") ? tz.slice(tz.lastIndexOf("/") + 1) : tz;
  return last.replace(/_/g, " ");
}

function formatRow(tz: string, now: Date): ClockRow {
  let abbr = "";
  try {
    const parts = new Intl.DateTimeFormat("en-US", {
      timeZone: tz,
      timeZoneName: "short",
    }).formatToParts(now);
    abbr = parts.find((p) => p.type === "timeZoneName")?.value ?? "";
  } catch {
    abbr = "";
  }
  let time = "";
  try {
    time = new Intl.DateTimeFormat("en-GB", {
      timeZone: tz,
      hour: "2-digit",
      minute: "2-digit",
      hour12: false,
    }).format(now);
  } catch {
    time = "—";
  }
  return { tz, city: cityFromIANA(tz), abbr, time };
}

export function WorldClockWidget({ onUnpin }: Props) {
  const { data } = useSettings();
  const [now, setNow] = useState<Date>(() => new Date());

  const zones = useMemo<string[]>(() => {
    if (!data?.world_clocks) return [];
    return data.world_clocks
      .split(",")
      .map((s) => s.trim())
      .filter(Boolean);
  }, [data?.world_clocks]);

  // Tick on each minute boundary so HH:MM stays accurate without a
  // per-second re-render. Schedule a one-shot timeout to the next
  // boundary, then a 60s interval after that.
  useEffect(() => {
    const msToNextMinute = () => {
      const d = new Date();
      return 60_000 - (d.getSeconds() * 1000 + d.getMilliseconds());
    };
    let interval: ReturnType<typeof setInterval> | null = null;
    const timeout = window.setTimeout(() => {
      setNow(new Date());
      interval = setInterval(() => setNow(new Date()), 60_000);
    }, msToNextMinute());
    return () => {
      window.clearTimeout(timeout);
      if (interval) clearInterval(interval);
    };
  }, []);

  const rows = useMemo<ClockRow[]>(
    () => zones.map((tz) => formatRow(tz, now)),
    [zones, now]
  );

  return (
    <div className="group/widget relative border border-line rounded-z-md bg-bg-elev px-3.5 py-3 transition-colors hover:border-line-strong">
      <div className="flex justify-between items-start gap-1.5 mb-2.5">
        <span className="font-mono text-[10px] text-ink-4 uppercase tracking-[0.08em]">
          World clocks
        </span>
        <PinIcon onClick={onUnpin} />
      </div>

      {rows.length === 0 && (
        <div className="font-mono text-[11px] text-ink-4">
          add timezones in Settings
        </div>
      )}

      {rows.length > 0 && (
        <div className="flex flex-col">
          {rows.map((r) => (
            <div
              key={r.tz}
              className="grid grid-cols-[1fr_auto_auto] gap-2 items-baseline py-1.5 border-t border-line/60 first:border-t-0"
            >
              <span className="font-mono text-[12px] text-ink truncate">
                {r.city}
              </span>
              <span className="font-mono text-[9.5px] text-ink-4">
                {r.abbr}
              </span>
              <span className="font-mono text-[12px] text-ink tabular-nums [font-feature-settings:'tnum']">
                {r.time}
              </span>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
