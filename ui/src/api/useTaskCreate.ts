import { useMutation, useQueryClient } from "@tanstack/react-query";
import type { ActionResult, TaskPriority } from "../types";

export interface TaskCreateArgs {
  title: string;
  body?: string;
  due?: string;          // YYYY-MM-DD
  priority?: TaskPriority;
  tags?: string[];
  // V2.11: optional alarm. RFC3339 ("2026-05-12T18:30:00Z") or
  // relative ("+30m" / "+2h" / "+1d"). When set, the sweeper fires
  // the task as a reminder card at that moment.
  fire_at?: string;
}

export function useTaskCreate() {
  const qc = useQueryClient();
  return useMutation<ActionResult, Error, TaskCreateArgs>({
    mutationFn: async (args) => {
      const r = await fetch("/api/tasks", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(args),
      });
      if (!r.ok) {
        const body = await r.json().catch(() => ({}));
        throw new Error(body.error || `/api/tasks returned ${r.status}`);
      }
      return r.json();
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["tasks"] });
    },
  });
}
