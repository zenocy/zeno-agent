import { useQuery } from "@tanstack/react-query";

export type Health = {
  ok: boolean;
  version: string;
  uptime: string;
  db_ok: boolean;
  llm_reachable: boolean;
  llm_error?: string;
};

export function useHealth() {
  return useQuery<Health>({
    queryKey: ["health"],
    queryFn: async () => {
      const r = await fetch("/api/health");
      if (!r.ok) throw new Error(`/api/health returned ${r.status}`);
      return r.json();
    },
    // SSE-driven: the server-side metrics ticker publishes
    // health.changed on transitions + 60s heartbeat. Initial fetch on
    // mount remains so the page paints with current data.
    refetchInterval: false,
    staleTime: Infinity,
  });
}
