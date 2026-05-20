// Command zeno is the single-binary entrypoint for the Zeno V2 service.
//
// Subcommands:
//
//	zeno serve         — start the daemon (default)
//	zeno replay        — re-run synth against a historical date for prompt iteration
//	zeno memory export — dump derived memory to JSON / CSV (V2.2.0)
//	zeno concerns      — inspect / dismiss concerns (V2.5.0): list, show, dismiss
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	gsessions "github.com/gorilla/sessions"
	"github.com/sirupsen/logrus"
	"github.com/wader/gormstore/v2"
	"gorm.io/gorm"

	"github.com/zenocy/zeno-v2/internal/action"
	"github.com/zenocy/zeno-v2/internal/clock"
	"github.com/zenocy/zeno-v2/internal/config"
	"github.com/zenocy/zeno-v2/internal/embeddings"
	"github.com/zenocy/zeno-v2/internal/eventbus"
	httpserver "github.com/zenocy/zeno-v2/internal/http"
	"github.com/zenocy/zeno-v2/internal/http/api"
	httpmw "github.com/zenocy/zeno-v2/internal/http/middleware"
	"github.com/zenocy/zeno-v2/internal/injectsub"
	"github.com/zenocy/zeno-v2/internal/jina"
	"github.com/zenocy/zeno-v2/internal/llm"
	_ "github.com/zenocy/zeno-v2/internal/llm/gemini" // registers the Gemini provider with llm.New
	logp "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/metrics"
	"github.com/zenocy/zeno-v2/internal/projection"
	"github.com/zenocy/zeno-v2/internal/replycard"
	"github.com/zenocy/zeno-v2/internal/schedule"
	"github.com/zenocy/zeno-v2/internal/sensor"
	caldavsensor "github.com/zenocy/zeno-v2/internal/sensor/caldav"
	carddavsensor "github.com/zenocy/zeno-v2/internal/sensor/carddav"
	"github.com/zenocy/zeno-v2/internal/sensor/geocode"
	imapsensor "github.com/zenocy/zeno-v2/internal/sensor/imap"
	jinasensor "github.com/zenocy/zeno-v2/internal/sensor/jina"
	smtpsensor "github.com/zenocy/zeno-v2/internal/sensor/smtp"
	stocksensor "github.com/zenocy/zeno-v2/internal/sensor/stock"
	weathersensor "github.com/zenocy/zeno-v2/internal/sensor/weather"
	"github.com/zenocy/zeno-v2/internal/settings"
	"github.com/zenocy/zeno-v2/internal/store"
	"github.com/zenocy/zeno-v2/internal/synth"
	"github.com/zenocy/zeno-v2/internal/whatsapp"
)

func main() {
	args := os.Args[1:]
	sub := "serve"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		sub = args[0]
		args = args[1:]
	}

	switch sub {
	case "serve":
		runServe(args)
	case "replay":
		runReplay(args)
	case "health":
		runHealth(args)
	case "memory":
		runMemory(args)
	case "concerns":
		runConcerns(args)
	case "hash-password":
		runHashPassword(args)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q. Use 'serve', 'replay', 'memory', 'concerns', 'hash-password', or 'health'.\n", sub)
		os.Exit(2)
	}
}

// bootArgs are the shared flags both subcommands accept.
type bootArgs struct {
	configPath string
	dbPath     string
	promptsDir string
}

func parseBootFlags(fs *flag.FlagSet, args []string) bootArgs {
	cfgPath := fs.String("config", "config.yaml", "path to config.yaml")
	dbPath := fs.String("db", "", "SQLite database path (overrides config db.path)")
	promptsDir := fs.String("prompts", "", "directory holding _voice.md + templates (default: embedded)")
	_ = fs.Parse(args)
	return bootArgs{
		configPath: *cfgPath,
		dbPath:     *dbPath,
		promptsDir: *promptsDir,
	}
}

// bootCommon loads config, opens the DB, sets up logger and timezone, and
// runs the synth migrations. Both serve and replay use this.
type bootContext struct {
	cfg    *config.Config
	logger *logrus.Logger
	db     *gorm.DB
	store  logp.Store
	// clk is the canonical Clock for date-bound code. Production wraps
	// settingsSvc so every Now()/Location() call observes live TZ edits.
	// Replay swaps in a *clock.Fixed pinned to --as-of so projections
	// compute against the historical wall clock.
	clk          clock.Clock
	settingsSvc  *settings.Service
	settingsRepo *store.SettingsRepo
	llm          llm.Provider
	metrics      *metrics.Metrics // nil when cfg.Metrics.Enabled is false
}

func boot(ba bootArgs) *bootContext {
	cfg, err := config.Load(ba.configPath)
	if err != nil {
		logrus.WithError(err).Fatal("load config")
	}
	if ba.dbPath != "" {
		cfg.DB.Path = ba.dbPath
	}

	logger := logp.SetupLogging(logp.LoggingConfig{
		Level:  cfg.Logging.Level,
		Format: cfg.Logging.Format,
	})
	logger.WithFields(logrus.Fields{"version": api.Version}).Info("zeno booting")

	var mtx *metrics.Metrics
	if cfg.Metrics.Enabled {
		m, err := metrics.New()
		if err != nil {
			logger.WithError(err).Fatal("metrics: register collectors")
		}
		mtx = m
	}

	if dir := filepath.Dir(cfg.DB.Path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			logger.WithError(err).Fatal("create db directory")
		}
	}

	gormCfg := &gorm.Config{}
	if mtx != nil && cfg.Metrics.SlowQueryThresholdMs > 0 {
		threshold := time.Duration(cfg.Metrics.SlowQueryThresholdMs) * time.Millisecond
		gormCfg.Logger = store.NewSlowQueryLogger(
			logger.WithField("c", "db"),
			threshold,
			mtx.ObserveDBQuery,
		)
	}
	db, st, err := logp.OpenWith(cfg.DB.Path, gormCfg)
	if err != nil {
		logger.WithError(err).Fatal("open observation log")
	}
	if err := synth.Migrate(db, true, true); err != nil {
		logger.WithError(err).Fatal("migrate synth tables")
	}

	settingsRepo := &store.SettingsRepo{DB: db}
	settingsSvc := settings.New(settingsRepo)
	if err := settingsSvc.Load(context.Background()); err != nil {
		logger.WithError(err).Fatal("load app settings")
	}

	// Legacy YAML migration: if the user upgraded from a pre-Settings-UI
	// build their config.yaml may still carry sensors.weather.{lat,lon,tz}.
	// Seed the DB row from those values once, then nudge them to clean up.
	if !settingsSvc.Snapshot().Set {
		w := cfg.Sensors.Weather
		if w.Latitude != 0 || w.Longitude != 0 || w.Timezone != "" {
			if err := settingsRepo.Upsert(context.Background(), store.AppSettings{
				Timezone: w.Timezone, Latitude: w.Latitude, Longitude: w.Longitude,
			}); err != nil {
				logger.WithError(err).Warn("settings: legacy YAML seed failed")
			} else if err := settingsSvc.Reload(context.Background()); err == nil {
				logger.WithFields(logrus.Fields{
					"timezone": w.Timezone, "latitude": w.Latitude, "longitude": w.Longitude,
				}).Warn("settings: seeded app_settings from legacy YAML; remove sensors.weather.{latitude,longitude,timezone} from config.yaml and manage via Settings UI")
			}
		}
	}

	clk := clock.NewReal(settingsSvc)

	retryPolicy := llm.RetryPolicy{
		MaxAttempts:    cfg.LLM.RetryMaxAttempts,
		InitialBackoff: time.Duration(cfg.LLM.RetryInitialBackoff) * time.Millisecond,
		MaxBackoff:     time.Duration(cfg.LLM.RetryMaxBackoff) * time.Millisecond,
	}
	llmClient, err := llm.New(llm.Config{
		Provider: cfg.LLM.Provider,
		OpenAI: llm.ClientConfig{
			Endpoint:               cfg.LLM.OpenAI.Endpoint,
			APIKey:                 cfg.LLM.OpenAI.APIKey,
			Model:                  cfg.LLM.Model,
			Timeout:                time.Duration(cfg.LLM.TimeoutSec) * time.Second,
			JSONSchemaMode:         cfg.LLM.OpenAI.JSONSchemaMode,
			MaxTokens:              cfg.LLM.MaxTokens,
			NoThink:                cfg.LLM.NoThink,
			StreamSchema:           cfg.LLM.OpenAI.StreamSchema,
			ServiceTierBackground:  cfg.LLM.OpenAI.ServiceTierBackground,
			ServiceTierInteractive: cfg.LLM.OpenAI.ServiceTierInteractive,
			Retry:                  retryPolicy,
		},
		Gemini: llm.GeminiClientConfig{
			APIKey:                   cfg.LLM.Gemini.APIKey,
			Endpoint:                 cfg.LLM.Gemini.Endpoint,
			Model:                    geminiModel(cfg.LLM),
			Timeout:                  time.Duration(cfg.LLM.TimeoutSec) * time.Second,
			Retry:                    retryPolicy,
			MaxTokens:                cfg.LLM.MaxTokens,
			EnableGoogleSearch:       cfg.LLM.Gemini.EnableGoogleSearch,
			ThinkingLevelBackground:  cfg.LLM.Gemini.ThinkingLevelBackground,
			ThinkingLevelInteractive: cfg.LLM.Gemini.ThinkingLevelInteractive,
			IncludeThoughts:          cfg.LLM.Gemini.IncludeThoughts,
			ServiceTierBackground:    cfg.LLM.Gemini.ServiceTierBackground,
			ServiceTierInteractive:   cfg.LLM.Gemini.ServiceTierInteractive,
		},
	})
	if err != nil {
		logger.WithError(err).Fatal("llm: construct provider")
	}
	if mtx != nil {
		llmClient.SetRetryInstrumentation(logger.WithField("c", "llm-retry"), mtx.IncRetry)
	}
	llmClient.SetTrafficLogger(logger.WithField("c", "llm-http"))

	return &bootContext{
		cfg: cfg, logger: logger, db: db, store: st, clk: clk, llm: llmClient,
		settingsSvc:  settingsSvc,
		settingsRepo: settingsRepo,
		metrics:      mtx,
	}
}

// geminiModel returns the Gemini-specific model when set in the
// gemini: sub-block; falls back to the common llm.model otherwise so
// single-provider deployments don't have to repeat themselves.
func geminiModel(cfg config.LLMConfig) string {
	if cfg.Gemini.Model != "" {
		return cfg.Gemini.Model
	}
	return cfg.Model
}

// memoryFactLister adapts *store.MemoryRepo to embeddings.FactLister so the
// warmup walker can read every visible fact without the embeddings package
// importing internal/store.
type memoryFactLister struct{ repo *store.MemoryRepo }

func (m memoryFactLister) ListAllFactRows(ctx context.Context) ([]embeddings.FactRow, error) {
	rows, err := m.repo.ListAllVisible(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]embeddings.FactRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, embeddings.FactRow{ID: r.ID, Fact: r.Fact})
	}
	return out, nil
}

func factListerFromRepo(repo *store.MemoryRepo) embeddings.FactLister {
	return memoryFactLister{repo: repo}
}

// wiredIntentsFromRegistry bridges the action.Registry (which holds the
// live Executors) to a synth.WiredIntent slice the synth package
// consumes. Filters to canonical-vocabulary intents so a stray
// registration doesn't pollute the prompt; reads description/mode
// from action.CanonicalIntents (the source-of-truth catalog).
func wiredIntentsFromRegistry(reg *action.Registry) []synth.WiredIntent {
	if reg == nil {
		return nil
	}
	live := reg.Modes() // map[intent]Mode for everything currently registered
	if len(live) == 0 {
		return nil
	}
	catalog := action.CanonicalIntentMap()
	out := make([]synth.WiredIntent, 0, len(live))
	for intent, mode := range live {
		ci, ok := catalog[intent]
		if !ok {
			continue // unknown intent — don't advertise
		}
		out = append(out, synth.WiredIntent{
			Intent:      intent,
			Mode:        string(mode),
			Description: ci.Description,
		})
	}
	return out
}

// projCfgFromConfig builds the projection.Config from the user's settings.
//
// The supplied Clock is the canonical source for "now" and "user TZ"; both
// fields propagate through every projection and synth call. Production
// passes *clock.Real(settingsSvc) so live edits via PUT /api/settings take
// effect on the next projection compute. Replay passes *clock.Fixed pinned
// to --as-of.
func projCfgFromConfig(cfg *config.Config, clk clock.Clock) projection.Config {
	return projection.Config{
		Clock:                 clk,
		LookbackDays:          cfg.Projections.LookbackDays,
		RunWindowMinMinutes:   cfg.Projections.RunWindowMinMinutes,
		RunWindowMaxWindKmh:   cfg.Projections.RunWindowMaxWindKmh,
		RunWindowEarliestHour: cfg.Projections.RunWindowEarliestHour,
		RunWindowLatestHour:   cfg.Projections.RunWindowLatestHour,
		OpenThreadsMax:        cfg.Projections.OpenThreadsMax,
	}
}

// runServe starts the daemon. The pre-Phase-2 main() body, plus synth
// runner wiring and cards/briefing API.
func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", "config.yaml", "path to config.yaml")
	dbPath := fs.String("db", "", "SQLite database path (overrides config db.path)")
	promptsDir := fs.String("prompts", "", "directory holding _voice.md + templates (default: embedded)")
	// Serve-only flags: override the YAML's network knobs without editing it.
	// Useful for `zeno serve --bind 0.0.0.0 --lan-token <secret>` from a
	// systemd unit or one-off invocation.
	bindFlag := fs.String("bind", "", "override server.bind (e.g. 0.0.0.0)")
	lanTokenFlag := fs.String("lan-token", "", "override server.lan_token (required when bind is non-loopback)")
	_ = fs.Parse(args)

	bc := boot(bootArgs{configPath: *cfgPath, dbPath: *dbPath, promptsDir: *promptsDir})
	if *bindFlag != "" {
		bc.cfg.Server.Bind = *bindFlag
	}
	if *lanTokenFlag != "" {
		bc.cfg.Server.LANToken = *lanTokenFlag
	}

	prompts, err := synth.LoadPrompts(*promptsDir)
	if err != nil {
		bc.logger.WithError(err).Fatal("load prompts")
	}

	// V2.6: Jina web tools. APIKey absent → tools/sensor are not
	// registered (web access disabled). SavedSearches non-empty with
	// no key is a config error: silent disable would be a surprise on
	// a paid API.
	if bc.cfg.Web.Jina.APIKey == "" && len(bc.cfg.Web.Jina.SavedSearches) > 0 {
		bc.logger.Fatal("web.jina.api_key is required when web.jina.saved_searches is non-empty")
	}
	var jinaClient *jina.Client
	var jinaToolClient synth.JinaClient // nil interface when jinaClient is nil — avoids the typed-nil trap
	var jinaStore *jina.Store
	jinaSearchTTL := time.Duration(bc.cfg.Web.Jina.SearchTTLSec) * time.Second
	jinaReadTTL := time.Duration(bc.cfg.Web.Jina.ReadTTLSec) * time.Second
	if bc.cfg.Web.Jina.APIKey != "" {
		jinaClient = jina.NewClient(jina.Config{
			APIKey:        bc.cfg.Web.Jina.APIKey,
			BaseURL:       bc.cfg.Web.Jina.BaseURL,
			SearchBaseURL: bc.cfg.Web.Jina.SearchBaseURL,
			Timeout:       time.Duration(bc.cfg.Web.Jina.TimeoutSec) * time.Second,
		}, nil)
		jinaToolClient = jinaClient
		jinaStore = &jina.Store{DB: bc.db}
		bc.logger.WithFields(map[string]any{
			"max_results":    bc.cfg.Web.Jina.MaxResults,
			"search_ttl_s":   bc.cfg.Web.Jina.SearchTTLSec,
			"read_ttl_s":     bc.cfg.Web.Jina.ReadTTLSec,
			"saved_searches": len(bc.cfg.Web.Jina.SavedSearches),
		}).Info("jina: web tools enabled")
	}

	// V2.12: forward-construct the CardDAV repo so we can append the
	// carddav sensor below alongside the buildSensors result. Migrate is
	// idempotent — calling it twice (here + below near concernRepo) is
	// safe.
	preliminaryCardDAVRepo := &store.CardDAVRepo{DB: bc.db}
	if err := preliminaryCardDAVRepo.Migrate(); err != nil {
		bc.logger.WithError(err).Fatal("migrate carddav_contacts (early)")
	}

	sensors := buildSensors(context.Background(), bc.cfg, bc.settingsSvc, bc.store, bc.logger)
	if bc.cfg.Sensors.CardDAV.Enabled {
		provider, err := carddavsensor.NewFastmail(context.Background(), bc.cfg.Sensors.CardDAV)
		if err != nil {
			bc.logger.WithError(err).Warn("carddav sensor disabled: provider init failed")
		} else {
			sensors = append(sensors, carddavsensor.New(provider, preliminaryCardDAVRepo, bc.logger.WithField("c", "carddav")))
			bc.logger.Info("carddav sensor enabled")
		}
	}
	if jinaClient != nil && len(bc.cfg.Web.Jina.SavedSearches) > 0 {
		sensors = append(sensors, jinasensor.New(
			bc.cfg.Web.Jina.SavedSearches,
			jinaClient,
			jinaStore,
			bc.store,
			jinaSearchTTL,
			bc.cfg.Web.Jina.MaxResults,
			bc.logger.WithField("c", "jina"),
		))
	}

	srvCfg := httpserver.ServerConfig{
		Bind:                   bc.cfg.Server.Bind,
		Port:            bc.cfg.Server.Port,
		ReadTimeout:     time.Duration(bc.cfg.Server.ReadTimeoutSec) * time.Second,
		WriteTimeout:    time.Duration(bc.cfg.Server.WriteTimeoutSec) * time.Second,
		ShutdownTimeout: time.Duration(bc.cfg.Server.ShutdownSec) * time.Second,
		LANToken:        bc.cfg.Server.LANToken,
		HTTPSlowMs:      time.Duration(bc.cfg.Metrics.HTTPSlowMs) * time.Millisecond,
	}
	if bc.metrics != nil {
		srvCfg.MetricsObserver = bc.metrics.ObserveHTTP
		srvCfg.MetricsSlow = bc.metrics.MarkHTTPSlow
	}

	// V2.14 cookie auth. When enabled, mount the gormstore-backed session
	// store and configure the unified auth middleware. The PeriodicCleanup
	// goroutine drops expired session rows; quitCh is closed at shutdown.
	authQuitCh := make(chan struct{})
	if bc.cfg.Auth.Enabled {
		store := gormstore.New(bc.db, []byte(bc.cfg.Auth.SessionSecret))
		store.SessionOpts = &gsessions.Options{
			Path:     "/",
			MaxAge:   int(bc.cfg.Auth.SessionTTL.Seconds()),
			HttpOnly: true,
			Secure:   bc.cfg.Auth.CookieSecure,
			SameSite: http.SameSiteLaxMode,
		}
		store.MaxAge(int(bc.cfg.Auth.SessionTTL.Seconds()))
		go store.PeriodicCleanup(1*time.Hour, authQuitCh)
		srvCfg.SessionStore = store
		srvCfg.Auth = httpmw.AuthConfig{
			Enabled:    true,
			Username:   bc.cfg.Auth.Username,
			CookieName: bc.cfg.Auth.CookieName,
			LANToken:   bc.cfg.Server.LANToken,
			SessionTTL: bc.cfg.Auth.SessionTTL,
		}
		bc.logger.WithField("username", bc.cfg.Auth.Username).Info("auth: cookie login enabled")
	}
	defer close(authQuitCh)

	srv := httpserver.New(srvCfg, bc.logger)

	healthStartedAt := time.Now()
	(&api.HealthHandler{DB: bc.db, LLM: bc.llm, Reader: bc.store, StartedAt: healthStartedAt}).Register(srv.Echo)

	if bc.cfg.Auth.Enabled {
		(&api.AuthHandler{
			Username:     bc.cfg.Auth.Username,
			PasswordHash: bc.cfg.Auth.PasswordHash,
			CookieName:   bc.cfg.Auth.CookieName,
			Log:          bc.logger.WithField("c", "auth"),
		}).Register(srv.Echo)
	}

	if bc.metrics != nil {
		(&api.MetricsHandler{Metrics: bc.metrics, Reader: bc.store}).Register(srv.Echo)
	}

	projCfg := projCfgFromConfig(bc.cfg, bc.clk)

	// V2.11: tasks repo moved up so the projections handler (and the
	// migration block below) can both read it. Migrate runs the
	// AutoMigrate; MigrateRemindersToTasks (called below) handles the
	// one-shot import of legacy reminders rows.
	taskRepo := &store.TaskRepo{DB: bc.db, Table: "tasks"}
	if err := taskRepo.Migrate(); err != nil {
		bc.logger.WithError(err).Fatal("migrate tasks")
	}

	(&api.ProjectionsHandler{
		Reader:  bc.store,
		Tasks:   taskRepo,
		Cfg:     projCfg,
		Tickers: bc.settingsSvc,
		Log:     bc.logger.WithField("c", "projections"),
	}).Register(srv.Echo)

	cardRepo := &store.CardRepo{DB: bc.db, Table: "cards"}
	briefingRepo := &store.BriefingRepo{DB: bc.db, Table: "briefings"}
	traceRepo := &store.TraceRepo{DB: bc.db, Table: "traces"}
	memoryRepo := &store.MemoryRepo{DB: bc.db, Table: "memory_facts"}

	// V2.11: one-shot import of legacy reminders rows into the tasks
	// table, then DROP TABLE reminders. taskRepo itself was migrated
	// up above so the projections handler can read it.
	if err := store.MigrateRemindersToTasks(context.Background(), bc.db); err != nil {
		bc.logger.WithError(err).Fatal("migrate reminders → tasks")
	}

	// V2.10: card-conversation persistence. One thread per card, append-only
	// turns. Migrate before the ConverseHandler is wired below so the first
	// request finds both tables.
	conversationRepo := &store.ConversationRepo{DB: bc.db}
	if err := conversationRepo.Migrate(); err != nil {
		bc.logger.WithError(err).Fatal("migrate conversations")
	}

	// V2.13.0: assistant-mode reply correlation. ExpectedReplyRepo holds
	// open proactive sends keyed by chat_jid + event UID; resolved when
	// an inbound DM lands within the 24h window. Migrate unconditionally
	// — the repo is a no-op when the user hasn't enabled assistant mode.
	expectedReplyRepo := &store.ExpectedReplyRepo{DB: bc.db}
	if err := expectedReplyRepo.Migrate(); err != nil {
		bc.logger.WithError(err).Fatal("migrate expected_replies")
	}

	embEndpoint := bc.cfg.Memory.EmbedderEndpoint
	if embEndpoint == "" {
		embEndpoint = bc.cfg.LLM.OpenAI.Endpoint
	}
	embAPIKey := bc.cfg.Memory.EmbedderAPIKey
	if embAPIKey == "" {
		embAPIKey = bc.cfg.LLM.OpenAI.APIKey
	}
	embedder := embeddings.NewLMStudioEmbedder(
		embEndpoint,
		bc.cfg.Memory.EmbedderModel,
		embAPIKey,
		time.Duration(bc.cfg.Memory.EmbedderTimeout)*time.Millisecond,
	)
	embedder.Dimensions = bc.cfg.Memory.EmbedderDims
	embedder.Logger = bc.logger.WithField("c", "embed")
	bc.logger.WithFields(logrus.Fields{
		"endpoint":       embEndpoint,
		"model":          bc.cfg.Memory.EmbedderModel,
		"dims":           bc.cfg.Memory.EmbedderDims,
		"timeout_ms":     bc.cfg.Memory.EmbedderTimeout,
		"rerank_enabled": bc.cfg.Memory.RerankEnabled,
	}).Info("embed: configured")
	embStore := &embeddings.Store{DB: bc.db, Table: "memory_embeddings"}
	memIdx := embeddings.NewMemoryIndex(embedder)
	if _, err := embeddings.Warmup(
		context.Background(),
		embStore,
		memIdx,
		factListerFromRepo(memoryRepo),
		bc.logger.WithField("c", "embed-warmup"),
	); err != nil {
		bc.logger.WithError(err).Warn("embeddings warmup failed — ranker will fall back to ListTop ordering")
	}
	if bc.metrics != nil {
		if rows, err := memoryRepo.ListAllVisible(context.Background()); err == nil {
			bc.metrics.SetMemoryFacts(len(rows))
		}
	}

	var ranker *synth.MemoryRanker
	if bc.cfg.Memory.RerankEnabled {
		ranker = &synth.MemoryRanker{
			Index:    memIdx,
			WSim:     bc.cfg.Memory.RerankWSim,
			WConf:    bc.cfg.Memory.RerankWConf,
			MinScore: bc.cfg.Memory.RerankMinScore,
			Logger:   bc.logger.WithField("c", "rerank"),
		}
	}

	(&api.BriefingHandler{
		Repo: briefingRepo,
		TZ:   bc.settingsSvc.TZ,
		Now:  time.Now,
		Log:  bc.logger.WithField("c", "briefing"),
	}).Register(srv.Echo)
	(&api.CardsHandler{
		Cards:  cardRepo,
		Traces: traceRepo,
		TZ:     bc.settingsSvc.TZ,
		Now:    time.Now,
		Log:    bc.logger.WithField("c", "cards"),
	}).Register(srv.Echo)
	(&api.ThreadsHandler{
		Reader: bc.store,
		Now:    time.Now,
		Log:    bc.logger.WithField("c", "threads"),
	}).Register(srv.Echo)

	reactiveDeadline := time.Duration(bc.cfg.LLM.ReactiveDeadlineSec) * time.Second
	cardsTimeout := time.Duration(bc.cfg.Synth.CardsTimeoutSec) * time.Second
	briefingTimeout := time.Duration(bc.cfg.Synth.BriefingTimeoutSec) * time.Second
	cronBudget := time.Duration(bc.cfg.Synth.CronBudgetSec) * time.Second
	toolTimeout := time.Duration(bc.cfg.Synth.ToolTimeoutSec) * time.Second
	finalCallBudget := time.Duration(bc.cfg.Synth.FinalCallBudgetSec) * time.Second

	// V2.4: bus is constructed early so morning Runner, AskHandler, and
	// the V2.3 inject orchestrator all share the same fan-out hub.
	bus := eventbus.New(bc.logger.WithField("c", "eventbus"))
	if bc.metrics != nil {
		bus.WithDropObserver(bc.metrics.IncSSEDropped)
	}

	// V2.5.0: concern repos + retrospective dispatcher are constructed
	// before the AskHandler so the reactive Ask path can register
	// declare_concern with the dispatcher already bound. The dispatcher's
	// parent context is the daemon's lifetime — request cancellation
	// must not abort an in-flight historical-tagging walk.
	concernRepo := &store.ConcernRepo{DB: bc.db, Table: "concerns"}
	concernObsRepo := &store.ConcernObservationRepo{DB: bc.db, Table: "concern_observations"}

	// V2.12: contact + CardDAV repos for the WhatsApp send action
	// surface. Both tables are always migrated so the action handler
	// has somewhere to look; the CardDAV sensor is wired conditionally
	// further down.
	contactLinkRepo := &store.ContactLinkRepo{DB: bc.db}
	if err := contactLinkRepo.Migrate(); err != nil {
		bc.logger.WithError(err).Fatal("migrate contact_links")
	}
	cardDAVRepo := preliminaryCardDAVRepo // already migrated up at the buildSensors call
	contactResolver := &whatsapp.Resolver{
		Memory:  memoryRepo,
		Link:    contactLinkRepo,
		CardDAV: cardDAVRepo,
	}

	// Forward-declare the WhatsApp service so the send_whatsapp executor's
	// Sender closure can capture it. Constructed below alongside the
	// dispatcher; nil-safe at registration time.
	var waSvc *whatsapp.Service
	dispatcherCtx, cancelDispatcher := context.WithCancel(context.Background())
	defer cancelDispatcher()

	// SSE-driven UI: subscribe to bus-internal sensor observation events
	// and republish the corresponding high-level projection event
	// (weather/stock/calendar) so the React UI cache updates without
	// polling. Lifetime tied to dispatcherCtx — cancelled at shutdown.
	startProjectionPublisher(
		dispatcherCtx,
		bus,
		bc.store,
		bc.settingsSvc,
		bc.logger.WithField("c", "projection-publisher"),
	)
	// 10s server-side ticker for stats + transitional health. Replaces
	// the per-client 30s polls of /api/metrics/snapshot and /api/health
	// with one shared broadcast. Health publishes only when fields
	// flip + a 60s heartbeat for first-mount clients.
	startMetricsPublisher(
		dispatcherCtx,
		bus,
		bc.metrics,
		bc.db,
		bc.llm,
		bc.store,
		healthStartedAt,
		bc.logger.WithField("c", "metrics-publisher"),
	)
	retroDispatcher := synth.NewRetrospectiveDispatcher(dispatcherCtx, synth.RetrospectiveDeps{
		LLM:          bc.llm,
		Reader:       bc.store,
		Concerns:     concernRepo,
		Observations: concernObsRepo,
		Bus:          bus,
		EventLog:     bc.store,
		Logger:       bc.logger.WithField("c", "retrospective"),
	})

	// V2.8.1: predeclare so the askFn closure below can read it by
	// reference. Assigned after the action registry is fully built —
	// the closure body runs per-request, by which time the slice is
	// populated.
	var wiredIntents []synth.WiredIntent

	// V2.10: predeclare add_task dispatch closure for the reactive
	// loop. Same reference-capture pattern as wiredIntents — assigned
	// once the actionHandler is constructed, below. Stays nil on
	// builds without sensors.tasks.enabled so the AddTaskTool is not
	// registered into the loop.
	var addTaskFn synth.AddTaskFn

	// V2.12: predeclare WhatsApp send closures. resolveContactFn is
	// always assigned (the resolver works with memory facts alone, no
	// CardDAV required). sendWhatsAppPreviewFn is assigned only after
	// the actionHandler is constructed and only when WhatsApp is
	// enabled — the model's tool registration honors the nil pattern
	// so a build without WhatsApp leaves the tools unregistered.
	var resolveContactFn synth.ResolveContactFn
	var sendWhatsAppPreviewFn synth.SendWhatsAppPreviewFn
	resolveContactFn = func(ctx context.Context, query string) (synth.ResolveContactResult, error) {
		c, err := contactResolver.Resolve(ctx, query)
		if err != nil {
			var amb *whatsapp.ErrAmbiguous
			if errors.As(err, &amb) {
				return synth.ResolveContactResult{Candidates: amb.Candidates}, nil
			}
			var nf *whatsapp.ErrContactNotFound
			if errors.As(err, &nf) {
				return synth.ResolveContactResult{NotFound: true}, nil
			}
			return synth.ResolveContactResult{}, err
		}
		return synth.ResolveContactResult{OK: true, Name: c.Name, IsGroup: c.IsGroup}, nil
	}

	// V2.8.0 Phase 3: shared AskFn so both the AskHandler (input bar)
	// and AskFollowupExec (action button on a card) call the same
	// reactive-synth path. Extracting here keeps the closure DRY and
	// guarantees identical behavior across the two surfaces.
	askFn := func(ctx context.Context, query string) (synth.Card, llm.Trace, []llm.MemoryCandidate, error) {
		tz := bc.settingsSvc.TZ()
		now := time.Now()
		// V2.5.0 P3: surface top-3 active+paused concerns in the reactive
		// prompt so the model sees what's available before deciding to
		// call lookup_concern. Best-effort — error degrades to empty.
		projConcerns, _ := projection.ActiveConcerns{
			Repo:    concernRepo,
			TagRepo: concernObsRepo,
			Config:  projection.ActiveConcernsConfig{Limit: 3, IncludePaused: true},
		}.Compute(ctx, bc.store)
		deps := synth.ReactiveDeps{
			LLM:                     bc.llm,
			Reader:                  bc.store,
			Tasks:                   taskRepo,
			ProjCfg:                 projCfg,
			Memory:                  memoryRepo,
			MemoryRanker:            ranker,
			Prompts:                 prompts,
			Date:                    now.In(tz).Format("2006-01-02"),
			Now:                     now,
			Deadline:                reactiveDeadline,
			ToolTimeout:             toolTimeout,
			FinalCallBudget:         finalCallBudget,
			MaxIterations:           bc.cfg.Synth.ReactiveMaxIterations,
			Logger:                  bc.logger.WithField("c", "reactive"),
			Concerns:                concernRepo,
			ConcernObservations:     concernObsRepo,
			Bus:                     bus,
			EventLogWriter:          bc.store,
			RetrospectiveDispatcher: retroDispatcher,
			ConcernsProjected:       projConcerns,
			JinaClient:              jinaToolClient,
			JinaCache:               jinaStore,
			SearchTTL:               jinaSearchTTL,
			ReadTTL:                 jinaReadTTL,
			WiredIntents:            wiredIntents,
			AddTask:                 addTaskFn,
			ResolveContact:          resolveContactFn,
			SendWhatsAppMessage:     sendWhatsAppPreviewFn,
			ExpectedReplies:         expectedReplyRepo,
		}
		if bc.metrics != nil {
			deps.LoopObserver = llm.LoopObserver{
				OnLLMCall:      bc.metrics.ObserveLLMCall,
				OnSchemaRepair: bc.metrics.IncSchemaRepair,
				OnTool:         bc.metrics.ObserveTool,
				OnLoopIters:    bc.metrics.ObserveLoopIters,
			}
			deps.OnSynthRun = bc.metrics.ObserveSynthRun
		}
		return synth.Ask(ctx, deps, query)
	}

	(&api.AskHandler{
		AskFn: askFn,
		ExtractFn: func(ctx context.Context, query string) []llm.MemoryCandidate {
			return synth.ExtractFacts(ctx, bc.llm, query, reactiveDeadline, bc.logger.WithField("c", "ask"))
		},
		Cards:           cardRepo,
		Traces:          traceRepo,
		Memory:          memoryRepo,
		EmbeddingStore:  embStore,
		EmbeddingIndex:  memIdx,
		EventLog:        bc.store,
		Bus:             bus,
		TZ:              bc.settingsSvc.TZ,
		Now:             time.Now,
		Deadline:        reactiveDeadline,
		ExtractDeadline: reactiveDeadline,
		AskCardTTL:      bc.cfg.Synth.AskCardTTL,
		Log:             bc.logger.WithField("c", "ask"),
	}).Register(srv.Echo)

	// V2.10: card-conversation surface. converseFn mirrors askFn's plumbing
	// but routes through synth.Converse with a pinned card + prior turns
	// rendered into the prompt. The same wiredIntents catalog applies so
	// sub-card actions go through the V2.8 action handler unchanged.
	converseFn := func(ctx context.Context, card synth.PinnedCard, prior []synth.PriorTurn, query string) (synth.SubCard, llm.Trace, error) {
		tz := bc.settingsSvc.TZ()
		now := time.Now()
		deps := synth.ConverseDeps{
			LLM:                 bc.llm,
			Reader:              bc.store,
			Tasks:               taskRepo,
			ProjCfg:             projCfg,
			Memory:              memoryRepo,
			MemoryRanker:        ranker,
			Prompts:             prompts,
			Date:                now.In(tz).Format("2006-01-02"),
			Now:                 now,
			Deadline:            reactiveDeadline,
			ToolTimeout:         toolTimeout,
			FinalCallBudget:     finalCallBudget,
			MaxIterations:       bc.cfg.Synth.ReactiveMaxIterations,
			Logger:              bc.logger.WithField("c", "converse"),
			Bus:                 bus,
			JinaClient:          jinaToolClient,
			JinaCache:           jinaStore,
			SearchTTL:           jinaSearchTTL,
			ReadTTL:             jinaReadTTL,
			WiredIntents:        wiredIntents,
			ResolveContact:      resolveContactFn,
			SendWhatsAppMessage: sendWhatsAppPreviewFn,
			ExpectedReplies:     expectedReplyRepo,
			Card:                card,
			PriorTurns:          prior,
		}
		if bc.metrics != nil {
			deps.LoopObserver = llm.LoopObserver{
				OnLLMCall:      bc.metrics.ObserveLLMCall,
				OnSchemaRepair: bc.metrics.IncSchemaRepair,
				OnTool:         bc.metrics.ObserveTool,
				OnLoopIters:    bc.metrics.ObserveLoopIters,
			}
			deps.OnSynthRun = bc.metrics.ObserveSynthRun
		}
		sub, trace, _, err := synth.Converse(ctx, deps, query)
		return sub, trace, err
	}

	(&api.ConverseHandler{
		Cards:         cardRepo,
		Conversations: conversationRepo,
		Traces:        traceRepo,
		ConverseFn:    converseFn,
		EventLog:      bc.store,
		TZ:            bc.settingsSvc.TZ,
		Now:           time.Now,
		Deadline:      reactiveDeadline,
		Log:           bc.logger.WithField("c", "converse"),
	}).Register(srv.Echo)

	// V2.8.0: action surface. The Registry is the dispatch table the
	// handler consults; Phase 0 wired no-I/O verbs and Phase 1 added
	// mail. Calendar, concerns/memory and ask_followup executors land
	// in later phases — until then the handler's "no executor" branch
	// preserves the legacy log-only audit row so the UI never sees a
	// failed mutation for an unknown verb.
	actionRegistry := action.NewRegistry()
	actionRegistry.Register("dismiss", &action.DismissExec{Cards: cardRepo})
	actionRegistry.Register("snooze", &action.SnoozeExec{Cards: cardRepo})
	actionRegistry.Register("open_url", &action.OpenURLExec{})
	actionRegistry.Register("pin_card", &action.PinCardExec{Cards: cardRepo})
	actionRegistry.Register("unpin_card", &action.UnpinCardExec{Cards: cardRepo})

	// V2.8.0 Phase 1: mail executors. Wired only when IMAP is configured
	// (Host non-empty); SMTP is optional — when missing, send_reply
	// degrades to a "drafts only" toast and forward / draft_reply still
	// work via APPEND.
	if bc.cfg.Sensors.IMAP.Host != "" && bc.cfg.Sensors.IMAP.Username != "" {
		smtpReal := smtpsensor.New(bc.cfg.Sensors.SMTP)
		var smtpIface smtpsensor.Client
		if smtpReal != nil {
			smtpIface = smtpReal
		}
		mailDeps := action.MailDeps{
			Dialer:  imapsensor.NewRealDialer(bc.cfg.Sensors.IMAP),
			IMAPCfg: bc.cfg.Sensors.IMAP,
			SMTP:    smtpIface,
			SMTPCfg: bc.cfg.Sensors.SMTP,
			Reader:  bc.store,
			LLM:     bc.llm,
			Voice:   prompts.VoiceShort,
			Logger:  bc.logger.WithField("c", "action-mail"),
		}
		actionRegistry.Register("mark_read", &action.MarkReadExec{Deps: mailDeps})
		actionRegistry.Register("move_mail", &action.MoveMailExec{Deps: mailDeps})
		actionRegistry.Register("draft_reply", &action.DraftReplyExec{Deps: mailDeps})
		actionRegistry.Register("forward", &action.ForwardExec{Deps: mailDeps})
		actionRegistry.Register("send_reply", &action.SendReplyExec{Deps: mailDeps})
		actionRegistry.Register("flag_mail", &action.FlagMailExec{Deps: mailDeps})
		if smtpReal == nil {
			bc.logger.Warn("action: smtp not configured; send_reply will return a 'drafts only' toast")
		}
	} else {
		bc.logger.Warn("action: imap not configured; mail executors not registered")
	}

	// V2.8.0 Phase 2: calendar executors. Wired only when CalDAV is
	// configured. The poller's Provider was already constructed in
	// buildSensors above; rebuild here so the action surface has its
	// own write-capable handle and a non-nil Provider on RSVPs.
	if bc.cfg.Sensors.CalDAV.URL != "" {
		caldavProvider, err := caldavsensor.NewFastmail(context.Background(), bc.cfg.Sensors.CalDAV)
		if err != nil {
			bc.logger.WithError(err).Warn("action: caldav write provider init failed; calendar executors not registered")
		} else {
			calDeps := action.CalendarDeps{
				Provider: caldavProvider,
				UserMail: action.UserMailtoFromConfig(bc.cfg.Sensors.CalDAV.Username),
				Logger:   bc.logger.WithField("c", "action-calendar"),
			}
			actionRegistry.Register("add_event", &action.AddEventExec{Deps: calDeps})
			actionRegistry.Register("block_calendar", &action.BlockCalendarExec{Deps: calDeps})
			actionRegistry.Register("rsvp_yes", &action.RsvpYesExec{Deps: calDeps})
			actionRegistry.Register("rsvp_no", &action.RsvpNoExec{Deps: calDeps})
			actionRegistry.Register("rsvp_maybe", &action.RsvpMaybeExec{Deps: calDeps})
			actionRegistry.Register("reschedule_event", &action.RescheduleEventExec{Deps: calDeps})
			actionRegistry.Register("cancel_event", &action.CancelEventExec{Deps: calDeps})
		}
	}

	// V2.8.0 Phase 3: internal-only executors. add_concern / add_memory
	// hit the existing repos; ask_followup reuses the Phase 3 askFn so
	// the input bar and card buttons share one reactive path.
	actionRegistry.Register("add_concern", &action.AddConcernExec{
		Concerns: concernRepo,
		Now:      time.Now,
		Logger:   bc.logger.WithField("c", "action-concern"),
	})
	actionRegistry.Register("add_memory", &action.AddMemoryExec{
		Memory:  memoryRepo,
		Link:    contactLinkRepo, // link memory facts to known CardDAV contacts at write time
		CardDAV: cardDAVRepo,
		Now:     time.Now,
	})
	actionRegistry.Register("ask_followup", &action.AskFollowupExec{
		AskFn:  askFn,
		Logger: bc.logger.WithField("c", "action-ask"),
	})

	// V2.11: set_reminder + add_task + complete_task + delete_task all
	// route to the unified TaskRepo. The sweeper goroutine (started
	// below, after the scheduler is constructed) fires due alarms.
	actionRegistry.Register("set_reminder", &action.SetReminderExec{
		Tasks: taskRepo,
		TZ:    bc.settingsSvc.TZ,
	})
	actionRegistry.Register("add_task", &action.AddTaskExec{Tasks: taskRepo})
	actionRegistry.Register("complete_task", &action.CompleteTaskExec{Tasks: taskRepo})
	actionRegistry.Register("delete_task", &action.DeleteTaskExec{Tasks: taskRepo})
	actionRegistry.Register("edit_task", &action.EditTaskExec{Tasks: taskRepo})

	// V2.12: outbound WhatsApp send. Wired only when WhatsApp is enabled.
	// The Sender closure resolves the live whatsmeow client at send time
	// so re-pair across Unlink uses the new client without rebuilding
	// the executor (mirrors the V2.9 reminder-sweeper pattern at line
	// ~1054). Resolver works without a CardDAV cache — when carddav.enabled
	// is false the resolver falls back to manually-curated memory facts.
	// V2.13.0: assistant persona closure. Reads live AppSettings each
	// call so settings edits made via the Profile UI take effect on the
	// next send without a process restart.
	assistantPersonaFn := func() (userName, assistantName, tone string) {
		snap := bc.settingsSvc.Snapshot()
		if snap == nil {
			return "", "", ""
		}
		return snap.UserName, snap.AssistantName, snap.AssistantTone
	}
	whatsAppSendThrottle := whatsapp.NewThrottle()
	if bc.cfg.Sensors.WhatsApp.Enabled {
		waSendDeps := action.WhatsAppDeps{
			Sender: func() action.WhatsAppSender {
				if waSvc == nil {
					return nil
				}
				c := waSvc.Client()
				if c == nil {
					return nil
				}
				return c
			},
			Resolver:          contactResolverAdapter{contactResolver},
			Throttle:          whatsAppSendThrottle,
			LLM:               bc.llm,
			Voice:             prompts.VoiceShort,
			Reader:            bc.store,
			EventLog:          bc.store,
			Logger:            bc.logger.WithField("c", "send_whatsapp"),
			MinChatInterval:   3 * time.Second,
			DailySendCap:      bc.cfg.Sensors.WhatsApp.DailySendCap,
			AssistantPersona:  assistantPersonaFn,
			AssistantRegister: prompts.AssistantRegister,
			ExpectedReplies:   expectedReplyRepo,
		}
		actionRegistry.Register("send_whatsapp", &action.SendWhatsAppExec{Deps: waSendDeps})
	} else {
		bc.logger.Debug("send_whatsapp executor not registered: sensors.whatsapp.enabled=false")
	}

	actionHandler := &action.Handler{
		Cards:    cardRepo,
		Registry: actionRegistry,
		EventLog: bc.store,
		TZ:       bc.settingsSvc.TZ,
		Now:      time.Now,
		Log:      bc.logger.WithField("c", "action"),
	}
	actionHandler.Register(srv.Echo)

	// V2.11: dispatch closure for the AddTaskTool LLM tool. Always
	// wired now that the unified TaskRepo replaces the file-gated
	// V2.6 path. Captured by reference in the askFn closure above.
	{
		handler := actionHandler
		addTaskFn = func(ctx context.Context, target map[string]any) (bool, string, error) {
			result, err := handler.DispatchIntent(ctx, action.DispatchInput{
				Intent: "add_task",
				Target: target,
			})
			return result.OK, result.Toast, err
		}
	}

	// V2.12: SendWhatsAppMessageTool dispatch closure. Wired only when
	// WhatsApp is enabled — leaves the LLM tool unregistered otherwise
	// so the model can't propose a send the action surface can't honor.
	if bc.cfg.Sensors.WhatsApp.Enabled {
		handler := actionHandler
		sendWhatsAppPreviewFn = func(ctx context.Context, target map[string]any) (map[string]any, string, error) {
			result, err := handler.DispatchIntent(ctx, action.DispatchInput{
				Intent: "send_whatsapp",
				Target: target,
			})
			if err != nil {
				return nil, result.Toast, err
			}
			if !result.OK {
				return nil, result.Toast, nil
			}
			return result.Preview, result.Toast, nil
		}
	}

	(&api.ActionsHandler{Registry: actionRegistry}).Register(srv.Echo)

	// V2.11: Tasks panel CRUD against the unified store. No more
	// 503-when-disabled gate — the table is always live.
	(&api.TasksHandler{
		Action: actionHandler,
		Tasks:  taskRepo,
		Bus:    bus,
		Log:    bc.logger.WithField("c", "tasks"),
	}).Register(srv.Echo)

	// V2.8.1: populate the WiredIntents slice that the synth prompts
	// and post-process pass consult. The variable was predeclared
	// above so the askFn closure could capture it by reference; the
	// runner picks it up below at construction time.
	wiredIntents = wiredIntentsFromRegistry(actionRegistry)

	(&api.MemoryHandler{
		Memory:         memoryRepo,
		EmbeddingStore: embStore,
		EmbeddingIndex: memIdx,
		EventLog:       bc.store,
		Bus:            bus,
		TZ:             bc.settingsSvc.TZ,
		Now:            time.Now,
		Log:            bc.logger.WithField("c", "memory"),
	}).Register(srv.Echo)

	// V2.12: contacts CRUD for the Settings UI's WhatsApp send picker.
	(&api.ContactsHandler{
		Memory:   memoryRepo,
		Link:     contactLinkRepo,
		CardDAV:  cardDAVRepo,
		EventLog: bc.store,
		Now:      time.Now,
		Log:      bc.logger.WithField("c", "contacts"),
	}).Register(srv.Echo)

	// V2.13.2: Sends panel — read-only view over expected_replies for
	// the Profile → Sends tab and the CardFocus inline banner.
	(&api.SendsHandler{
		Cards:   cardRepo,
		Replies: expectedReplyRepo,
		Reader:  bc.store,
		ProjCfg: projCfg,
		Now:     time.Now,
		Log:     bc.logger.WithField("c", "sends"),
	}).Register(srv.Echo)

	// V2.5.0 — concerns API surface. Phase 1 ships CRUD + lifecycle;
	// Phase 2 adds /approve + /dismiss with the retrospective dispatcher
	// (constructed above with the AskHandler's deps).
	concernsHandler := &api.ConcernsHandler{
		Concerns:       concernRepo,
		Observations:   concernObsRepo,
		Bus:            bus,
		EventLog:       bc.store,
		Now:            time.Now,
		Log:            bc.logger.WithField("c", "concerns"),
		Dispatcher:     retroDispatcher,
		AutoRetireDays: bc.cfg.Concerns.AutoRetireDays,
	}
	concernsHandler.Register(srv.Echo)

	var loopObserver llm.LoopObserver
	var onSynthRun func(stage, outcome string, dur time.Duration)
	if bc.metrics != nil {
		loopObserver = llm.LoopObserver{
			OnLLMCall:      bc.metrics.ObserveLLMCall,
			OnSchemaRepair: bc.metrics.IncSchemaRepair,
			OnTool:         bc.metrics.ObserveTool,
			OnLoopIters:    bc.metrics.ObserveLoopIters,
		}
		onSynthRun = bc.metrics.ObserveSynthRun
	}

	runner := &synth.Runner{
		LLM:                 bc.llm,
		Reader:              bc.store,
		Tasks:               taskRepo,
		DB:                  bc.db,
		EventLog:            bc.store,
		Bus:                 bus,
		ProjCfg:             projCfg,
		Prompts:             prompts,
		Now:                 time.Now,
		Logger:              bc.logger.WithField("c", "synth"),
		CardsTable:          "cards",
		BriefingTable:       "briefings",
		TraceTable:          "traces",
		MemoryTable:         "memory_facts",
		EmbeddingTable:      "memory_embeddings",
		MemoryRanker:        ranker,
		EmbeddingStore:      embStore,
		EmbeddingIndex:      memIdx,
		CardsTimeout:        cardsTimeout,
		BriefingTimeout:     briefingTimeout,
		ToolTimeout:         toolTimeout,
		FinalCallBudget:     finalCallBudget,
		CardsMaxIterations:  bc.cfg.Synth.CardsMaxIterations,
		Concerns:            concernRepo,
		ConcernObservations: concernObsRepo,
		LoopObserver:        loopObserver,
		OnSynthRun:          onSynthRun,
		JinaClient:          jinaToolClient,
		JinaCache:           jinaStore,
		SearchTTL:           jinaSearchTTL,
		ReadTTL:             jinaReadTTL,
		Tickers:             bc.settingsSvc,
		WiredIntents:        wiredIntents,
		ExpectedReplies:     expectedReplyRepo,
	}

	scheduler, err := schedule.New(bc.cfg.Schedule, sensors, runner.Run, bc.logger.WithField("c", "scheduler"))
	if err != nil {
		bc.logger.WithError(err).Fatal("build scheduler")
	}
	scheduler.WithLocation(bc.clk.Location())
	scheduler.WithEventLog(bc.store)
	scheduler.WithMorningBudget(cronBudget)
	if bc.metrics != nil {
		scheduler.WithCronObserver(bc.metrics.ObserveCron)
		scheduler.WithSensorObserver(bc.metrics.ObserveSensor)
	}
	// Live TZ reload: when the user changes timezone via PUT /api/settings,
	// re-target the scheduler so cron entries fire at the new local times.
	// Subscribers run synchronously on the Reload caller's goroutine (the
	// settings HTTP handler) — Retarget waits for in-flight cron jobs but
	// does not block other API traffic.
	bc.settingsSvc.Subscribe(func(s *settings.Snapshot) {
		if s == nil || s.Location == nil {
			return
		}
		scheduler.Retarget(s.Location)
	})

	// V2.3.0 P3: wire the inject pipeline. The bus fans inject cards out
	// over SSE; the scheduler invokes injectFn on each cron tick (with
	// signal=nil so the orchestrator runs Detect itself) and on the manual
	// /api/synth/now?kind=inject path (with a synthetic signal supplied).
	// V2.4: bus is constructed earlier so morning Runner / AskHandler /
	// inject orchestrator share it.
	injectCfg := sensor.DefaultInjectConfig()
	scheduler.WithInject(buildInjectFn(injectFnDeps{
		Reader:          bc.store,
		EventLog:        bc.store,
		ProjCfg:         projCfg,
		Bus:             bus,
		LLM:             bc.llm,
		Memory:          memoryRepo,
		MemoryRanker:    ranker,
		Prompts:         prompts,
		CardRepo:        cardRepo,
		BriefingRepo:    briefingRepo,
		ConcernRepo:     concernRepo,
		ConcernTagRepo:  concernObsRepo,
		DetectorCfg:     injectCfg,
		Logger:          bc.logger.WithField("c", "inject"),
		Now:             time.Now,
		LoopObserver:    loopObserver,
		OnSynthRun:      onSynthRun,
		JinaClient:      jinaToolClient,
		JinaCache:       jinaStore,
		SearchTTL:       jinaSearchTTL,
		ReadTTL:         jinaReadTTL,
		WiredIntents:    wiredIntents,
		FinalCallBudget: finalCallBudget,
	}))

	// V2.4: attach the bus so SyncAll wraps each per-sensor ctx with an
	// EventPublisher; sensors call sensor.PublishObserved after every
	// successful log append.
	scheduler.WithBus(bus)

	// V2.5.0: daily concern recognition pass. Cron defaults to 03:00; the
	// scheduler's single-flight guard rejects manual mid-tick invocation.
	scheduler.WithRecognition(func(ctx context.Context) error {
		_, err := synth.Recognize(ctx, synth.RecognizeDeps{
			LLM:          bc.llm,
			Reader:       bc.store,
			Concerns:     concernRepo,
			Observations: concernObsRepo,
			Bus:          bus,
			EventLog:     bc.store,
			Logger:       bc.logger.WithField("c", "recognition"),
		}, synth.RecognizeOpts{
			Now:            time.Now(),
			AutoRetireDays: bc.cfg.Concerns.AutoRetireDays,
		})
		return err
	})

	// V2.4: start the reactive inject subscriber. It consumes
	// SensorEventObservedEvent from the bus and calls
	// scheduler.RunInjectNow under the shared injectRunning single-flight
	// guard. Process-lifetime goroutine; cancel via subscriberCtx.
	subscriberCtx, cancelSubscriber := context.WithCancel(context.Background())
	defer cancelSubscriber()
	go injectsub.Run(subscriberCtx, injectsub.Deps{
		Bus:    bus,
		Runner: scheduler,
		Budget: scheduler.InjectBudget(),
		Logger: bc.logger.WithField("c", "inject-subscriber"),
	})

	// V2.8.1 / V2.9: reminder sweeper goroutine. Polls the reminders
	// table every 60s, fires due reminders into the configured outbound
	// channels (inject pipeline + optional WhatsApp). Lives alongside
	// the inject subscriber so they share cancellation.
	//
	// The WhatsAppSender is wired as a closure that captures the waSvc
	// variable (declared above near the contact repos) by reference —
	// required because the Service swaps clients across re-pair, and a
	// value captured here at boot would go stale.
	reminderDeps := schedule.ReminderSweeperDeps{
		Tasks:    taskRepo,
		Injector: scheduler,
		BuildSignal: func(t store.Task, at time.Time) any {
			return &synth.InjectSignal{
				Kind:       "reminder",
				Subject:    t.Title,
				EvidenceID: t.ID,
				At:         at,
			}
		},
		Logger:        bc.logger.WithField("c", "reminder-sweeper"),
		Now:           time.Now,
		InjectEnabled: bc.cfg.Reminders.InjectEnabled,
		EventLog:      bc.store,
	}
	if bc.cfg.Reminders.WhatsAppEnabled && bc.cfg.Reminders.WhatsAppTo != "" {
		reminderDeps.WhatsAppSender = func() schedule.WhatsAppSender {
			if waSvc == nil {
				return nil
			}
			c := waSvc.Client()
			if c == nil {
				return nil
			}
			return c
		}
		reminderDeps.WhatsAppTo = bc.cfg.Reminders.WhatsAppTo
	}
	go schedule.RunReminderSweeper(subscriberCtx, reminderDeps)

	(&api.SyncHandler{
		Scheduler: scheduler,
		Log:       bc.logger.WithField("c", "sync"),
	}).Register(srv.Echo)

	// Settings handler is registered after the scheduler is built so it
	// can trigger an immediate SyncAll after a successful PUT — without
	// this, the weather widget keeps showing the old place name until
	// the next cron tick.
	(&api.SettingsHandler{
		Repo:     bc.settingsRepo,
		Service:  bc.settingsSvc,
		Geocoder: geocode.NewOpenMeteo(),
		AfterSave: func(ctx context.Context) {
			scheduler.SyncAll(ctx)
		},
		Bus: bus,
		Now: time.Now,
		Log: bc.logger.WithField("c", "settings"),
	}).Register(srv.Echo)
	(&api.SynthHandler{
		Scheduler: scheduler,
		Timeout:   cronBudget,
		Log:       bc.logger.WithField("c", "synth-api"),
	}).Register(srv.Echo)
	(&api.TodayStreamHandler{
		Bus:    bus,
		Logger: bc.logger.WithField("c", "today-stream"),
	}).Register(srv.Echo)

	mountStaticUI(srv)

	// V2.7: WhatsApp integration (push, not poll). The Service runs
	// independent of the cron scheduler — see internal/whatsapp/types.go
	// for the architectural rationale. Start is non-fatal: a failed
	// init logs WARN and the daemon proceeds without WhatsApp; the
	// Settings UI lets the operator re-pair without restart.
	waLogger := bc.logger.WithField("c", "whatsapp")
	waSvc = whatsapp.NewService(
		whatsapp.ServiceConfig{
			Enabled: bc.cfg.Sensors.WhatsApp.Enabled,
			DBPath:  bc.cfg.Sensors.WhatsApp.DBPath,
		},
		whatsapp.RealClientFactory(bc.cfg.Sensors.WhatsApp.DBPath, waLogger),
		waLogger,
	)
	waSvc.SetBus(bus)
	// Bridge the dispatcher to synth + whatsmeow: incoming Process
	// decisions run through the same Reactive Ask the in-app surface
	// uses, but with Conversation populated so the WhatsApp register
	// in reactive.tmpl activates and the model fills Card.Speech.
	waAskFn := func(ctx context.Context, query string, conv *synth.ConversationContext) (synth.Card, error) {
		tz := bc.settingsSvc.TZ()
		now := time.Now()
		projConcerns, _ := projection.ActiveConcerns{
			Repo:    concernRepo,
			TagRepo: concernObsRepo,
			Config:  projection.ActiveConcernsConfig{Limit: 3, IncludePaused: true},
		}.Compute(ctx, bc.store)
		deps := synth.ReactiveDeps{
			LLM:                     bc.llm,
			Reader:                  bc.store,
			Tasks:                   taskRepo,
			ProjCfg:                 projCfg,
			Memory:                  memoryRepo,
			MemoryRanker:            ranker,
			Prompts:                 prompts,
			Date:                    now.In(tz).Format("2006-01-02"),
			Now:                     now,
			Deadline:                reactiveDeadline,
			ToolTimeout:             toolTimeout,
			FinalCallBudget:         finalCallBudget,
			MaxIterations:           bc.cfg.Synth.ReactiveMaxIterations,
			Logger:                  bc.logger.WithField("c", "whatsapp-ask"),
			Concerns:                concernRepo,
			ConcernObservations:     concernObsRepo,
			Bus:                     bus,
			EventLogWriter:          bc.store,
			RetrospectiveDispatcher: retroDispatcher,
			ConcernsProjected:       projConcerns,
			JinaClient:              jinaToolClient,
			JinaCache:               jinaStore,
			SearchTTL:               jinaSearchTTL,
			ReadTTL:                 jinaReadTTL,
			Conversation:            conv,
			WiredIntents:            wiredIntents,
			AddTask:                 addTaskFn,
			ResolveContact:          resolveContactFn,
			SendWhatsAppMessage:     sendWhatsAppPreviewFn,
			ExpectedReplies:         expectedReplyRepo,
		}
		if bc.metrics != nil {
			deps.LoopObserver = llm.LoopObserver{
				OnLLMCall:      bc.metrics.ObserveLLMCall,
				OnSchemaRepair: bc.metrics.IncSchemaRepair,
				OnTool:         bc.metrics.ObserveTool,
				OnLoopIters:    bc.metrics.ObserveLoopIters,
			}
			deps.OnSynthRun = bc.metrics.ObserveSynthRun
		}
		card, _, _, err := synth.Ask(ctx, deps, query)
		return card, err
	}
	replyCardNotifier := &replycard.Notifier{
		Cards: cardRepo,
		Bus:   bus,
		Now:   time.Now,
		Log:   bc.logger.WithField("c", "replycard"),
	}
	waHandler := &whatsapp.SynthHandler{
		Ask:             waAskFn,
		Client:          waSvc.Client,
		EventLog:        bc.store,
		Now:             time.Now,
		Logger:          waLogger.WithField("c", "whatsapp-handler"),
		Action:          actionHandler,
		ExpectedReplies: expectedReplyRepo,
		ReplyReceived:   replyCardNotifier,
	}
	waSvc.SetMessageHandler(waHandler.Handle)
	// V2.13.3: auto-eligibility for inbound DMs that satisfy an open
	// ExpectedReply. Closure stays cheap — one indexed lookup against
	// expected_replies. Returns false on any error so the static
	// allowlist remains the floor.
	waSvc.SetOpenReplyChecker(func(ctx context.Context, jid string, now time.Time) bool {
		if expectedReplyRepo == nil {
			return false
		}
		open, err := expectedReplyRepo.OpenForJID(ctx, jid, now)
		return err == nil && open != nil
	})
	waConfigRepo := &store.WhatsAppConfigRepo{DB: bc.db}
	if err := waConfigRepo.Migrate(); err != nil {
		bc.logger.WithError(err).Fatal("migrate whatsapp_config")
	}
	api.LoadInitialConfig(context.Background(), waConfigRepo, waSvc, waLogger)
	if bc.cfg.Sensors.WhatsApp.Enabled {
		if dir := filepath.Dir(bc.cfg.Sensors.WhatsApp.DBPath); dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				bc.logger.WithError(err).Warn("whatsapp: create db dir failed; integration disabled")
			} else if err := waSvc.Start(context.Background()); err != nil {
				bc.logger.WithError(err).Warn("whatsapp: start failed; integration idle")
			}
		}
	}
	(&api.WhatsAppHandler{
		Service: waSvc,
		Repo:    waConfigRepo,
		Log:     waLogger.WithField("c", "whatsapp-http"),
	}).Register(srv.Echo)

	scheduler.Start()
	go primeSensors(scheduler, bc.logger)

	// V2.13.0: weekly cleanup of resolved/expired ExpectedReply rows.
	// One week of audit retention, then prune. The repo is small (one
	// row per proactive send) so a daily tick would be fine — weekly
	// is just to avoid a hot path on the schedule loop. Runs in a
	// process-lifetime goroutine using the dispatcher context (already
	// cancelled at shutdown by the dispatcher cancel above).
	go func() {
		ticker := time.NewTicker(7 * 24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-dispatcherCtx.Done():
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(dispatcherCtx, 30*time.Second)
				deleted, err := expectedReplyRepo.DeleteExpired(ctx, time.Now().Add(-7*24*time.Hour))
				cancel()
				if err != nil {
					bc.logger.WithError(err).Warn("expected_replies: weekly cleanup failed")
				} else if deleted > 0 {
					bc.logger.WithField("deleted", deleted).Info("expected_replies: cleanup ran")
				}
			}
		}
	}()

	var emitter *metrics.Emitter
	if bc.metrics != nil && bc.cfg.Metrics.SnapshotEnabled {
		interval := time.Duration(bc.cfg.Metrics.SnapshotIntervalSec) * time.Second
		emitter = metrics.NewEmitter(bc.metrics, metrics.EmitterConfig{
			Interval: interval,
			Logger:   bc.logger.WithField("c", "metrics"),
			Append: func(ctx context.Context, kind, source string, payload any) error {
				_, err := bc.store.Append(ctx, kind, source, payload)
				return err
			},
			Source: "metrics",
			Hooks: []metrics.SnapshotHook{
				func(m *metrics.Metrics) { m.SetSSESubscribers(bus.SubscriberCount()) },
			},
		})
		emitterCtx, cancelEmitter := context.WithCancel(context.Background())
		defer cancelEmitter()
		emitter.Start(emitterCtx)
		defer emitter.Stop()
	}

	go func() {
		if err := srv.Start(); err != nil {
			bc.logger.WithError(err).Fatal("http server")
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	bc.logger.Info("shutdown signal received")

	stopped := scheduler.Stop()
	select {
	case <-stopped.Done():
	case <-time.After(10 * time.Second):
		bc.logger.Warn("scheduler did not drain in time")
	}

	stopCtx, cancelStop := context.WithTimeout(context.Background(), 5*time.Second)
	if err := waSvc.Stop(stopCtx); err != nil {
		bc.logger.WithError(err).Warn("whatsapp: stop")
	}
	cancelStop()

	if err := srv.Shutdown(context.Background()); err != nil {
		bc.logger.WithError(err).Error("shutdown")
	}
}

// buildSensors constructs every enabled sensor. A sensor is "enabled" when
// its required config fields are non-empty; missing config logs a warning
// and the sensor is skipped (no fatal — the user may want only weather).
//
// Sensors that need a timezone (caldav floating-time fallback) read it via
// the live settingsSvc on every Sync, so a TZ change in the Settings UI
// takes effect on the next poll.
func buildSensors(ctx context.Context, cfg *config.Config, settingsSvc *settings.Service, store logp.Store, logger *logrus.Logger) []sensor.Sensor {
	var sensors []sensor.Sensor

	// Weather is always wired — the sensor itself becomes a no-op when the
	// SettingsService reports no location, and starts emitting snapshots
	// the moment the user saves a city/country in the Settings UI.
	ws := weathersensor.New(settingsSvc, store, logger.WithField("c", "weather"))
	ws.WithGeocoder(geocode.NewNominatim())
	sensors = append(sensors, ws)
	if !settingsSvc.Snapshot().Set {
		logger.Warn("weather sensor: no location set yet — sync will no-op until configured via the Settings UI")
	}

	if cfg.Sensors.IMAP.Host != "" && cfg.Sensors.IMAP.Username != "" {
		sensors = append(sensors, imapsensor.New(cfg.Sensors.IMAP, store, store, logger.WithField("c", "imap")))
	} else {
		logger.Warn("imap sensor disabled: host or username not configured")
	}

	if cfg.Sensors.CalDAV.URL != "" {
		provider, err := caldavsensor.NewFastmail(ctx, cfg.Sensors.CalDAV)
		if err != nil {
			logger.WithError(err).Warn("caldav sensor disabled: provider init failed")
		} else {
			sensors = append(sensors, caldavsensor.New(cfg.Sensors.CalDAV, settingsSvc, provider, store, store, logger.WithField("c", "caldav")))
		}
	} else {
		logger.Warn("caldav sensor disabled: url not configured")
	}

	// Stock sensor is always wired — it self-noops via SettingsSource
	// when no tickers are configured, and starts polling the moment
	// the user saves a watchlist via the Settings UI.
	stockProvider := stocksensor.NewYahoo()
	sensors = append(sensors, stocksensor.New(settingsSvc, stockProvider, store, store, logger.WithField("c", "stock")))
	if tickers, _, _, ok := settingsSvc.StockConfig(); !ok || len(tickers) == 0 {
		logger.Info("stock sensor: no tickers configured — sync will no-op until a watchlist is saved")
	}

	return sensors
}

// primeSensors runs SyncAll once at boot so projections aren't empty for the
// first few minutes after a fresh start. Bounded to 5 minutes. Each result
// is rolled into a single summary INFO line; per-sensor failures still
// surface as a separate WARN so they're not swallowed by the rollup.
func primeSensors(s *schedule.Scheduler, logger *logrus.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	results := s.SyncAll(ctx)
	parts := make([]string, 0, len(results))
	ok, fail := 0, 0
	for _, r := range results {
		status := "ok"
		if !r.OK {
			status = "err"
			fail++
			logger.WithFields(logrus.Fields{
				"sensor":   r.Name,
				"duration": r.Duration,
				"error":    r.Err,
			}).Warn("boot prime: sensor failed")
		} else {
			ok++
		}
		parts = append(parts, fmt.Sprintf("%s:%s:%s", r.Name, status, r.Duration.Round(time.Millisecond)))
	}
	logger.WithFields(logrus.Fields{
		"results": parts,
		"ok":      ok,
		"fail":    fail,
	}).Info("boot prime: sensors complete")
}
