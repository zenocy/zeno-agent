package llm

import (
	"fmt"
	"strings"
	"time"
)

// Config is the provider-agnostic constructor input. Selects an
// implementation via Provider and dispatches the corresponding
// provider-specific sub-block to its constructor. main.go owns the
// mapping from the YAML LLMConfig to this struct.
//
// OpenAI is the default — an empty Provider string is equivalent to
// "openai". Future providers (gemini, anthropic) are added as additional
// switch arms in New plus a new sub-config block here.
type Config struct {
	// Provider selects the backend: "" | "openai" | "gemini". Case-
	// insensitive. Empty defaults to "openai" so existing configs that
	// predate the multi-provider refactor keep working.
	Provider string

	// OpenAI carries the OpenAI-compatible client knobs (endpoint, key,
	// model, retry, json_schema_mode, ...). Used when Provider is ""
	// or "openai".
	OpenAI ClientConfig

	// Gemini carries the native Gemini client knobs. Used when Provider
	// is "gemini".
	Gemini GeminiClientConfig

	// geminiConstructor injects the Gemini provider factory at boot so
	// the gemini sub-package depends on llm (one direction) instead of
	// the other way around. Wired exactly once by an init in the
	// internal/llm/llmfactory bootstrap (or equivalent) and never read
	// outside this package.
	geminiConstructor func(GeminiClientConfig) (Provider, error)
}

// GeminiClientConfig is the provider-specific constructor input for
// the native Gemini client. Filled by main.go from
// config.LLMConfig.Gemini plus the cross-provider common knobs
// (timeout, retry, max tokens). The actual constructor lives in
// internal/llm/gemini and is registered via RegisterGeminiProvider.
//
// Model selection: the gemini.model YAML field is the source of
// truth — model IDs differ between providers (e.g. "gemini-3.5-flash"
// vs "gemma3:4b"), so the Gemini block carries its own. main.go falls
// back to the common llm.model when the sub-block leaves it empty so
// single-provider deployments don't need to set it twice.
type GeminiClientConfig struct {
	APIKey                   string
	Endpoint                 string // optional Vertex AI base URL; empty targets generativelanguage.googleapis.com
	Model                    string
	Timeout                  time.Duration
	Retry                    RetryPolicy
	MaxTokens                int
	EnableGoogleSearch       bool
	ThinkingLevelBackground  string
	ThinkingLevelInteractive string
	IncludeThoughts          bool

	// ServiceTierBackground/Interactive define the CallProfile →
	// Gemini service_tier mapping the client applies on outbound
	// requests. Allowed values: "" (omit; standard tier) | "flex" |
	// "standard" | "priority". Background tier defaults to "flex" on
	// cron-fired and manually-triggered runs to reduce cost; interactive
	// stays empty (= standard) so chat latency isn't degraded.
	ServiceTierBackground  string
	ServiceTierInteractive string
}

// geminiCtor is the package-level slot the gemini sub-package fills in
// its init(). Keeping it as an unexported package variable (set via
// RegisterGeminiProvider) lets factory.New dispatch to Gemini without
// importing the gemini package directly, breaking what would otherwise
// be a cyclic import.
var geminiCtor func(GeminiClientConfig) (Provider, error)

// RegisterGeminiProvider is called by the gemini sub-package's init()
// to wire up the constructor. Safe to call from any goroutine before
// main.main starts producing traffic; not safe to call after.
func RegisterGeminiProvider(ctor func(GeminiClientConfig) (Provider, error)) {
	geminiCtor = ctor
}

// New constructs the configured LLM Provider. Returns an error for
// invalid provider names or when the Gemini provider was requested but
// the sub-package was not imported (i.e. RegisterGeminiProvider was
// never called).
func New(cfg Config) (Provider, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case "", "openai":
		return NewClient(cfg.OpenAI), nil
	case "gemini":
		if geminiCtor == nil {
			return nil, fmt.Errorf("llm: provider=gemini requires importing internal/llm/gemini at the binary entry point")
		}
		return geminiCtor(cfg.Gemini)
	default:
		return nil, fmt.Errorf("llm: unknown provider %q (supported: openai, gemini)", cfg.Provider)
	}
}
