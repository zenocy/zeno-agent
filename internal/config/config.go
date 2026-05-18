// Package config loads the Zeno configuration from a YAML file with
// environment-variable overrides. Single-user, single-process — there is one
// Config per binary.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config is the full Zeno configuration.
type Config struct {
	Server      ServerConfig      `mapstructure:"server"`
	Auth        AuthConfig        `mapstructure:"auth"`
	Logging     LoggingConfig     `mapstructure:"logging"`
	DB          DBConfig          `mapstructure:"db"`
	LLM         LLMConfig         `mapstructure:"llm"`
	Sensors     SensorsConfig     `mapstructure:"sensors"`
	Schedule    ScheduleConfig    `mapstructure:"schedule"`
	Projections ProjectionsConfig `mapstructure:"projections"`
	Synth       SynthConfig       `mapstructure:"synth"`
	Memory      MemoryConfig      `mapstructure:"memory"`
	Concerns    ConcernsConfig    `mapstructure:"concerns"`
	Reminders   RemindersConfig   `mapstructure:"reminders"`
	Metrics     MetricsConfig     `mapstructure:"metrics"`
	Web         WebConfig         `mapstructure:"web"`
}

// AuthConfig configures the cookie-based login that gates the browser UI
// and the /api/* surface. Single user — credentials are kept in YAML.
//
// When Enabled is false the server falls back to the pre-V2.14 behavior
// (loopback-only or LANToken bearer); flip Enabled=false to recover access
// if a hash is mistyped.
//
// SessionSecret is the HMAC key gorilla/sessions uses to sign the cookie's
// session-id payload. If empty at startup, a random 32-byte key is written
// to <db-dir>/session.key and reused on subsequent boots so existing
// sessions survive a restart.
type AuthConfig struct {
	Enabled       bool          `mapstructure:"enabled"`        // default true
	Username      string        `mapstructure:"username"`
	PasswordHash  string        `mapstructure:"password_hash"`  // bcrypt; produced by `zeno hash-password`
	SessionTTL    time.Duration `mapstructure:"session_ttl"`    // default 720h (30 days)
	CookieName    string        `mapstructure:"cookie_name"`    // default "zeno_session"
	CookieSecure  bool          `mapstructure:"cookie_secure"`  // set true behind TLS
	SessionSecret string        `mapstructure:"session_secret"` // empty → auto-generated and persisted
}

// RemindersConfig controls how the V2.9 reminder sweeper dispatches
// fired reminders. The sweeper itself runs unconditionally when a
// reminder store + injector are wired; this block only governs the
// outbound channels. Both knobs default off-ish in a way that
// preserves the V2.8 behavior for upgraders: InjectEnabled defaults
// true (existing card-injection path) and WhatsApp dispatch defaults
// off until WhatsAppEnabled is flipped.
type RemindersConfig struct {
	WhatsAppEnabled bool   `mapstructure:"whatsapp_enabled"` // gate for WhatsApp send
	WhatsAppTo      string `mapstructure:"whatsapp_to"`      // recipient JID (e.g. "447xxxxxxxxx@s.whatsapp.net"); empty disables even when Enabled
	InjectEnabled   bool   `mapstructure:"inject_enabled"`   // default true; preserves V2.8 inject-pipeline path
}

// WebConfig groups external web-tool settings. Currently only Jina; if
// other vendors are added later (Brave Search, Perplexity, etc.) they
// nest as siblings under Web.
type WebConfig struct {
	Jina JinaConfig `mapstructure:"jina"`
}

// JinaConfig configures the Jina AI Reader + Search client and the
// scheduled saved-search sensor. APIKey is required when SavedSearches
// is non-empty AND for the LLM-tool path; the boot path treats the
// missing-key-with-saved-searches combination as a fatal misconfig.
type JinaConfig struct {
	APIKey        string        `mapstructure:"api_key"`
	BaseURL       string        `mapstructure:"base_url"`        // default https://r.jina.ai
	SearchBaseURL string        `mapstructure:"search_base_url"` // default https://s.jina.ai
	TimeoutSec    int           `mapstructure:"timeout_sec"`     // default 20
	SearchTTLSec  int           `mapstructure:"search_ttl_sec"`  // default 21600 (6h)
	ReadTTLSec    int           `mapstructure:"read_ttl_sec"`    // default 86400 (24h)
	MaxResults    int           `mapstructure:"max_results"`     // default 5
	SavedSearches []SavedSearch `mapstructure:"saved_searches"`  // sensor input; empty disables the sensor
}

// SavedSearch is one query the Jina sensor refreshes on each SyncCron
// tick (gated by the cache TTL — see internal/sensor/jina). Name must
// be unique; Site is optional and maps to the X-Site header.
type SavedSearch struct {
	Name  string `mapstructure:"name"`
	Query string `mapstructure:"query"`
	Site  string `mapstructure:"site"`
}

// MetricsConfig holds operational-visibility tunables.
//
// /api/metrics is gated by the same lan_token bearer that protects the rest
// of /api/*, so Prometheus scrapers must send the bearer header.
//
// SnapshotIntervalSec drives the periodic stats.snapshot event written to
// the observation log; the UI Stats panel reads the latest such row and may
// lag by up to one tick. Prometheus is the source of truth for percentiles.
type MetricsConfig struct {
	Enabled              bool `mapstructure:"enabled"`                 // default true
	SnapshotEnabled      bool `mapstructure:"snapshot_enabled"`        // default true
	SnapshotIntervalSec  int  `mapstructure:"snapshot_interval_sec"`   // default 60
	SlowQueryThresholdMs int  `mapstructure:"slow_query_threshold_ms"` // default 200
	HTTPSlowMs           int  `mapstructure:"http_slow_ms"`            // default 500
}

// ConcernsConfig holds tunables for the V2.5.0 Concerns subsystem. The
// recognition + retrospective constants live in the synth package today;
// this block exposes the operator-facing knobs that change behavior at
// runtime. AutoRetireDays is read by both the recognition retirement
// survey and the projection-derived `ready_to_retire` DTO field — both
// must use one source of truth.
type ConcernsConfig struct {
	AutoRetireDays int `mapstructure:"auto_retire_days"` // default 90
}

// MemoryConfig holds the relevance-ranking knobs for derived memory facts.
// Defaults preserve the V2.2.0 baseline when RerankEnabled is false.
type MemoryConfig struct {
	EmbedderEndpoint string  `mapstructure:"embedder_endpoint"`   // empty → falls back to LLM.Endpoint
	EmbedderModel    string  `mapstructure:"embedder_model"`      // default "nomic-embed-text-v2-moe" (768-dim, MoE variant)
	EmbedderAPIKey   string  `mapstructure:"embedder_api_key"`    // empty → falls back to LLM.APIKey
	EmbedderTimeout  int     `mapstructure:"embedder_timeout_ms"` // default 3000
	EmbedderDims     int     `mapstructure:"embedder_dims"`       // 0 → model's native dims; >0 sent as the OpenAI "dimensions" parameter for Matryoshka-style models (e.g. Qwen3-Embedding-8B up to 4096, text-embedding-3-small/large)
	RerankEnabled    bool    `mapstructure:"rerank_enabled"`      // default true
	RerankWSim       float32 `mapstructure:"rerank_w_sim"`        // default 1.0
	RerankWConf      float32 `mapstructure:"rerank_w_conf"`       // default 0.3
	RerankMinScore   float32 `mapstructure:"rerank_min_score"`    // default 0.0
	ReactivePool     int     `mapstructure:"reactive_pool"`       // default 30
	CardsPool        int     `mapstructure:"cards_pool"`          // default 50
}

// SynthConfig holds per-stage timeout budgets and synthesizer-loop tunables.
// All durations are in seconds and surface as ZENO_SYNTH_<KEY> env overrides.
type SynthConfig struct {
	CardsTimeoutSec       int `mapstructure:"cards_timeout_sec"`       // default 30
	BriefingTimeoutSec    int `mapstructure:"briefing_timeout_sec"`    // default 45
	CronBudgetSec         int `mapstructure:"cron_budget_sec"`         // default 90 (cards + briefing + margin)
	ToolTimeoutSec        int `mapstructure:"tool_timeout_sec"`        // default 5 (per individual tool execution)
	CardsMaxIterations    int `mapstructure:"cards_max_iterations"`    // default 6 (LLM calls in the cards tool loop)
	ReactiveMaxIterations int `mapstructure:"reactive_max_iterations"` // default 4 (LLM calls in the reactive tool loop)
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Bind            string `mapstructure:"bind"`      // "127.0.0.1" by default
	Port            int    `mapstructure:"port"`      // 7777 by default
	LANToken        string `mapstructure:"lan_token"` // empty = loopback only
	ReadTimeoutSec  int    `mapstructure:"read_timeout_sec"`
	WriteTimeoutSec int    `mapstructure:"write_timeout_sec"`
	ShutdownSec     int    `mapstructure:"shutdown_sec"`
}

// LoggingConfig is logrus setup.
type LoggingConfig struct {
	Level  string `mapstructure:"level"`  // "info" by default
	Format string `mapstructure:"format"` // "text" or "json"
}

// DBConfig holds SQLite settings.
type DBConfig struct {
	Path string `mapstructure:"path"` // "./data/zeno.db"
}

// LLMConfig points at an OpenAI-compatible endpoint.
type LLMConfig struct {
	Endpoint            string `mapstructure:"endpoint"`                 // e.g. http://host.docker.internal:11434/v1
	APIKey              string `mapstructure:"api_key"`                  // optional
	Model               string `mapstructure:"model"`                    // e.g. "gemma3:4b" or whatever
	TimeoutSec          int    `mapstructure:"timeout_sec"`              // default 120 (per-HTTP-call transport timeout — must exceed any per-stage deadline below)
	ReactiveDeadlineSec int    `mapstructure:"reactive_deadline_sec"`    // deadline for reactive Ask loop; default 45
	RetryMaxAttempts    int    `mapstructure:"retry_max_attempts"`       // default 3 (network-level retry attempts)
	RetryInitialBackoff int    `mapstructure:"retry_initial_backoff_ms"` // default 250 (ms; doubles each attempt with ±20% jitter)
	RetryMaxBackoff     int    `mapstructure:"retry_max_backoff_ms"`     // default 8000 (ms; ceiling per backoff sleep)
	JSONSchemaMode      string `mapstructure:"json_schema_mode"`         // "auto" | "on" | "off"; default "off" until Phase 0 probe
	MaxTokens           int    `mapstructure:"max_tokens"`               // default 0 = no cap (let the upstream provider decide). The previous 16384 was a guardrail for local llama.cpp/LM Studio runs that would happily run away on long prompts; remote providers like Gemini cap themselves and the explicit cap was strangling repair calls in thinking mode (16K tokens of reasoning + truncated JSON).
	NoThink             bool   `mapstructure:"no_think"`                 // when true, prepend "/no_think" to the briefing system prompt on Qwen3 models; cuts briefing latency ~2min → ~30s. Default false; flip on after measuring voice with `make eval`.
}

// SensorsConfig groups all sensor settings.
type SensorsConfig struct {
	IMAP     IMAPConfig     `mapstructure:"imap"`
	SMTP     SMTPConfig     `mapstructure:"smtp"`
	CalDAV   CalDAVConfig   `mapstructure:"caldav"`
	CardDAV  CardDAVConfig  `mapstructure:"carddav"`
	Weather  WeatherConfig  `mapstructure:"weather"`
	WhatsApp WhatsAppConfig `mapstructure:"whatsapp"`
}

// WhatsAppConfig configures the V2.7 WhatsApp integration. Despite
// living under "sensors" for grouping symmetry, WhatsApp is NOT a
// polling sensor — see internal/whatsapp/types.go. The block enables /
// disables the integration and points at the per-account session file.
//
// All other knobs (allowlist, mention name, throttling) live in the
// `whatsapp_config` SQLite table managed via the Settings UI; storing
// them in YAML would require restarts on every allowlist change and
// duplicate the singleton pattern used for app_settings.
type WhatsAppConfig struct {
	Enabled bool   `mapstructure:"enabled"`
	DBPath  string `mapstructure:"db_path"` // default "./data/whatsapp.db"

	// V2.12: outbound proactive sends (the user-initiated `send_whatsapp`
	// action surface). DailySendCap bounds how many proactive messages
	// Zeno will dispatch in a single user-tz day; 0 means no cap. The
	// receive-reply path (synth_handler) is not counted against this cap
	// because it's a 1:1 response to a message the user/contact sent first.
	DailySendCap int `mapstructure:"daily_send_cap"` // default 50
}

// IMAPConfig is one IMAP account.
//
// V2.8.0 added DraftsFolder + AllowedMoveFolders. The action surface
// uses DraftsFolder as the target for `draft_reply` / `forward` /
// `send_reply` (which always saves a copy of the sent message); the
// allowlist gates `move_mail` so the LLM can't emit a folder name
// that is unrecoverable for the user (typo, non-existent path, or
// nested IMAP namespace they don't realize their server uses).
type IMAPConfig struct {
	Host               string   `mapstructure:"host"`
	Port               int      `mapstructure:"port"`
	Username           string   `mapstructure:"username"`
	Password           string   `mapstructure:"password"`
	TLS                string   `mapstructure:"tls"` // "implicit" or "starttls"
	Folders            []string `mapstructure:"folders"`
	DraftsFolder       string   `mapstructure:"drafts_folder"`        // default "Drafts"
	SentFolder         string   `mapstructure:"sent_folder"`          // default "Sent"
	AllowedMoveFolders []string `mapstructure:"allowed_move_folders"` // default ["Inbox","Archive","Trash"]
}

// SMTPConfig is the single SMTP account used by the V2.8.0 action
// surface for `send_reply` / `forward`. Empty Host disables SMTP and
// the corresponding Executors degrade to draft-only behavior (see
// internal/action/executors_mail.go).
type SMTPConfig struct {
	Host     string `mapstructure:"host"`
	Port     int    `mapstructure:"port"`
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password"`
	TLS      string `mapstructure:"tls"`  // "implicit" | "starttls"; default "starttls"
	From     string `mapstructure:"from"` // RFC 5322 From header; defaults to Username
}

// CalDAVConfig is one CalDAV calendar.
type CalDAVConfig struct {
	URL      string `mapstructure:"url"`
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password"`
}

// CardDAVConfig configures the V2.12 CardDAV contacts sensor used to
// resolve WhatsApp recipients by name. Disabled by default — the
// `send_whatsapp` action falls back to manual memory-fact contacts
// (groups, or person facts created without a CardDAV link) when no
// CardDAV source is configured.
//
// URL is the server root (e.g. https://carddav.fastmail.com/); the
// principal/home-set discovery handles the address-book path lookup.
// Username/Password are basic-auth credentials — Fastmail accepts an
// app-specific password.
type CardDAVConfig struct {
	Enabled  bool   `mapstructure:"enabled"`
	URL      string `mapstructure:"url"`
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password"`
	SyncSec  int    `mapstructure:"sync_sec"` // poll cadence; default 300 (5min)
}

// WeatherConfig configures the Open-Meteo lookup.
//
// Latitude/Longitude/Timezone are DEPRECATED at runtime: they're now
// managed via the Settings UI and persisted in the app_settings table.
// They survive in YAML solely as a one-time seed for upgrades — on first
// boot, if no settings row exists and these YAML keys are present, the
// boot path migrates them into the DB and logs a warning. New installs
// should leave them empty and set values via the Settings UI instead.
type WeatherConfig struct {
	Latitude  float64 `mapstructure:"latitude"`
	Longitude float64 `mapstructure:"longitude"`
	Timezone  string  `mapstructure:"timezone"` // e.g. "America/Los_Angeles"
}

// ScheduleConfig holds cron expressions.
//
// RefreshCron fires the same synth pass as MorningCron at additional times
// of day, so the briefing/state register tracks the user's actual daypart
// instead of remaining frozen on the 07:00 morning_calm computation.
// Default `0 12,16 * * *` lights up at noon and 16:00 to flip into
// deep_work / end_of_day registers respectively. Cron strings here are
// evaluated in the server's local time (robfig/cron with no explicit
// location); align with the user's timezone if the server runs elsewhere.
// Set to "" to disable mid-day re-runs and keep the 07:00-only behavior.
type ScheduleConfig struct {
	MorningCron string `mapstructure:"morning_cron"` // e.g. "0 7 * * *"
	RefreshCron string `mapstructure:"refresh_cron"` // e.g. "0 12,16 * * *"; "" disables
	SyncCron    string `mapstructure:"sync_cron"`    // e.g. "*/10 * * * *"
}

// ProjectionsConfig holds tunables for the read-side projections.
type ProjectionsConfig struct {
	RunWindowMinMinutes   int     `mapstructure:"run_window_min_minutes"`   // 45
	RunWindowMaxWindKmh   float64 `mapstructure:"run_window_max_wind_kmh"`  // 25
	RunWindowEarliestHour int     `mapstructure:"run_window_earliest_hour"` // 6
	RunWindowLatestHour   int     `mapstructure:"run_window_latest_hour"`   // 20
	OpenThreadsMax        int     `mapstructure:"open_threads_max"`         // 20
	LookbackDays          int     `mapstructure:"lookback_days"`            // 14
}

// Load reads configuration from path. Environment variables prefixed ZENO_
// override file values; nested keys use ZENO_SERVER_PORT etc.
func Load(path string) (*Config, error) {
	v := viper.New()
	v.SetConfigFile(path)

	v.SetEnvPrefix("ZENO")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	setDefaults(v)

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if err := finalizeAuth(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// finalizeAuth validates the AuthConfig and bootstraps the session secret.
// Split out from Load so tests can exercise it on a hand-built Config.
//
// When auth is disabled we skip the secret bootstrap entirely — no session
// middleware will be mounted, so a key on disk would be dead weight (and
// would surprise users with a `data/session.key` file they didn't ask for).
func finalizeAuth(cfg *Config) error {
	if !cfg.Auth.Enabled {
		return nil
	}
	if cfg.Auth.Username == "" || cfg.Auth.PasswordHash == "" {
		return fmt.Errorf("auth.enabled=true requires auth.username and auth.password_hash (run `zeno hash-password` to generate the hash)")
	}
	if cfg.Auth.SessionSecret == "" {
		secret, err := loadOrCreateSessionSecret(cfg.DB.Path)
		if err != nil {
			return fmt.Errorf("auth: bootstrap session secret: %w", err)
		}
		cfg.Auth.SessionSecret = secret
	}
	return nil
}

// loadOrCreateSessionSecret reads <db-dir>/session.key, creating it with
// a fresh 32-byte hex-encoded secret on first boot. Persisting the key on
// disk lets the cookie HMAC survive restarts so users stay logged in.
func loadOrCreateSessionSecret(dbPath string) (string, error) {
	dir := filepath.Dir(dbPath)
	if dir == "" || dir == "." {
		dir = "./data"
	}
	keyPath := filepath.Join(dir, "session.key")
	if b, err := os.ReadFile(keyPath); err == nil {
		s := strings.TrimSpace(string(b))
		if s != "" {
			return s, nil
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	secret := hex.EncodeToString(raw)
	if err := os.WriteFile(keyPath, []byte(secret+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", keyPath, err)
	}
	return secret, nil
}

func setDefaults(v *viper.Viper) {
	v.SetDefault("server.bind", "127.0.0.1")
	v.SetDefault("server.port", 7777)
	v.SetDefault("server.read_timeout_sec", 30)
	v.SetDefault("server.write_timeout_sec", 0) // 0 = no server-level timeout; route-level deadlines apply
	v.SetDefault("server.shutdown_sec", 10)

	v.SetDefault("auth.enabled", true)
	v.SetDefault("auth.session_ttl", "720h") // 30 days
	v.SetDefault("auth.cookie_name", "zeno_session")
	v.SetDefault("auth.cookie_secure", false)

	v.SetDefault("logging.level", "info")
	v.SetDefault("logging.format", "text")

	v.SetDefault("db.path", "./data/zeno.db")

	v.SetDefault("llm.endpoint", "http://host.docker.internal:11434/v1")
	v.SetDefault("llm.model", "gemma3:4b")
	v.SetDefault("llm.timeout_sec", 120)
	v.SetDefault("llm.reactive_deadline_sec", 45)
	v.SetDefault("llm.retry_max_attempts", 3)
	v.SetDefault("llm.retry_initial_backoff_ms", 250)
	v.SetDefault("llm.retry_max_backoff_ms", 8000)
	v.SetDefault("llm.json_schema_mode", "off")
	v.SetDefault("llm.max_tokens", 0) // 0 → upstream default, no client-imposed cap

	v.SetDefault("synth.cards_timeout_sec", 30)
	v.SetDefault("synth.briefing_timeout_sec", 45)
	v.SetDefault("synth.cron_budget_sec", 90)
	v.SetDefault("synth.tool_timeout_sec", 5)
	v.SetDefault("synth.cards_max_iterations", 6)
	v.SetDefault("synth.reactive_max_iterations", 4)

	v.SetDefault("schedule.morning_cron", "0 7 * * *")
	v.SetDefault("schedule.refresh_cron", "0 12,16 * * *")
	v.SetDefault("schedule.sync_cron", "*/10 * * * *")

	v.SetDefault("projections.run_window_min_minutes", 45)
	v.SetDefault("projections.run_window_max_wind_kmh", 25.0)
	v.SetDefault("projections.run_window_earliest_hour", 6)
	v.SetDefault("projections.run_window_latest_hour", 20)
	v.SetDefault("projections.open_threads_max", 20)
	v.SetDefault("projections.lookback_days", 14)

	v.SetDefault("memory.embedder_model", "nomic-embed-text-v2-moe")
	v.SetDefault("memory.embedder_timeout_ms", 3000)
	v.SetDefault("memory.rerank_enabled", true)
	v.SetDefault("memory.rerank_w_sim", 1.0)
	v.SetDefault("memory.rerank_w_conf", 0.3)
	v.SetDefault("memory.rerank_min_score", 0.0)
	v.SetDefault("memory.reactive_pool", 30)
	v.SetDefault("memory.cards_pool", 50)

	v.SetDefault("concerns.auto_retire_days", 90)

	v.SetDefault("reminders.inject_enabled", true)

	v.SetDefault("web.jina.base_url", "https://r.jina.ai")
	v.SetDefault("web.jina.search_base_url", "https://s.jina.ai")
	v.SetDefault("web.jina.timeout_sec", 20)
	v.SetDefault("web.jina.search_ttl_sec", 21600) // 6h
	v.SetDefault("web.jina.read_ttl_sec", 86400)   // 24h
	v.SetDefault("web.jina.max_results", 5)

	v.SetDefault("sensors.whatsapp.enabled", false)
	v.SetDefault("sensors.whatsapp.db_path", "./data/whatsapp.db")
	v.SetDefault("sensors.whatsapp.daily_send_cap", 50)

	v.SetDefault("sensors.carddav.enabled", false)
	v.SetDefault("sensors.carddav.sync_sec", 300)

	// V2.8.0: action surface. Folder-name defaults are tuned for
	// Fastmail; users on other providers override under sensors.imap.*.
	v.SetDefault("sensors.imap.drafts_folder", "Drafts")
	v.SetDefault("sensors.imap.sent_folder", "Sent")
	v.SetDefault("sensors.imap.allowed_move_folders", []string{"Inbox", "Archive", "Trash"})
	v.SetDefault("sensors.smtp.tls", "starttls")

	v.SetDefault("metrics.enabled", true)
	v.SetDefault("metrics.snapshot_enabled", true)
	v.SetDefault("metrics.snapshot_interval_sec", 60)
	v.SetDefault("metrics.slow_query_threshold_ms", 200)
	v.SetDefault("metrics.http_slow_ms", 500)
}
