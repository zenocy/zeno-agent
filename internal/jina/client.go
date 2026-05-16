// Package jina is a thin client for Jina AI's hosted REST surface
// (r.jina.ai for URL→markdown reading and s.jina.ai for query→top-N
// pre-rendered search). One bearer key covers both endpoints.
//
// The package intentionally wraps only the two endpoints Zeno needs and
// avoids the pre-existing jina-ai/client-go (which targets gRPC Flow
// services, not the hosted REST APIs). Cache-aware tool wrappers live
// in internal/synth/tools_jina.go; persistence lives in cache.go.
//
// Errors are typed so callers can branch on them without parsing strings:
// ErrAuth, ErrRateLimit, ErrTimeout, ErrUpstream. On 429 the client
// returns ErrRateLimit without retrying — the LLM tool layer maps this
// to a fixed prose response so the model stops re-calling within the
// same loop.
package jina

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Client is a thin wrapper around r.jina.ai (Reader) and s.jina.ai (Search).
type Client struct {
	httpClient *http.Client
	apiKey     string
	readerURL  string
	searchURL  string
}

// Config carries the user-tunable knobs. The matching config.JinaConfig
// in internal/config maps onto this verbatim; this file does not import
// internal/config to keep the package's import surface tiny.
type Config struct {
	APIKey        string
	BaseURL       string        // default https://r.jina.ai
	SearchBaseURL string        // default https://s.jina.ai
	Timeout       time.Duration // default 20s
}

// NewClient constructs a Client. If httpClient is nil, a fresh
// *http.Client with cfg.Timeout is used.
func NewClient(cfg Config, httpClient *http.Client) *Client {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://r.jina.ai"
	}
	if cfg.SearchBaseURL == "" {
		cfg.SearchBaseURL = "https://s.jina.ai"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 20 * time.Second
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: cfg.Timeout}
	}
	return &Client{
		httpClient: httpClient,
		apiKey:     cfg.APIKey,
		readerURL:  strings.TrimRight(cfg.BaseURL, "/"),
		searchURL:  strings.TrimRight(cfg.SearchBaseURL, "/"),
	}
}

// Errors returned by Read and Search. Callers should branch on these
// with errors.Is — they wrap an underlying status/cause where useful.
var (
	ErrAuth      = errors.New("jina: authentication failed")
	ErrRateLimit = errors.New("jina: rate-limited")
	ErrTimeout   = errors.New("jina: request timed out")
	ErrUpstream  = errors.New("jina: upstream error")
)

// ReadOpts controls one Read call.
type ReadOpts struct {
	WithLinks      bool
	WithImageAlt   bool
	NoCache        bool
	TargetSelector string
}

// Document is what r.jina.ai returns under Accept: application/json.
type Document struct {
	URL       string    `json:"url"`
	Title     string    `json:"title"`
	Content   string    `json:"content"`
	FetchedAt time.Time `json:"fetched_at"`
	// LooksAuthWalled is set by the client when the body looks like a
	// login/sign-in page rather than real content. Callers (the cache
	// in particular) shorten the TTL on these so a one-off auth-walled
	// fetch doesn't poison the cache for 24h.
	LooksAuthWalled bool `json:"-"`
}

var authWallRE = regexp.MustCompile(`(?i)(sign in|log in|please authenticate|authentication required)`)

// Read fetches a URL through r.jina.ai and returns its markdown.
func (c *Client) Read(ctx context.Context, target string, opts ReadOpts) (Document, error) {
	if strings.TrimSpace(target) == "" {
		return Document{}, fmt.Errorf("jina: empty url")
	}
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return Document{}, fmt.Errorf("jina: invalid url %q (must be absolute http(s))", target)
	}
	endpoint := c.readerURL + "/" + target
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Document{}, fmt.Errorf("jina: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	if opts.WithLinks {
		req.Header.Set("X-With-Links-Summary", "true")
	}
	if opts.WithImageAlt {
		req.Header.Set("X-With-Generated-Alt", "true")
	}
	if opts.NoCache {
		req.Header.Set("X-No-Cache", "true")
	}
	if opts.TargetSelector != "" {
		req.Header.Set("X-Target-Selector", opts.TargetSelector)
	}

	body, err := c.do(req)
	if err != nil {
		return Document{}, err
	}

	// r.jina.ai with Accept: application/json wraps the markdown under
	// {"data": {"url","title","content"}}. Tolerate both wrapped and
	// flat shapes — Jina has changed this twice historically.
	var wrapped struct {
		Code   int      `json:"code"`
		Status int      `json:"status"`
		Data   Document `json:"data"`
	}
	if err := json.Unmarshal(body, &wrapped); err == nil && wrapped.Data.Content != "" {
		doc := wrapped.Data
		doc.FetchedAt = time.Now().UTC()
		doc.LooksAuthWalled = looksAuthWalled(doc.Content)
		return doc, nil
	}
	var flat Document
	if err := json.Unmarshal(body, &flat); err == nil && flat.Content != "" {
		flat.FetchedAt = time.Now().UTC()
		flat.LooksAuthWalled = looksAuthWalled(flat.Content)
		return flat, nil
	}
	// Fall back to treating the whole body as markdown. Title remains empty.
	doc := Document{
		URL:             target,
		Content:         string(body),
		FetchedAt:       time.Now().UTC(),
		LooksAuthWalled: looksAuthWalled(string(body)),
	}
	return doc, nil
}

// SearchOpts controls one Search call.
type SearchOpts struct {
	MaxResults int    // 1-10; default 5 when ≤0
	Site       string // optional domain restriction
	NoCache    bool
}

// Result is one entry from s.jina.ai.
type Result struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description"`
	Content     string `json:"content"`
}

// Search runs a query through s.jina.ai. Results come back with each
// page pre-rendered as markdown — that's the "killer feature" vs.
// Bing/Brave: one round trip yields LLM-ready context.
func (c *Client) Search(ctx context.Context, query string, opts SearchOpts) ([]Result, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, fmt.Errorf("jina: empty query")
	}
	endpoint := c.searchURL + "/" + url.PathEscape(q)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("jina: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	if opts.MaxResults > 0 {
		req.Header.Set("X-Respond-With", "no-content") // we'll re-fetch full content via Read on demand
		req.Header.Set("X-Top-K", strconv.Itoa(opts.MaxResults))
	}
	if opts.Site != "" {
		req.Header.Set("X-Site", opts.Site)
	}
	if opts.NoCache {
		req.Header.Set("X-No-Cache", "true")
	}

	body, err := c.do(req)
	if err != nil {
		return nil, err
	}
	var wrapped struct {
		Code   int      `json:"code"`
		Status int      `json:"status"`
		Data   []Result `json:"data"`
	}
	if err := json.Unmarshal(body, &wrapped); err == nil && len(wrapped.Data) > 0 {
		return wrapped.Data, nil
	}
	var flat []Result
	if err := json.Unmarshal(body, &flat); err == nil {
		return flat, nil
	}
	return nil, fmt.Errorf("%w: unparseable search response", ErrUpstream)
}

// do executes req and returns the body, mapping HTTP status to typed errors.
func (c *Client) do(req *http.Request) ([]byte, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeoutErr(err) {
			return nil, fmt.Errorf("%w: %v", ErrTimeout, err)
		}
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("%w: status=%d body=%s", ErrAuth, resp.StatusCode, snippet(body))
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, fmt.Errorf("%w: status=429 body=%s", ErrRateLimit, snippet(body))
	case resp.StatusCode >= 400:
		return nil, fmt.Errorf("%w: status=%d body=%s", ErrUpstream, resp.StatusCode, snippet(body))
	}
	return body, nil
}

func isTimeoutErr(err error) bool {
	type timeoutter interface{ Timeout() bool }
	var t timeoutter
	if errors.As(err, &t) {
		return t.Timeout()
	}
	return false
}

func looksAuthWalled(s string) bool {
	if len(s) < 200 {
		return true
	}
	head := s
	if len(head) > 500 {
		head = head[:500]
	}
	return authWallRE.MatchString(head)
}

func snippet(b []byte) string {
	const max = 200
	s := strings.TrimSpace(string(b))
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// NormalizeURL lowercases the host, strips fragments, and removes common
// tracking params. The cache key for Read uses this so equivalent URLs
// share a cache entry.
func NormalizeURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return strings.TrimSpace(raw)
	}
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	if u.RawQuery != "" {
		q := u.Query()
		for k := range q {
			lk := strings.ToLower(k)
			if strings.HasPrefix(lk, "utm_") || lk == "fbclid" || lk == "gclid" || lk == "mc_cid" || lk == "mc_eid" {
				q.Del(k)
			}
		}
		u.RawQuery = q.Encode()
	}
	if u.Path != "/" && strings.HasSuffix(u.Path, "/") {
		u.Path = strings.TrimRight(u.Path, "/")
	}
	return u.String()
}
