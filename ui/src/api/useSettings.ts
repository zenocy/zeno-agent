import { useQuery } from "@tanstack/react-query";

export interface Settings {
  timezone: string;
  city: string;
  country: string;
  latitude: number;
  longitude: number;
  stock_tickers: string;
  stock_threshold_pct: number;
  stock_always_poll: boolean;
  world_clocks: string;
  user_name: string;
  assistant_name: string;
  assistant_tone: string;
  set: boolean;
  geocode_error?: string;
}

export function useSettings() {
  return useQuery<Settings>({
    queryKey: ["settings"],
    queryFn: async () => {
      const r = await fetch("/api/settings");
      if (!r.ok) throw new Error(`/api/settings returned ${r.status}`);
      return r.json();
    },
    // SSE-driven: useTodayStream applies settings.changed events to this
    // cache. Initial fetch on mount.
    refetchInterval: false,
    staleTime: Infinity,
  });
}
