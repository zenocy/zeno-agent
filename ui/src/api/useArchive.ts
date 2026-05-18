import { useQuery } from "@tanstack/react-query";
import type { CardsResponse } from "../types";

// useArchive fetches every card row the server has for the given date
// (no dismissed/snoozed/expired filters), backing the Archive page.
// The date string is "YYYY-MM-DD" in the user's timezone.
export function useArchive(date: string) {
  return useQuery<CardsResponse>({
    queryKey: ["archive", date],
    queryFn: async () => {
      const r = await fetch(`/api/cards/archive?date=${encodeURIComponent(date)}`);
      if (!r.ok) throw new Error(`/api/cards/archive returned ${r.status}`);
      return r.json();
    },
    staleTime: 60 * 1000,
  });
}
