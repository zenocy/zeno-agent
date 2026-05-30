package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/projection"
	"github.com/zenocy/zeno-v2/internal/store"
)

// CardsHandler answers GET /api/cards and GET /api/cards/:id/trace.
//
// TZ is a getter so handlers pick up live timezone changes from the
// Settings UI without a process restart. nil is treated as time.UTC.
type CardsHandler struct {
	Cards  *store.CardRepo
	Traces *store.TraceRepo
	TZ     func() *time.Location
	Now    func() time.Time
	Log    *logrus.Entry

	// V2.x live binding: Reader + ProjCfg + Tickers back the serve-time
	// resolution of `live` fields (weather / stock / countdown). When
	// Reader is nil the resolution is skipped and cards render their static
	// skeletons (with any sentinels stripped) unchanged.
	Reader  log.Reader
	ProjCfg projection.Config
	Tickers projection.TickerSource
}

// Register attaches the cards routes to the Echo instance.
func (h *CardsHandler) Register(e *echo.Echo) {
	e.GET("/api/cards", h.list)
	e.GET("/api/cards/archive", h.archive)
	e.GET("/api/cards/:id/trace", h.trace)
	e.GET("/api/traces/:id", h.traceByID)
}

// cardDTO is the JSON shape returned to the UI. Mirrors zeno-data.jsx so the
// frontend is drop-in.
type cardDTO struct {
	ID       string            `json:"id"`
	Date     string            `json:"date"`
	Src      string            `json:"src"`
	SrcLabel string            `json:"src_label"`
	Rel      string            `json:"rel"`
	Kind     string            `json:"kind,omitempty"`
	Title    string            `json:"title"`
	Sub      string            `json:"sub"`
	Body     string            `json:"body,omitempty"`
	Meta     []string          `json:"meta"`
	Actions  []cardActionDTO   `json:"actions"`
	Expand   map[string]string `json:"expand,omitempty"`
	Sources  []cardSourceDTO   `json:"sources,omitempty"`
	Items    []digestItemDTO   `json:"items,omitempty"` // V2.x digest children
	Live     []liveChipDTO     `json:"live,omitempty"`  // V2.x resolved live values
	TraceID  string            `json:"trace_id,omitempty"`
	Pinned   bool              `json:"pinned,omitempty"` // V2.8.1
}

// digestItemDTO is one rolled-up child of a kind="digest" card.
type digestItemDTO struct {
	Title string `json:"title"`
	Sub   string `json:"sub,omitempty"`
	Src   string `json:"src,omitempty"`
	Ref   string `json:"ref,omitempty"`
}

type cardActionDTO struct {
	Label   string         `json:"label"`
	Primary bool           `json:"primary,omitempty"`
	Intent  string         `json:"intent,omitempty"`
	Target  map[string]any `json:"target,omitempty"`
}

type cardSourceDTO struct {
	T string `json:"t"`
	U string `json:"u"`
}

type cardsListResponse struct {
	Date  string    `json:"date"`
	Cards []cardDTO `json:"cards"`
}

func (h *CardsHandler) list(c echo.Context) error {
	tz := tzFrom(h.TZ)
	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	date := c.QueryParam("date")
	if date == "" {
		date = now().In(tz).Format("2006-01-02")
	}

	rows, err := h.Cards.ListByDate(c.Request().Context(), date)
	if err != nil {
		if h.Log != nil {
			h.Log.WithError(err).WithField("date", date).Error("cards list failed")
		}
		return Internal(c, err)
	}

	lv := buildLiveViews(c.Request().Context(), h, rows, now())
	out := cardsListResponse{Date: date, Cards: make([]cardDTO, 0, len(rows))}
	for _, r := range rows {
		out.Cards = append(out.Cards, toCardDTO(r, lv))
	}
	return c.JSON(http.StatusOK, out)
}

// archive serves GET /api/cards/archive?date=YYYY-MM-DD. Returns every
// card row for the given date with no visibility filters — dismissed,
// snoozed, and expired ask cards all come back so the user can browse
// what ever appeared on a given day. Defaults to today in the handler
// TZ when ?date= is omitted.
func (h *CardsHandler) archive(c echo.Context) error {
	tz := tzFrom(h.TZ)
	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	date := c.QueryParam("date")
	if date == "" {
		date = now().In(tz).Format("2006-01-02")
	}

	rows, err := h.Cards.ListAllByDate(c.Request().Context(), date)
	if err != nil {
		if h.Log != nil {
			h.Log.WithError(err).WithField("date", date).Error("cards archive failed")
		}
		return Internal(c, err)
	}

	lv := buildLiveViews(c.Request().Context(), h, rows, now())
	out := cardsListResponse{Date: date, Cards: make([]cardDTO, 0, len(rows))}
	for _, r := range rows {
		out.Cards = append(out.Cards, toCardDTO(r, lv))
	}
	return c.JSON(http.StatusOK, out)
}

func (h *CardsHandler) trace(c echo.Context) error {
	id := c.Param("id")
	card, err := h.Cards.GetByID(c.Request().Context(), id)
	if err != nil {
		return Internal(c, err)
	}
	if card == nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "card not found", "id": id})
	}
	if card.TraceID == "" {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "card has no trace", "id": id})
	}
	tr, err := h.Traces.Get(c.Request().Context(), card.TraceID)
	if err != nil {
		return Internal(c, err)
	}
	if tr == nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "trace not found", "trace_id": card.TraceID})
	}
	return c.JSON(http.StatusOK, tr)
}

func (h *CardsHandler) traceByID(c echo.Context) error {
	id := c.Param("id")
	tr, err := h.Traces.Get(c.Request().Context(), id)
	if err != nil {
		return Internal(c, err)
	}
	if tr == nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "trace not found", "id": id})
	}
	return c.JSON(http.StatusOK, tr)
}

// toCardDTO unmarshals the card's JSON columns and shapes the DTO to match
// the UI's expected JSON keys. lv carries the per-request projections used
// to resolve serve-time live fields; pass the zero value to skip live
// resolution (sentinels are still stripped so no raw tokens leak).
func toCardDTO(r store.Card, lv liveViews) cardDTO {
	d := cardDTO{
		ID:       r.ID,
		Date:     r.Date,
		Src:      r.Source,
		SrcLabel: r.SrcLabel,
		Rel:      r.Rel,
		Kind:     r.Kind,
		Title:    r.Title,
		Sub:      r.Sub,
		Body:     r.Body,
		TraceID:  r.TraceID,
		Pinned:   r.Pinned,
	}
	if len(r.Meta) > 0 {
		_ = json.Unmarshal(r.Meta, &d.Meta)
	}
	if d.Meta == nil {
		d.Meta = []string{}
	}
	if len(r.Actions) > 0 {
		_ = json.Unmarshal(r.Actions, &d.Actions)
	}
	if d.Actions == nil {
		d.Actions = []cardActionDTO{}
	}
	if len(r.Expand) > 0 {
		_ = json.Unmarshal(r.Expand, &d.Expand)
	}
	if len(r.Sources) > 0 {
		_ = json.Unmarshal(r.Sources, &d.Sources)
	}
	if len(r.Items) > 0 {
		_ = json.Unmarshal(r.Items, &d.Items)
	}

	// V2.x live binding: substitute sentinels in meta/sub from the latest
	// projection and expose the resolved chips. Always runs the strip path
	// so an un-resolved sentinel never leaks to the UI.
	meta, sub, chips := applyLiveFields(r.Live, d.Meta, d.Sub, lv)
	d.Meta = meta
	if d.Meta == nil {
		d.Meta = []string{}
	}
	d.Sub = sub
	d.Live = chips
	return d
}
