// Package jina is the saved-search sensor. It runs each configured
// SavedSearch through the Jina search client on every SyncCron tick;
// per-search throttling is delegated to the cache TTL so a 10-minute
// SyncCron paired with a 6h SearchTTL means each saved search hits
// Jina at most every 6 hours. On miss it appends one observation
// (kind: web.search.result) to the durable log and publishes the
// SensorEventObservedEvent so the inject subscriber can react.
//
// The sensor is intentionally simple: no cursors, no per-search state.
// "What did we last fetch?" is answered by the cache; "what observations
// has the user seen?" is answered by the durable log.
package jina

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/config"
	"github.com/zenocy/zeno-v2/internal/jina"
	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/sensor"
)

// Client is the narrow surface the sensor depends on. *jina.Client
// satisfies it implicitly. Tests can pass a fake.
type Client interface {
	Search(ctx context.Context, query string, opts jina.SearchOpts) ([]jina.Result, error)
}

// Sensor runs the configured saved searches.
type Sensor struct {
	searches   []config.SavedSearch
	client     Client
	cache      *jina.Store
	writer     log.Writer
	searchTTL  time.Duration
	maxResults int
	now        func() time.Time
	log        *logrus.Entry
}

// New constructs a Sensor. cache may be nil (every Sync hits Jina).
// searchTTL ≤ 0 falls back to 6h. maxResults ≤ 0 falls back to 5.
func New(searches []config.SavedSearch, client Client, cache *jina.Store, writer log.Writer, searchTTL time.Duration, maxResults int, l *logrus.Entry) *Sensor {
	if searchTTL <= 0 {
		searchTTL = 6 * time.Hour
	}
	if maxResults <= 0 {
		maxResults = 5
	}
	return &Sensor{
		searches:   searches,
		client:     client,
		cache:      cache,
		writer:     writer,
		searchTTL:  searchTTL,
		maxResults: maxResults,
		now:        time.Now,
		log:        l,
	}
}

// WithNow swaps the clock (tests).
func (s *Sensor) WithNow(now func() time.Time) *Sensor { s.now = now; return s }

// Name implements sensor.Sensor.
func (s *Sensor) Name() string { return "jina" }

// Sync implements sensor.Sensor. Iterates SavedSearches; each query is
// served from cache when fresh (no observation, no log append) or
// fetched and recorded otherwise. One bad search does not abort the
// rest — partial successes are the common case on a flaky network.
func (s *Sensor) Sync(ctx context.Context) error {
	if len(s.searches) == 0 {
		return nil
	}
	var firstErr error
	for _, ss := range s.searches {
		if err := s.runOne(ctx, ss); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			if s.log != nil {
				s.log.WithError(err).WithField("search", ss.Name).Warn("jina sensor: saved search failed")
			}
		}
	}
	return firstErr
}

func (s *Sensor) runOne(ctx context.Context, ss config.SavedSearch) error {
	if ss.Name == "" || ss.Query == "" {
		return fmt.Errorf("invalid saved_search: name and query are required")
	}
	key := jina.SearchKey(ss.Query, ss.Site, s.maxResults)
	if s.cache != nil {
		if _, hit, err := s.cache.Get(ctx, "search", key); err == nil && hit {
			// Cache fresh → already observed within TTL window. No-op.
			return nil
		}
	}

	results, err := s.client.Search(ctx, ss.Query, jina.SearchOpts{
		MaxResults: s.maxResults,
		Site:       ss.Site,
	})
	if err != nil {
		// On rate-limit, mark as a soft error: we don't want one
		// rate-limited search to cascade into the inject subscriber
		// (which doesn't observe a failed Sync). Returning err lets
		// the scheduler log it but no observation lands.
		if errors.Is(err, jina.ErrRateLimit) {
			return fmt.Errorf("rate-limited; will retry next tick: %w", err)
		}
		return err
	}

	if s.cache != nil {
		if blob, jerr := json.Marshal(results); jerr == nil {
			_ = s.cache.Put(ctx, "search", key, s.searchTTL, blob)
		}
	}

	payload := map[string]any{
		"name":       ss.Name,
		"query":      ss.Query,
		"site":       ss.Site,
		"fetched_at": s.now().UTC(),
		"results":    summarizeResults(results),
	}
	ev, err := s.writer.Append(ctx, log.KindWebSearchResult, s.Name(), payload)
	if err != nil {
		return fmt.Errorf("append observation: %w", err)
	}

	// Strict ordering: publish AFTER the successful append so the inject
	// subscriber's projections see the freshly-arrived row.
	sensor.PublishObserved(ctx, log.KindWebSearchResult, ev.ID, map[string]any{
		"name":  ss.Name,
		"query": ss.Query,
		"site":  ss.Site,
		"count": len(results),
	})
	return nil
}

// summarizeResults trims each result to the fields downstream consumers
// (inject detector, cards loop) actually need. Full Content is dropped —
// the LLM tools can re-fetch via read_url if a URL warrants expansion,
// keeping the observation payload small.
func summarizeResults(in []jina.Result) []map[string]string {
	out := make([]map[string]string, 0, len(in))
	for _, r := range in {
		desc := r.Description
		if desc == "" {
			desc = r.Content
		}
		if len(desc) > 400 {
			desc = desc[:400] + "…"
		}
		out = append(out, map[string]string{
			"title":       r.Title,
			"url":         r.URL,
			"description": desc,
		})
	}
	return out
}
