package api

import (
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/store"
)

// BriefingHandler answers GET /api/briefing/today.
//
// TZ is a getter so timezone edits from the Settings UI take effect on
// the next request without a process restart.
type BriefingHandler struct {
	Repo *store.BriefingRepo
	TZ   func() *time.Location
	Now  func() time.Time
	Log  *logrus.Entry
}

// Register attaches the briefing route to the Echo instance.
func (h *BriefingHandler) Register(e *echo.Echo) {
	e.GET("/api/briefing/today", h.handle)
}

func (h *BriefingHandler) handle(c echo.Context) error {
	tz := tzFrom(h.TZ)
	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	date := c.QueryParam("date")
	if date == "" {
		date = now().In(tz).Format("2006-01-02")
	}

	b, err := h.Repo.ByDate(c.Request().Context(), date)
	if err != nil {
		if h.Log != nil {
			h.Log.WithError(err).WithField("date", date).Error("briefing fetch failed")
		}
		return Internal(c, err)
	}
	if b == nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "no briefing for date", "date": date})
	}
	return c.JSON(http.StatusOK, b)
}
