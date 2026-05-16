import { useQuery } from "@tanstack/react-query";
import type { WeatherView } from "../types";

export function useWeatherSnapshot() {
  return useQuery<WeatherView | null>({
    queryKey: ["weather-snapshot"],
    queryFn: async () => {
      const r = await fetch("/api/projections/weather");
      if (!r.ok) throw new Error(`/api/projections/weather returned ${r.status}`);
      const body = await r.text();
      if (body === "" || body === "null\n" || body === "null") return null;
      return JSON.parse(body) as WeatherView;
    },
    // SSE-driven: useTodayStream applies weather.updated events to this
    // cache after each weather sensor sync. Initial fetch on mount.
    refetchInterval: false,
    staleTime: Infinity,
  });
}
