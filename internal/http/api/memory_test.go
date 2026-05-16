package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/log/logtest"
	"github.com/zenocy/zeno-v2/internal/store"
)

func buildMemoryHandler(t *testing.T) (*echo.Echo, *store.MemoryRepo) {
	t.Helper()
	db := openHandlerTestDB(t)
	repo := &store.MemoryRepo{DB: db}
	e := echo.New()
	(&MemoryHandler{
		Memory:   repo,
		EventLog: logtest.NewMemReader(),
		TZ:       tzUTC,
		Now:      func() time.Time { return time.Date(2026, 4, 25, 8, 0, 0, 0, time.UTC) },
		Log:      quietHandlerEntry(),
	}).Register(e)
	return e, repo
}

func doMemoryRequest(t *testing.T, e *echo.Echo, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	e.ServeHTTP(rr, req)
	return rr
}

func TestMemory_GET_ReturnsList(t *testing.T) {
	e, repo := buildMemoryHandler(t)
	now := time.Now()
	require.NoError(t, repo.Insert(t.Context(), store.MemoryFact{
		ID: "partner-aaaa", Subject: "partner", Fact: "Partner is Sam.",
		Category: "relationship", Confidence: "high", Source: "user",
		EvidenceCount: 1, FirstSeen: now, LastReinforced: now,
	}))

	rr := doMemoryRequest(t, e, http.MethodGet, "/api/memory", "")
	require.Equal(t, http.StatusOK, rr.Code)

	var resp listResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Len(t, resp.Facts, 1)
	require.Equal(t, "partner", resp.Facts[0].Subject)
	require.Equal(t, "high", resp.Facts[0].Confidence)
	require.Equal(t, "user", resp.Facts[0].Source)
}

func TestMemory_POST_CreatesFact(t *testing.T) {
	e, repo := buildMemoryHandler(t)
	body := `{"subject":"partner","fact":"Partner is Sam.","category":"relationship"}`
	rr := doMemoryRequest(t, e, http.MethodPost, "/api/memory", body)
	require.Equal(t, http.StatusCreated, rr.Code)

	var got memoryFactDTO
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	require.Equal(t, "partner", got.Subject)
	require.Equal(t, "high", got.Confidence, "user-source manual add inserts at high confidence")
	require.Equal(t, "user", got.Source)
	require.Equal(t, 1, got.EvidenceCount)
	require.NotEmpty(t, got.ID)

	// Confirm the row landed in the store via the repo (not via the API,
	// to avoid GET-via-list masking a write bug).
	stored, err := repo.GetByID(t.Context(), got.ID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.Equal(t, "Partner is Sam.", stored.Fact)
}

func TestMemory_POST_ConflictOnExistingSubject(t *testing.T) {
	e, _ := buildMemoryHandler(t)
	body := `{"subject":"partner","fact":"Partner is Sam.","category":"relationship"}`
	require.Equal(t, http.StatusCreated, doMemoryRequest(t, e, http.MethodPost, "/api/memory", body).Code)

	rr := doMemoryRequest(t, e, http.MethodPost, "/api/memory", body)
	require.Equal(t, http.StatusConflict, rr.Code, "second add of same subject must conflict")

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Equal(t, "subject already exists", resp["error"])
}

func TestMemory_POST_400OnInvalidCategory(t *testing.T) {
	e, _ := buildMemoryHandler(t)
	body := `{"subject":"partner","fact":"Partner is Sam.","category":"weather"}`
	rr := doMemoryRequest(t, e, http.MethodPost, "/api/memory", body)
	require.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestMemory_POST_400OnSubjectWithSpaces(t *testing.T) {
	e, _ := buildMemoryHandler(t)
	body := `{"subject":"my partner","fact":"x","category":"relationship"}`
	rr := doMemoryRequest(t, e, http.MethodPost, "/api/memory", body)
	require.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestMemory_PATCH_UpdatesFact(t *testing.T) {
	e, repo := buildMemoryHandler(t)
	now := time.Now()
	require.NoError(t, repo.Insert(t.Context(), store.MemoryFact{
		ID: "anniversary-aa", Subject: "anniversary", Fact: "Anniversary is May 7.",
		Category: "identity", Confidence: "high", Source: "user", EvidenceCount: 1,
		FirstSeen: now, LastReinforced: now,
	}))

	body := `{"fact":"Anniversary is May 8."}`
	rr := doMemoryRequest(t, e, http.MethodPatch, "/api/memory/anniversary-aa", body)
	require.Equal(t, http.StatusOK, rr.Code)

	var got memoryFactDTO
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	require.Equal(t, "Anniversary is May 8.", got.Fact)

	stored, _ := repo.GetByID(t.Context(), "anniversary-aa")
	require.Equal(t, "Anniversary is May 8.", stored.Fact)
}

func TestMemory_PATCH_404OnUnknownID(t *testing.T) {
	e, _ := buildMemoryHandler(t)
	body := `{"fact":"new"}`
	rr := doMemoryRequest(t, e, http.MethodPatch, "/api/memory/no-such-id", body)
	require.Equal(t, http.StatusNotFound, rr.Code)
}

func TestMemory_PATCH_400WhenNothingToChange(t *testing.T) {
	e, repo := buildMemoryHandler(t)
	now := time.Now()
	require.NoError(t, repo.Insert(t.Context(), store.MemoryFact{
		ID: "x", Subject: "x", Fact: "x",
		Category: "misc", Confidence: "low", Source: "synth", EvidenceCount: 1,
		FirstSeen: now, LastReinforced: now,
	}))
	rr := doMemoryRequest(t, e, http.MethodPatch, "/api/memory/x", `{}`)
	require.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestMemory_DELETE_Idempotent(t *testing.T) {
	e, repo := buildMemoryHandler(t)
	now := time.Now()
	require.NoError(t, repo.Insert(t.Context(), store.MemoryFact{
		ID: "runs-aa", Subject: "runs", Fact: "Runs Tue/Thu.",
		Category: "routine", Confidence: "med", Source: "synth", EvidenceCount: 3,
		FirstSeen: now, LastReinforced: now,
	}))

	rr1 := doMemoryRequest(t, e, http.MethodDelete, "/api/memory/runs-aa", "")
	require.Equal(t, http.StatusNoContent, rr1.Code)

	rr2 := doMemoryRequest(t, e, http.MethodDelete, "/api/memory/runs-aa", "")
	require.Equal(t, http.StatusNoContent, rr2.Code, "second DELETE must still return 204")
}

func TestMemory_DELETE_FactGoneFromList(t *testing.T) {
	e, repo := buildMemoryHandler(t)
	now := time.Now()
	require.NoError(t, repo.Insert(t.Context(), store.MemoryFact{
		ID: "dinner-aa", Subject: "dinner", Fact: "Otto's is the spot.",
		Category: "preference", Confidence: "low", Source: "synth", EvidenceCount: 1,
		FirstSeen: now, LastReinforced: now,
	}))
	require.NoError(t, repo.Insert(t.Context(), store.MemoryFact{
		ID: "partner-aa", Subject: "partner", Fact: "Partner is Sam.",
		Category: "relationship", Confidence: "high", Source: "user", EvidenceCount: 1,
		FirstSeen: now, LastReinforced: now,
	}))

	require.Equal(t, http.StatusNoContent, doMemoryRequest(t, e, http.MethodDelete, "/api/memory/dinner-aa", "").Code)

	rr := doMemoryRequest(t, e, http.MethodGet, "/api/memory", "")
	require.Equal(t, http.StatusOK, rr.Code)
	var resp listResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Len(t, resp.Facts, 1, "soft-deleted row must not surface in GET")
	require.Equal(t, "partner", resp.Facts[0].Subject)
}
