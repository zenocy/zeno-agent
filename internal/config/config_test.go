package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLoad_Defaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("server:\n  port: 7777\nauth:\n  enabled: false\n"), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)

	require.Equal(t, "127.0.0.1", cfg.Server.Bind)
	require.Equal(t, 7777, cfg.Server.Port)
	require.Equal(t, "info", cfg.Logging.Level)
	require.Equal(t, "./data/zeno.db", cfg.DB.Path)
	require.Equal(t, "0 7 * * *", cfg.Schedule.MorningCron)
	require.Equal(t, "0 12,16 * * *", cfg.Schedule.RefreshCron)
	require.Equal(t, 6*time.Hour, cfg.Synth.AskCardTTL)
}

func TestLoad_AskCardTTLOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("server:\n  port: 7777\nauth:\n  enabled: false\nsynth:\n  ask_card_ttl: 30m\n"), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)

	require.Equal(t, 30*time.Minute, cfg.Synth.AskCardTTL)
}

func TestLoad_EnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("server:\n  port: 7777\nauth:\n  enabled: false\n"), 0o600))

	t.Setenv("ZENO_SERVER_PORT", "9999")
	t.Setenv("ZENO_LLM_MODEL", "qwen3:6b")

	cfg, err := Load(path)
	require.NoError(t, err)

	require.Equal(t, 9999, cfg.Server.Port)
	require.Equal(t, "qwen3:6b", cfg.LLM.Model)
}

func TestProjectionsDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("server:\n  port: 7777\nauth:\n  enabled: false\n"), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)

	require.Equal(t, 45, cfg.Projections.RunWindowMinMinutes)
	require.Equal(t, 25.0, cfg.Projections.RunWindowMaxWindKmh)
	require.Equal(t, 6, cfg.Projections.RunWindowEarliestHour)
	require.Equal(t, 20, cfg.Projections.RunWindowLatestHour)
	require.Equal(t, 20, cfg.Projections.OpenThreadsMax)
	require.Equal(t, 14, cfg.Projections.LookbackDays)
}

func TestProjectionsEnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("server:\n  port: 7777\nauth:\n  enabled: false\n"), 0o600))

	t.Setenv("ZENO_PROJECTIONS_RUN_WINDOW_MIN_MINUTES", "30")
	t.Setenv("ZENO_PROJECTIONS_OPEN_THREADS_MAX", "5")

	cfg, err := Load(path)
	require.NoError(t, err)

	require.Equal(t, 30, cfg.Projections.RunWindowMinMinutes)
	require.Equal(t, 5, cfg.Projections.OpenThreadsMax)
}

func TestServiceTierValidation(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string // substring; "" = expect success
	}{
		{
			name: "empty defaults pass",
			yaml: "auth:\n  enabled: false\n",
		},
		{
			name: "flex background tier accepted",
			yaml: "auth:\n  enabled: false\nllm:\n  service_tier_background: flex\n",
		},
		{
			name: "priority interactive tier accepted",
			yaml: "auth:\n  enabled: false\nllm:\n  service_tier_interactive: priority\n",
		},
		{
			name: "default tier accepted on both",
			yaml: "auth:\n  enabled: false\nllm:\n  service_tier_background: default\n  service_tier_interactive: default\n",
		},
		{
			name:    "unknown background tier rejected",
			yaml:    "auth:\n  enabled: false\nllm:\n  service_tier_background: turbo\n",
			wantErr: "service_tier_background",
		},
		{
			name:    "unknown interactive tier rejected",
			yaml:    "auth:\n  enabled: false\nllm:\n  service_tier_interactive: blazing\n",
			wantErr: "service_tier_interactive",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			require.NoError(t, os.WriteFile(path, []byte("server:\n  port: 7777\n"+tc.yaml), 0o600))
			_, err := Load(path)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestMetricsDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("server:\n  port: 7777\nauth:\n  enabled: false\n"), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)

	require.True(t, cfg.Metrics.Enabled)
	require.True(t, cfg.Metrics.SnapshotEnabled)
	require.Equal(t, 60, cfg.Metrics.SnapshotIntervalSec)
	require.Equal(t, 200, cfg.Metrics.SlowQueryThresholdMs)
	require.Equal(t, 500, cfg.Metrics.HTTPSlowMs)
}

func TestAuthConfig_Defaults_RequireCredentials(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// Bare config: defaults make auth.enabled=true, so a missing username/
	// password must be a hard error to prevent silently-unauthed boots.
	require.NoError(t, os.WriteFile(path, []byte("server:\n  port: 7777\n"), 0o600))

	_, err := Load(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "auth.enabled=true")
}

func TestAuthConfig_DisabledSkipsValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("auth:\n  enabled: false\n"), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)
	require.False(t, cfg.Auth.Enabled)
	require.Equal(t, "zeno_session", cfg.Auth.CookieName)
	require.Equal(t, 720*time.Hour, cfg.Auth.SessionTTL)
}

func TestAuthConfig_LoadsCredentials(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := "auth:\n  enabled: true\n  username: alice\n  password_hash: \"$2a$10$dummyhashforparsingonly\"\n  session_ttl: 24h\n  cookie_secure: true\ndb:\n  path: " + filepath.Join(dir, "zeno.db") + "\n"
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)
	require.True(t, cfg.Auth.Enabled)
	require.Equal(t, "alice", cfg.Auth.Username)
	require.Equal(t, 24*time.Hour, cfg.Auth.SessionTTL)
	require.True(t, cfg.Auth.CookieSecure)
}

func TestAuthConfig_SessionSecretAutoGenerated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	hash := "$2a$04$" + "abcdefghijklmnopqrstuv" // bcrypt-shaped placeholder; not validated at load
	yaml := "auth:\n  enabled: true\n  username: alice\n  password_hash: \"" + hash + "\"\ndb:\n  path: " + filepath.Join(dir, "zeno.db") + "\n"
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))

	cfg1, err := Load(path)
	require.NoError(t, err)
	require.NotEmpty(t, cfg1.Auth.SessionSecret, "secret should be auto-generated on first boot")

	keyPath := filepath.Join(dir, "session.key")
	persisted, err := os.ReadFile(keyPath)
	require.NoError(t, err, "secret must be persisted next to the DB")
	require.NotEmpty(t, persisted)

	// Second load reuses the same secret so existing cookies survive
	// restarts; this is the property gormstore relies on.
	cfg2, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, cfg1.Auth.SessionSecret, cfg2.Auth.SessionSecret)
}

func TestAuthConfig_DisabledDoesNotWriteKeyFile(t *testing.T) {
	dir := t.TempDir()
	cwd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("auth:\n  enabled: false\n"), 0o600))

	_, err = Load(path)
	require.NoError(t, err)

	// With auth off, no session.key should be written anywhere — the
	// disabled-mode rollback path is supposed to be inert.
	_, statErr := os.Stat(filepath.Join(dir, "data", "session.key"))
	require.True(t, os.IsNotExist(statErr), "disabled mode must not create data/session.key")
}

func TestMetricsEnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("server:\n  port: 7777\nauth:\n  enabled: false\n"), 0o600))

	t.Setenv("ZENO_METRICS_SNAPSHOT_INTERVAL_SEC", "10")
	t.Setenv("ZENO_METRICS_HTTP_SLOW_MS", "1000")
	t.Setenv("ZENO_METRICS_ENABLED", "false")

	cfg, err := Load(path)
	require.NoError(t, err)

	require.False(t, cfg.Metrics.Enabled)
	require.Equal(t, 10, cfg.Metrics.SnapshotIntervalSec)
	require.Equal(t, 1000, cfg.Metrics.HTTPSlowMs)
}
