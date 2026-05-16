import { useQuery } from "@tanstack/react-query";
import type { ConcernListResponse } from "../types";

export function useConcerns() {
  return useQuery<ConcernListResponse>({
    queryKey: ["concerns"],
    queryFn: async () => {
      const r = await fetch("/api/concerns");
      if (!r.ok) throw new Error(`/api/concerns returned ${r.status}`);
      return r.json();
    },
    staleTime: 10 * 60 * 1000,
  });
}
