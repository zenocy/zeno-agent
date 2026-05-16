import { useQuery } from "@tanstack/react-query";
import type { Trace } from "../types";

export function useTrace(traceId: string | undefined) {
  return useQuery<Trace>({
    queryKey: ["trace", traceId],
    queryFn: async () => {
      const r = await fetch(`/api/traces/${traceId}`);
      if (!r.ok) throw new Error(`/api/traces/${traceId} returned ${r.status}`);
      return r.json();
    },
    enabled: !!traceId,
    staleTime: 10 * 60 * 1000,
  });
}
