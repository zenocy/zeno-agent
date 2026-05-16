// Package metrics is the single registration site for Prometheus collectors
// and the typed counters that back the /api/metrics/snapshot view.
//
// Callers should use the Observe* / Inc* / Set* helper functions exclusively.
// Touching the package-level collectors directly is reserved for tests, which
// can construct an isolated Metrics with NewForTest.
package metrics

import (
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Metrics holds every collector and the typed snapshot state. Production code
// uses the package-level Default(); tests can call NewForTest for isolation.
type Metrics struct {
	registry *prometheus.Registry

	// Collectors — all label sets are bounded enums.
	llmCalls       *prometheus.CounterVec   // stage,outcome
	llmDuration    *prometheus.HistogramVec // stage
	llmTokens      *prometheus.CounterVec   // stage,kind
	llmRetries     *prometheus.CounterVec   // outcome
	schemaRepairs  *prometheus.CounterVec   // stage,outcome
	toolCalls      *prometheus.CounterVec   // tool,outcome
	toolDuration   *prometheus.HistogramVec // tool
	loopIters      *prometheus.HistogramVec // stage
	synthRuns      *prometheus.CounterVec   // stage,outcome
	synthDuration  *prometheus.HistogramVec // stage
	sensorRuns     *prometheus.CounterVec   // sensor,outcome
	sensorDuration *prometheus.HistogramVec // sensor
	sensorRecords  *prometheus.CounterVec   // sensor,kind
	cronRuns       *prometheus.CounterVec   // job,outcome
	httpRequests   *prometheus.CounterVec   // route,status_class
	httpDuration   *prometheus.HistogramVec // route
	dbDuration     *prometheus.HistogramVec // op
	sseSubscribers prometheus.Gauge
	sseDropped     *prometheus.CounterVec // kind
	memoryFacts    prometheus.Gauge
	cardsState     *prometheus.GaugeVec // state

	// Typed snapshot state — bounded by enum cardinality. Mutex-protected.
	mu    sync.Mutex
	state snapshotState
}

// snapshotState carries the small typed view used by Snapshot(). Histograms
// are summarised here as count + recent average + recent max for the curated
// UI snapshot — Prometheus stays the source of truth for percentiles.
type snapshotState struct {
	startedAt   time.Time
	llm         map[string]*latencyStats // keyed by stage
	llmRetries  map[string]uint64        // outcome → count
	synth       map[string]*synthStats   // stage → stats (last outcome + last dur + counts)
	sensors     map[string]*sensorStats  // sensor name → stats
	cron        map[string]*cronStats    // job → stats
	http        *httpStats
	sse         sseStats
	db          map[string]*latencyStats // op → stats
	memoryFacts int
	cardsState  map[string]int // state → gauge
}

type latencyStats struct {
	Count    uint64
	LastMs   int64
	MaxMs    int64
	SumMs    int64 // for naive average
	OkCount  uint64
	ErrCount uint64
}

type synthStats struct {
	Runs        uint64
	LastOutcome string
	LastDurMs   int64
	LastAt      time.Time
	OkCount     uint64
	DegradedCnt uint64
	FailedCnt   uint64
}

type sensorStats struct {
	Runs        uint64
	LastOutcome string
	LastDurMs   int64
	LastAt      time.Time
	OkCount     uint64
	ErrCount    uint64
	Records     map[string]uint64 // record kind → count
}

type cronStats struct {
	Runs        uint64
	LastOutcome string
	LastAt      time.Time
	OkCount     uint64
	ErrCount    uint64
}

type httpStats struct {
	Requests    uint64
	Slow        uint64
	Status2xx   uint64
	Status4xx   uint64
	Status5xx   uint64
	LastRouteAt map[string]time.Time
}

type sseStats struct {
	Subscribers   int
	DroppedTotal  uint64
	DroppedByKind map[string]uint64
}

// New constructs a Metrics with its own registry and registers default Go /
// process collectors plus all zeno_* series. Returns an error if any collector
// fails registration (typically only on duplicate registration).
func New() (*Metrics, error) {
	reg := prometheus.NewRegistry()
	m := &Metrics{registry: reg}
	if err := m.registerCollectors(reg); err != nil {
		return nil, err
	}
	if err := reg.Register(collectors.NewGoCollector()); err != nil {
		return nil, err
	}
	if err := reg.Register(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{})); err != nil {
		return nil, err
	}
	m.state = newSnapshotState()
	return m, nil
}

// NewForTest builds a Metrics without the default Go/Process collectors so
// tests can assert exclusively against zeno_* series.
func NewForTest() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{registry: reg}
	if err := m.registerCollectors(reg); err != nil {
		panic(err)
	}
	m.state = newSnapshotState()
	return m
}

func newSnapshotState() snapshotState {
	return snapshotState{
		startedAt:  time.Now().UTC(),
		llm:        map[string]*latencyStats{},
		llmRetries: map[string]uint64{},
		synth:      map[string]*synthStats{},
		sensors:    map[string]*sensorStats{},
		cron:       map[string]*cronStats{},
		http:       &httpStats{LastRouteAt: map[string]time.Time{}},
		sse:        sseStats{DroppedByKind: map[string]uint64{}},
		db:         map[string]*latencyStats{},
		cardsState: map[string]int{},
	}
}

func (m *Metrics) registerCollectors(reg *prometheus.Registry) error {
	m.llmCalls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "zeno_llm_calls_total",
		Help: "LLM calls by synth stage and outcome.",
	}, []string{"stage", "outcome"})
	m.llmDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "zeno_llm_call_duration_seconds",
		Help:    "Wall-clock duration of one LLM call.",
		Buckets: []float64{0.5, 1, 2, 5, 10, 20, 40},
	}, []string{"stage"})
	m.llmTokens = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "zeno_llm_tokens_total",
		Help: "Total tokens consumed by stage and kind (prompt|completion).",
	}, []string{"stage", "kind"})
	m.llmRetries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "zeno_llm_retries_total",
		Help: "Outcomes of the LLM retry loop (succeeded after retry|exhausted).",
	}, []string{"outcome"})
	m.schemaRepairs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "zeno_schema_repairs_total",
		Help: "Schema-repair attempts inside the LLM tool loop.",
	}, []string{"stage", "outcome"})
	m.toolCalls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "zeno_tool_calls_total",
		Help: "Tool dispatches by tool name and outcome.",
	}, []string{"tool", "outcome"})
	m.toolDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "zeno_tool_duration_seconds",
		Help:    "Duration of a single tool dispatch.",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5},
	}, []string{"tool"})
	m.loopIters = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "zeno_loop_iterations",
		Help:    "Iterations consumed by the LLM tool loop per stage.",
		Buckets: []float64{1, 2, 3, 4, 5, 6, 8},
	}, []string{"stage"})
	m.synthRuns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "zeno_synth_runs_total",
		Help: "Synth pipeline outcomes by stage.",
	}, []string{"stage", "outcome"})
	m.synthDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "zeno_synth_duration_seconds",
		Help:    "End-to-end synth pipeline duration by stage.",
		Buckets: []float64{1, 5, 10, 20, 30, 60, 90, 120},
	}, []string{"stage"})
	m.sensorRuns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "zeno_sensor_runs_total",
		Help: "Sensor sync outcomes per sensor.",
	}, []string{"sensor", "outcome"})
	m.sensorDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "zeno_sensor_duration_seconds",
		Help:    "Sensor sync wall-clock duration.",
		Buckets: []float64{0.5, 1, 2, 5, 10, 30, 60},
	}, []string{"sensor"})
	m.sensorRecords = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "zeno_sensor_records_fetched_total",
		Help: "New records emitted by a sensor by record kind.",
	}, []string{"sensor", "kind"})
	m.cronRuns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "zeno_cron_runs_total",
		Help: "Cron job firings and outcomes.",
	}, []string{"job", "outcome"})
	m.httpRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "zeno_http_requests_total",
		Help: "HTTP requests by route template and status class (2xx|3xx|4xx|5xx).",
	}, []string{"route", "status_class"})
	m.httpDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "zeno_http_request_duration_seconds",
		Help:    "HTTP request duration by route template.",
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5},
	}, []string{"route"})
	m.dbDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "zeno_db_query_duration_seconds",
		Help:    "Slow SQLite query durations by op.",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5},
	}, []string{"op"})
	m.sseSubscribers = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "zeno_sse_subscribers",
		Help: "Currently subscribed SSE clients on the event bus.",
	})
	m.sseDropped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "zeno_sse_events_dropped_total",
		Help: "SSE events dropped because a subscriber buffer was full.",
	}, []string{"kind"})
	m.memoryFacts = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "zeno_memory_facts",
		Help: "Total derived-memory facts currently stored.",
	})
	m.cardsState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "zeno_cards_state",
		Help: "Cards count by state (open|dismissed|snoozed).",
	}, []string{"state"})

	for _, c := range []prometheus.Collector{
		m.llmCalls, m.llmDuration, m.llmTokens, m.llmRetries, m.schemaRepairs,
		m.toolCalls, m.toolDuration, m.loopIters,
		m.synthRuns, m.synthDuration,
		m.sensorRuns, m.sensorDuration, m.sensorRecords,
		m.cronRuns,
		m.httpRequests, m.httpDuration,
		m.dbDuration,
		m.sseSubscribers, m.sseDropped,
		m.memoryFacts, m.cardsState,
	} {
		if err := reg.Register(c); err != nil {
			return err
		}
	}
	return nil
}

// Registry exposes the Prometheus registry for the HTTP handler.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// ObserveLLMCall records one LLM round-trip. promptTok / completionTok are
// added to the token counter. outcome must be one of "ok", "timeout", "error".
func (m *Metrics) ObserveLLMCall(stage, outcome string, dur time.Duration, promptTok, completionTok int) {
	stage = nz(stage)
	outcome = nz(outcome)
	m.llmCalls.WithLabelValues(stage, outcome).Inc()
	m.llmDuration.WithLabelValues(stage).Observe(dur.Seconds())
	if promptTok > 0 {
		m.llmTokens.WithLabelValues(stage, "prompt").Add(float64(promptTok))
	}
	if completionTok > 0 {
		m.llmTokens.WithLabelValues(stage, "completion").Add(float64(completionTok))
	}
	m.mu.Lock()
	stats := m.state.llm[stage]
	if stats == nil {
		stats = &latencyStats{}
		m.state.llm[stage] = stats
	}
	updateLatency(stats, outcome, dur)
	m.mu.Unlock()
}

// IncRetry records a retry-loop terminal outcome ("succeeded" or "exhausted").
func (m *Metrics) IncRetry(outcome string) {
	outcome = nz(outcome)
	m.llmRetries.WithLabelValues(outcome).Inc()
	m.mu.Lock()
	m.state.llmRetries[outcome]++
	m.mu.Unlock()
}

// IncSchemaRepair records a schema-repair attempt outcome ("attempted" |
// "succeeded" | "exhausted").
func (m *Metrics) IncSchemaRepair(stage, outcome string) {
	m.schemaRepairs.WithLabelValues(nz(stage), nz(outcome)).Inc()
}

// ObserveTool records one tool dispatch.
func (m *Metrics) ObserveTool(tool, outcome string, dur time.Duration) {
	tool = nz(tool)
	outcome = nz(outcome)
	m.toolCalls.WithLabelValues(tool, outcome).Inc()
	m.toolDuration.WithLabelValues(tool).Observe(dur.Seconds())
}

// ObserveLoopIters records how many iterations the LLM loop consumed.
func (m *Metrics) ObserveLoopIters(stage string, n int) {
	if n < 0 {
		return
	}
	m.loopIters.WithLabelValues(nz(stage)).Observe(float64(n))
}

// ObserveSynthRun records one end-to-end synth pipeline run.
func (m *Metrics) ObserveSynthRun(stage, outcome string, dur time.Duration) {
	stage = nz(stage)
	outcome = nz(outcome)
	m.synthRuns.WithLabelValues(stage, outcome).Inc()
	m.synthDuration.WithLabelValues(stage).Observe(dur.Seconds())
	m.mu.Lock()
	stats := m.state.synth[stage]
	if stats == nil {
		stats = &synthStats{}
		m.state.synth[stage] = stats
	}
	stats.Runs++
	stats.LastOutcome = outcome
	stats.LastDurMs = dur.Milliseconds()
	stats.LastAt = time.Now().UTC()
	switch outcome {
	case "ok":
		stats.OkCount++
	case "degraded":
		stats.DegradedCnt++
	case "failed":
		stats.FailedCnt++
	}
	m.mu.Unlock()
}

// ObserveSensor records one sensor sync.
func (m *Metrics) ObserveSensor(sensor, outcome string, dur time.Duration, records map[string]int) {
	sensor = nz(sensor)
	outcome = nz(outcome)
	m.sensorRuns.WithLabelValues(sensor, outcome).Inc()
	m.sensorDuration.WithLabelValues(sensor).Observe(dur.Seconds())
	for kind, n := range records {
		if n <= 0 {
			continue
		}
		m.sensorRecords.WithLabelValues(sensor, nz(kind)).Add(float64(n))
	}
	m.mu.Lock()
	stats := m.state.sensors[sensor]
	if stats == nil {
		stats = &sensorStats{Records: map[string]uint64{}}
		m.state.sensors[sensor] = stats
	}
	stats.Runs++
	stats.LastOutcome = outcome
	stats.LastDurMs = dur.Milliseconds()
	stats.LastAt = time.Now().UTC()
	if outcome == "ok" {
		stats.OkCount++
	} else {
		stats.ErrCount++
	}
	for kind, n := range records {
		if n <= 0 {
			continue
		}
		stats.Records[kind] += uint64(n)
	}
	m.mu.Unlock()
}

// ObserveCron records a cron firing's outcome.
func (m *Metrics) ObserveCron(job, outcome string) {
	job = nz(job)
	outcome = nz(outcome)
	m.cronRuns.WithLabelValues(job, outcome).Inc()
	m.mu.Lock()
	stats := m.state.cron[job]
	if stats == nil {
		stats = &cronStats{}
		m.state.cron[job] = stats
	}
	stats.Runs++
	stats.LastOutcome = outcome
	stats.LastAt = time.Now().UTC()
	if outcome == "ok" {
		stats.OkCount++
	} else {
		stats.ErrCount++
	}
	m.mu.Unlock()
}

// ObserveHTTP records one HTTP request. route must be the matched template
// (Echo's c.Path()), never the raw URL.
func (m *Metrics) ObserveHTTP(route string, status int, dur time.Duration) {
	route = nz(route)
	class := statusClass(status)
	m.httpRequests.WithLabelValues(route, class).Inc()
	m.httpDuration.WithLabelValues(route).Observe(dur.Seconds())
	m.mu.Lock()
	m.state.http.Requests++
	switch class {
	case "2xx":
		m.state.http.Status2xx++
	case "4xx":
		m.state.http.Status4xx++
	case "5xx":
		m.state.http.Status5xx++
	}
	m.state.http.LastRouteAt[route] = time.Now().UTC()
	m.mu.Unlock()
}

// MarkHTTPSlow flags the most recent request as slow for the snapshot view.
func (m *Metrics) MarkHTTPSlow() {
	m.mu.Lock()
	m.state.http.Slow++
	m.mu.Unlock()
}

// ObserveDBQuery records one slow DB query (only above the threshold).
func (m *Metrics) ObserveDBQuery(op string, dur time.Duration) {
	op = nz(op)
	m.dbDuration.WithLabelValues(op).Observe(dur.Seconds())
	m.mu.Lock()
	stats := m.state.db[op]
	if stats == nil {
		stats = &latencyStats{}
		m.state.db[op] = stats
	}
	updateLatency(stats, "ok", dur)
	m.mu.Unlock()
}

// IncSSEDropped increments the dropped-event counter for a given event kind.
func (m *Metrics) IncSSEDropped(kind string) {
	kind = nz(kind)
	m.sseDropped.WithLabelValues(kind).Inc()
	m.mu.Lock()
	m.state.sse.DroppedTotal++
	m.state.sse.DroppedByKind[kind]++
	m.mu.Unlock()
}

// SetSSESubscribers updates the subscriber gauge.
func (m *Metrics) SetSSESubscribers(n int) {
	m.sseSubscribers.Set(float64(n))
	m.mu.Lock()
	m.state.sse.Subscribers = n
	m.mu.Unlock()
}

// SetMemoryFacts updates the derived-memory fact gauge.
func (m *Metrics) SetMemoryFacts(n int) {
	m.memoryFacts.Set(float64(n))
	m.mu.Lock()
	m.state.memoryFacts = n
	m.mu.Unlock()
}

// SetCardsState updates the cards-by-state gauge.
func (m *Metrics) SetCardsState(state string, n int) {
	state = nz(state)
	m.cardsState.WithLabelValues(state).Set(float64(n))
	m.mu.Lock()
	m.state.cardsState[state] = n
	m.mu.Unlock()
}

// --- Helpers ---

func updateLatency(s *latencyStats, outcome string, dur time.Duration) {
	ms := dur.Milliseconds()
	s.Count++
	s.LastMs = ms
	s.SumMs += ms
	if ms > s.MaxMs {
		s.MaxMs = ms
	}
	if outcome == "ok" {
		s.OkCount++
	} else {
		s.ErrCount++
	}
}

func statusClass(status int) string {
	switch {
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	case status >= 300:
		return "3xx"
	case status >= 200:
		return "2xx"
	}
	return "1xx"
}

// nz returns "unknown" when s is empty so collectors never see an empty label.
func nz(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// --- Default singleton ---

var (
	defaultMu sync.Mutex
	def       *Metrics
)

// Default returns the lazily-constructed package-level Metrics. Returns nil
// only if registration of the default Go/Process collectors fails, which
// indicates a programmer error (duplicate package init) rather than a runtime
// fault.
func Default() *Metrics {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	if def != nil {
		return def
	}
	m, err := New()
	if err != nil {
		// Should never happen with NewRegistry; panic so the failure is loud
		// at boot rather than silently noop'd.
		panic("metrics: default registry init: " + err.Error())
	}
	def = m
	return def
}

// ResetDefault is a test helper that drops the cached singleton so each test
// can start with a fresh registry. Not safe for concurrent use across tests.
func ResetDefault() {
	defaultMu.Lock()
	def = nil
	defaultMu.Unlock()
}

// FormatStatus is a small helper exposed for callers that want to log the
// same status_class label string.
func FormatStatus(status int) string { return statusClass(status) }

// FormatInt converts a small integer to string without pulling fmt — used by
// hot-path callers that build label values.
func FormatInt(n int) string { return strconv.Itoa(n) }
