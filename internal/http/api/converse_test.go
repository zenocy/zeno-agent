package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	"github.com/zenocy/zeno-v2/internal/llm"
	"github.com/zenocy/zeno-v2/internal/log/logtest"
	"github.com/zenocy/zeno-v2/internal/store"
	"github.com/zenocy/zeno-v2/internal/synth"
)

func seedTestCard(t *testing.T, cards *store.CardRepo) {
	t.Helper()
	require.NoError(t, cards.Upsert(context.Background(), []store.Card{{
		ID: "saru-redline", Date: "2026-05-09", Source: "mail", Rel: "high",
		SrcLabel: "Email · Acuity", Title: "Saru Patel · re: redline",
		Sub:       "Walked the redline with Lin. Two questions remain.",
		Meta:      datatypes.JSON([]byte(`["06:14","·","thread of 7"]`)),
		Actions:   datatypes.JSON([]byte(`[{"label":"Reply"}]`)),
		RunID:     "run-1",
		CreatedAt: time.Now(),
	}}))
}

func buildConverseHandler(t *testing.T, fn func(ctx context.Context, card synth.PinnedCard, prior []synth.PriorTurn, query string) (synth.SubCard, llm.Trace, error)) (*echo.Echo, *store.CardRepo, *store.ConversationRepo) {
	t.Helper()
	db := openHandlerTestDB(t)
	cards := &store.CardRepo{DB: db}
	traces := &store.TraceRepo{DB: db}
	conv := &store.ConversationRepo{DB: db}

	e := echo.New()
	(&ConverseHandler{
		Cards:         cards,
		Conversations: conv,
		Traces:        traces,
		ConverseFn:    fn,
		EventLog:      logtest.NewMemReader(),
		TZ:            tzUTC,
		Now:           func() time.Time { return time.Date(2026, 5, 9, 8, 0, 0, 0, time.UTC) },
		Log:           quietHandlerEntry(),
	}).Register(e)
	return e, cards, conv
}

func postConverse(e *echo.Echo, cardID, query string) *httptest.ResponseRecorder {
	body := `{"query":"` + query + `"}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/cards/"+cardID+"/converse", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(rr, req)
	return rr
}

func getThread(e *echo.Echo, cardID string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/cards/"+cardID+"/thread", nil)
	e.ServeHTTP(rr, req)
	return rr
}

func TestConverseHandler_FirstPostCreatesThreadAndAppendsTurn(t *testing.T) {
	e, cards, _ := buildConverseHandler(t, func(_ context.Context, _ synth.PinnedCard, _ []synth.PriorTurn, q string) (synth.SubCard, llm.Trace, error) {
		return synth.SubCard{
			ID: "sub-1", Kind: "answer", Eyebrow: "answer",
			Title: "Echo: " + q, Body: "Stub reply.",
			Actions: []synth.Action{{Label: "Done"}},
		}, llm.Trace{Stopped: "ok", TotalMs: 100}, nil
	})
	seedTestCard(t, cards)

	rr := postConverse(e, "saru-redline", "What did Aria say?")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var turn turnDTO
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &turn))
	require.Equal(t, 0, turn.Position)
	require.Equal(t, "What did Aria say?", turn.Prompt)
	require.Equal(t, "answer", turn.Reply.Kind)
	require.NotEmpty(t, turn.TraceID)
}

func TestConverseHandler_ThreadGETReturnsTurnsInOrder(t *testing.T) {
	e, cards, _ := buildConverseHandler(t, func(_ context.Context, _ synth.PinnedCard, prior []synth.PriorTurn, q string) (synth.SubCard, llm.Trace, error) {
		return synth.SubCard{
			ID: "sub", Kind: "answer", Eyebrow: "answer",
			Title: "Reply " + q, Body: "n=" + intToString(len(prior)),
			Actions: []synth.Action{{Label: "Done"}},
		}, llm.Trace{}, nil
	})
	seedTestCard(t, cards)

	require.Equal(t, http.StatusOK, postConverse(e, "saru-redline", "first").Code)
	require.Equal(t, http.StatusOK, postConverse(e, "saru-redline", "second").Code)

	rr := getThread(e, "saru-redline")
	require.Equal(t, http.StatusOK, rr.Code)
	var resp threadResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Equal(t, "saru-redline", resp.CardID)
	require.Len(t, resp.Turns, 2)
	require.Equal(t, "first", resp.Turns[0].Prompt)
	require.Equal(t, "second", resp.Turns[1].Prompt)
	require.Equal(t, 0, resp.Turns[0].Position)
	require.Equal(t, 1, resp.Turns[1].Position)
}

func TestConverseHandler_PriorTurnsPassedToConverseFn(t *testing.T) {
	var seen []synth.PriorTurn
	e, cards, _ := buildConverseHandler(t, func(_ context.Context, _ synth.PinnedCard, prior []synth.PriorTurn, q string) (synth.SubCard, llm.Trace, error) {
		seen = append([]synth.PriorTurn(nil), prior...)
		return synth.SubCard{
			ID: "sub", Kind: "answer", Eyebrow: "answer",
			Title: "Reply " + q, Body: "OK",
			Actions: []synth.Action{{Label: "Done"}},
		}, llm.Trace{}, nil
	})
	seedTestCard(t, cards)

	require.Equal(t, http.StatusOK, postConverse(e, "saru-redline", "first").Code)
	require.Equal(t, http.StatusOK, postConverse(e, "saru-redline", "second").Code)

	require.Len(t, seen, 1, "second call sees one prior turn")
	require.Equal(t, "first", seen[0].Prompt)
	require.Equal(t, "answer", seen[0].Reply.Kind)
}

func TestConverseHandler_404OnMissingCard(t *testing.T) {
	e, _, _ := buildConverseHandler(t, func(_ context.Context, _ synth.PinnedCard, _ []synth.PriorTurn, _ string) (synth.SubCard, llm.Trace, error) {
		t.Fatal("ConverseFn must not be called when card is missing")
		return synth.SubCard{}, llm.Trace{}, nil
	})

	require.Equal(t, http.StatusNotFound, postConverse(e, "nope", "anything").Code)
	require.Equal(t, http.StatusNotFound, getThread(e, "nope").Code)
}

func TestConverseHandler_BlankQueryRejected(t *testing.T) {
	e, cards, _ := buildConverseHandler(t, func(_ context.Context, _ synth.PinnedCard, _ []synth.PriorTurn, _ string) (synth.SubCard, llm.Trace, error) {
		t.Fatal("ConverseFn must not be called for blank query")
		return synth.SubCard{}, llm.Trace{}, nil
	})
	seedTestCard(t, cards)

	require.Equal(t, http.StatusBadRequest, postConverse(e, "saru-redline", "").Code)
}

func TestConverseHandler_AnchorIDsBypassCardLookup(t *testing.T) {
	// The design's left-rail Calendar / Tasks clicks open the focus
	// modal with synthetic anchor IDs ("calendar_day", "tasks_view"),
	// not real cards. The handler should accept these and synthesize
	// a virtual PinnedCard.
	for _, anchor := range []struct {
		id    string
		title string
		src   string
	}{
		{"calendar_day", "Today's calendar", "Calendar · today"},
		{"tasks_view", "Tasks", "Tasks · all"},
	} {
		var seen synth.PinnedCard
		e, _, _ := buildConverseHandler(t, func(_ context.Context, card synth.PinnedCard, _ []synth.PriorTurn, q string) (synth.SubCard, llm.Trace, error) {
			seen = card
			return synth.SubCard{
				ID: "sub", Kind: "answer", Eyebrow: "answer",
				Title: "Reply " + q, Body: "OK",
				Actions: []synth.Action{{Label: "Done"}},
			}, llm.Trace{}, nil
		})
		// Note: no seedTestCard — the anchor must work without a real
		// card row in the store.
		require.Equal(t, http.StatusOK, postConverse(e, anchor.id, "what's on?").Code)
		require.Equal(t, anchor.id, seen.ID)
		require.Equal(t, anchor.title, seen.Title)
		require.Equal(t, anchor.src, seen.SrcLabel)

		// Thread GET should also succeed (the handler must skip the
		// 404 for anchor IDs).
		rr := getThread(e, anchor.id)
		require.Equal(t, http.StatusOK, rr.Code, anchor.id)
	}
}

func TestConverseHandler_PinnedCardSurfacedToConverseFn(t *testing.T) {
	var seen synth.PinnedCard
	e, cards, _ := buildConverseHandler(t, func(_ context.Context, card synth.PinnedCard, _ []synth.PriorTurn, q string) (synth.SubCard, llm.Trace, error) {
		seen = card
		return synth.SubCard{
			ID: "sub", Kind: "answer", Eyebrow: "answer",
			Title: "Reply " + q, Body: "OK",
			Actions: []synth.Action{{Label: "Done"}},
		}, llm.Trace{}, nil
	})
	seedTestCard(t, cards)

	require.Equal(t, http.StatusOK, postConverse(e, "saru-redline", "test").Code)
	require.Equal(t, "saru-redline", seen.ID)
	require.Equal(t, "Saru Patel · re: redline", seen.Title)
	require.Equal(t, "Email · Acuity", seen.SrcLabel)
	require.Contains(t, seen.Meta, "06:14")
}

