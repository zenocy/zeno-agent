package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/action"
)

func TestActionsModes_ReturnsCanonicalCatalog(t *testing.T) {
	reg := action.NewRegistry()
	reg.Register("dismiss", &action.DismissExec{})
	reg.Register("snooze", &action.SnoozeExec{})

	e := echo.New()
	(&ActionsHandler{Registry: reg}).Register(e)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/actions/modes", nil)
	e.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	var resp struct {
		Intents []struct {
			Intent      string `json:"intent"`
			Mode        string `json:"mode"`
			Description string `json:"description"`
			Wired       bool   `json:"wired"`
		} `json:"intents"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))

	// All canonical intents are present.
	require.Len(t, resp.Intents, 27)

	byIntent := map[string]struct {
		Mode  string
		Wired bool
	}{}
	for _, i := range resp.Intents {
		byIntent[i.Intent] = struct {
			Mode  string
			Wired bool
		}{i.Mode, i.Wired}
	}

	require.Equal(t, "one_click", byIntent["dismiss"].Mode)
	require.True(t, byIntent["dismiss"].Wired)
	require.Equal(t, "preflight", byIntent["draft_reply"].Mode)
	require.False(t, byIntent["draft_reply"].Wired, "draft_reply executor lands in Phase 1; wired=false in P0")
}

func TestActionsModes_NoRegistry(t *testing.T) {
	e := echo.New()
	(&ActionsHandler{Registry: nil}).Register(e)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/actions/modes", nil)
	e.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	// Catalog still served — Wired=false everywhere.
	var resp struct {
		Intents []struct {
			Wired bool `json:"wired"`
		} `json:"intents"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Len(t, resp.Intents, 27)
	for _, i := range resp.Intents {
		require.False(t, i.Wired)
	}
}
