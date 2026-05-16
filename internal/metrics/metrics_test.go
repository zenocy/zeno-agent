package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestObserveLLMCall_IncrementsCountersAndHistogram(t *testing.T) {
	m := NewForTest()

	m.ObserveLLMCall("cards", "ok", 1500*time.Millisecond, 100, 50)
	m.ObserveLLMCall("cards", "ok", 2500*time.Millisecond, 200, 75)

	if got := testutil.ToFloat64(m.llmCalls.WithLabelValues("cards", "ok")); got != 2 {
		t.Fatalf("zeno_llm_calls_total{cards,ok} = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.llmTokens.WithLabelValues("cards", "prompt")); got != 300 {
		t.Fatalf("zeno_llm_tokens_total{cards,prompt} = %v, want 300", got)
	}
	if got := testutil.ToFloat64(m.llmTokens.WithLabelValues("cards", "completion")); got != 125 {
		t.Fatalf("zeno_llm_tokens_total{cards,completion} = %v, want 125", got)
	}

	snap := m.Snapshot()
	if snap.LLM["cards"].Count != 2 {
		t.Fatalf("snapshot LLM[cards].Count = %d, want 2", snap.LLM["cards"].Count)
	}
	if snap.LLM["cards"].MaxMs != 2500 {
		t.Fatalf("snapshot LLM[cards].MaxMs = %d, want 2500", snap.LLM["cards"].MaxMs)
	}
	if snap.LLM["cards"].AvgMs != 2000 {
		t.Fatalf("snapshot LLM[cards].AvgMs = %d, want 2000", snap.LLM["cards"].AvgMs)
	}
}

func TestObserveSynthRun_TracksOutcomeBuckets(t *testing.T) {
	m := NewForTest()
	m.ObserveSynthRun("cards", "ok", 25*time.Second)
	m.ObserveSynthRun("cards", "degraded", 30*time.Second)
	m.ObserveSynthRun("cards", "failed", 5*time.Second)

	snap := m.Snapshot()
	got := snap.Synth["cards"]
	if got.Runs != 3 || got.Ok != 1 || got.Degraded != 1 || got.Failed != 1 {
		t.Fatalf("synth[cards] = %+v, want runs=3 ok=1 degraded=1 failed=1", got)
	}
	if got.LastOutcome != "failed" || got.LastDurMs != 5000 {
		t.Fatalf("synth[cards] last outcome/dur = %s/%d, want failed/5000", got.LastOutcome, got.LastDurMs)
	}
}

func TestObserveSensor_AccumulatesRecords(t *testing.T) {
	m := NewForTest()
	m.ObserveSensor("imap", "ok", 800*time.Millisecond, map[string]int{"mail": 5})
	m.ObserveSensor("imap", "ok", 1200*time.Millisecond, map[string]int{"mail": 3})
	m.ObserveSensor("imap", "error", 60*time.Second, nil)

	if got := testutil.ToFloat64(m.sensorRecords.WithLabelValues("imap", "mail")); got != 8 {
		t.Fatalf("zeno_sensor_records_fetched_total{imap,mail} = %v, want 8", got)
	}
	snap := m.Snapshot()
	s := snap.Sensors["imap"]
	if s.Runs != 3 || s.Ok != 2 || s.Err != 1 {
		t.Fatalf("sensor imap stats = %+v, want runs=3 ok=2 err=1", s)
	}
	if s.Records["mail"] != 8 {
		t.Fatalf("sensor imap records[mail] = %d, want 8", s.Records["mail"])
	}
}

func TestObserveCron_TracksLastOutcome(t *testing.T) {
	m := NewForTest()
	m.ObserveCron("morning_synth", "ok")
	m.ObserveCron("morning_synth", "error")

	snap := m.Snapshot()
	got := snap.Cron["morning_synth"]
	if got.Runs != 2 || got.Ok != 1 || got.Err != 1 || got.LastOutcome != "error" {
		t.Fatalf("cron morning_synth = %+v, want runs=2 ok=1 err=1 last=error", got)
	}
}

func TestObserveHTTP_BucketsByStatusClass(t *testing.T) {
	m := NewForTest()
	m.ObserveHTTP("/api/cards/:id", 200, 50*time.Millisecond)
	m.ObserveHTTP("/api/cards/:id", 200, 50*time.Millisecond)
	m.ObserveHTTP("/api/cards/:id", 404, 20*time.Millisecond)
	m.ObserveHTTP("/api/cards/:id", 500, 1*time.Second)

	if got := testutil.ToFloat64(m.httpRequests.WithLabelValues("/api/cards/:id", "2xx")); got != 2 {
		t.Fatalf("http requests 2xx = %v, want 2", got)
	}
	snap := m.Snapshot()
	if snap.HTTP.Requests != 4 || snap.HTTP.Status2xx != 2 || snap.HTTP.Status4xx != 1 || snap.HTTP.Status5xx != 1 {
		t.Fatalf("http snap = %+v", snap.HTTP)
	}
}

func TestSetMemoryFacts_GaugeAndSnapshot(t *testing.T) {
	m := NewForTest()
	m.SetMemoryFacts(42)
	if got := testutil.ToFloat64(m.memoryFacts); got != 42 {
		t.Fatalf("memory_facts gauge = %v, want 42", got)
	}
	if snap := m.Snapshot(); snap.MemoryFacts != 42 {
		t.Fatalf("snapshot.MemoryFacts = %d, want 42", snap.MemoryFacts)
	}
}

func TestSetSSESubscribersAndDropped(t *testing.T) {
	m := NewForTest()
	m.SetSSESubscribers(3)
	m.IncSSEDropped("card.appended")
	m.IncSSEDropped("trace.step")
	m.IncSSEDropped("trace.step")

	if got := testutil.ToFloat64(m.sseSubscribers); got != 3 {
		t.Fatalf("subscribers gauge = %v, want 3", got)
	}
	if got := testutil.ToFloat64(m.sseDropped.WithLabelValues("trace.step")); got != 2 {
		t.Fatalf("sse dropped trace.step = %v, want 2", got)
	}
	snap := m.Snapshot()
	if snap.SSE.Subscribers != 3 || snap.SSE.DroppedTotal != 3 || snap.SSE.DroppedByKind["trace.step"] != 2 {
		t.Fatalf("sse snap = %+v", snap.SSE)
	}
}

func TestNz_SubsUnknownForEmpty(t *testing.T) {
	if nz("") != "unknown" {
		t.Fatal("nz(\"\") should return unknown")
	}
	if nz("imap") != "imap" {
		t.Fatal("nz(imap) should round-trip")
	}
}

func TestPrometheusHandler_ExposesZenoSeries(t *testing.T) {
	m := NewForTest()
	m.ObserveLLMCall("cards", "ok", time.Second, 10, 5)
	m.ObserveSensor("imap", "ok", time.Second, map[string]int{"mail": 1})
	m.ObserveSynthRun("cards", "ok", 25*time.Second)

	mfs, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	names := make(map[string]bool, len(mfs))
	for _, mf := range mfs {
		names[mf.GetName()] = true
	}
	for _, want := range []string{"zeno_llm_calls_total", "zeno_sensor_runs_total", "zeno_synth_runs_total"} {
		if !names[want] {
			t.Errorf("missing metric family %s in gather output", want)
		}
	}
	// Sanity: never emit "go_*" or "process_*" from NewForTest.
	for name := range names {
		if strings.HasPrefix(name, "go_") || strings.HasPrefix(name, "process_") {
			t.Errorf("test registry should not include default collector %s", name)
		}
	}
}

func TestStatusClass(t *testing.T) {
	cases := map[int]string{99: "1xx", 200: "2xx", 301: "3xx", 404: "4xx", 503: "5xx"}
	for status, want := range cases {
		if got := statusClass(status); got != want {
			t.Errorf("statusClass(%d) = %q, want %q", status, got, want)
		}
	}
}
