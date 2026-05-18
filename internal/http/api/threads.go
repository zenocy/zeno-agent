package api

import (
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"

	logp "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/synth"
)

// ThreadsHandler answers GET /api/threads/preview — returns the verbatim
// body preview for the most recent mail.received event whose subject
// contains the given hint. Used by the SubCard `document` kind's
// "view original" toggle: the LLM stores the subject_hint it passed to
// read_thread, and the UI resolves that hint back to the durable-log
// payload here without involving the model.
type ThreadsHandler struct {
	Reader   logp.Reader
	Lookback time.Duration // 0 → 14 days, mirroring ReadThreadTool
	Now      func() time.Time
	Log      *logrus.Entry
}

// Register attaches the threads routes to the Echo instance.
func (h *ThreadsHandler) Register(e *echo.Echo) {
	e.GET("/api/threads/preview", h.preview)
}

type threadPreviewResponse struct {
	Subject string `json:"subject"`
	From    string `json:"from"`
	Date    string `json:"date"`
	Body    string `json:"body"`
}

func (h *ThreadsHandler) preview(c echo.Context) error {
	hint := c.QueryParam("hint")
	if hint == "" {
		return BadRequest(c, "hint is required")
	}

	now := time.Now
	if h.Now != nil {
		now = h.Now
	}

	hit, ok, err := synth.FindLatestThread(c.Request().Context(), h.Reader, hint, h.Lookback, now())
	if err != nil {
		if h.Log != nil {
			h.Log.WithError(err).WithField("hint", hint).Warn("threads preview: lookup failed")
		}
		return Internal(c, err)
	}
	if !ok {
		return NotFound(c, "no thread matches that hint")
	}

	return c.JSON(http.StatusOK, threadPreviewResponse{
		Subject: hit.Subject,
		From:    hit.From,
		Date:    hit.Date.Format(time.RFC3339),
		Body:    hit.BodyPreview,
	})
}
