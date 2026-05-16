import { useQuery } from "@tanstack/react-query";
import type { CalendarEvent } from "../types";

export function useTomorrowCalendar() {
  return useQuery<CalendarEvent[]>({
    queryKey: ["tomorrow"],
    queryFn: async () => {
      const r = await fetch("/api/projections/calendar/tomorrow");
      if (!r.ok) throw new Error(`/api/projections/calendar/tomorrow returned ${r.status}`);
      return r.json();
    },
    // SSE-driven: useTodayStream applies calendar.tomorrow_changed events
    // to this cache after each caldav sync. Initial fetch on mount.
    refetchInterval: false,
    staleTime: Infinity,
  });
}
