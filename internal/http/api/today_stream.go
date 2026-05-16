package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/eventbus"
)

// DefaultSSEKeepAlive is the default heartbeat interval. Sent as an SSE
// comment line so the browser + any proxies detect dead clients within
// ~30s instead of waiting for TCP keepalive (which can take hours).
const DefaultSSEKeepAlive = 15 * time.Second

// TodayStreamHandler streams typed bus events to an open browser tab
// over Server-Sent Events.
//
// V2.3.0 P3 used it to deliver card.appended on the inject path. V2.4.0
// extends the wire to also carry synth.started, trace.step, synth.delta,
// and synth.completed for the live-trace UI. The card.appended payload
// shape is byte-equal to V2.3 (the data is the marshaled Card itself,
// not a wrapper), so V2.3 React clients keep working unmodified — they
// silently skip the unknown event names per EventSource's spec.
//
// One subscriber per browser tab is the expected pattern (single-user
// daemon). The bus's non-blocking Publish makes a slow subscriber
// degrade to "dropped from live stream" rather than block the
// publisher; durable persistence in the database backstops every drop.
type TodayStreamHandler struct {
	Bus               *eventbus.Bus
	Logger            *logrus.Entry
	KeepAliveInterval time.Duration // 0 → DefaultSSEKeepAlive
}

// Register wires GET /api/today/stream onto the Echo instance.
func (h *TodayStreamHandler) Register(e *echo.Echo) {
	e.GET("/api/today/stream", h.handle)
}

func (h *TodayStreamHandler) handle(c echo.Context) error {
	if h.Bus == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "event bus not configured")
	}

	w := c.Response()
	w.Header().Set(echo.HeaderContentType, "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Disables proxy buffering when we're behind nginx/cloudflare so
	// events flush in real time. Harmless when no proxy is in front.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	w.Flush()

	keepAlive := h.KeepAliveInterval
	if keepAlive <= 0 {
		keepAlive = DefaultSSEKeepAlive
	}

	sub := h.Bus.Subscribe()
	defer h.Bus.Unsubscribe(sub)

	notify := c.Request().Context().Done()
	ticker := time.NewTicker(keepAlive)
	defer ticker.Stop()

	for {
		select {
		case <-notify:
			if h.Logger != nil {
				h.Logger.Debug("today_stream: client disconnected")
			}
			return nil
		case ev, ok := <-sub:
			if !ok {
				// Bus unsubscribed us out from under (shouldn't happen — bus
				// only closes on Unsubscribe, which we own).
				return nil
			}
			if err := h.emit(w, ev); err != nil {
				// Marshal failure is non-fatal: the failing event is dropped
				// from the stream, but the connection stays open so future
				// events can still flow. Subscriber slot is also reclaimed.
				if h.Logger != nil {
					h.Logger.WithError(err).WithField("kind", ev.Kind()).
						Warn("today_stream: marshal event failed")
				}
				continue
			}
		case <-ticker.C:
			// SSE comment lines start with `:` and are ignored by EventSource
			// clients. They keep the connection warm and trigger immediate
			// dead-peer detection on most network paths.
			fmt.Fprint(w, ": ping\n\n")
			w.Flush()
		}
	}
}

// emit serializes one event onto the SSE wire. Each concrete event type
// maps to a distinct SSE `event:` name so the React consumer can route
// without a payload-side discriminator. card.appended is the V2.3 wire
// shape (the Card itself, not a wrapper) — byte-equal preservation is a
// V2.4 backward-compat guarantee.
//
// SensorEventObservedEvent is bus-internal; it's recognized here purely
// to suppress emission. Only Go subscribers (the V2.4 inject subscriber)
// read those events.
func (h *TodayStreamHandler) emit(w io.Writer, ev eventbus.Event) error {
	switch e := ev.(type) {
	case eventbus.CardAppendedEvent:
		return writeSSE(w, "card.appended", e.Card)
	case eventbus.SynthStartedEvent:
		return writeSSE(w, "synth.started", e)
	case eventbus.TraceStepEvent:
		return writeSSE(w, "trace.step", e)
	case eventbus.SynthDeltaEvent:
		return writeSSE(w, "synth.delta", e)
	case eventbus.SynthCompletedEvent:
		return writeSSE(w, "synth.completed", e)
	case eventbus.SensorEventObservedEvent:
		// Bus-internal; not exposed over SSE.
		return nil
	case eventbus.ConcernProposedEvent:
		return writeSSE(w, "concern.proposed", e)
	case eventbus.ConcernStateChangedEvent:
		return writeSSE(w, "concern.state_changed", e)
	case eventbus.ConcernTaggedEvent:
		return writeSSE(w, "concern.tagged", e)
	case eventbus.RetrospectiveProgressEvent:
		return writeSSE(w, "concern.retrospective_progress", e)
	case eventbus.ConcernRetirementProposedEvent:
		return writeSSE(w, "concern.retirement_proposed", e)
	case eventbus.WhatsAppStatusEvent:
		return writeSSE(w, "whatsapp.status_changed", e)
	case eventbus.TaskCreatedEvent:
		return writeSSE(w, "task.created", e)
	case eventbus.TaskCompletedEvent:
		return writeSSE(w, "task.completed", e)
	case eventbus.TaskDeletedEvent:
		return writeSSE(w, "task.deleted", e)
	case eventbus.TaskReminderSetEvent:
		return writeSSE(w, "task.reminder_set", e)
	case eventbus.TaskEditedEvent:
		return writeSSE(w, "task.edited", e)
	case eventbus.SettingsChangedEvent:
		return writeSSE(w, "settings.changed", e)
	case eventbus.WeatherUpdatedEvent:
		return writeSSE(w, "weather.updated", e)
	case eventbus.StockUpdatedEvent:
		return writeSSE(w, "stock.updated", e)
	case eventbus.CalendarTodayChangedEvent:
		return writeSSE(w, "calendar.today_changed", e)
	case eventbus.CalendarTomorrowChangedEvent:
		return writeSSE(w, "calendar.tomorrow_changed", e)
	case eventbus.CalendarWeekChangedEvent:
		return writeSSE(w, "calendar.week_changed", e)
	case eventbus.MemoryChangedEvent:
		return writeSSE(w, "memory.changed", e)
	case eventbus.StatsSnapshotEvent:
		return writeSSE(w, "stats.snapshot", e)
	case eventbus.HealthChangedEvent:
		return writeSSE(w, "health.changed", e)
	case eventbus.ConcernObservationsChangedEvent:
		return writeSSE(w, "concern.observations_changed", e)
	default:
		// Unknown event types are silently skipped. New event types can
		// land in the bus without this handler crashing in the field; a
		// matching case here is the only thing needed to surface them.
		if h.Logger != nil {
			h.Logger.WithField("kind", ev.Kind()).
				Debug("today_stream: unknown event type, skipping")
		}
		return nil
	}
}

// writeSSE marshals payload as JSON, writes one `event:` + `data:` block
// followed by the blank-line terminator, and flushes.
func writeSSE(w io.Writer, name string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, body); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}
