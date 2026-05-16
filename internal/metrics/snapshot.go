package metrics

import "time"

// Snapshot is the curated typed view of the runtime that the periodic emitter
// writes into the observation log and the UI Stats panel renders. Prometheus
// is the source of truth for percentiles; this view is a lightweight
// "what's happening right now" summary.
type Snapshot struct {
	TS          time.Time              `json:"ts"`
	UptimeSec   int64                  `json:"uptime_sec"`
	LLM         map[string]LatencySnap `json:"llm,omitempty"`
	LLMRetries  map[string]uint64      `json:"llm_retries,omitempty"`
	Synth       map[string]SynthSnap   `json:"synth,omitempty"`
	Sensors     map[string]SensorSnap  `json:"sensors,omitempty"`
	Cron        map[string]CronSnap    `json:"cron,omitempty"`
	HTTP        HTTPSnap               `json:"http"`
	SSE         SSESnap                `json:"sse"`
	DB          map[string]LatencySnap `json:"db,omitempty"`
	MemoryFacts int                    `json:"memory_facts"`
	Cards       map[string]int         `json:"cards,omitempty"`
}

// LatencySnap is a count + last/max/avg view in milliseconds.
type LatencySnap struct {
	Count    uint64 `json:"count"`
	OkCount  uint64 `json:"ok"`
	ErrCount uint64 `json:"err"`
	LastMs   int64  `json:"last_ms"`
	MaxMs    int64  `json:"max_ms"`
	AvgMs    int64  `json:"avg_ms"`
}

// SynthSnap captures the latest synth pipeline state per stage.
type SynthSnap struct {
	Runs        uint64    `json:"runs"`
	Ok          uint64    `json:"ok"`
	Degraded    uint64    `json:"degraded"`
	Failed      uint64    `json:"failed"`
	LastOutcome string    `json:"last_outcome"`
	LastDurMs   int64     `json:"last_dur_ms"`
	LastAt      time.Time `json:"last_at"`
}

// SensorSnap captures the latest sensor state per sensor name.
type SensorSnap struct {
	Runs        uint64            `json:"runs"`
	Ok          uint64            `json:"ok"`
	Err         uint64            `json:"err"`
	LastOutcome string            `json:"last_outcome"`
	LastDurMs   int64             `json:"last_dur_ms"`
	LastAt      time.Time         `json:"last_at"`
	Records     map[string]uint64 `json:"records,omitempty"`
}

// CronSnap captures the latest cron job state.
type CronSnap struct {
	Runs        uint64    `json:"runs"`
	Ok          uint64    `json:"ok"`
	Err         uint64    `json:"err"`
	LastOutcome string    `json:"last_outcome"`
	LastAt      time.Time `json:"last_at"`
}

// HTTPSnap captures aggregate HTTP request stats.
type HTTPSnap struct {
	Requests  uint64 `json:"requests"`
	Slow      uint64 `json:"slow"`
	Status2xx uint64 `json:"status_2xx"`
	Status4xx uint64 `json:"status_4xx"`
	Status5xx uint64 `json:"status_5xx"`
}

// SSESnap captures eventbus SSE state.
type SSESnap struct {
	Subscribers   int               `json:"subscribers"`
	DroppedTotal  uint64            `json:"dropped_total"`
	DroppedByKind map[string]uint64 `json:"dropped_by_kind,omitempty"`
}

// Snapshot returns a deep copy of the curated typed view.
func (m *Metrics) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now().UTC()
	out := Snapshot{
		TS:          now,
		UptimeSec:   int64(now.Sub(m.state.startedAt).Seconds()),
		MemoryFacts: m.state.memoryFacts,
		HTTP: HTTPSnap{
			Requests:  m.state.http.Requests,
			Slow:      m.state.http.Slow,
			Status2xx: m.state.http.Status2xx,
			Status4xx: m.state.http.Status4xx,
			Status5xx: m.state.http.Status5xx,
		},
		SSE: SSESnap{
			Subscribers:  m.state.sse.Subscribers,
			DroppedTotal: m.state.sse.DroppedTotal,
		},
	}
	if len(m.state.llm) > 0 {
		out.LLM = make(map[string]LatencySnap, len(m.state.llm))
		for k, v := range m.state.llm {
			out.LLM[k] = latencyToSnap(v)
		}
	}
	if len(m.state.llmRetries) > 0 {
		out.LLMRetries = make(map[string]uint64, len(m.state.llmRetries))
		for k, v := range m.state.llmRetries {
			out.LLMRetries[k] = v
		}
	}
	if len(m.state.synth) > 0 {
		out.Synth = make(map[string]SynthSnap, len(m.state.synth))
		for k, v := range m.state.synth {
			out.Synth[k] = SynthSnap{
				Runs: v.Runs, Ok: v.OkCount, Degraded: v.DegradedCnt, Failed: v.FailedCnt,
				LastOutcome: v.LastOutcome, LastDurMs: v.LastDurMs, LastAt: v.LastAt,
			}
		}
	}
	if len(m.state.sensors) > 0 {
		out.Sensors = make(map[string]SensorSnap, len(m.state.sensors))
		for k, v := range m.state.sensors {
			records := make(map[string]uint64, len(v.Records))
			for rk, rv := range v.Records {
				records[rk] = rv
			}
			out.Sensors[k] = SensorSnap{
				Runs: v.Runs, Ok: v.OkCount, Err: v.ErrCount,
				LastOutcome: v.LastOutcome, LastDurMs: v.LastDurMs, LastAt: v.LastAt,
				Records: records,
			}
		}
	}
	if len(m.state.cron) > 0 {
		out.Cron = make(map[string]CronSnap, len(m.state.cron))
		for k, v := range m.state.cron {
			out.Cron[k] = CronSnap{
				Runs: v.Runs, Ok: v.OkCount, Err: v.ErrCount,
				LastOutcome: v.LastOutcome, LastAt: v.LastAt,
			}
		}
	}
	if len(m.state.db) > 0 {
		out.DB = make(map[string]LatencySnap, len(m.state.db))
		for k, v := range m.state.db {
			out.DB[k] = latencyToSnap(v)
		}
	}
	if len(m.state.sse.DroppedByKind) > 0 {
		out.SSE.DroppedByKind = make(map[string]uint64, len(m.state.sse.DroppedByKind))
		for k, v := range m.state.sse.DroppedByKind {
			out.SSE.DroppedByKind[k] = v
		}
	}
	if len(m.state.cardsState) > 0 {
		out.Cards = make(map[string]int, len(m.state.cardsState))
		for k, v := range m.state.cardsState {
			out.Cards[k] = v
		}
	}
	return out
}

func latencyToSnap(s *latencyStats) LatencySnap {
	avg := int64(0)
	if s.Count > 0 {
		avg = s.SumMs / int64(s.Count)
	}
	return LatencySnap{
		Count: s.Count, OkCount: s.OkCount, ErrCount: s.ErrCount,
		LastMs: s.LastMs, MaxMs: s.MaxMs, AvgMs: avg,
	}
}
