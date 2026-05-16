package api

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/zenocy/zeno-v2/internal/action"
)

// ActionsHandler answers GET /api/actions/modes — the static
// {intent → mode} catalog the UI fetches at boot to know which
// action buttons should open the confirm modal versus commit
// immediately. The list is the V2.8.0 canonical vocabulary
// (action.CanonicalIntents); the UI then merges in the per-card
// Action.Intent emitted by synth to decide what each button does.
//
// No auth scope beyond the existing /api/* bearer; the catalog
// is not user-specific and revealing it doesn't disclose any
// state.
type ActionsHandler struct {
	Registry *action.Registry
}

// Register attaches the actions catalog route.
func (h *ActionsHandler) Register(e *echo.Echo) {
	e.GET("/api/actions/modes", h.modes)
}

type intentEntry struct {
	Intent      string `json:"intent"`
	Mode        string `json:"mode"`
	Description string `json:"description"`
	Wired       bool   `json:"wired"` // true when an Executor is registered for this intent
}

type modesResponse struct {
	Intents []intentEntry `json:"intents"`
}

func (h *ActionsHandler) modes(c echo.Context) error {
	wired := map[string]action.Mode{}
	if h.Registry != nil {
		wired = h.Registry.Modes()
	}
	out := modesResponse{Intents: make([]intentEntry, 0, len(action.CanonicalIntents))}
	for _, ci := range action.CanonicalIntents {
		// Prefer the Mode the live registry reports — an Executor wired
		// at boot wins over the canonical default if they disagree (lets
		// us tighten or relax safety policy without re-cutting the table).
		mode := ci.Mode
		_, isWired := wired[ci.Intent]
		if isWired {
			mode = wired[ci.Intent]
		}
		out.Intents = append(out.Intents, intentEntry{
			Intent:      ci.Intent,
			Mode:        string(mode),
			Description: ci.Description,
			Wired:       isWired,
		})
	}
	return c.JSON(http.StatusOK, out)
}
