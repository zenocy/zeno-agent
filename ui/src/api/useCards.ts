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
    // V2.x live binding: cards carry serve-time-resolved values (weather,
    // prices, countdowns) that the server re-resolves on every fetch. Keep
    // the data fresh enough that those values stay current without a
    // manual reload — refetch on a 60s cadence and when the tab refocuses.
    staleTime: 60 * 1000,
    refetchInterval: 60 * 1000,
    refetchOnWindowFocus: true,
  });
}
