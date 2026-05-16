import { useQuery } from "@tanstack/react-query";
import type { CalendarEvent } from "../types";

export function useWeekCalendar() {
  return useQuery<CalendarEvent[]>({
    queryKey: ["week"],
    queryFn: async () => {
      const r = await fetch("/api/projections/calendar/week");
      if (!r.ok) throw new Error(`/api/projections/calendar/week returned ${r.status}`);
      return r.json();
    },
    // SSE-driven: useTodayStream applies calendar.week_changed events
    // to this cache after each caldav sync. Initial fetch on mount.
    refetchInterval: false,
    staleTime: Infinity,
  });
}
