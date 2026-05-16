import { Plus } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { WIDGET_CATALOG } from "./registry";
import type { WidgetId } from "../../hooks/usePinnedWidgets";

interface Props {
  pinned: WidgetId[];
  onPin: (id: WidgetId) => void;
}

export function AddWidgetButton({ pinned, onPin }: Props) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    const onDoc = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", onDoc);
    return () => document.removeEventListener("mousedown", onDoc);
  }, [open]);

  const available = WIDGET_CATALOG.filter((w) => !pinned.includes(w.id));

  return (
    <div ref={ref} className="relative">
      <button
        type="button"
        title="Add a widget"
        aria-label="Add a widget"
        onClick={() => setOpen((v) => !v)}
        disabled={available.length === 0}
        className="bg-transparent border-0 cursor-pointer text-ink-4 w-[22px] h-[22px] grid place-items-center rounded-[4px] hover:bg-bg-elev hover:text-ink transition-colors disabled:opacity-30 disabled:cursor-not-allowed"
      >
        <Plus className="h-3.5 w-3.5" />
      </button>
      {open && available.length > 0 && (
        <div className="absolute right-0 top-[26px] z-10 min-w-[140px] rounded-z-sm border border-line bg-bg-card shadow-md py-1">
          {available.map((w) => (
            <button
              key={w.id}
              type="button"
              onClick={() => {
                onPin(w.id);
                setOpen(false);
              }}
              className="w-full text-left px-3 py-1.5 text-[12px] text-ink-2 hover:bg-bg-elev"
            >
              {w.label}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}
