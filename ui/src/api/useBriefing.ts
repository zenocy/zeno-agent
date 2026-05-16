import { useQuery } from "@tanstack/react-query";
import type { Briefing } from "../types";

export function useBriefing() {
  return useQuery<Briefing>({
    queryKey: ["briefing"],
    queryFn: async () => {
      const r = await fetch("/api/briefing/today");
      if (r.status === 404) return null as unknown as Briefing;
      if (!r.ok) throw new Error(`/api/briefing/today returned ${r.status}`);
      return r.json();
    },
    staleTime: 5 * 60 * 1000,
  });
}
