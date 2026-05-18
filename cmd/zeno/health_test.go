package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const minimalConfig = `
server:
  bind: 127.0.0.1
  port: 7777
db:
  path: ./data/zeno.db
llm:
  endpoint: http://localhost:11434/v1
  model: gemma3:4b
sensors:
  weather:
    timezone: UTC
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

func TestHealthMain_OKResponseExitsZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"db_ok":true,"llm_reachable":true}`))
	}))
	defer srv.Close()

	cfgPath := writeConfig(t, minimalConfig)
	var buf bytes.Buffer
	rc := healthMain([]string{"-config", cfgPath, "-addr", srv.URL}, &buf)
	require.Equal(t, 0, rc, "stderr: %s", buf.String())
}

func TestHealthMain_LLMDownStillExitsZero(t *testing.T) {
	// Per the policy in internal/http/api/health.go, LLM unreachable does not
	// flip ok=false. The probe must mirror that.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"db_ok":true,"llm_reachable":false,"llm_error":"connection refused"}`))
	}))
	defer srv.Close()

	cfgPath := writeConfig(t, minimalConfig)
	var buf bytes.Buffer
	rc := healthMain([]string{"-config", cfgPath, "-addr", srv.URL}, &buf)
	require.Equal(t, 0, rc)
}

func TestHealthMain_DBDownExitsOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"db_ok":false}`))
	}))
	defer srv.Close()

	cfgPath := writeConfig(t, minimalConfig)
	var buf bytes.Buffer
	rc := healthMain([]string{"-config", cfgPath, "-addr", srv.URL}, &buf)
	require.Equal(t, 1, rc)
	require.Contains(t, buf.String(), "ok=false")
}

func TestHealthMain_HTTPErrorExitsOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "broken", http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfgPath := writeConfig(t, minimalConfig)
	var buf bytes.Buffer
	rc := healthMain([]string{"-config", cfgPath, "-addr", srv.URL}, &buf)
	require.Equal(t, 1, rc)
	require.Contains(t, buf.String(), "500")
}

func TestHealthMain_UnreachableExitsOne(t *testing.T) {
	cfgPath := writeConfig(t, minimalConfig)
	var buf bytes.Buffer
	// Pick a port nothing is listening on.
	rc := healthMain([]string{"-config", cfgPath, "-addr", "http://127.0.0.1:1", "-timeout", "1"}, &buf)
	require.Equal(t, 1, rc)
	require.Contains(t, strings.ToLower(buf.String()), "unreachable")
}

func TestHealthMain_SendsBearerTokenWhenConfigured(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"ok":true,"db_ok":true}`))
	}))
	defer srv.Close()

	cfg := `
server:
  bind: 127.0.0.1
  port: 7777
  lan_token: secret-xyz
auth:
  enabled: false
db:
  path: ./data/zeno.db
llm:
  endpoint: http://localhost:11434/v1
  model: gemma3:4b
sensors:
  weather:
    timezone: UTC
`
	cfgPath := writeConfig(t, cfg)

	// Important: even with --addr override the token must still be read from
	// config and attached. healthURL() uses --addr only for the URL, not
	// auth, so the token comes from cfgPath.
	rc := healthMain([]string{"-config", cfgPath, "-addr", srv.URL}, &bytes.Buffer{})
	require.Equal(t, 0, rc)
	require.Equal(t, "Bearer secret-xyz", gotAuth)
}
