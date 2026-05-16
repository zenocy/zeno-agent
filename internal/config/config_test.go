package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoad_Defaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("server:\n  port: 7777\n"), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)

	require.Equal(t, "127.0.0.1", cfg.Server.Bind)
	require.Equal(t, 7777, cfg.Server.Port)
	require.Equal(t, "info", cfg.Logging.Level)
	require.Equal(t, "./data/zeno.db", cfg.DB.Path)
	require.Equal(t, "0 7 * * *", cfg.Schedule.MorningCron)
	require.Equal(t, "0 12,16 * * *", cfg.Schedule.RefreshCron)
}

func TestLoad_EnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("server:\n  port: 7777\n"), 0o600))

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
	require.NoError(t, os.WriteFile(path, []byte("server:\n  port: 7777\n"), 0o600))

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
	require.NoError(t, os.WriteFile(path, []byte("server:\n  port: 7777\n"), 0o600))

	t.Setenv("ZENO_PROJECTIONS_RUN_WINDOW_MIN_MINUTES", "30")
	t.Setenv("ZENO_PROJECTIONS_OPEN_THREADS_MAX", "5")

	cfg, err := Load(path)
	require.NoError(t, err)

	require.Equal(t, 30, cfg.Projections.RunWindowMinMinutes)
	require.Equal(t, 5, cfg.Projections.OpenThreadsMax)
}

func TestMetricsDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("server:\n  port: 7777\n"), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)

	require.True(t, cfg.Metrics.Enabled)
	require.True(t, cfg.Metrics.SnapshotEnabled)
	require.Equal(t, 60, cfg.Metrics.SnapshotIntervalSec)
	require.Equal(t, 200, cfg.Metrics.SlowQueryThresholdMs)
	require.Equal(t, 500, cfg.Metrics.HTTPSlowMs)
}

func TestMetricsEnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("server:\n  port: 7777\n"), 0o600))

	t.Setenv("ZENO_METRICS_SNAPSHOT_INTERVAL_SEC", "10")
	t.Setenv("ZENO_METRICS_HTTP_SLOW_MS", "1000")
	t.Setenv("ZENO_METRICS_ENABLED", "false")

	cfg, err := Load(path)
	require.NoError(t, err)

	require.False(t, cfg.Metrics.Enabled)
	require.Equal(t, 10, cfg.Metrics.SnapshotIntervalSec)
	require.Equal(t, 1000, cfg.Metrics.HTTPSlowMs)
}
