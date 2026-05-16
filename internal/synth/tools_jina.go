package synth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zenocy/zeno-v2/internal/jina"
	"github.com/zenocy/zeno-v2/internal/llm"
)

// JinaClient is the narrow surface tools_jina depends on. Tests pass a
// fake; production passes *jina.Client. Keeping the interface tiny (two
// methods) means we don't pay for wider mocking surface than the loop
// actually exercises.
type JinaClient interface {
	Read(ctx context.Context, url string, opts jina.ReadOpts) (jina.Document, error)
	Search(ctx context.Context, query string, opts jina.SearchOpts) ([]jina.Result, error)
}

// jinaTTLs carries the per-kind TTLs used when caching responses.
type jinaTTLs struct {
	Search     time.Duration
	Read       time.Duration
	AuthWalled time.Duration // applied to login-page-looking responses; <=0 → 5min
}

func (t jinaTTLs) read() time.Duration {
	if t.Read <= 0 {
		return 24 * time.Hour
	}
	return t.Read
}

func (t jinaTTLs) search() time.Duration {
	if t.Search <= 0 {
		return 6 * time.Hour
	}
	return t.Search
}

func (t jinaTTLs) authWalled() time.Duration {
	if t.AuthWalled <= 0 {
		return 5 * time.Minute
	}
	return t.AuthWalled
}

// rateLimitedReply is the prose returned to the LLM when Jina rate-limits
// us. Constant string keeps the loop's duplicate-detection (which keys on
// (tool_name, args)) from re-running an already-rate-limited call.
const rateLimitedReply = "Web access is rate-limited right now; do not retry this query in this loop. Try again later or answer without web evidence."

// ---------------------------------------------------------------------------
// search_web
// ---------------------------------------------------------------------------

// SearchWebTool runs a Jina web search and returns a compact prose
// summary the model can cite or refine. Cache-aware: identical
// (query|site|max_results) within SearchTTL is served without an
// HTTP round trip. On rate-limit, returns a fixed prose so the LLM
// stops re-calling.
type SearchWebTool struct {
	Client         JinaClient
	Cache          *jina.Store
	TTLs           jinaTTLs
	MaxCap         int // hard cap on output bytes; 0 → 6000
	DefaultResults int // default max_results when arg omitted; 0 → 3
}

func (t *SearchWebTool) Name() string { return "search_web" }

func (t *SearchWebTool) Description() string {
	return "Search the public web. Returns a numbered list of (title, url, snippet) — call read_url for full content. Use sparingly; prefer first-party data."
}

func (t *SearchWebTool) Parameters() []llm.ToolParamSpec {
	return []llm.ToolParamSpec{
		{Name: "query", Type: "string", Description: "Search query. Be specific.", Required: true},
		{Name: "max_results", Type: "integer", Description: "How many results to fetch (1-5, default 3).", Required: false},
		{Name: "site", Type: "string", Description: "Restrict to one domain, e.g. 'go.dev'.", Required: false},
	}
}

func (t *SearchWebTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	query := argString(args, "query")
	if query == "" {
		return "", fmt.Errorf("query is required")
	}
	site := argString(args, "site")
	maxRes := argInt(args, "max_results")
	if maxRes <= 0 {
		maxRes = t.DefaultResults
		if maxRes <= 0 {
			maxRes = 3
		}
	}
	if maxRes > 5 {
		maxRes = 5
	}

	cap := t.MaxCap
	if cap <= 0 {
		cap = 6000
	}

	key := jina.SearchKey(query, site, maxRes)
	if t.Cache != nil {
		if entry, hit, err := t.Cache.Get(ctx, "search", key); err == nil && hit {
			var results []jina.Result
			if jerr := json.Unmarshal(entry.Response, &results); jerr == nil {
				return formatSearchResults(query, results, cap), nil
			}
		}
	}

	results, err := t.Client.Search(ctx, query, jina.SearchOpts{MaxResults: maxRes, Site: site})
	if err != nil {
		if errors.Is(err, jina.ErrRateLimit) || errors.Is(err, jina.ErrAuth) {
			return rateLimitedReply, nil
		}
		return "", err
	}
	if t.Cache != nil {
		if blob, jerr := json.Marshal(results); jerr == nil {
			_ = t.Cache.Put(ctx, "search", key, t.TTLs.search(), blob)
		}
	}
	return formatSearchResults(query, results, cap), nil
}

func formatSearchResults(query string, results []jina.Result, cap int) string {
	if len(results) == 0 {
		return fmt.Sprintf("No results for %q.", query)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Results for %q:\n", query)
	const perResult = 600
	for i, r := range results {
		title := strings.TrimSpace(r.Title)
		if title == "" {
			title = "(untitled)"
		}
		snippet := strings.TrimSpace(r.Description)
		if snippet == "" {
			snippet = strings.TrimSpace(r.Content)
		}
		snippet = collapseWS(snippet)
		if len(snippet) > perResult {
			snippet = snippet[:perResult] + "…"
		}
		fmt.Fprintf(&b, "\n%d. %s\n   %s\n   %s\n", i+1, title, r.URL, snippet)
	}
	return capOutput(b.String(), cap)
}

// ---------------------------------------------------------------------------
// read_url
// ---------------------------------------------------------------------------

// ReadURLTool fetches one URL through Jina Reader and returns its
// markdown body. Cache-aware. The model can pass max_chars (capped at
// 12000) to read more than the default 4096 when the page is dense.
type ReadURLTool struct {
	Client     JinaClient
	Cache      *jina.Store
	TTLs       jinaTTLs
	DefaultMax int // default max_chars; 0 → 4096
	HardMax    int // hard ceiling on max_chars; 0 → 12000
}

func (t *ReadURLTool) Name() string { return "read_url" }

func (t *ReadURLTool) Description() string {
	return "Fetch one URL and return its content as markdown. Use after search_web finds something worth reading in full."
}

func (t *ReadURLTool) Parameters() []llm.ToolParamSpec {
	return []llm.ToolParamSpec{
		{Name: "url", Type: "string", Description: "Absolute http(s) URL.", Required: true},
		{Name: "max_chars", Type: "integer", Description: "Truncate body (default 4096, max 12000).", Required: false},
	}
}

func (t *ReadURLTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	target := argString(args, "url")
	if target == "" {
		return "", fmt.Errorf("url is required")
	}
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		return "", fmt.Errorf("url must be absolute http(s)")
	}
	maxChars := argInt(args, "max_chars")
	def := t.DefaultMax
	if def <= 0 {
		def = 4096
	}
	hard := t.HardMax
	if hard <= 0 {
		hard = 12000
	}
	if maxChars <= 0 {
		maxChars = def
	}
	if maxChars > hard {
		maxChars = hard
	}

	key := jina.ReadKey(target)
	if t.Cache != nil {
		if entry, hit, err := t.Cache.Get(ctx, "read", key); err == nil && hit {
			var doc jina.Document
			if jerr := json.Unmarshal(entry.Response, &doc); jerr == nil {
				return formatDocument(doc, maxChars), nil
			}
		}
	}

	doc, err := t.Client.Read(ctx, target, jina.ReadOpts{})
	if err != nil {
		if errors.Is(err, jina.ErrRateLimit) || errors.Is(err, jina.ErrAuth) {
			return rateLimitedReply, nil
		}
		return "", err
	}
	if t.Cache != nil {
		ttl := t.TTLs.read()
		if doc.LooksAuthWalled {
			ttl = t.TTLs.authWalled()
		}
		if blob, jerr := json.Marshal(doc); jerr == nil {
			_ = t.Cache.Put(ctx, "read", key, ttl, blob)
		}
	}
	return formatDocument(doc, maxChars), nil
}

func formatDocument(doc jina.Document, maxChars int) string {
	title := strings.TrimSpace(doc.Title)
	if title == "" {
		title = "(untitled)"
	}
	body := doc.Content
	if maxChars > 0 && len(body) > maxChars {
		dropped := len(body) - maxChars
		body = body[:maxChars] + fmt.Sprintf("\n\n… (truncated, %d more characters; call read_url with a higher max_chars or refine the query)", dropped)
	}
	return fmt.Sprintf("Title: %s\nURL: %s\n\n%s", title, doc.URL, body)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func argInt(args map[string]any, key string) int {
	if args == nil {
		return 0
	}
	v, ok := args[key]
	if !ok {
		return 0
	}
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	case string:
		var n int
		fmt.Sscanf(strings.TrimSpace(x), "%d", &n)
		return n
	}
	return 0
}

var wsRunRE = strings.NewReplacer("\n", " ", "\r", " ", "\t", " ")

func collapseWS(s string) string {
	s = wsRunRE.Replace(s)
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return strings.TrimSpace(s)
}
