import { useQuery } from "@tanstack/react-query";
import type { CalendarEvent } from "../types";

export function useTodayCalendar() {
  return useQuery<CalendarEvent[]>({
    queryKey: ["today"],
    queryFn: async () => {
      const r = await fetch("/api/projections/calendar/today");
      if (!r.ok) throw new Error(`/api/projections/calendar/today returned ${r.status}`);
      return r.json();
    },
    // SSE-driven: useTodayStream applies calendar.today_changed events
    // to this cache after each caldav sync. Initial fetch on mount.
    refetchInterval: false,
    staleTime: Infinity,
  });
}
