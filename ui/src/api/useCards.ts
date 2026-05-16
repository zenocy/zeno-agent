import { useQuery } from "@tanstack/react-query";
import type { CardsResponse } from "../types";

export function useCards() {
  return useQuery<CardsResponse>({
    queryKey: ["cards"],
    queryFn: async () => {
      const r = await fetch("/api/cards");
      if (!r.ok) throw new Error(`/api/cards returned ${r.status}`);
      return r.json();
    },
    staleTime: 5 * 60 * 1000,
  });
}
