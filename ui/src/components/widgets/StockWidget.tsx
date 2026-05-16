import { useStockSnapshot } from "../../api/useStockSnapshot";
import type { StockQuote, StockTick } from "../../types";
import { PinIcon } from "./PinIcon";

interface Props {
  onUnpin?: () => void;
}

const SPARK_W = 60;
const SPARK_H = 16;

function formatPrice(p: number, currency?: string): string {
  // tabular-nums-friendly: 2 decimals always so columns align.
  const n = p.toFixed(2);
  if (!currency || currency === "USD") return `$${n}`;
  return `${n} ${currency}`;
}

function formatChange(pct: number): string {
  const sign = pct > 0 ? "+" : "";
  return `${sign}${pct.toFixed(2)}%`;
}

function rowToneClass(quote: StockQuote): string {
  if (quote.stale) return "text-ink-4";
  if (quote.change_pct > 0) return "text-good";
  if (quote.change_pct < 0) return "text-crit";
  return "text-ink-3";
}

// sparkPath returns an SVG path string for the given series, or "" when
// there's nothing useful to draw (less than 2 points or zero variance).
function sparkPath(series: StockTick[]): string {
  if (series.length < 2) return "";
  const prices = series.map((t) => t.price);
  const max = Math.max(...prices);
  const min = Math.min(...prices);
  const span = max - min || 1;
  return prices
    .map((p, i) => {
      const x = (i / (prices.length - 1)) * SPARK_W;
      const y = SPARK_H - ((p - min) / span) * (SPARK_H - 2) - 1;
      return `${i === 0 ? "M" : "L"}${x.toFixed(1)},${y.toFixed(1)}`;
    })
    .join(" ");
}

function formatRange(low?: number, high?: number): string {
  if (!low || !high) return "";
  return `${low.toFixed(2)} — ${high.toFixed(2)}`;
}

function showAfterHours(q: StockQuote): boolean {
  return q.market_state === "POST" && (q.post_price ?? 0) > 0;
}

export function StockWidget({ onUnpin }: Props) {
  const { data, isLoading } = useStockSnapshot();

  const quotes = data?.quotes ?? [];

  return (
    <div className="group/widget relative border border-line rounded-z-md bg-bg-elev px-3.5 py-3 transition-colors hover:border-line-strong">
      <div className="flex justify-between items-start gap-1.5 mb-2.5">
        <span className="font-mono text-[10px] text-ink-4 uppercase tracking-[0.08em]">
          Markets
        </span>
        <PinIcon onClick={onUnpin} />
      </div>

      {isLoading && quotes.length === 0 && (
        <div className="font-mono text-[11px] text-ink-4">loading…</div>
      )}

      {!isLoading && quotes.length === 0 && (
        <div className="font-mono text-[11px] text-ink-4">
          add tickers in Settings
        </div>
      )}

      {quotes.length > 0 && (
        <div className="flex flex-col gap-2">
          {quotes.map((q) => {
            const path = sparkPath(q.series ?? []);
            const range = formatRange(q.day_low, q.day_high);
            return (
              <div key={q.ticker} className="flex flex-col gap-0.5">
                <div className="flex items-baseline justify-between gap-2 font-mono text-[12px]">
                  <span className="text-ink uppercase tracking-[0.05em] w-12">
                    {q.ticker}
                  </span>
                  {path ? (
                    <svg
                      viewBox={`0 0 ${SPARK_W} ${SPARK_H}`}
                      preserveAspectRatio="none"
                      className={`flex-1 h-4 ${rowToneClass(q)}`}
                      data-testid={`sparkline-${q.ticker}`}
                    >
                      <path
                        d={path}
                        fill="none"
                        stroke="currentColor"
                        strokeWidth="1.2"
                        strokeLinejoin="round"
                        strokeLinecap="round"
                        opacity="0.7"
                      />
                    </svg>
                  ) : (
                    <span className="flex-1" />
                  )}
                  <span className="w-20 text-right text-ink [font-feature-settings:'tnum'] tabular-nums">
                    {formatPrice(q.price, q.currency)}
                  </span>
                  <span
                    className={`w-16 text-right [font-feature-settings:'tnum'] tabular-nums ${rowToneClass(q)}`}
                  >
                    {formatChange(q.change_pct)}
                  </span>
                </div>
                {(range || showAfterHours(q)) && (
                  <div className="flex justify-end gap-2 font-mono text-[9.5px] text-ink-4 [font-feature-settings:'tnum'] tabular-nums">
                    {range && <span>{range}</span>}
                    {showAfterHours(q) && (
                      <span>
                        AH {q.post_price?.toFixed(2)}
                        {q.post_change_pct !== undefined && q.post_change_pct !== 0 && (
                          <span className="ml-1">{formatChange(q.post_change_pct)}</span>
                        )}
                      </span>
                    )}
                  </div>
                )}
              </div>
            );
          })}
        </div>
      )}

      {data?.quotes.some((q) => q.stale) && (
        <div className="mt-2 pt-2 border-t border-line/60 font-mono text-[9.5px] text-ink-4">
          some quotes are stale; sensor not running?
        </div>
      )}
    </div>
  );
}
