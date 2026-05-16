package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/projection"
	"github.com/zenocy/zeno-v2/internal/store"
	"github.com/zenocy/zeno-v2/internal/synth"
)

// sendsRetentionWindow is the lookback the Sends panel displays.
// The scheduler prunes resolved/expired rows older than 7 days, so
// this matches the persistence horizon — the user sees what's
// retained, no more.
const sendsRetentionWindow = 7 * 24 * time.Hour

// SendsHandler answers the V2.13.2 Sends surface — the assistant-mode
// outbound activity log. Reads `expected_replies` rows directly,
// derives status, joins event titles from today's calendar.
//
//	GET /api/sends           — last 7 days, all sends
//	GET /api/sends?card_id=X — sends anchored to card X (title-matched
//	                            against today's calendar)
type SendsHandler struct {
	Cards   *store.CardRepo
	Replies *store.ExpectedReplyRepo
	Reader  log.Reader
	ProjCfg projection.Config
	Now     func() time.Time
	Log     *logrus.Entry
}

// SendDTO is one row in the panel.
type SendDTO struct {
	ID            string     `json:"id"`
	SentAt        time.Time  `json:"sent_at"`
	RecipientName string     `json:"recipient_name"`
	Status        string     `json:"status"` // awaiting_reply | replied | expired
	EventTitle    string     `json:"event_title,omitempty"`
	EventUID      string     `json:"event_uid,omitempty"`
	DraftBody     string     `json:"draft_body"`
	ResolvedAt    *time.Time `json:"resolved_at,omitempty"`
	ReplyBody     string     `json:"reply_body,omitempty"`
}

type sendsResponse struct {
	Sends []SendDTO `json:"sends"`
}

// Register attaches the route.
func (h *SendsHandler) Register(e *echo.Echo) {
	e.GET("/api/sends", h.list)
}

func (h *SendsHandler) list(c echo.Context) error {
	if h.Replies == nil {
		return c.JSON(http.StatusOK, sendsResponse{Sends: []SendDTO{}})
	}
	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	since := now().Add(-sendsRetentionWindow)
	rows, err := h.Replies.ListRecent(c.Request().Context(), since, 50)
	if err != nil {
		return Internal(c, err)
	}

	cardID := strings.TrimSpace(c.QueryParam("card_id"))
	cal := h.todaysCalendar(c)

	var anchorUID string
	if cardID != "" && h.Cards != nil {
		if card, _ := h.Cards.GetByID(c.Request().Context(), cardID); card != nil {
			anchorUID = synth.AnchorEventUID(synth.PinnedCard{
				ID:    card.ID,
				Title: card.Title,
				Sub:   card.Sub,
			}, cal)
		}
		if anchorUID == "" {
			// Card has no calendar anchor → empty list. The banner is
			// non-essential, no need to 404.
			return c.JSON(http.StatusOK, sendsResponse{Sends: []SendDTO{}})
		}
	}

	titleByUID := make(map[string]string, len(cal))
	for _, ev := range cal {
		titleByUID[ev.UID] = ev.Title
	}

	out := make([]SendDTO, 0, len(rows))
	nowTime := now()
	for _, r := range rows {
		if anchorUID != "" && r.ContextID != anchorUID {
			continue
		}
		out = append(out, toSendDTO(r, titleByUID, nowTime))
	}
	return c.JSON(http.StatusOK, sendsResponse{Sends: out})
}

func (h *SendsHandler) todaysCalendar(c echo.Context) []projection.CalendarEvent {
	if h.Reader == nil {
		return nil
	}
	cal, err := (projection.TodaysCalendar{Cfg: h.ProjCfg}).Compute(c.Request().Context(), h.Reader)
	if err != nil {
		if h.Log != nil {
			h.Log.WithError(err).Warn("sends: calendar projection failed")
		}
		return nil
	}
	return cal
}

// toSendDTO derives the view shape for one ExpectedReply row.
func toSendDTO(r store.ExpectedReply, titleByUID map[string]string, now time.Time) SendDTO {
	out := SendDTO{
		ID:            r.ID,
		SentAt:        r.SentAt,
		RecipientName: r.RecipientName,
		Status:        sendStatus(r, now),
		EventUID:      r.ContextID,
		DraftBody:     r.DraftBody,
	}
	if r.ContextKind == "event" {
		if title, ok := titleByUID[r.ContextID]; ok {
			out.EventTitle = title
		}
	}
	if r.ResolvedAt != nil && !r.ResolvedAt.IsZero() {
		t := *r.ResolvedAt
		out.ResolvedAt = &t
		out.ReplyBody = r.InboundBody
	}
	return out
}

func sendStatus(r store.ExpectedReply, now time.Time) string {
	if r.ResolvedAt != nil && !r.ResolvedAt.IsZero() {
		return "replied"
	}
	if !r.ExpiresAt.IsZero() && r.ExpiresAt.Before(now) {
		return "expired"
	}
	return "awaiting_reply"
}
