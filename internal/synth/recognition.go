package synth

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
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

// V2.5.0 Phase 2 — daily concern recognition.
//
// Recognition is one LLM call: read recent observations (mail + calendar
// events from the durable log), ask the model to surface long-running
// situations that look like concerns. Output is filtered against
// (a) confidence threshold, (b) the 90-day dismiss denylist, (c) already
// tracked concerns by normalized name, (d) a daily cap. Surviving
// proposals are inserted as state=proposed/source=model and pre-tagged
// with the observations the model cited. Recognition fires daily via
// the scheduler (default 03:00) and on-demand via `zeno concerns
// recognition-run`.

// recognitionDenylistWindow is the period after a dismiss during which
// recognition will NOT re-propose a concern with the same normalized
// name. Mirrors the brief's "90-day denylist" decision.
const recognitionDenylistWindow = 90 * 24 * time.Hour

// recognitionDefaultLookback is the default time window the daily pass
// scans for clusters worth proposing.
const recognitionDefaultLookback = 14 * 24 * time.Hour

// recognitionDefaultDailyCap is the default ceiling on proposals per
// run. Tunable via RecognizeOpts.DailyCap. Set conservatively so the
// user is not buried in proposals on day 1.
const recognitionDefaultDailyCap = 2

// recognitionDefaultMinConfidence is the floor below which proposals
// are dropped without surfacing on the review surface.
const recognitionDefaultMinConfidence = 0.7

// recognitionMaxObservations caps the number of observations included in
// the prompt context. Larger logs are truncated to the most recent N so
// the prompt stays bounded. The post-tag classifier (separate, cheap
// embedding-cosine path) handles per-observation classification.
const recognitionMaxObservations = 80

//go:embed templates/recognition.tmpl templates/retrospective.tmpl
var recognitionTemplateFS embed.FS

var (
	recognitionTemplateOnce sync.Once
	recognitionTemplate     *template.Template
	recognitionTemplateErr  error
)

func loadRecognitionTemplate() (*template.Template, error) {
	recognitionTemplateOnce.Do(func() {
		b, err := recognitionTemplateFS.ReadFile("templates/recognition.tmpl")
		if err != nil {
			recognitionTemplateErr = fmt.Errorf("read recognition template: %w", err)
			return
		}
		recognitionTemplate, recognitionTemplateErr = template.New("recognition").Parse(string(b))
	})
	return recognitionTemplate, recognitionTemplateErr
}

// recognitionResponseSchema is the strict JSON-Schema the model is
// constrained to. Unknown keys rejected; observation_ids must be a
// string array.
var recognitionResponseSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"proposals": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type":      "string",
						"minLength": 1,
						"maxLength": 80,
					},
					"description": map[string]any{
						"type":      "string",
						"minLength": 1,
						"maxLength": 240,
					},
					"confidence": map[string]any{
						"type":    "number",
						"minimum": 0,
						"maximum": 1,
					},
					"observation_ids": map[string]any{
						"type":  "array",
						"items": map[string]any{"type": "string"},
					},
				},
				"required":             []string{"name", "description", "confidence", "observation_ids"},
				"additionalProperties": false,
			},
		},
	},
	"required":             []string{"proposals"},
	"additionalProperties": false,
}

// RecognizeOpts tunes one recognition pass.
type RecognizeOpts struct {
	Lookback       time.Duration // 0 → 14 days
	DailyCap       int           // 0 → 2
	MinConfidence  float64       // 0 → 0.7
	RunID          string        // 0 → uuid
	Now            time.Time     // 0 → time.Now
	AutoRetireDays int           // 0 → 90 (Phase 5 retirement survey threshold)
}

// RecognizeDeps carries the dependencies the runner needs. Bus and
// EventLog are optional but recommended in production: Bus drives SSE
// to the review surface; EventLog provides the audit trail for
// post-hoc analysis.
type RecognizeDeps struct {
	LLM          llm.Provider
	Reader       log.Reader
	Concerns     *store.ConcernRepo
	Observations *store.ConcernObservationRepo
	Bus          *eventbus.Bus // optional
	EventLog     log.Writer    // optional
	Logger       *logrus.Entry // optional
}

// RecognizeResult is the runner's report — proposals accepted (already
// inserted) plus any rejected with a reason. The CLI's --dry-run reads
// this without persisting.
type RecognizeResult struct {
	RunID          string
	StartedAt      time.Time
	CompletedAt    time.Time
	ObservationN   int                // number of observations the prompt saw
	RawProposals   []ProposedConcern  // what the model emitted, pre-filter
	Accepted       []AcceptedProposal // what landed in the store
	Rejected       []RejectedProposal // what didn't, with a reason
	PromptTokens   int
	ResponseTokens int
}

// ProposedConcern is the schema-bound shape the model returns.
type ProposedConcern struct {
	Name           string   `json:"name"`
	Description    string   `json:"description"`
	Confidence     float64  `json:"confidence"`
	ObservationIDs []string `json:"observation_ids"`
}

// AcceptedProposal is the post-persist record: the new concern's ID
// plus what the model said. Bus + audit events fire only for accepted.
type AcceptedProposal struct {
	ConcernID string
	Proposed  ProposedConcern
}

// RejectedProposal carries one reason a proposal didn't survive.
// Reasons (closed set) are exposed so the eval rubric can score them.
type RejectedProposal struct {
	Proposed ProposedConcern
	Reason   string // "low_confidence" | "duplicate_norm_name" | "denylisted" | "daily_cap"
}

// recognitionPromptObs is one observation as the prompt template sees it.
type recognitionPromptObs struct {
	ID      string
	Date    string
	Kind    string
	Title   string
	Sender  string
	Preview string
}

type recognitionPromptData struct {
	Today         string
	LookbackDays  int
	DailyCap      int
	Observations  []recognitionPromptObs
	ExistingNames []string
	DenylistNames []string
}

type mailPayloadView struct {
	Folder      string    `json:"folder"`
	UID         uint32    `json:"uid"`
	UIDValidity uint32    `json:"uidvalidity"`
	From        string    `json:"from"`
	Subject     string    `json:"subject"`
	Date        time.Time `json:"date"`
	BodyPreview string    `json:"body_preview"`
}

type calPayloadView struct {
	UID   string    `json:"uid"`
	Title string    `json:"title"`
	Tag   string    `json:"tag"`
	Start time.Time `json:"start"`
}

// Recognize runs one recognition pass. The deps must include Concerns,
// Observations, and Reader; LLM is required for the call itself; Bus
// and EventLog are optional. Errors fall into three buckets:
//   - reader/store I/O: returned as-is, no retry, no audit
//   - LLM call failure: returned as-is, no audit (caller may retry)
//   - response parse failure: returned, audit "recognition_run_failed"
//
// On success a single KindConcernRecognitionRun event lands in the
// durable log with a count summary; per-accepted ConcernProposedEvent
// + ConcernTaggedEvent fire on the bus.
func Recognize(ctx context.Context, d RecognizeDeps, opts RecognizeOpts) (*RecognizeResult, error) {
	if d.Concerns == nil || d.Observations == nil || d.Reader == nil {
		return nil, errors.New("recognize: missing required deps")
	}
	if d.LLM == nil {
		return nil, errors.New("recognize: nil LLM client")
	}
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
		lookback = recognitionDefaultLookback
	}
	cap := opts.DailyCap
	if cap <= 0 {
		cap = recognitionDefaultDailyCap
	}
	minConf := opts.MinConfidence
	if minConf <= 0 {
		minConf = recognitionDefaultMinConfidence
	}
	runID := opts.RunID
	if runID == "" {
		runID = idgen.New()
	}
	logger = logger.WithField("run_id", runID).WithField("op", "recognition")

	result := &RecognizeResult{RunID: runID, StartedAt: now}

	// ----- Read observations (mail + calendar) within lookback ---------------
	since := now.Add(-lookback)
	mail, err := d.Reader.ByKind(ctx, log.KindMailReceived)
	if err != nil {
		return nil, fmt.Errorf("recognize: read mail: %w", err)
	}
	calSeen, err := d.Reader.ByKind(ctx, log.KindCalEventSeen)
	if err != nil {
		return nil, fmt.Errorf("recognize: read cal seen: %w", err)
	}
	calChanged, err := d.Reader.ByKind(ctx, log.KindCalEventChanged)
	if err != nil {
		return nil, fmt.Errorf("recognize: read cal changed: %w", err)
	}
	obs := buildRecognitionObservations(since, mail, calSeen, calChanged)
	result.ObservationN = len(obs)

	// ----- Pre-prompt dedupe + denylist sets --------------------------------
	existing, err := d.Concerns.ListAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("recognize: list existing: %w", err)
	}
	existingNorm := make(map[string]struct{}, len(existing))
	existingNames := make([]string, 0, len(existing))
	for _, c := range existing {
		// Skip terminal-merged so a merged tombstone doesn't crowd the prompt.
		if c.State == store.ConcernStateMerged || c.State == store.ConcernStateEnded {
			continue
		}
		existingNorm[c.NormName] = struct{}{}
		existingNames = append(existingNames, c.Name)
	}

	// ----- Empty observations: skip LLM entirely ----------------------------
	if len(obs) == 0 {
		result.CompletedAt = now
		appendRecognitionAudit(ctx, d, runID, now, 0, 0, 0, "skipped_no_observations")
		return result, nil
	}

	// ----- Render prompt + call LLM ----------------------------------------
	tmpl, terr := loadRecognitionTemplate()
	if terr != nil {
		return nil, terr
	}

	pdata := recognitionPromptData{
		Today:         now.Format("2006-01-02"),
		LookbackDays:  int(lookback / (24 * time.Hour)),
		DailyCap:      cap,
		Observations:  obs,
		ExistingNames: existingNames,
	}
	var sysBuf bytes.Buffer
	if err := tmpl.Execute(&sysBuf, pdata); err != nil {
		return nil, fmt.Errorf("recognize: render prompt: %w", err)
	}

	chatOpts := []llm.ChatOption{llm.WithTemperature(0.0)}
	if d.LLM.JSONSchemaEnabled() {
		chatOpts = append(chatOpts, llm.WithJSONSchema("concern_recognition", recognitionResponseSchema))
	}

	res, err := d.LLM.ChatCompletion(ctx,
		[]llm.Message{
			{Role: "system", Content: sysBuf.String()},
			{Role: "user", Content: "Surface concerns from the observations above."},
		},
		nil,
		chatOpts...,
	)
	if err != nil {
		return nil, fmt.Errorf("recognize: llm: %w", err)
	}
	result.PromptTokens = res.PromptTokens
	result.ResponseTokens = res.CompletionTokens

	if strings.TrimSpace(res.Content) == "" {
		// Empty: model declined. Audit but don't fail.
		result.CompletedAt = time.Now()
		appendRecognitionAudit(ctx, d, runID, now, len(obs), 0, 0, "empty_response")
		return result, nil
	}

	type wire struct {
		Proposals []ProposedConcern `json:"proposals"`
	}
	var w wire
	if err := json.Unmarshal([]byte(stripCodeFences(res.Content)), &w); err != nil {
		appendRecognitionAudit(ctx, d, runID, now, len(obs), 0, 0, "parse_failed")
		return nil, fmt.Errorf("recognize: parse response: %w", err)
	}
	result.RawProposals = w.Proposals

	// ----- Filter + persist surviving proposals -----------------------------
	denylist := loadRecognitionDenylist(ctx, d, now)
	accepted := make([]AcceptedProposal, 0, cap)
	rejected := make([]RejectedProposal, 0)

	for _, p := range w.Proposals {
		if len(accepted) >= cap {
			rejected = append(rejected, RejectedProposal{Proposed: p, Reason: "daily_cap"})
			continue
		}
		if p.Confidence < minConf {
			rejected = append(rejected, RejectedProposal{Proposed: p, Reason: "low_confidence"})
			continue
		}
		norm := store.NormalizeConcernName(p.Name)
		if _, dup := existingNorm[norm]; dup {
			rejected = append(rejected, RejectedProposal{Proposed: p, Reason: "duplicate_norm_name"})
			continue
		}
		if _, dl := denylist[norm]; dl {
			rejected = append(rejected, RejectedProposal{Proposed: p, Reason: "denylisted"})
			continue
		}
		// Persist.
		c := store.Concern{
			ID:             idgen.New(),
			Name:           p.Name,
			NormName:       norm,
			Description:    p.Description,
			State:          store.ConcernStateProposed,
			Source:         store.ConcernSourceModel,
			Confidence:     p.Confidence,
			LastActiveAt:   now,
			FirstSeenRunID: runID,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		if err := d.Concerns.Insert(ctx, c); err != nil {
			logger.WithError(err).Warn("recognize: insert concern failed; skipping")
			rejected = append(rejected, RejectedProposal{Proposed: p, Reason: "insert_failed"})
			continue
		}
		// Pre-tag the observations the model cited; ignore unknown IDs.
		if len(p.ObservationIDs) > 0 {
			tags := make([]store.ConcernObservation, 0, len(p.ObservationIDs))
			for _, oid := range p.ObservationIDs {
				tags = append(tags, store.ConcernObservation{
					ConcernID:  c.ID,
					EventID:    oid,
					Source:     store.ConcernTagSourceModel,
					Confidence: p.Confidence,
					TaggedAt:   now,
				})
			}
			if err := d.Observations.TagBatch(ctx, tags); err != nil {
				logger.WithError(err).Warn("recognize: tag pre-attach failed")
			}
		}
		// Existing-norm bookkeeping prevents this same prompt's later proposals
		// from claiming the same normalized name on the second LLM go-round.
		existingNorm[norm] = struct{}{}
		accepted = append(accepted, AcceptedProposal{ConcernID: c.ID, Proposed: p})
		publishRecognitionEvents(d, c, p, now)
	}
	result.Accepted = accepted
	result.Rejected = rejected
	result.CompletedAt = time.Now()

	appendRecognitionAudit(ctx, d, runID, now, len(obs), len(accepted), len(w.Proposals), "ok")

	// V2.5.0 Phase 5 — retirement survey. After recognition completes,
	// scan active concerns whose last_active_at crosses the retirement
	// threshold and emit one audit + bus event per newly-crossed
	// concern. Idempotent: a concern that already has a
	// KindConcernRetirementProposed in the audit log is skipped until it
	// re-engages (which will reset last_active_at and put it back below
	// the threshold).
	surveyForRetirement(ctx, d, now, opts.AutoRetireDays, logger)

	return result, nil
}

// recognitionDefaultAutoRetireDays mirrors config.ConcernsConfig.AutoRetireDays
// and projection.DefaultAutoRetireDays. All three paths must agree on
// what "inactive" means — a divergence between the survey threshold,
// the DTO badge, and the config knob would surface as a phantom note.
const recognitionDefaultAutoRetireDays = 90

// surveyForRetirement scans active concerns whose last_active_at is older
// than the threshold and emits a retirement-proposed audit + bus event
// for each one not already proposed. Best-effort: I/O errors are logged
// but never propagate to the recognition caller (recognition's primary
// job is proposal generation; retirement is a side pass).
func surveyForRetirement(ctx context.Context, d RecognizeDeps, now time.Time, daysOpt int, logger *logrus.Entry) {
	if d.Concerns == nil {
		return
	}
	days := daysOpt
	if days <= 0 {
		days = recognitionDefaultAutoRetireDays
	}
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)

	rows, err := d.Concerns.ListInactiveSince(ctx, cutoff)
	if err != nil {
		if logger != nil {
			logger.WithError(err).Warn("retirement survey: list inactive failed")
		}
		return
	}
	if len(rows) == 0 {
		return
	}

	already, err := loadRetirementProposedSet(ctx, d, now, days)
	if err != nil && logger != nil {
		logger.WithError(err).Debug("retirement survey: read prior audit failed; treating set as empty")
	}

	for _, c := range rows {
		if _, seen := already[c.ID]; seen {
			continue
		}
		daysInactive := int(now.Sub(c.LastActiveAt) / (24 * time.Hour))
		if d.EventLog != nil {
			_, _ = d.EventLog.Append(ctx, log.KindConcernRetirementProposed, "synth", map[string]any{
				"concern_id":     c.ID,
				"last_active_at": c.LastActiveAt,
				"days_inactive":  daysInactive,
			})
		}
		if d.Bus != nil {
			d.Bus.Publish(eventbus.ConcernRetirementProposedEvent{
				ConcernID:    c.ID,
				DaysInactive: daysInactive,
			})
		}
	}
}

// loadRetirementProposedSet returns the set of concern IDs that have
// already received a retirement proposal since the threshold's start
// (now - daysWindow). The window matches the threshold so a concern
// that re-engages and then goes idle again gets a fresh proposal.
func loadRetirementProposedSet(ctx context.Context, d RecognizeDeps, now time.Time, daysWindow int) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	if d.Reader == nil {
		return out, nil
	}
	events, err := d.Reader.ByKind(ctx, log.KindConcernRetirementProposed)
	if err != nil {
		return out, err
	}
	since := now.Add(-time.Duration(daysWindow) * 24 * time.Hour)
	for _, e := range events {
		if e.TS.Before(since) {
			continue
		}
		var p struct {
			ConcernID string `json:"concern_id"`
		}
		_ = json.Unmarshal(e.Payload, &p)
		if p.ConcernID != "" {
			out[p.ConcernID] = struct{}{}
		}
	}
	return out, nil
}

// buildRecognitionObservations maps log events within the lookback window
// to recognitionPromptObs. Mail events use the IMAP folder/UID as the
// observation ID so the model's `observation_ids` correlate with what
// retrospective tagging will see. Calendar events use the VEVENT UID.
func buildRecognitionObservations(since time.Time, mail, calSeen, calChanged []log.Event) []recognitionPromptObs {
	out := make([]recognitionPromptObs, 0, len(mail)+len(calSeen)+len(calChanged))
	for _, e := range mail {
		if e.TS.Before(since) {
			continue
		}
		var p mailPayloadView
		_ = json.Unmarshal(e.Payload, &p)
		title := strings.TrimSpace(p.Subject)
		if title == "" {
			title = "(no subject)"
		}
		out = append(out, recognitionPromptObs{
			ID:      eventObservationID(e),
			Date:    e.TS.Format("2006-01-02"),
			Kind:    "mail",
			Title:   title,
			Sender:  p.From,
			Preview: collapseWhitespace(p.BodyPreview, 160),
		})
	}
	addCal := func(events []log.Event, kind string) {
		for _, e := range events {
			if e.TS.Before(since) {
				continue
			}
			var p calPayloadView
			_ = json.Unmarshal(e.Payload, &p)
			title := strings.TrimSpace(p.Title)
			if title == "" {
				title = "(no title)"
			}
			out = append(out, recognitionPromptObs{
				ID:    e.ID,
				Date:  e.TS.Format("2006-01-02"),
				Kind:  kind,
				Title: title,
			})
		}
	}
	addCal(calSeen, "cal")
	addCal(calChanged, "cal_changed")

	// Most recent first; truncate to bound the prompt.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Date > out[j].Date })
	if len(out) > recognitionMaxObservations {
		out = out[:recognitionMaxObservations]
	}
	return out
}

// eventObservationID returns a stable ID we expose to the model. For
// mail we prefer the log Event.ID (the durable handle); fixtures and
// other code paths may use synthetic IDs (e.g. "thread:<subject>") and
// that's fine — tagging is by event_id string.
func eventObservationID(e log.Event) string {
	if e.ID != "" {
		return e.ID
	}
	return ""
}

// loadRecognitionDenylist fetches soft-deleted concerns whose deleted_at
// is within the denylist window and returns a set of their normalized
// names. Recognition skips re-proposing any of these.
func loadRecognitionDenylist(ctx context.Context, d RecognizeDeps, now time.Time) map[string]struct{} {
	out := map[string]struct{}{}
	if d.Concerns == nil {
		return out
	}
	// We don't have a dedicated "list soft-deleted within window" repo
	// method; do a single Unscoped query directly. This is the only
	// place that needs it and it's not in the hot path.
	type row struct {
		NormName  string
		DeletedAt time.Time
	}
	var rows []row
	tbl := d.Concerns.TableName()
	if err := d.Concerns.DB.WithContext(ctx).Table(tbl).Unscoped().
		Select("norm_name, deleted_at").
		Where("deleted_at IS NOT NULL AND deleted_at >= ?", now.Add(-recognitionDenylistWindow)).
		Scan(&rows).Error; err != nil {
		// Best-effort; an empty denylist is the safe default.
		return out
	}
	for _, r := range rows {
		out[r.NormName] = struct{}{}
	}
	return out
}

func publishRecognitionEvents(d RecognizeDeps, c store.Concern, p ProposedConcern, now time.Time) {
	if d.Bus != nil {
		d.Bus.Publish(eventbus.ConcernProposedEvent{
			ConcernID:   c.ID,
			Name:        c.Name,
			Description: c.Description,
			Source:      c.Source,
			Confidence:  c.Confidence,
		})
		if len(p.ObservationIDs) > 0 {
			d.Bus.Publish(eventbus.ConcernTaggedEvent{
				ConcernID:   c.ID,
				EventIDs:    p.ObservationIDs,
				Source:      store.ConcernTagSourceModel,
				BatchOrigin: "recognition",
			})
		}
	}
}

func appendRecognitionAudit(ctx context.Context, d RecognizeDeps, runID string, now time.Time, observed, accepted, raw int, status string) {
	if d.EventLog == nil {
		return
	}
	_, _ = d.EventLog.Append(ctx, log.KindConcernRecognitionRun, "synth", map[string]any{
		"run_id":        runID,
		"started_at":    now,
		"observation_n": observed,
		"raw_proposals": raw,
		"accepted":      accepted,
		"status":        status,
	})
}

// collapseWhitespace folds runs of whitespace to a single space and
// truncates at max characters with an ellipsis. Used to keep the prompt
// stable when previews carry awkward whitespace.
func collapseWhitespace(s string, max int) string {
	cleaned := strings.Join(strings.Fields(s), " ")
	if len(cleaned) > max {
		cleaned = cleaned[:max] + "…"
	}
	return cleaned
}
