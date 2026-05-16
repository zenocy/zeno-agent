import { useStats, type StatsSnapshot } from "../api/useStats";

// StatsPanel renders the latest stats.snapshot event from /api/metrics/snapshot.
// Mirrors ProfilePanel's typography: mono uppercase headers, table-ish rows.
// Prometheus is the source of truth for percentiles; this view is the
// in-process "what's happening right now" summary.
export function StatsPanel() {
  const { data, isLoading, error } = useStats();

  return (
    <div className="flex flex-col h-full">
      <div className="border-b border-line bg-bg sticky top-0 z-10">
        <div className="px-8 pt-6 pb-3 max-w-3xl mx-auto">
          <h1 className="font-mono text-[10px] uppercase tracking-wide text-ink-5">
            Operations
          </h1>
        </div>
      </div>

      <div className="flex-1 overflow-y-auto">
        <div className="px-8 py-6 max-w-3xl mx-auto">
          {isLoading && (
            <p className="font-mono text-[11px] text-ink-5">Loading…</p>
          )}
          {error && (
            <p className="font-mono text-[11px] text-red-500">
              {(error as Error).message}
            </p>
          )}
          {data && <Sections snap={data} />}
        </div>
      </div>
    </div>
  );
}

function Sections({ snap }: { snap: StatsSnapshot }) {
  return (
    <div className="space-y-8">
      <Section title="Uptime">
        <Row label="seconds" value={fmtInt(snap.uptime_sec)} />
        <Row label="memory facts" value={fmtInt(snap.memory_facts)} />
      </Section>

      {snap.synth && Object.keys(snap.synth).length > 0 && (
        <Section title="Synth">
          {Object.entries(snap.synth).map(([stage, s]) => (
            <Row
              key={stage}
              label={stage}
              value={`${s.last_outcome ?? "—"} · ${fmtMs(s.last_dur_ms)} · ok=${s.ok} degraded=${s.degraded} failed=${s.failed}`}
            />
          ))}
        </Section>
      )}

      {snap.sensors && Object.keys(snap.sensors).length > 0 && (
        <Section title="Sensors">
          {Object.entries(snap.sensors).map(([name, s]) => (
            <Row
              key={name}
              label={name}
              value={`${s.last_outcome ?? "—"} · ${fmtMs(s.last_dur_ms)} · ok=${s.ok} err=${s.err}${fmtRecords(s.records)}`}
            />
          ))}
        </Section>
      )}

      {snap.llm && Object.keys(snap.llm).length > 0 && (
        <Section title="LLM">
          {Object.entries(snap.llm).map(([stage, s]) => (
            <Row
              key={stage}
              label={stage}
              value={`calls=${s.count} avg=${fmtMs(s.avg_ms)} max=${fmtMs(s.max_ms)} err=${s.err}`}
            />
          ))}
          {snap.llm_retries && (
            <Row
              label="retries"
              value={Object.entries(snap.llm_retries)
                .map(([k, v]) => `${k}=${v}`)
                .join(" · ")}
            />
          )}
        </Section>
      )}

      {snap.cron && Object.keys(snap.cron).length > 0 && (
        <Section title="Cron">
          {Object.entries(snap.cron).map(([job, c]) => (
            <Row
              key={job}
              label={job}
              value={`${c.last_outcome ?? "—"} · ok=${c.ok} err=${c.err} · last ${fmtTime(c.last_at)}`}
            />
          ))}
        </Section>
      )}

      <Section title="HTTP">
        <Row
          label="requests"
          value={`total=${snap.http.requests} 2xx=${snap.http.status_2xx} 4xx=${snap.http.status_4xx} 5xx=${snap.http.status_5xx} slow=${snap.http.slow}`}
        />
      </Section>

      <Section title="SSE">
        <Row
          label="stream"
          value={`subscribers=${snap.sse.subscribers} dropped=${snap.sse.dropped_total}${fmtRecords(snap.sse.dropped_by_kind)}`}
        />
      </Section>

      {snap.db && Object.keys(snap.db).length > 0 && (
        <Section title="DB (slow queries)">
          {Object.entries(snap.db).map(([op, s]) => (
            <Row
              key={op}
              label={op}
              value={`count=${s.count} avg=${fmtMs(s.avg_ms)} max=${fmtMs(s.max_ms)}`}
            />
          ))}
        </Section>
      )}
    </div>
  );
}

function Section({
  title,
  children,
}: {
  title: string;
  children: React.ReactNode;
}) {
  return (
    <section>
      <h2 className="font-mono text-[10px] uppercase tracking-wide text-ink-4 mb-2">
        {title}
      </h2>
      <dl className="border border-line rounded-z-sm bg-bg-card divide-y divide-line">
        {children}
      </dl>
    </section>
  );
}

function Row({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-baseline gap-3 px-3 py-2 font-mono text-[11px]">
      <dt className="w-32 shrink-0 text-ink-4">{label}</dt>
      <dd className="text-ink">{value}</dd>
    </div>
  );
}

function fmtInt(n: number | undefined): string {
  if (n === undefined || n === null) return "—";
  return n.toLocaleString();
}

function fmtMs(ms: number | undefined): string {
  if (ms === undefined || ms === null || ms === 0) return "—";
  if (ms < 1000) return `${ms}ms`;
  return `${(ms / 1000).toFixed(1)}s`;
}

function fmtTime(iso: string | undefined): string {
  if (!iso) return "—";
  try {
    const d = new Date(iso);
    if (Number.isNaN(d.getTime()) || d.getTime() === 0) return "—";
    return d.toLocaleTimeString();
  } catch {
    return "—";
  }
}

function fmtRecords(rec: Record<string, number> | undefined): string {
  if (!rec || Object.keys(rec).length === 0) return "";
  const parts = Object.entries(rec).map(([k, v]) => `${k}=${v}`);
  return ` · ${parts.join(" ")}`;
}
