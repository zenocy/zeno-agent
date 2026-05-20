// Package gemini is the native Google Gemini implementation of
// llm.Provider. It wraps google.golang.org/genai (the official Go SDK,
// v1.57+) and exposes Gemini's distinctive knobs — Google Search
// grounding, thinkingConfig.thinkingLevel, responseSchema — through
// the provider-agnostic llm types (Message, ToolDefinition,
// ChatResult, ChatOption).
//
// The package registers itself with the parent llm factory via an
// init() so the binary entry point only needs a blank import of
// internal/llm/gemini to enable the provider. Direct construction via
// New is also supported for tests.
package gemini

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"google.golang.org/genai"

	"github.com/zenocy/zeno-v2/internal/llm"
)

func init() {
	llm.RegisterGeminiProvider(func(cfg llm.GeminiClientConfig) (llm.Provider, error) {
		return New(cfg)
	})
}

// Client is the native Gemini implementation of llm.Provider.
type Client struct {
	api     *genai.Client
	model   string
	timeout time.Duration
	retry   llm.RetryPolicy
	cfg     llm.GeminiClientConfig

	trafficLogger *logrus.Entry
}

// Compile-time guarantee that *Client satisfies llm.Provider.
var _ llm.Provider = (*Client)(nil)

// New constructs a Gemini Client. Returns an error when the API key is
// missing (the SDK will not produce a coherent error message in that
// case; failing fast at boot is friendlier) or when the SDK rejects
// the supplied configuration.
func New(cfg llm.GeminiClientConfig) (*Client, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("gemini: api_key is required (set llm.gemini.api_key or GEMINI_API_KEY env)")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 120 * time.Second
	}

	cc := &genai.ClientConfig{
		APIKey:  cfg.APIKey,
		Backend: genai.BackendGeminiAPI,
		HTTPClient: &http.Client{
			Timeout: cfg.Timeout,
		},
	}
	if cfg.Endpoint != "" {
		cc.HTTPOptions = genai.HTTPOptions{BaseURL: cfg.Endpoint}
	}

	api, err := genai.NewClient(context.Background(), cc)
	if err != nil {
		return nil, fmt.Errorf("gemini: construct genai client: %w", err)
	}
	return &Client{
		api:     api,
		model:   cfg.Model,
		timeout: cfg.Timeout,
		retry:   cfg.Retry,
		cfg:     cfg,
	}, nil
}

// Model returns the configured model name.
func (c *Client) Model() string {
	if c == nil {
		return ""
	}
	return c.model
}

// JSONSchemaEnabled reports whether the caller should attach a
// response_schema to structured-output calls. Gemini supports
// responseSchema natively for the models we target (3.x family), so
// this returns true unconditionally — callers gate their schema
// attachment on this flag the same way they do for the OpenAI client.
func (c *Client) JSONSchemaEnabled() bool {
	return c != nil
}

// NoThink reports whether callers should prepend "/no_think" to
// system prompts. Gemini has no Qwen3-style trigger; always false.
// Callers that want to suppress thinking use WithThinkingLevel("minimal")
// or set thinking_level_* in config.
func (c *Client) NoThink() bool { return false }

// NativeSearchEnabled reports whether this client is configured to
// ground responses via the built-in google_search tool. When true,
// synth surfaces SHOULD skip registering the third-party search_web
// tool and attach WithGoogleSearch() to their ChatOptions instead —
// Gemini decides when to ground transparently, returning citations
// via ChatResult.Citations.
//
// Driven by the gemini.enable_google_search YAML flag. Defaults to
// false so accidentally provider-switching doesn't kick off billed
// search queries.
func (c *Client) NativeSearchEnabled() bool {
	if c == nil {
		return false
	}
	return c.cfg.EnableGoogleSearch
}

// SetRetryInstrumentation wires per-attempt logging and the terminal
// retry observer. Safe to call once at boot before any concurrent
// traffic.
func (c *Client) SetRetryInstrumentation(logger *logrus.Entry, observer llm.RetryObserver) {
	if c == nil {
		return
	}
	c.retry.Logger = logger
	c.retry.Observer = observer
}

// SetTrafficLogger wires a per-call DEBUG logger that emits one line
// per upstream Gemini API call. Nil clears it.
func (c *Client) SetTrafficLogger(logger *logrus.Entry) {
	if c == nil {
		return
	}
	c.trafficLogger = logger
}

func (c *Client) trafficLog(fields logrus.Fields, msg string) {
	if c == nil || c.trafficLogger == nil {
		return
	}
	c.trafficLogger.WithFields(fields).Debug(msg)
}

// Reachable probes the Gemini API by issuing a free Get on the
// configured model. Returns an error when the API key is invalid, the
// model name is unknown, or the endpoint is unreachable. Used by the
// /api/health handler and the metrics publisher tick.
func (c *Client) Reachable(ctx context.Context) error {
	if c == nil {
		return errors.New("nil gemini client")
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	start := time.Now()
	_, err := c.api.Models.Get(ctx, c.model, nil)
	if err != nil {
		c.trafficLog(logrus.Fields{
			"purpose":     "reachable",
			"model":       c.model,
			"duration_ms": time.Since(start).Milliseconds(),
			"error":       err.Error(),
		}, "gemini: reachable probe failed")
		return err
	}
	c.trafficLog(logrus.Fields{
		"purpose":     "reachable",
		"model":       c.model,
		"duration_ms": time.Since(start).Milliseconds(),
	}, "gemini: reachable probe ok")
	return nil
}

// resolveThinkingLevel selects the per-call thinking level. Precedence:
// per-call WithThinkingLevel > ctx CallProfile + config map > "" (SDK default).
func (c *Client) resolveThinkingLevel(ctx context.Context, perCall string) string {
	if perCall != "" {
		return perCall
	}
	switch llm.CallProfileFromContext(ctx) {
	case llm.CallProfileBackground:
		return c.cfg.ThinkingLevelBackground
	case llm.CallProfileInteractive:
		return c.cfg.ThinkingLevelInteractive
	}
	return ""
}

// resolveIncludeThoughts picks the per-call include_thoughts setting.
// Per-call WithIncludeThoughts wins; otherwise the client's config
// default applies.
func (c *Client) resolveIncludeThoughts(perCall *bool) bool {
	if perCall != nil {
		return *perCall
	}
	return c.cfg.IncludeThoughts
}

// resolveServiceTier picks the Gemini service_tier value to send on a
// single call. Precedence (highest first):
//
//  1. Per-call WithServiceTier(tier) — explicit override.
//  2. ctx CallProfile + client serviceTier{Background,Interactive}
//     mapping — the V2.x provider-agnostic path.
//  3. "" — omit the field (Gemini defaults to standard).
//
// Allowed values are validated at config load (validateGeminiServiceTier)
// and again at request time by mapServiceTier so a typo never reaches
// the wire silently.
func (c *Client) resolveServiceTier(ctx context.Context, perCall string) string {
	if perCall != "" {
		return perCall
	}
	switch llm.CallProfileFromContext(ctx) {
	case llm.CallProfileBackground:
		return c.cfg.ServiceTierBackground
	case llm.CallProfileInteractive:
		return c.cfg.ServiceTierInteractive
	}
	return ""
}
