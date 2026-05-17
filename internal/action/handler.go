// Package action handles user-initiated card actions.
//
// V2.8.0 rewrote the dispatch path: the pre-V2.8 string-match
// switch on "dismiss"/"snooze"/<everything-else> is replaced by a
// typed Executor registry (registry.go) where each Intent has its
// own Mode and side-effect contract. The handler is now a thin
// shim that parses the request, resolves the Card, looks up the
// Executor, runs it, and serializes the Result.
//
// Backward-compatible wire shape: legacy clients send
// {"action": "<label>"} and rely on lowercase substring routing
// for dismiss/snooze. The handler folds Action into Intent when
// Intent is empty (label-to-intent inference reuses the same
// table synth.postProcessIntent uses for stored cards), so the
// pre-V2.8 UI keeps working unchanged across the V2.8 phase rollout.
package action

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"

	logp "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/store"
	"github.com/zenocy/zeno-v2/internal/synth"
)

// Handler answers POST /api/cards/:id/action.
//
// TZ is a getter so timezone edits from the Settings UI take effect on
// the next request without a process restart.
type Handler struct {
	Cards    *store.CardRepo
	Registry *Registry
	EventLog logp.Writer
	TZ       func() *time.Location
	Now      func() time.Time
	Log      *logrus.Entry
}

// Register attaches the action route to the Echo instance.
func (h *Handler) Register(e *echo.Echo) {
	e.POST("/api/cards/:id/action", h.action)
}

func (h *Handler) tz() *time.Location {
	if h.TZ == nil {
		return time.UTC
	}
	if loc := h.TZ(); loc != nil {
		return loc
	}
	return time.UTC
}

type actionRequest struct {
	Intent  string         `json:"intent"`
	Action  string         `json:"action,omitempty"` // legacy alias; folded into Intent when Intent is empty
	Target  map[string]any `json:"target,omitempty"`
	Confirm bool           `json:"confirm,omitempty"`
	// Payload preserves the pre-V2.8 escape hatch where the UI
	// could attach arbitrary JSON to a "custom action". New clients
	// should populate Target instead.
	Payload map[string]any `json:"payload,omitempty"`
}

func (h *Handler) action(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "card id is required"})
	}

	var req actionRequest
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}

	intent := strings.TrimSpace(req.Intent)
	if intent == "" {
		intent = inferIntentForLabel(req.Action)
	}
	if intent == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "action or intent is required"})
	}

	target := req.Target
	if target == nil && req.Payload != nil {
		target = req.Payload
	}

	ctx := c.Request().Context()

	card, err := h.Cards.GetByID(ctx, id)
	if err != nil {
		if h.Log != nil {
			h.Log.WithError(err).WithField("card_id", id).Error("action: get card failed")
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	result, _ := h.DispatchIntent(ctx, DispatchInput{
		Intent:       intent,
		Card:         card,
		CardID:       id,
		Target:       target,
		Confirm:      req.Confirm,
		LegacyAction: legacyActionString(req, intent),
	})

	return c.JSON(http.StatusOK, result)
}

// DispatchInput is the parameter bag for DispatchIntent. CardID is the
// audit-row identifier and may be empty for non-card-bound dispatches
// (e.g. the V2.9 TasksHandler routes that operate without a source
// card). Card is the looked-up *store.Card; pass nil when the dispatch
// has no card context.
type DispatchInput struct {
	Intent       string
	Card         *store.Card
	CardID       string
	Target       map[string]any
	Confirm      bool
	LegacyAction string // optional; surfaces in the audit row's "action" field
}

// DispatchIntent runs the named intent's Executor and emits the same
// audit + outcome event rows that the card-bound /api/cards/:id/action
// route writes. Pulled out of action() so the V2.9 TasksHandler routes
// can dispatch without a card binding while keeping the audit trail
// identical (dashboards keyed off user.action_taken stay accurate).
//
// Returns the executor's Result. Executor errors are logged + audited
// as action.failed; the returned Result.OK reflects the executor's
// own opinion on success.
func (h *Handler) DispatchIntent(ctx context.Context, in DispatchInput) (Result, error) {
	start := time.Now()
	tz := h.tz()
	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}
	today := now.In(tz).Format("2006-01-02")

	ec := ExecCtx{
		Intent:   in.Intent,
		Card:     in.Card,
		Now:      now,
		TZ:       tz,
		Today:    today,
		Confirm:  in.Confirm,
		Target:   in.Target,
		EventLog: h.EventLog,
		Logger:   h.Log,
	}

	var result Result
	var execErr error
	if ex, ok := h.lookup(in.Intent); ok {
		result, execErr = ex.Execute(ctx, ec)
		if execErr != nil {
			if h.Log != nil {
				h.Log.WithError(execErr).
					WithField("card_id", in.CardID).
					WithField("intent", in.Intent).
					Error("action: executor failed")
			}
			h.appendEvent(ctx, logp.KindActionFailed, map[string]any{
				"card_id": in.CardID, "intent": in.Intent, "error": execErr.Error(),
			})
		}
	} else {
		result = Result{OK: true}
	}

	auditPayload := map[string]any{
		"card_id":     in.CardID,
		"intent":      in.Intent,
		"action":      in.LegacyAction,
		"duration_ms": time.Since(start).Milliseconds(),
	}
	if in.LegacyAction == "" {
		auditPayload["action"] = in.Intent
	}
	if len(in.Target) > 0 {
		auditPayload["target"] = in.Target
	}
	h.appendEvent(ctx, logp.KindUserActionTaken, auditPayload)

	if result.EventKind != "" {
		payload := result.EventPayload
		if payload == nil {
			payload = map[string]any{}
		}
		if in.CardID != "" {
			payload["card_id"] = in.CardID
		}
		payload["intent"] = in.Intent
		h.appendEvent(ctx, result.EventKind, payload)
	}

	if h.Log != nil {
		fields := logrus.Fields{
			"card_id":     in.CardID,
			"intent":      in.Intent,
			"confirm":     in.Confirm,
			"ok":          result.OK,
			"duration_ms": time.Since(start).Milliseconds(),
		}
		// Surface the toast on failure — without it, operator logs show
		// "ok=false" with no signal as to *why*. The toast is the same
		// string the UI shows the user, so this is just making the
		// already-public reason visible to the daemon log too.
		if !result.OK && result.Toast != "" {
			fields["toast"] = result.Toast
			h.Log.WithFields(fields).Warn("action: dispatched (failed)")
		} else {
			h.Log.WithFields(fields).Info("action: dispatched")
		}
	}

	return result, execErr
}

func (h *Handler) lookup(intent string) (Executor, bool) {
	if h.Registry == nil {
		return nil, false
	}
	return h.Registry.Lookup(intent)
}

func (h *Handler) appendEvent(ctx context.Context, kind string, payload map[string]any) {
	if h.EventLog == nil {
		return
	}
	if _, err := h.EventLog.Append(ctx, kind, "ui", payload); err != nil && h.Log != nil {
		h.Log.WithError(err).WithField("kind", kind).Warn("action: append event failed")
	}
}

// legacyActionString returns the value the pre-V2.8 audit payload used
// for the "action" key. New clients send Intent directly; legacy clients
// send a label like "Snooze" — preserve their lowercased label so logs
// from a V2.7 UI replayed against a V2.8 server still grep cleanly.
func legacyActionString(req actionRequest, intent string) string {
	if req.Action != "" {
		return strings.ToLower(strings.TrimSpace(req.Action))
	}
	return intent
}

// inferIntentForLabel maps a legacy action-label string ("Snooze",
// "Draft a reply", "Tell Sam yes") to a canonical intent.
//
// Empty input is the only case where we return "" — that signals the
// handler to 400 ("action or intent is required"). Every non-empty
// label maps to *something* (worst case: "dismiss") so a legacy UI
// emitting an unrecognized verb still produces an audit row instead
// of a failed mutation.
//
// The actual table lives in synth.InferIntent so the dispatch decision
// is identical whether the intent arrives via a stored card's Actions
// JSON (synth.postProcessIntent) or via this handler from the wire.
func inferIntentForLabel(label string) string {
	if strings.TrimSpace(label) == "" {
		return ""
	}
	return synth.InferIntent(label)
}
