import { useCallback, useEffect, useState } from "react";

const STORAGE_KEY = "zeno.pinnedWidgets";
const DEFAULT_PINNED: WidgetId[] = ["weather"];

export type WidgetId = string;

function readStored(): WidgetId[] {
  if (typeof window === "undefined") return DEFAULT_PINNED;
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    if (raw === null) return DEFAULT_PINNED;
    const parsed = JSON.parse(raw);
    if (!Array.isArray(parsed)) return DEFAULT_PINNED;
    return parsed.filter((x): x is string => typeof x === "string");
  } catch {
    return DEFAULT_PINNED;
  }
}

function writeStored(ids: WidgetId[]) {
  if (typeof window === "undefined") return;
  window.localStorage.setItem(STORAGE_KEY, JSON.stringify(ids));
}

export function usePinnedWidgets() {
  const [pinned, setPinned] = useState<WidgetId[]>(() => readStored());

  useEffect(() => {
    writeStored(pinned);
  }, [pinned]);

  const isPinned = useCallback((id: WidgetId) => pinned.includes(id), [pinned]);

  const pin = useCallback((id: WidgetId) => {
    setPinned((prev) => (prev.includes(id) ? prev : [...prev, id]));
  }, []);

  const unpin = useCallback((id: WidgetId) => {
    setPinned((prev) => prev.filter((x) => x !== id));
  }, []);

  return { pinned, isPinned, pin, unpin };
}
