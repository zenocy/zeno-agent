import { createContext, useCallback, useContext, useEffect, useState } from "react";
import clsx from "clsx";

export type ToastTone = "info" | "error";

export interface ToastItem {
  id: number;
  message: string;
  tone: ToastTone;
}

interface Ctx {
  push: (message: string, tone?: ToastTone) => void;
}

const ToastContext = createContext<Ctx>({ push: () => {} });

export function useToast() {
  return useContext(ToastContext);
}

interface ToastProviderProps {
  children: React.ReactNode;
}

// ToastProvider mounts the toast container at the App root and exposes
// `push(message, tone)` via context. Toasts auto-dismiss after 4 s.
//
// Kept dependency-free: a small array of items and a fixed-position
// container. If we later want richer features (action buttons,
// stacking limits, per-toast TTL) drop in `sonner` or
// `react-hot-toast`.
export function ToastProvider({ children }: ToastProviderProps) {
  const [items, setItems] = useState<ToastItem[]>([]);

  const push = useCallback((message: string, tone: ToastTone = "info") => {
    if (!message) return;
    const id = Date.now() + Math.random();
    setItems((prev) => [...prev, { id, message, tone }]);
  }, []);

  // Drop each toast 4s after it appears.
  useEffect(() => {
    if (items.length === 0) return;
    const id = items[0].id;
    const t = setTimeout(() => {
      setItems((prev) => prev.filter((i) => i.id !== id));
    }, 4000);
    return () => clearTimeout(t);
  }, [items]);

  return (
    <ToastContext.Provider value={{ push }}>
      {children}
      <div className="fixed bottom-4 right-4 z-50 flex flex-col gap-2">
        {items.map((t) => (
          <div
            key={t.id}
            role="status"
            className={clsx(
              "rounded-z-sm border px-3 py-2 text-[12px] shadow-md max-w-sm",
              t.tone === "error"
                ? "bg-bg border-crit text-crit"
                : "bg-bg-elev border-line text-ink"
            )}
          >
            {t.message}
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  );
}
