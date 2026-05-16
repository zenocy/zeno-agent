import { useQuery } from "@tanstack/react-query";
import type { OpenTask } from "../types";

export function useOpenTasks() {
  return useQuery<OpenTask[]>({
    queryKey: ["tasks", "open"],
    queryFn: async () => {
      const r = await fetch("/api/projections/tasks/open");
      if (!r.ok) throw new Error(`/api/projections/tasks/open returned ${r.status}`);
      return r.json();
    },
    // SSE-driven: useTodayStream invalidates this key on every task.*
    // event so the projection (filtered, sorted, capped) is recomputed
    // from the server. Initial fetch on mount.
    refetchInterval: false,
    staleTime: Infinity,
  });
}
