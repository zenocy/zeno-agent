// useConcernRetrospectiveProgress — subscribes to the broker's
// concern.retrospective_progress events and exposes a per-concern
// progress map for the review surface.
//
// Entries with status="complete" or "failed" are auto-pruned after
// CompleteFadeMs so the row's progress slot disappears cleanly without
// the panel needing to track expiration timestamps itself.

import { useEffect, useState } from "react";

import type { RetrospectiveProgress } from "../types";
import { subscribeLive, type LiveEvent } from "./liveBroker";

// CompleteFadeMs is the grace window between status="complete" and the
// row losing its progress text. 3 seconds is long enough for the user
// to see "tagging history… complete" without lingering forever.
export const CompleteFadeMs = 3000;

export type ProgressMap = Record<string, RetrospectiveProgress>;

export function useConcernRetrospectiveProgress(): ProgressMap {
  const [progress, setProgress] = useState<ProgressMap>({});

  useEffect(() => {
    const fadeTimers = new Map<string, ReturnType<typeof setTimeout>>();

    const cancelFade = (id: string) => {
      const t = fadeTimers.get(id);
      if (t) {
        clearTimeout(t);
        fadeTimers.delete(id);
      }
    };

    const unsub = subscribeLive((ev: LiveEvent) => {
      if (ev.kind !== "concern.retrospective_progress") return;
      const id = ev.concern_id;
      const next: RetrospectiveProgress = {
        concern_id: id,
        processed: ev.processed,
        total: ev.total,
        status: ev.status,
        error: ev.error,
      };
      // A fresh "running" event invalidates any pending fade — the
      // dispatcher resumed work for this concern.
      cancelFade(id);
      setProgress((m) => ({ ...m, [id]: next }));
      if (ev.status === "completed" || ev.status === "cancelled" || ev.status === "failed") {
        const t = setTimeout(() => {
          setProgress((m) => {
            const { [id]: _, ...rest } = m;
            return rest;
          });
          fadeTimers.delete(id);
        }, CompleteFadeMs);
        fadeTimers.set(id, t);
      }
    });

    return () => {
      unsub();
      for (const t of fadeTimers.values()) clearTimeout(t);
      fadeTimers.clear();
    };
  }, []);

  return progress;
}
