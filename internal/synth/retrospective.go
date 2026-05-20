package synth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"text/template"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/zenocy/zeno-v2/internal/idgen"

	"github.com/zenocy/zeno-v2/internal/eventbus"
	"github.com/zenocy/zeno-v2/internal/llm"
	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/store"
)

// V2.5.0 Phase 2 — retrospective tagging.
//
// When a concern is approved or declared, retrospective walks the
// historical observation log and asks the LLM, in batches, which
// observations belong to the concern. Tags land in the
// concern_observations join table; idempotency is free via the
// composite (concern_id, event_id) primary key. The walk is bounded
// (max-calls cap) and cancellable; progress events fire on the bus
// per batch so the review surface can show "tagging history... 47 of
// ~200" without polling.
//
// One concern at a time. Two retrospectives for the same concern are
// rejected with ErrRetrospectiveInFlight; two for different concerns
// run serially via the dispatcher. The single-flight guard lives here
// so callers (Approve handler, declare_concern tool, CLI) don't have
// to reimplement it.

// Defaults that the brief pinned at plan approval.
const (
	retrospectiveDefaultLookback = 180 * 24 * time.Hour // 6 months
	retrospectiveDefaultBatch    = 20
	retrospectiveDefaultMaxCalls = 50
)

// ErrRetrospectiveInFlight is returned by Retrospective when a prior
// retrospective on the same concern is still running. Callers should
// surface this as a 409 Conflict rather than retry.
var ErrRetrospectiveInFlight = errors.New("retrospective already in flight for concern")

// Reuse recognitionTemplateFS — both .tmpl files live in templates/ and
// recognition.go's `//go:embed templates/recognition.tmpl` does not cover
// retrospective.tmpl, so this file extends the embed via a separate
// directive below.

var (
	retrospectiveTemplateOnce sync.Once
	retrospectiveTemplate     *template.Template
	retrospectiveTemplateErr  error
)

func loadRetrospectiveTemplate() (*template.Template, error) {
	retrospectiveTemplateOnce.Do(func() {
		// Reuse recognition's embed.FS — both .tmpl files live under
		// `templates/`. The single source of truth keeps the embed list
		// short and avoids two parallel embed declarations diverging.
		b, err := recognitionTemplateFS.ReadFile("templates/retrospective.tmpl")
		if err != nil {
			retrospectiveTemplateErr = fmt.Errorf("read retrospective template: %w", err)
			return
		}
		retrospectiveTemplate, retrospectiveTemplateErr = template.New("retrospective").Parse(string(b))
	})
	return retrospectiveTemplate, retrospectiveTemplateErr
}

// retrospectiveResponseSchema constrains the model to a tagging output.
var retrospectiveResponseSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"tags": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"observation_id": map[string]any{"type": "string"},
					"tag":            map[string]any{"type": "boolean"},
					"confidence": map[string]any{
						"type":    "number",
						"minimum": 0,
						"maximum": 1,
					},
				},
				"required":             []string{"observation_id", "tag", "confidence"},
				"additionalProperties": false,
			},
		},
	},
	"required":             []string{"tags"},
	"additionalProperties": false,
}

// RetrospectiveOpts tunes one retrospective walk.
type RetrospectiveOpts struct {
	Lookback time.Duration // 0 → 180 days
	Batch    int           // 0 → 20 observations per LLM call
	MaxCalls int           // 0 → 50 (hard ceiling)
	Now      time.Time     // 0 → time.Now
	RunID    string        // 0 → uuid (only used in audit)
}

// RetrospectiveDeps wires the runner. Bus + EventLog optional.
type RetrospectiveDeps struct {
	LLM          llm.Provider
	Reader       log.Reader
	Concerns     *store.ConcernRepo
	Observations *store.ConcernObservationRepo
	Bus          *eventbus.Bus
	EventLog     log.Writer
	Logger       *logrus.Entry
}

// RetrospectiveResult is the runner's report. Tagged is the count of
// new join rows the walk produced. Calls is the total LLM calls made
// (≤ MaxCalls). Status mirrors the closed set in
// RetrospectiveProgressEvent.
type RetrospectiveResult struct {
	ConcernID string
	RunID     string
	Total     int // observations in scope
	Processed int // observations actually classified (≤ Total)
	Tagged    int // join rows created
	Calls     int // LLM calls
	StartedAt time.Time
	EndedAt   time.Time
	Status    string // "completed" | "cancelled" | "failed"
}

// retrospectiveSingleFlight is a process-wide guard keyed by concern_id.
// Use a sync.Map so the lock-free common path stays cheap.
var retrospectiveSingleFlight sync.Map // map[concernID]struct{}

func acquireRetrospectiveSlot(concernID string) bool {
	_, loaded := retrospectiveSingleFlight.LoadOrStore(concernID, struct{}{})
	return !loaded
}

func releaseRetrospectiveSlot(concernID string) {
	retrospectiveSingleFlight.Delete(concernID)
}

// Retrospective tags every historical observation that belongs to the
// concern. The concern must already exist (the caller — approve API,
// declare_concern tool — creates it first). Cancellation is honored
// at every batch boundary; the partial state is consistent because
// each batch's tags are persisted via composite-PK INSERT-OR-IGNORE.
//
// The progress contract: status="running" fires after every successful
// batch; one terminal event fires at the end with status in
// {completed, cancelled, failed}.
func Retrospective(ctx context.Context, d RetrospectiveDeps, concernID string, opts RetrospectiveOpts) (*RetrospectiveResult, error) {
	if d.Concerns == nil || d.Observations == nil || d.Reader == nil {
		return nil, errors.New("retrospective: missing required deps")
	}
	if d.LLM == nil {
		return nil, errors.New("retrospective: nil LLM client")
	}
	concern, err := d.Concerns.GetByID(ctx, concernID)
	if err != nil {
		return nil, fmt.Errorf("retrospective: load concern: %w", err)
	}
	if concern == nil {
		return nil, fmt.Errorf("retrospective: %w: %s", store.ErrConcernNotFound, concernID)
	}
	if !acquireRetrospectiveSlot(concernID) {
		return nil, ErrRetrospectiveInFlight
	}
	defer releaseRetrospectiveSlot(concernID)

	logger := d.Logger
	if logger == nil {
		logger = logrus.NewEntry(logrus.New())
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	lookback := opts.Lookback
	if lookback <= 0 {
		lookback = retrospectiveDefaultLookback
	}
	batch := opts.Batch
	if batch <= 0 {
		batch = retrospectiveDefaultBatch
	}
	maxCalls := opts.MaxCalls
	if maxCalls <= 0 {
		maxCalls = retrospectiveDefaultMaxCalls
	}
	runID := opts.RunID
	if runID == "" {
		runID = idgen.New()
	}
	logger = logger.WithField("concern_id", concernID).WithField("op", "retrospective").WithField("run_id", runID)

	res := &RetrospectiveResult{
		ConcernID: concernID,
		RunID:     runID,
		StartedAt: now,
	}

	since := now.Add(-lookback)
	mail, err := d.Reader.ByKind(ctx, log.KindMailReceived)
	if err != nil {
		return res, fmt.Errorf("retrospective: read mail: %w", err)
	}
	calSeen, err := d.Reader.ByKind(ctx, log.KindCalEventSeen)
	if err != nil {
		return res, fmt.Errorf("retrospective: read cal seen: %w", err)
	}
	calChanged, err := d.Reader.ByKind(ctx, log.KindCalEventChanged)
	if err != nil {
		return res, fmt.Errorf("retrospective: read cal changed: %w", err)
	}
	candidates := buildRecognitionObservations(since, mail, calSeen, calChanged)

	// Drop already-tagged observations (composite-PK guard does this for
	// us, but pre-filtering keeps prompts efficient and progress accurate).
	pruned := candidates[:0]
	for _, c := range candidates {
		ok, err := d.Observations.IsTaggedIncludingDeleted(ctx, concernID, c.ID)
		if err != nil {
			return res, fmt.Errorf("retrospective: tag check: %w", err)
		}
		if ok {
			continue
		}
		pruned = append(pruned, c)
	}
	res.Total = len(pruned)

	// Sort oldest-first so the operator (and the LLM) sees a chronological
	// walk; recognition's most-recent-first sort is right for "what's hot
	// now" but historical tagging reads better when the model can see the
	// arc of a concern.
	sort.SliceStable(pruned, func(i, j int) bool { return pruned[i].Date < pruned[j].Date })

	appendRetrospectiveAudit(ctx, d, concernID, runID, log.KindConcernRetrospectiveStarted, map[string]any{
		"total":      res.Total,
		"lookback":   lookback.String(),
		"batch_size": batch,
		"max_calls":  maxCalls,
	})
	publishRetrospectiveProgress(d, concernID, 0, res.Total, "running", "")

	// Empty? Complete immediately so the UI doesn't sit on "running" forever.
	if res.Total == 0 {
		res.Status = "completed"
		res.EndedAt = time.Now()
		appendRetrospectiveAudit(ctx, d, concernID, runID, log.KindConcernRetrospectiveCompleted, map[string]any{
			"total": 0, "tagged": 0, "calls": 0,
		})
		publishRetrospectiveProgress(d, concernID, 0, 0, "completed", "")
		return res, nil
	}

	tmpl, terr := loadRetrospectiveTemplate()
	if terr != nil {
		return res, terr
	}

	// ----- Batched walk -----------------------------------------------------
	for off := 0; off < len(pruned); off += batch {
		// Cancellation at the batch boundary.
		if err := ctx.Err(); err != nil {
			res.Status = "cancelled"
			res.EndedAt = time.Now()
			appendRetrospectiveAudit(ctx, d, concernID, runID, log.KindConcernRetrospectiveCancelled, map[string]any{
				"processed": res.Processed, "tagged": res.Tagged, "calls": res.Calls,
			})
			publishRetrospectiveProgress(d, concernID, res.Processed, res.Total, "cancelled", err.Error())
			return res, nil
		}
		if res.Calls >= maxCalls {
			break
		}
		end := off + batch
		if end > len(pruned) {
			end = len(pruned)
		}
		slice := pruned[off:end]

		var sysBuf bytes.Buffer
		if err := tmpl.Execute(&sysBuf, map[string]any{
			"ConcernName":        concern.Name,
			"ConcernDescription": concern.Description,
			"Items":              slice,
		}); err != nil {
			res.Status = "failed"
			res.EndedAt = time.Now()
			appendRetrospectiveAudit(ctx, d, concernID, runID, log.KindConcernRetrospectiveFailed, map[string]any{
				"reason": "render_failed", "error": err.Error(),
			})
			publishRetrospectiveProgress(d, concernID, res.Processed, res.Total, "failed", err.Error())
			return res, fmt.Errorf("retrospective: render: %w", err)
		}

		chatOpts := []llm.ChatOption{llm.WithTemperature(0.0)}
		if d.LLM.JSONSchemaEnabled() {
			chatOpts = append(chatOpts, llm.WithJSONSchema("concern_retrospective", retrospectiveResponseSchema))
		}
		out, err := d.LLM.ChatCompletion(ctx,
			[]llm.Message{
				{Role: "system", Content: sysBuf.String()},
				{Role: "user", Content: "Tag the batch above against the concern."},
			},
			nil, chatOpts...,
		)
		res.Calls++
		if err != nil {
			// Distinguish "user cancelled" from "upstream broke." A cancel
			// reaches the LLM call in flight and surfaces here as a wrapped
			// context.Canceled / DeadlineExceeded; treat as the cancelled
			// terminal state, not a failure.
			if cerr := ctx.Err(); cerr != nil {
				res.Status = "cancelled"
				res.EndedAt = time.Now()
				appendRetrospectiveAudit(ctx, d, concernID, runID, log.KindConcernRetrospectiveCancelled, map[string]any{
					"processed": res.Processed, "tagged": res.Tagged, "calls": res.Calls,
				})
				publishRetrospectiveProgress(d, concernID, res.Processed, res.Total, "cancelled", cerr.Error())
				return res, nil
			}
			res.Status = "failed"
			res.EndedAt = time.Now()
			appendRetrospectiveAudit(ctx, d, concernID, runID, log.KindConcernRetrospectiveFailed, map[string]any{
				"reason": "llm_error", "error": err.Error(),
				"processed": res.Processed, "tagged": res.Tagged, "calls": res.Calls,
			})
			publishRetrospectiveProgress(d, concernID, res.Processed, res.Total, "failed", err.Error())
			return res, fmt.Errorf("retrospective: llm: %w", err)
		}

		type wireTag struct {
			ObservationID string  `json:"observation_id"`
			Tag           bool    `json:"tag"`
			Confidence    float64 `json:"confidence"`
		}
		var w struct {
			Tags []wireTag `json:"tags"`
		}
		if err := json.Unmarshal([]byte(stripCodeFences(out.Content)), &w); err != nil {
			// Treat parse failure as a soft failure: log and skip this batch.
			logger.WithError(err).Warn("retrospective: parse failed; skipping batch")
			res.Processed += len(slice)
			publishRetrospectiveProgress(d, concernID, res.Processed, res.Total, "running", "")
			continue
		}

		// Persist tags. Confidence threshold: only tag if model said true AND
		// confidence ≥ 0.5. Below that floor the precision/recall banding
		// degrades sharply on local-model output.
		toTag := make([]store.ConcernObservation, 0, len(w.Tags))
		now2 := time.Now()
		for _, t := range w.Tags {
			if !t.Tag || t.Confidence < 0.5 {
				continue
			}
			toTag = append(toTag, store.ConcernObservation{
				ConcernID:  concernID,
				EventID:    t.ObservationID,
				Source:     store.ConcernTagSourceModel,
				Confidence: t.Confidence,
				TaggedAt:   now2,
			})
		}
		if len(toTag) > 0 {
			if err := d.Observations.TagBatch(ctx, toTag); err != nil {
				logger.WithError(err).Warn("retrospective: tag batch failed; continuing")
			} else {
				res.Tagged += len(toTag)
				if d.Bus != nil {
					ids := make([]string, 0, len(toTag))
					for _, t := range toTag {
						ids = append(ids, t.EventID)
					}
					d.Bus.Publish(eventbus.ConcernTaggedEvent{
						ConcernID:   concernID,
						EventIDs:    ids,
						Source:      store.ConcernTagSourceModel,
						BatchOrigin: "retrospective",
					})
				}
				_ = d.Concerns.BumpLastActive(ctx, concernID, now2)
			}
		}

		res.Processed += len(slice)
		publishRetrospectiveProgress(d, concernID, res.Processed, res.Total, "running", "")
	}

	res.Status = "completed"
	res.EndedAt = time.Now()
	appendRetrospectiveAudit(ctx, d, concernID, runID, log.KindConcernRetrospectiveCompleted, map[string]any{
		"total": res.Total, "processed": res.Processed, "tagged": res.Tagged, "calls": res.Calls,
	})
	publishRetrospectiveProgress(d, concernID, res.Processed, res.Total, "completed", "")
	return res, nil
}

func appendRetrospectiveAudit(ctx context.Context, d RetrospectiveDeps, concernID, runID, kind string, extra map[string]any) {
	if d.EventLog == nil {
		return
	}
	payload := map[string]any{
		"concern_id": concernID,
		"run_id":     runID,
	}
	for k, v := range extra {
		payload[k] = v
	}
	_, _ = d.EventLog.Append(ctx, kind, "synth", payload)
}

func publishRetrospectiveProgress(d RetrospectiveDeps, concernID string, processed, total int, status, errMsg string) {
	if d.Bus == nil {
		return
	}
	d.Bus.Publish(eventbus.RetrospectiveProgressEvent{
		ConcernID: concernID,
		Processed: processed,
		Total:     total,
		Status:    status,
		Error:     errMsg,
	})
}

// RetrospectiveDispatcher hides the goroutine + parent-context choice from
// the API handler and reactive tool. The dispatcher owns a long-lived
// context (typically tied to the daemon's lifetime) so a request that
// triggered the retrospective doesn't kill it when the request closes.
//
// One concrete dispatcher (defaultDispatcher) is used in production; tests
// inject a synchronous dispatcher (synchronousDispatcher) so behavior is
// deterministic.
type RetrospectiveDispatcher interface {
	Dispatch(concernID string)
}

// NewRetrospectiveDispatcher returns the default dispatcher: each call
// spawns one goroutine bound to the supplied parent context.
func NewRetrospectiveDispatcher(parent context.Context, deps RetrospectiveDeps) RetrospectiveDispatcher {
	return &goroutineDispatcher{parent: parent, deps: deps}
}

type goroutineDispatcher struct {
	parent context.Context
	deps   RetrospectiveDeps
}

func (g *goroutineDispatcher) Dispatch(concernID string) {
	go func() {
		_, err := Retrospective(g.parent, g.deps, concernID, RetrospectiveOpts{})
		if err != nil && g.deps.Logger != nil {
			g.deps.Logger.WithError(err).WithField("concern_id", concernID).
				Warn("retrospective dispatcher: run failed")
		}
	}()
}

// SynchronousRetrospectiveDispatcher runs the walk inline. Used in tests
// and by the CLI's `zeno concerns retrospective-run` command where the
// operator wants to see completion before the process exits.
func SynchronousRetrospectiveDispatcher(deps RetrospectiveDeps) RetrospectiveDispatcher {
	return &synchronousDispatcher{deps: deps}
}

type synchronousDispatcher struct {
	deps RetrospectiveDeps
}

func (s *synchronousDispatcher) Dispatch(concernID string) {
	_, _ = Retrospective(context.Background(), s.deps, concernID, RetrospectiveOpts{})
}
