import { useQuery } from "@tanstack/react-query";

export interface LatencySnap {
  count: number;
  ok: number;
  err: number;
  last_ms: number;
  max_ms: number;
  avg_ms: number;
}

export interface SynthSnap {
  runs: number;
  ok: number;
  degraded: number;
  failed: number;
  last_outcome: string;
  last_dur_ms: number;
  last_at: string;
}

export interface SensorSnap {
  runs: number;
  ok: number;
  err: number;
  last_outcome: string;
  last_dur_ms: number;
  last_at: string;
  records?: Record<string, number>;
}

export interface CronSnap {
  runs: number;
  ok: number;
  err: number;
  last_outcome: string;
  last_at: string;
}

export interface HTTPSnap {
  requests: number;
  slow: number;
  status_2xx: number;
  status_4xx: number;
  status_5xx: number;
}

export interface SSESnap {
  subscribers: number;
  dropped_total: number;
  dropped_by_kind?: Record<string, number>;
}

export interface StatsSnapshot {
  ts: string;
  uptime_sec: number;
  llm?: Record<string, LatencySnap>;
  llm_retries?: Record<string, number>;
  synth?: Record<string, SynthSnap>;
  sensors?: Record<string, SensorSnap>;
  cron?: Record<string, CronSnap>;
  http: HTTPSnap;
  sse: SSESnap;
  db?: Record<string, LatencySnap>;
  memory_facts: number;
  cards?: Record<string, number>;
}

export function useStats() {
  return useQuery<StatsSnapshot>({
    queryKey: ["stats", "snapshot"],
    queryFn: async () => {
      const r = await fetch("/api/metrics/snapshot");
      if (!r.ok) throw new Error(`/api/metrics/snapshot returned ${r.status}`);
      return r.json();
    },
    // SSE-driven: a server-side 10s ticker publishes stats.snapshot to
    // every connected SSE subscriber; useTodayStream writes them into
    // this cache. Initial fetch on mount.
    refetchInterval: false,
    staleTime: Infinity,
  });
}
