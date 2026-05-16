import { useQuery } from "@tanstack/react-query";
import type { StockView } from "../types";

export function useStockSnapshot() {
  return useQuery<StockView | null>({
    queryKey: ["stock-snapshot"],
    queryFn: async () => {
      const r = await fetch("/api/projections/stock");
      if (!r.ok) throw new Error(`/api/projections/stock returned ${r.status}`);
      const body = await r.text();
      if (body === "" || body === "null\n" || body === "null") return null;
      return JSON.parse(body) as StockView;
    },
    // SSE-driven: useTodayStream applies stock.updated events to this
    // cache after each stock sensor sync. Initial fetch on mount.
    refetchInterval: false,
    staleTime: Infinity,
  });
}
