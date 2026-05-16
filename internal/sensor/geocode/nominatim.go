// Package geocode provides reverse geocoding for sensor data.
//
// The default backend is OpenStreetMap Nominatim, which is free and key-less
// but rate-limited and bound by an attribution / fair-use policy. We comply
// by sending a real User-Agent and caching results in-process so a long-
// running server makes at most one request per ~1km cell.
package geocode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const (
	defaultBase      = "https://nominatim.openstreetmap.org"
	defaultUserAgent = "zeno-v2 (https://github.com/zenocy/zeno-v2)"
	cellPrecision    = 100.0 // ~0.01° ≈ 1.1 km cells for cache stability
)

// Reverser turns coordinates into a human display name. An empty string with
// a nil error means "no name found"; callers should treat that as a benign
// fallback and skip the location label rather than surfacing it as an error.
type Reverser interface {
	Reverse(ctx context.Context, lat, lon float64) (string, error)
}

// Nominatim is a Reverser backed by OpenStreetMap Nominatim.
type Nominatim struct {
	http      *http.Client
	base      string
	userAgent string

	mu    sync.Mutex
	cache map[uint64]string
}

// NewNominatim builds a Nominatim client with sensible defaults.
func NewNominatim() *Nominatim {
	return &Nominatim{
		http:      &http.Client{Timeout: 10 * time.Second},
		base:      defaultBase,
		userAgent: defaultUserAgent,
		cache:     make(map[uint64]string),
	}
}

// WithBaseURL overrides the upstream URL (used by tests with httptest).
func (n *Nominatim) WithBaseURL(u string) *Nominatim { n.base = u; return n }

// WithUserAgent overrides the User-Agent header.
func (n *Nominatim) WithUserAgent(ua string) *Nominatim { n.userAgent = ua; return n }

// WithHTTPClient swaps the HTTP client (used by tests).
func (n *Nominatim) WithHTTPClient(c *http.Client) *Nominatim { n.http = c; return n }

// Reverse implements Reverser. Cache hits do not perform I/O.
func (n *Nominatim) Reverse(ctx context.Context, lat, lon float64) (string, error) {
	key := cellKey(lat, lon)
	n.mu.Lock()
	if name, ok := n.cache[key]; ok {
		n.mu.Unlock()
		return name, nil
	}
	n.mu.Unlock()

	q := url.Values{}
	q.Set("format", "jsonv2")
	q.Set("lat", fmt.Sprintf("%.6f", lat))
	q.Set("lon", fmt.Sprintf("%.6f", lon))
	q.Set("zoom", "10") // city-level

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.base+"/reverse?"+q.Encode(), nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", n.userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := n.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch reverse: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("reverse returned %d", resp.StatusCode)
	}

	var out struct {
		Address struct {
			City    string `json:"city"`
			Town    string `json:"town"`
			Village string `json:"village"`
			Suburb  string `json:"suburb"`
			Hamlet  string `json:"hamlet"`
			County  string `json:"county"`
		} `json:"address"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}

	name := pickName(
		out.Address.City, out.Address.Town, out.Address.Village,
		out.Address.Suburb, out.Address.Hamlet, out.Address.County, out.Name,
	)

	n.mu.Lock()
	n.cache[key] = name
	n.mu.Unlock()
	return name, nil
}

func pickName(opts ...string) string {
	for _, s := range opts {
		if s != "" {
			return s
		}
	}
	return ""
}

func cellKey(lat, lon float64) uint64 {
	lr := uint32(int32(lat * cellPrecision))
	ln := uint32(int32(lon * cellPrecision))
	return uint64(lr)<<32 | uint64(ln)
}
