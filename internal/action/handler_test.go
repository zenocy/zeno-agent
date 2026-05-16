package action

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	logp "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/log/logtest"
	"github.com/zenocy/zeno-v2/internal/store"
)

func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_journal_mode=WAL&_foreign_keys=on"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, (&store.CardRepo{DB: db}).Migrate())
	return db
}

func quietLog() *logrus.Entry {
	l := logrus.New()
	l.Out = io.Discard
	return l.WithField("c", "test")
}

func seedCard(t *testing.T, repo *store.CardRepo, id string) {
	t.Helper()
	require.NoError(t, repo.Upsert(t.Context(), []store.Card{
		{ID: id, Date: "2026-04-25", Source: "mail", Rel: "high", Title: "test card", CreatedAt: time.Now()},
	}))
}

func buildHandler(db *gorm.DB, evLog logp.Writer) (*echo.Echo, *store.CardRepo) {
	cards := &store.CardRepo{DB: db}
	reg := NewRegistry()
	reg.Register("dismiss", &DismissExec{Cards: cards})
	reg.Register("snooze", &SnoozeExec{Cards: cards})
	reg.Register("open_url", &OpenURLExec{})
	e := echo.New()
	(&Handler{
		Cards:    cards,
		Registry: reg,
		EventLog: evLog,
		TZ:       func() *time.Location { return time.UTC },
		Now:      func() time.Time { return time.Date(2026, 4, 25, 8, 0, 0, 0, time.UTC) },
		Log:      quietLog(),
	}).Register(e)
	return e, cards
}

func postAction(e *echo.Echo, id, action string) *httptest.ResponseRecorder {
	body := `{"action":"` + action + `"}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/cards/"+id+"/action", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(rr, req)
	return rr
}

func TestActionHandler_Dismiss(t *testing.T) {
	db := openTestDB(t)
	evLog := logtest.NewMemReader()
	e, cards := buildHandler(db, evLog)
	seedCard(t, cards, "card-1")

	rr := postAction(e, "card-1", "dismiss")
	require.Equal(t, http.StatusOK, rr.Code)
	var result map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.Equal(t, true, result["ok"])
	require.Equal(t, true, result["hide"])

	// Card must be gone from the list.
	rows, err := cards.ListByDate(t.Context(), "2026-04-25")
	require.NoError(t, err)
	require.Empty(t, rows)

	// Event log must record the action.
	events := evLog.Events()
	require.Len(t, events, 1)
	require.Equal(t, logp.KindUserActionTaken, events[0].Kind)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(events[0].Payload, &payload))
	require.Equal(t, "card-1", payload["card_id"])
	require.Equal(t, "dismiss", payload["intent"])
	require.Equal(t, "dismiss", payload["action"])
}

func TestActionHandler_Snooze(t *testing.T) {
	db := openTestDB(t)
	evLog := logtest.NewMemReader()
	e, cards := buildHandler(db, evLog)
	seedCard(t, cards, "card-2")

	rr := postAction(e, "card-2", "snooze")
	require.Equal(t, http.StatusOK, rr.Code)

	// Card must be gone for today.
	rows, err := cards.ListByDate(t.Context(), "2026-04-25")
	require.NoError(t, err)
	require.Empty(t, rows)

	// Event log must record the action.
	events := evLog.Events()
	require.Len(t, events, 1)
	require.Equal(t, logp.KindUserActionTaken, events[0].Kind)
}

func TestActionHandler_CustomActionLogsEvent(t *testing.T) {
	db := openTestDB(t)
	evLog := logtest.NewMemReader()
	e, cards := buildHandler(db, evLog)
	seedCard(t, cards, "card-3")

	rr := postAction(e, "card-3", "reply")
	require.Equal(t, http.StatusOK, rr.Code)

	// Card still visible — custom actions don't change DB status.
	// (Phase 0 has no Executor for draft_reply yet, so the handler
	// falls through to the log-only branch.)
	rows, err := cards.ListByDate(t.Context(), "2026-04-25")
	require.NoError(t, err)
	require.Len(t, rows, 1)

	// Event still logged. The legacy "reply" label is folded into the
	// canonical draft_reply intent for the audit row's intent field;
	// the legacy "action" key preserves the original lowercased label
	// so dashboards keyed off it keep working.
	events := evLog.Events()
	require.Len(t, events, 1)
	var p map[string]any
	require.NoError(t, json.Unmarshal(events[0].Payload, &p))
	require.Equal(t, "draft_reply", p["intent"])
	require.Equal(t, "reply", p["action"])
}

func TestActionHandler_MissingCardLogsAndSucceeds(t *testing.T) {
	// Reactive ask cards are not persisted — acting on them must log the
	// event and return 204 without a DB hit. A 404 would surface in the UI
	// as a failed mutation even though removal is optimistic.
	db := openTestDB(t)
	evLog := logtest.NewMemReader()
	e, _ := buildHandler(db, evLog)

	rr := postAction(e, "reactive-no-row", "dismiss")
	require.Equal(t, http.StatusOK, rr.Code)

	events := evLog.Events()
	require.Len(t, events, 1)
	require.Equal(t, logp.KindUserActionTaken, events[0].Kind)
	var p map[string]any
	require.NoError(t, json.Unmarshal(events[0].Payload, &p))
	require.Equal(t, "reactive-no-row", p["card_id"])
	require.Equal(t, "dismiss", p["intent"])
}

func TestActionHandler_MissingAction(t *testing.T) {
	db := openTestDB(t)
	e, cards := buildHandler(db, logtest.NewMemReader())
	seedCard(t, cards, "card-5")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/cards/card-5/action", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(rr, req)
	require.Equal(t, http.StatusBadRequest, rr.Code)
}
