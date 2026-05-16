package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/store"
	"github.com/zenocy/zeno-v2/internal/whatsapp"
)

// WhatsAppHandler exposes the V2.7 WhatsApp control surface for the
// Settings page:
//
//   - GET  /api/whatsapp/status — render the live Status + config row.
//   - POST /api/whatsapp/pair   — open an SSE stream of QR frames.
//   - POST /api/whatsapp/unlink — log out + delete device row.
//   - PUT  /api/whatsapp/config — persist the operator-tunable config
//     (allowlist, mention name, throttle) and hot-reload the Service.
//
// The handler holds a pointer to the live whatsapp.Service so it can
// drive pair/unlink and read live status. Service methods are safe for
// concurrent callers.
type WhatsAppHandler struct {
	Service *whatsapp.Service
	Repo    *store.WhatsAppConfigRepo
	Log     *logrus.Entry

	// PairTimeout caps the pair flow so a forgotten browser tab doesn't
	// keep the QR stream open forever. Defaults to 3 minutes — whatsmeow
	// itself rotates QR codes every ~30s so a 3-minute window covers
	// six attempts.
	PairTimeout time.Duration
}

// Register wires the four routes onto the Echo instance.
func (h *WhatsAppHandler) Register(e *echo.Echo) {
	e.GET("/api/whatsapp/status", h.status)
	e.POST("/api/whatsapp/pair", h.pair)
	e.POST("/api/whatsapp/unlink", h.unlink)
	e.PUT("/api/whatsapp/config", h.putConfig)
}

// statusDTO is the JSON shape returned by GET /api/whatsapp/status.
// Status fields are flattened from whatsapp.Status; RuntimeConfig
// fields are nested so the UI can edit them as a unit.
type statusDTO struct {
	Enabled     bool      `json:"enabled"`
	HasSession  bool      `json:"has_session"`
	Connected   bool      `json:"connected"`
	LoggedIn    bool      `json:"logged_in"`
	OwnJID      string    `json:"own_jid,omitempty"`
	OwnPushName string    `json:"own_push_name,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	LastSeenAt  time.Time `json:"last_seen_at,omitempty"`
	PairedAt    time.Time `json:"paired_at,omitempty"`

	Config configDTO `json:"config"`
}

// configDTO is the editable subset.
type configDTO struct {
	MentionName        string   `json:"mention_name"`
	AllowedDMs         []string `json:"allowed_dms"`
	MinChatIntervalMs  int      `json:"min_chat_interval_ms"`
	MaxConcurrentSynth int      `json:"max_concurrent_synth"`
	PerChatBuffer      int      `json:"per_chat_buffer"`
}

func (h *WhatsAppHandler) status(c echo.Context) error {
	if h.Service == nil {
		return ServiceUnavailable(c, "whatsapp service not configured")
	}
	st := h.Service.Status()
	rt := h.Service.RuntimeConfig()
	return c.JSON(http.StatusOK, statusDTO{
		Enabled:     st.Enabled,
		HasSession:  st.HasSession,
		Connected:   st.Connected,
		LoggedIn:    st.LoggedIn,
		OwnJID:      st.OwnJID,
		OwnPushName: st.OwnPushName,
		LastError:   st.LastError,
		LastSeenAt:  st.LastSeenAt,
		PairedAt:    st.PairedAt,
		Config: configDTO{
			MentionName:        rt.MentionName,
			AllowedDMs:         rt.AllowedDMs,
			MinChatIntervalMs:  int(rt.MinChatInterval / time.Millisecond),
			MaxConcurrentSynth: rt.MaxConcurrentSynth,
			PerChatBuffer:      rt.PerChatBuffer,
		},
	})
}

// pair drives the QR-pair flow as a Server-Sent Events stream:
//
//	event: code
//	data: {"code":"<qr-string>"}
//
// On completion (success / timeout / error / cancellation) the handler
// emits one terminal event and closes the connection.
func (h *WhatsAppHandler) pair(c echo.Context) error {
	if h.Service == nil {
		return ServiceUnavailable(c, "whatsapp service not configured")
	}

	timeout := h.PairTimeout
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), timeout)
	defer cancel()

	stream, err := h.Service.BeginPair(ctx)
	if err != nil {
		// Already paired → 409; anything else → 500. Concurrent pair
		// requests are no longer 409: the Service pre-empts the prior
		// pair flow and grants this caller a fresh stream.
		if strings.Contains(err.Error(), "already paired") {
			return Conflict(c, err.Error())
		}
		return Internal(c, err)
	}

	w := c.Response()
	w.Header().Set(echo.HeaderContentType, "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	w.Flush()

	for {
		select {
		case <-ctx.Done():
			h.writeSSEEvent(c, "timeout", map[string]any{"reason": ctx.Err().Error()})
			return nil
		case ev, ok := <-stream:
			if !ok {
				// whatsmeow's qrchan closed without a terminal event
				// (silent EOF / output-full / qrc.ctx done). Surface
				// the latest Service.Status().LastError if the lifecycle
				// handler captured one, else a generic hint that gives
				// the user something actionable instead of a stuck QR.
				msg := h.Service.Status().LastError
				if msg == "" {
					msg = "Pairing connection ended unexpectedly. Try again, or check that WhatsApp on your phone is up to date."
				}
				h.writeSSEEvent(c, "error", map[string]any{"error": msg})
				return nil
			}
			payload := map[string]any{}
			if ev.Code != "" {
				payload["code"] = ev.Code
			}
			if ev.Err != nil {
				payload["error"] = ev.Err.Error()
			}
			h.writeSSEEvent(c, ev.Event, payload)
			if ev.Event == "success" || ev.Event == "timeout" || ev.Event == "error" {
				return nil
			}
		}
	}
}

// unlink invalidates the server session and deletes the local device
// row. Always returns 204; status will reflect the cleared state on
// the next /api/whatsapp/status call.
func (h *WhatsAppHandler) unlink(c echo.Context) error {
	if h.Service == nil {
		return ServiceUnavailable(c, "whatsapp service not configured")
	}
	if err := h.Service.Unlink(c.Request().Context()); err != nil {
		if h.Log != nil {
			h.Log.WithError(err).Warn("whatsapp: unlink failed")
		}
		return Internal(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// putConfig persists the operator-tunable config and hot-reloads the
// running Service so the next inbound message reflects the change.
func (h *WhatsAppHandler) putConfig(c echo.Context) error {
	if h.Service == nil || h.Repo == nil {
		return ServiceUnavailable(c, "whatsapp service not configured")
	}
	var req configDTO
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return BadRequest(c, "invalid request body")
	}

	mention := strings.TrimSpace(req.MentionName)
	if mention == "" {
		mention = "zeno"
	}
	if !validMentionName(mention) {
		return BadRequest(c, "mention_name must be 2–20 lowercase letters/digits")
	}

	// V2.13.3c: normalize each entry to the canonical
	// `digits@s.whatsapp.net` form so comparisons against inbound
	// JIDs (always digit-only on the wire) succeed regardless of how
	// the operator typed them ("+357…", "00357…", or already-canonical).
	// Validate AFTER normalization so a `+`-prefixed input is accepted.
	jids := make([]string, 0, len(req.AllowedDMs))
	seen := make(map[string]struct{}, len(req.AllowedDMs))
	for _, raw := range req.AllowedDMs {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		jid := whatsapp.NormalizeJID(trimmed)
		if jid == "" {
			return BadRequest(c, fmt.Sprintf("invalid jid: %q", raw))
		}
		if !validJID(jid) {
			return BadRequest(c, fmt.Sprintf("invalid jid: %q", raw))
		}
		if _, dup := seen[jid]; dup {
			continue
		}
		seen[jid] = struct{}{}
		jids = append(jids, jid)
	}

	min := req.MinChatIntervalMs
	if min < 1000 {
		// Sub-second throttle in production is a footgun: ban risk
		// rises sharply. The integration test suite passes intervals
		// well below this threshold by working with the dispatcher
		// directly, not via this handler.
		return BadRequest(c, "min_chat_interval_ms must be ≥ 1000")
	}
	if max := req.MaxConcurrentSynth; max < 1 || max > 16 {
		return BadRequest(c, "max_concurrent_synth must be in [1,16]")
	}
	if buf := req.PerChatBuffer; buf < 1 || buf > 32 {
		return BadRequest(c, "per_chat_buffer must be in [1,32]")
	}

	row := store.WhatsAppConfigRow{
		MentionName:        mention,
		MinChatIntervalMs:  min,
		MaxConcurrentSynth: req.MaxConcurrentSynth,
		PerChatBuffer:      req.PerChatBuffer,
	}
	if err := row.SetAllowedDMs(jids); err != nil {
		return Internal(c, err)
	}
	if err := h.Repo.Upsert(c.Request().Context(), row); err != nil {
		return Internal(c, err)
	}

	h.Service.SetRuntimeConfig(whatsapp.RuntimeConfig{
		MentionName:        mention,
		AllowedDMs:         jids,
		MinChatInterval:    time.Duration(min) * time.Millisecond,
		MaxConcurrentSynth: req.MaxConcurrentSynth,
		PerChatBuffer:      req.PerChatBuffer,
	})
	return c.JSON(http.StatusOK, configDTO{
		MentionName:        mention,
		AllowedDMs:         jids,
		MinChatIntervalMs:  min,
		MaxConcurrentSynth: req.MaxConcurrentSynth,
		PerChatBuffer:      req.PerChatBuffer,
	})
}

// LoadInitialConfig is called from main.go at boot to seed the running
// Service with the persisted RuntimeConfig (or the defaults if no row
// exists yet). Separating this from the handler avoids forcing main
// to import store + whatsapp + a glue function each.
func LoadInitialConfig(ctx context.Context, repo *store.WhatsAppConfigRepo, svc *whatsapp.Service, log *logrus.Entry) {
	if repo == nil || svc == nil {
		return
	}
	row, err := repo.Get(ctx)
	if err != nil {
		if log != nil {
			log.WithError(err).Warn("whatsapp: load config row failed; using defaults")
		}
		return
	}
	if row == nil {
		return // defaults from NewService stay in effect
	}
	cfg := whatsapp.RuntimeConfig{
		MentionName:        row.MentionName,
		AllowedDMs:         row.AllowedDMs(),
		MinChatInterval:    time.Duration(row.MinChatIntervalMs) * time.Millisecond,
		MaxConcurrentSynth: row.MaxConcurrentSynth,
		PerChatBuffer:      row.PerChatBuffer,
	}
	svc.SetRuntimeConfig(cfg)
}

// writeSSEEvent writes one event-name + JSON-data block onto the SSE
// wire. Mirrors the helper in today_stream.go but stays scoped to the
// pair endpoint so a refactor of that file doesn't quietly break us.
func (h *WhatsAppHandler) writeSSEEvent(c echo.Context, name string, payload any) {
	w := c.Response()
	body, err := json.Marshal(payload)
	if err != nil {
		body = []byte("{}")
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, body)
	w.Flush()
}

// validMentionName accepts 2–20 chars of [a-z0-9]. Rejects empty,
// punctuation, and uppercase to keep classifier behavior predictable.
func validMentionName(s string) bool {
	return mentionRe.MatchString(s)
}

var mentionRe = regexp.MustCompile(`^[a-z0-9]{2,20}$`)

// validJID is a minimal sanity check on whatsapp JID strings. Real
// validation lives inside whatsmeow's types.ParseJID; this helper
// keeps invalid input out of the database without dragging the whole
// whatsmeow dependency into the api package.
func validJID(s string) bool {
	return jidRe.MatchString(s)
}

var jidRe = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9._-]+$`)
