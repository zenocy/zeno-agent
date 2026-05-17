package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
	"gorm.io/datatypes"

	"github.com/zenocy/zeno-v2/internal/idgen"
	"github.com/zenocy/zeno-v2/internal/llm"
	logp "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/store"
	"github.com/zenocy/zeno-v2/internal/synth"
)

// ConverseHandler answers GET /api/cards/:id/thread and
// POST /api/cards/:id/converse — the card-conversation surface.
//
// Each card has at most one persistent thread; appending a turn runs
// the synth.Converse loop with the pinned card and prior turns as
// context, then stores the new (prompt, reply) row.
type ConverseHandler struct {
	Cards         *store.CardRepo
	Conversations *store.ConversationRepo
	Traces        *store.TraceRepo
	ConverseFn    func(ctx context.Context, card synth.PinnedCard, prior []synth.PriorTurn, query string) (synth.SubCard, llm.Trace, error)
	EventLog      logp.Writer
	TZ            func() *time.Location
	Now           func() time.Time
	Deadline      time.Duration
	Log           *logrus.Entry
}

// Register attaches the converse routes to the Echo instance.
func (h *ConverseHandler) Register(e *echo.Echo) {
	e.GET("/api/cards/:id/thread", h.thread)
	e.POST("/api/cards/:id/converse", h.converse)
}

type converseRequest struct {
	Query string `json:"query"`
}

type turnDTO struct {
	ID        string        `json:"id"`
	Position  int           `json:"position"`
	Prompt    string        `json:"prompt"`
	Reply     synth.SubCard `json:"reply"`
	TraceID   string        `json:"trace_id"`
	CreatedAt time.Time     `json:"created_at"`
}

type threadResponse struct {
	ThreadID string    `json:"thread_id"`
	CardID   string    `json:"card_id"`
	Turns    []turnDTO `json:"turns"`
}

// anchorPinned returns a synthesized PinnedCard for the design's
// rail-anchor surfaces (Zeno V2/zeno-app.jsx:71–78). These IDs aren't
// real cards in the store; the modal uses them so the user can have a
// conversation against their calendar / tasks at large.
func anchorPinned(cardID string) (synth.PinnedCard, bool) {
	switch cardID {
	case "calendar_day":
		return synth.PinnedCard{
			ID:       cardID,
			Title:    "Today's calendar",
			Sub:      "What's on, what's next, and what to move.",
			SrcLabel: "Calendar · today",
		}, true
	case "tasks_view":
		return synth.PinnedCard{
			ID:       cardID,
			Title:    "Tasks",
			Sub:      "Open tasks, due dates, and reminders.",
			SrcLabel: "Tasks · all",
		}, true
	}
	return synth.PinnedCard{}, false
}

func (h *ConverseHandler) thread(c echo.Context) error {
	cardID := c.Param("id")
	if cardID == "" {
		return BadRequest(c, "card id is required")
	}

	if _, isAnchor := anchorPinned(cardID); !isAnchor {
		card, err := h.Cards.GetByID(c.Request().Context(), cardID)
		if err != nil {
			return Internal(c, err)
		}
		if card == nil {
			return NotFound(c, "card not found")
		}
	}

	thread, err := h.Conversations.GetOrCreateForCard(c.Request().Context(), cardID)
	if err != nil {
		return Internal(c, err)
	}
	turns, err := h.Conversations.ListTurns(c.Request().Context(), thread.ID)
	if err != nil {
		return Internal(c, err)
	}

	out := threadResponse{ThreadID: thread.ID, CardID: cardID, Turns: make([]turnDTO, 0, len(turns))}
	for _, t := range turns {
		out.Turns = append(out.Turns, toTurnDTO(t))
	}
	return c.JSON(http.StatusOK, out)
}

func (h *ConverseHandler) converse(c echo.Context) error {
	cardID := c.Param("id")
	if cardID == "" {
		return BadRequest(c, "card id is required")
	}

	var req converseRequest
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return BadRequest(c, "invalid request body")
	}
	if req.Query == "" {
		return BadRequest(c, "query is required")
	}

	pinned, isAnchor := anchorPinned(cardID)
	if !isAnchor {
		card, err := h.Cards.GetByID(c.Request().Context(), cardID)
		if err != nil {
			return Internal(c, err)
		}
		if card == nil {
			return NotFound(c, "card not found")
		}
		pinned = synth.PinnedCard{
			ID:       card.ID,
			Title:    card.Title,
			Sub:      card.Sub,
			SrcLabel: card.SrcLabel,
		}
		if len(card.Meta) > 0 {
			_ = json.Unmarshal(card.Meta, &pinned.Meta)
		}
	}

	thread, err := h.Conversations.GetOrCreateForCard(c.Request().Context(), cardID)
	if err != nil {
		return Internal(c, err)
	}
	priorRows, err := h.Conversations.ListTurns(c.Request().Context(), thread.ID)
	if err != nil {
		return Internal(c, err)
	}

	prior := make([]synth.PriorTurn, 0, len(priorRows))
	for _, t := range priorRows {
		var reply synth.SubCard
		if len(t.ReplyJSON) > 0 {
			_ = json.Unmarshal(t.ReplyJSON, &reply)
		}
		prior = append(prior, synth.PriorTurn{Prompt: t.Prompt, Reply: reply})
	}

	deadline := h.Deadline + time.Second
	if h.Deadline <= 0 {
		deadline = 46 * time.Second
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), deadline)
	defer cancel()

	runID := idgen.New()
	reply, trace, runErr := h.ConverseFn(ctx, pinned, prior, req.Query)
	if runErr != nil && h.Log != nil {
		h.Log.WithError(runErr).WithField("card_id", cardID).Warn("converse: synthesis error (degraded reply still surfaced)")
	}

	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}
	tz := tzFrom(h.TZ)
	date := now.In(tz).Format("2006-01-02")

	if persistErr := h.persistTrace(c.Request().Context(), runID, date, trace); persistErr != nil && h.Log != nil {
		h.Log.WithError(persistErr).Warn("converse: persist trace failed")
	}

	replyJSON, _ := json.Marshal(reply)
	turn, err := h.Conversations.AppendTurn(c.Request().Context(), thread.ID, req.Query, replyJSON, runID)
	if err != nil {
		return Internal(c, err)
	}

	if h.EventLog != nil {
		_, _ = h.EventLog.Append(c.Request().Context(), logp.KindCardConverse, "ui", map[string]any{
			"card_id":   cardID,
			"thread_id": thread.ID,
			"turn_id":   turn.ID,
			"position":  turn.Position,
			"trace_id":  runID,
		})
	}

	return c.JSON(http.StatusOK, toTurnDTO(*turn))
}

func (h *ConverseHandler) persistTrace(ctx context.Context, runID, date string, trace llm.Trace) error {
	stepsJSON, _ := json.Marshal(trace.Steps)
	row := store.Trace{
		ID:        runID,
		RunID:     runID,
		Date:      date,
		Stopped:   trace.Stopped,
		TotalMs:   trace.TotalMs,
		Steps:     datatypes.JSON(stepsJSON),
		CreatedAt: time.Now(),
	}
	return h.Traces.Create(ctx, row)
}

func toTurnDTO(t store.ConversationTurn) turnDTO {
	out := turnDTO{
		ID:        t.ID,
		Position:  t.Position,
		Prompt:    t.Prompt,
		TraceID:   t.TraceID,
		CreatedAt: t.CreatedAt,
	}
	if len(t.ReplyJSON) > 0 {
		_ = json.Unmarshal(t.ReplyJSON, &out.Reply)
	}
	return out
}
