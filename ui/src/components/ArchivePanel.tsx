import { useState } from "react";
import dayjs from "dayjs";
import { ChevronLeft, ChevronRight } from "lucide-react";
import { useArchive } from "../api/useArchive";
import { Card } from "./Card";

// ArchivePanel is the browsable history of every card the server has
// ever produced. Pick a date and see what landed on the rail (and what
// aged out of it) for that day.
export function ArchivePanel() {
  const today = dayjs().format("YYYY-MM-DD");
  const [date, setDate] = useState<string>(today);

  const { data, isLoading, error } = useArchive(date);
  const cards = data?.cards ?? [];

  const shift = (days: number) => {
    setDate(dayjs(date).add(days, "day").format("YYYY-MM-DD"));
  };

  return (
    <div className="flex flex-col h-full">
      <div className="border-b border-line bg-bg sticky top-0 z-10">
        <div className="px-8 pt-6 pb-3 max-w-3xl mx-auto">
          <h1 className="font-mono text-[10px] uppercase tracking-wide text-ink-5">
            Archive
          </h1>
          <div className="mt-3 flex items-center gap-2">
            <button
              type="button"
              aria-label="Previous day"
              onClick={() => shift(-1)}
              className="h-7 w-7 rounded-z-sm border border-line text-ink-4 hover:text-ink-3 hover:bg-bg-elev flex items-center justify-center"
            >
              <ChevronLeft className="h-3.5 w-3.5" />
            </button>
            <input
              type="date"
              value={date}
              max={today}
              onChange={(e) => setDate(e.target.value || today)}
              className="font-mono text-[11px] bg-bg-card border border-line rounded-z-sm px-2 py-1 text-ink"
            />
            <button
              type="button"
              aria-label="Next day"
              onClick={() => shift(1)}
              disabled={date >= today}
              className="h-7 w-7 rounded-z-sm border border-line text-ink-4 hover:text-ink-3 hover:bg-bg-elev flex items-center justify-center disabled:opacity-40 disabled:cursor-not-allowed"
            >
              <ChevronRight className="h-3.5 w-3.5" />
            </button>
            {date !== today && (
              <button
                type="button"
                onClick={() => setDate(today)}
                className="font-mono text-[10px] uppercase tracking-wide text-ink-4 hover:text-ink-3 px-2 py-1"
              >
                Today
              </button>
            )}
          </div>
        </div>
      </div>

      <div className="flex-1 overflow-y-auto">
        <div className="px-8 py-6 max-w-3xl mx-auto">
          {isLoading && (
            <p className="font-mono text-[11px] text-ink-5">Loading…</p>
          )}
          {error && (
            <p className="font-mono text-[11px] text-red-500">
              {(error as Error).message}
            </p>
          )}
          {!isLoading && !error && cards.length === 0 && (
            <p className="font-mono text-[11px] text-ink-5">
              No cards for {date}.
            </p>
          )}
          {cards.length > 0 && (
            <div className="space-y-3">
              {cards.map((card) => (
                <Card key={card.id} card={card} />
              ))}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
