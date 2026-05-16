package api

import (
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/projection"
	"github.com/zenocy/zeno-v2/internal/store"
)

// ProjectionsHandler exposes the read-side projections to the UI.
// Each request recomputes from the log; no caching.
//
// Tickers is nil when the stock sensor isn't wired (older deployments);
// the /api/projections/stock route returns null in that case.
type ProjectionsHandler struct {
	Reader  log.Reader
	Tasks   *store.TaskRepo // V2.11: backs /api/projections/tasks/open
	Cfg     projection.Config
	Tickers projection.TickerSource
	Log     *logrus.Entry
}

// Register wires the routes onto the Echo instance.
func (h *ProjectionsHandler) Register(e *echo.Echo) {
	e.GET("/api/projections/calendar/today", h.calendarToday)
	e.GET("/api/projections/calendar/tomorrow", h.calendarTomorrow)
	e.GET("/api/projections/calendar/week", h.calendarWeek)
	e.GET("/api/projections/email/open", h.emailOpen)
	e.GET("/api/projections/run-window", h.runWindow)
	e.GET("/api/projections/weather", h.weather)
	e.GET("/api/projections/tasks/open", h.tasksOpen)
	e.GET("/api/projections/stock", h.stock)
}

func (h *ProjectionsHandler) calendarToday(c echo.Context) error {
	out, err := projection.TodaysCalendar{Cfg: h.Cfg}.Compute(c.Request().Context(), h.Reader)
	if err != nil {
		return h.fail(c, err)
	}
	if out == nil {
		out = []projection.CalendarEvent{}
	}
	return c.JSON(http.StatusOK, out)
}

func (h *ProjectionsHandler) calendarTomorrow(c echo.Context) error {
	out, err := projection.TomorrowsCalendar{Cfg: h.Cfg}.Compute(c.Request().Context(), h.Reader)
	if err != nil {
		return h.fail(c, err)
	}
	if out == nil {
		out = []projection.CalendarEvent{}
	}
	return c.JSON(http.StatusOK, out)
}

func (h *ProjectionsHandler) calendarWeek(c echo.Context) error {
	out, err := projection.WeekCalendar{Cfg: h.Cfg}.Compute(c.Request().Context(), h.Reader)
	if err != nil {
		return h.fail(c, err)
	}
	if out == nil {
		out = []projection.CalendarEvent{}
	}
	return c.JSON(http.StatusOK, out)
}

func (h *ProjectionsHandler) emailOpen(c echo.Context) error {
	out, err := projection.OpenEmailThreads{Cfg: h.Cfg}.Compute(c.Request().Context(), h.Reader)
	if err != nil {
		return h.fail(c, err)
	}
	if out == nil {
		out = []projection.Thread{}
	}
	return c.JSON(http.StatusOK, out)
}

func (h *ProjectionsHandler) runWindow(c echo.Context) error {
	out, err := projection.RunWindow{Cfg: h.Cfg}.Compute(c.Request().Context(), h.Reader)
	if err != nil {
		return h.fail(c, err)
	}
	return c.JSON(http.StatusOK, out)
}

func (h *ProjectionsHandler) weather(c echo.Context) error {
	out, err := projection.Weather{Cfg: h.Cfg}.Compute(c.Request().Context(), h.Reader)
	if err != nil {
		return h.fail(c, err)
	}
	return c.JSON(http.StatusOK, out)
}

func (h *ProjectionsHandler) tasksOpen(c echo.Context) error {
	out, err := projection.OpenTasks{Cfg: h.Cfg, Tasks: h.Tasks}.Compute(c.Request().Context(), h.Reader)
	if err != nil {
		return h.fail(c, err)
	}
	if out == nil {
		out = []projection.OpenTasksTask{}
	}
	return c.JSON(http.StatusOK, out)
}

func (h *ProjectionsHandler) stock(c echo.Context) error {
	out, err := projection.Stock{Cfg: h.Cfg, Tickers: h.Tickers}.Compute(c.Request().Context(), h.Reader)
	if err != nil {
		return h.fail(c, err)
	}
	return c.JSON(http.StatusOK, out)
}

func (h *ProjectionsHandler) fail(c echo.Context, err error) error {
	if h.Log != nil {
		h.Log.WithError(err).Error("projection compute failed")
	}
	return Internal(c, err)
}
