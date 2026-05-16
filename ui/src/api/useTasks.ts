import { useQuery } from "@tanstack/react-query";
import type { OpenTask } from "../types";

// useTasks fetches the full parsed tasks list (no projection cap, no
// filter to open + done-today). Backs the dedicated Tasks panel.
//
// 503 from the backend means the tasks sensor is disabled — the panel
// renders an empty state in that case rather than a hard error.
export function useTasks() {
  return useQuery<OpenTask[]>({
    queryKey: ["tasks", "all"],
    queryFn: async () => {
      const r = await fetch("/api/tasks");
      if (r.status === 503) return [];
      if (!r.ok) throw new Error(`/api/tasks returned ${r.status}`);
      return r.json();
    },
    // SSE-driven: useTodayStream applies task.* events to this cache.
    // Initial fetch on mount; afterwards event-driven.
    refetchInterval: false,
    staleTime: Infinity,
  });
}
