import { useQuery } from "@tanstack/react-query";
import type { RunWindow } from "../types";

export function useRunWindow() {
  return useQuery<RunWindow>({
    queryKey: ["run-window"],
    queryFn: async () => {
      const r = await fetch("/api/projections/run-window");
      if (!r.ok) throw new Error(`/api/projections/run-window returned ${r.status}`);
      return r.json();
    },
    refetchInterval: 5 * 60 * 1000,
  });
}
