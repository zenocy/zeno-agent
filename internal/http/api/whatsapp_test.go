package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/store"
	"github.com/zenocy/zeno-v2/internal/whatsapp"
	"github.com/zenocy/zeno-v2/internal/whatsapp/whatsapptest"
)

type waTestHarness struct {
	e    *echo.Echo
	svc  *whatsapp.Service
	repo *store.WhatsAppConfigRepo
	fake *whatsapptest.FakeClient
}

// buildWAHandler wires a Service backed by a FakeClient and a real
// SQLite-backed config repo so handler tests cover the full path.
func buildWAHandler(t *testing.T, hasSession bool) *waTestHarness {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_journal_mode=WAL&_foreign_keys=on"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	repo := &store.WhatsAppConfigRepo{DB: db}
	require.NoError(t, repo.Migrate())

	fake := whatsapptest.New()
	if hasSession {
		fake.SetSession("9@s.whatsapp.net", "Jamie")
	}
	factory := func(_ context.Context) (whatsapp.Client, error) { return fake, nil }
	svc := whatsapp.NewService(whatsapp.ServiceConfig{Enabled: true}, factory, quietHandlerEntry())
	require.NoError(t, svc.Start(context.Background()))

	e := echo.New()
	(&WhatsAppHandler{
		Service:     svc,
		Repo:        repo,
		Log:         quietHandlerEntry(),
		PairTimeout: 2 * time.Second,
	}).Register(e)

	return &waTestHarness{e: e, svc: svc, repo: repo, fake: fake}
}

func TestWhatsAppHandler_StatusReportsLiveState(t *testing.T) {
	h := buildWAHandler(t, true)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/whatsapp/status", nil)
	h.e.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	var got statusDTO
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	assert.True(t, got.Enabled)
	assert.True(t, got.HasSession)
	assert.Equal(t, "9@s.whatsapp.net", got.OwnJID)
	assert.Equal(t, "zeno", got.Config.MentionName)
	assert.Equal(t, 3000, got.Config.MinChatIntervalMs)
}

func TestWhatsAppHandler_PairStreamsQRAndCloses(t *testing.T) {
	h := buildWAHandler(t, false)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/whatsapp/pair", nil)
	done := make(chan struct{})
	go func() {
		h.e.ServeHTTP(rr, req)
		close(done)
	}()

	// Drive the pair flow: emit two QR codes then success.
	time.Sleep(50 * time.Millisecond)
	h.fake.InjectQR(whatsapp.QREvent{Event: "code", Code: "qr1"})
	h.fake.InjectQR(whatsapp.QREvent{Event: "code", Code: "qr2"})
	h.fake.SetSession("12@s.whatsapp.net", "Jamie")
	h.fake.InjectQR(whatsapp.QREvent{Event: "success"})
	h.fake.CloseQR()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pair handler did not return")
	}

	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, "event: code")
	assert.Contains(t, body, "qr1")
	assert.Contains(t, body, "qr2")
	assert.Contains(t, body, "event: success")
}

func TestWhatsAppHandler_PairRejectsAlreadyPaired(t *testing.T) {
	h := buildWAHandler(t, true)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/whatsapp/pair", nil)
	h.e.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusConflict, rr.Code)
}

func TestWhatsAppHandler_UnlinkReturns204(t *testing.T) {
	h := buildWAHandler(t, true)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/whatsapp/unlink", nil)
	h.e.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusNoContent, rr.Code)
	assert.False(t, h.svc.Status().HasSession)
}

func TestWhatsAppHandler_ConfigRoundtrip(t *testing.T) {
	h := buildWAHandler(t, true)

	body := strings.NewReader(`{
		"mention_name":"zeno",
		"allowed_dms":["1@s.whatsapp.net","2@s.whatsapp.net"],
		"min_chat_interval_ms":2000,
		"max_concurrent_synth":2,
		"per_chat_buffer":3
	}`)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/whatsapp/config", body)
	req.Header.Set("Content-Type", "application/json")
	h.e.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	rt := h.svc.RuntimeConfig()
	assert.Equal(t, 2*time.Second, rt.MinChatInterval)
	assert.Equal(t, []string{"1@s.whatsapp.net", "2@s.whatsapp.net"}, rt.AllowedDMs)

	row, err := h.repo.Get(context.Background())
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, 2000, row.MinChatIntervalMs)
}

// TestWhatsAppHandler_ConfigNormalizesPlusPrefixed verifies V2.13.3c:
// `+`-prefixed JIDs (the natural form from CardDAV phone fields) are
// accepted by the PUT and normalized to the canonical digit-only form
// before storage. This unblocks the Settings UI: previously the regex
// validation rejected any `+` and the operator couldn't save an
// allowlist entry sourced from a vCard.
func TestWhatsAppHandler_ConfigNormalizesPlusPrefixed(t *testing.T) {
	h := buildWAHandler(t, true)

	body := strings.NewReader(`{
		"mention_name":"zeno",
		"allowed_dms":["+447700900333@s.whatsapp.net","+447700900111","447700900333@s.whatsapp.net"],
		"min_chat_interval_ms":3000,
		"max_concurrent_synth":4,
		"per_chat_buffer":4
	}`)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/whatsapp/config", body)
	req.Header.Set("Content-Type", "application/json")
	h.e.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	rt := h.svc.RuntimeConfig()
	// Both `+`-prefixed forms normalize; the duplicate of the same
	// number (with and without @-suffix) collapses.
	assert.Equal(t, []string{
		"447700900333@s.whatsapp.net",
		"447700900111@s.whatsapp.net",
	}, rt.AllowedDMs)

	// Persisted in canonical form too — round-trip clean.
	row, err := h.repo.Get(context.Background())
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, []string{
		"447700900333@s.whatsapp.net",
		"447700900111@s.whatsapp.net",
	}, row.AllowedDMs())
}

func TestWhatsAppHandler_ConfigRejectsInvalidInput(t *testing.T) {
	h := buildWAHandler(t, true)
	cases := []struct {
		name string
		body string
	}{
		{"bad jid", `{"mention_name":"zeno","allowed_dms":["not a jid"],"min_chat_interval_ms":3000,"max_concurrent_synth":4,"per_chat_buffer":4}`},
		{"too small interval", `{"mention_name":"zeno","allowed_dms":[],"min_chat_interval_ms":500,"max_concurrent_synth":4,"per_chat_buffer":4}`},
		{"bad mention", `{"mention_name":"!!!","allowed_dms":[],"min_chat_interval_ms":3000,"max_concurrent_synth":4,"per_chat_buffer":4}`},
		{"too high concurrency", `{"mention_name":"zeno","allowed_dms":[],"min_chat_interval_ms":3000,"max_concurrent_synth":99,"per_chat_buffer":4}`},
		{"buffer too small", `{"mention_name":"zeno","allowed_dms":[],"min_chat_interval_ms":3000,"max_concurrent_synth":4,"per_chat_buffer":0}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPut, "/api/whatsapp/config", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			h.e.ServeHTTP(rr, req)
			assert.Equal(t, http.StatusBadRequest, rr.Code, "body=%s", tc.body)
		})
	}
}

// TestWhatsAppHandler_SSELines does a finer-grained check on the SSE
// frame structure to catch regressions in the framing.
func TestWhatsAppHandler_SSELines(t *testing.T) {
	h := buildWAHandler(t, false)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/whatsapp/pair", nil)
	go func() {
		time.Sleep(30 * time.Millisecond)
		h.fake.InjectQR(whatsapp.QREvent{Event: "code", Code: "abc"})
		h.fake.SetSession("12@s.whatsapp.net", "")
		h.fake.InjectQR(whatsapp.QREvent{Event: "success"})
		h.fake.CloseQR()
	}()
	h.e.ServeHTTP(rr, req)

	scanner := bufio.NewScanner(strings.NewReader(rr.Body.String()))
	events := []string{}
	for scanner.Scan() {
		line := scanner.Text()
		if after, ok := strings.CutPrefix(line, "event: "); ok {
			events = append(events, after)
		}
	}
	assert.Contains(t, events, "code")
	assert.Contains(t, events, "success")
}
