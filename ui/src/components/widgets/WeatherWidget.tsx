import { useWeatherSnapshot } from "../../api/useWeatherSnapshot";
import type { WeatherDayPoint, WeatherHourPoint } from "../../types";
import { PinIcon } from "./PinIcon";

interface Props {
  onUnpin?: () => void;
}

const SPARK_W = 200;
const SPARK_H = 36;
const TICK_COUNT = 4;

function windDescriptor(kmh: number): string {
  if (kmh < 5) return "calm";
  if (kmh < 15) return "light wind";
  if (kmh < 25) return "breezy";
  return "windy";
}

function lowercaseLabel(label: string): string {
  if (!label) return "";
  return label.charAt(0).toLowerCase() + label.slice(1);
}

function pad2(n: number): string {
  return n.toString().padStart(2, "0");
}

function hourLabel(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  return pad2(d.getHours());
}

function weekdayLabel(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  return d.toLocaleDateString(undefined, { weekday: "short" });
}

function tickLabels(hourly: WeatherHourPoint[]): string[] {
  if (hourly.length === 0) return ["", "", "", ""];
  const labels: string[] = [];
  for (let i = 0; i < TICK_COUNT; i++) {
    const idx = Math.round((i / (TICK_COUNT - 1)) * (hourly.length - 1));
    labels.push(hourLabel(hourly[idx].time));
  }
  return labels;
}

export function WeatherWidget({ onUnpin }: Props) {
  const { data, isLoading } = useWeatherSnapshot();

  const hourly = data?.hourly ?? [];
  const temps = hourly.map((h) => h.temp_c);
  const max = temps.length ? Math.max(...temps) : 0;
  const min = temps.length ? Math.min(...temps) : 0;
  const span = max - min || 1;

  const path =
    temps.length > 1
      ? temps
          .map((p, i) => {
            const x = (i / (temps.length - 1)) * SPARK_W;
            const y = SPARK_H - ((p - min) / span) * (SPARK_H - 4) - 2;
            return `${i === 0 ? "M" : "L"}${x.toFixed(1)},${y.toFixed(1)}`;
          })
          .join(" ")
      : "";

  const nowI = data?.now_index ?? 0;
  const nowX = temps.length > 1 ? (nowI / (temps.length - 1)) * SPARK_W : 0;
  const nowY =
    temps.length && temps[nowI] !== undefined
      ? SPARK_H - ((temps[nowI] - min) / span) * (SPARK_H - 4) - 2
      : SPARK_H / 2;

  const location = data?.location ?? "";
  const tempC = data?.current.temp_c;
  const condition =
    data?.current
      ? `${lowercaseLabel(data.current.label)} · ${windDescriptor(data.current.wind_kmh)}`
      : "";

  const ticks = tickLabels(hourly);
  const daily: WeatherDayPoint[] = (data?.daily ?? []).slice(0, 3);

  return (
    <div className="group/widget relative border border-line rounded-z-md bg-bg-elev px-3.5 py-3 transition-colors hover:border-line-strong">
      <div className="flex justify-between items-start gap-1.5 mb-2.5">
        <div className="flex flex-col gap-0.5 min-w-0 flex-1">
          <span className="font-mono text-[10px] text-ink-4 uppercase tracking-[0.08em] whitespace-nowrap">
            {location || (isLoading ? "…" : "Weather")}
          </span>
          <div className="flex items-baseline gap-2">
            <span className="font-display text-[26px] leading-none tracking-[-0.02em] text-ink font-normal">
              {tempC === undefined ? "—" : `${Math.round(tempC)}°`}
            </span>
            <span className="font-mono text-[11px] text-ink-3 truncate">
              {condition || (isLoading ? "loading" : "no data")}
            </span>
          </div>
        </div>
        <PinIcon onClick={onUnpin} />
      </div>
      <svg
        viewBox={`0 0 ${SPARK_W} ${SPARK_H}`}
        preserveAspectRatio="none"
        className="w-full h-9 text-ink-3 block"
      >
        {path && (
          <path
            d={path}
            fill="none"
            stroke="currentColor"
            strokeWidth="1.4"
            strokeLinejoin="round"
            strokeLinecap="round"
            opacity=".4"
          />
        )}
        {temps.length > 0 && <circle cx={nowX} cy={nowY} r="3" fill="var(--accent)" />}
      </svg>
      <div className="flex justify-between text-[9.5px] text-ink-4 px-0.5 pt-1 [font-feature-settings:'tnum'] font-mono">
        {ticks.map((t, i) => (
          <span key={i}>{t}</span>
        ))}
      </div>
      {daily.length > 0 && (
        <div className="mt-2.5 pt-2 border-t border-line/60 flex flex-col gap-1">
          {daily.map((d) => (
            <div
              key={d.date}
              className="flex items-center justify-between gap-2 text-[11px] font-mono text-ink-3"
            >
              <span className="text-ink-4 uppercase tracking-[0.05em] w-9">
                {weekdayLabel(d.date)}
              </span>
              <span className="flex-1 truncate text-ink-3">
                {lowercaseLabel(d.label ?? "")}
              </span>
              <span className="text-ink [font-feature-settings:'tnum'] tabular-nums">
                {Math.round(d.temp_max_c)}°
                <span className="text-ink-4">
                  {" / "}
                  {Math.round(d.temp_min_c)}°
                </span>
              </span>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
