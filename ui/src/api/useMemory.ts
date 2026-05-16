import { useQuery } from "@tanstack/react-query";
import type { MemoryListResponse } from "../types";

export function useMemory() {
  return useQuery<MemoryListResponse>({
    queryKey: ["memory"],
    queryFn: async () => {
      const r = await fetch("/api/memory");
      if (!r.ok) throw new Error(`/api/memory returned ${r.status}`);
      return r.json();
    },
    // SSE-driven: useTodayStream applies memory.changed events (full
    // listResponse) to this cache. Initial fetch on mount.
    refetchInterval: false,
    staleTime: Infinity,
  });
}
