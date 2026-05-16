package geocode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const openMeteoDefaultBase = "https://geocoding-api.open-meteo.com"

// ErrNoMatch is returned by Forward.Geocode when the upstream service
// answered successfully but no result matched the city/country query.
// Distinct from a network/HTTP error so callers can present a friendlier
// "we couldn't find that place" message.
var ErrNoMatch = errors.New("geocode: no match for city/country")

// Forward turns a (city, country) pair into coordinates.
type Forward interface {
	Geocode(ctx context.Context, city, country string) (lat, lon float64, err error)
}

// OpenMeteo is a Forward backed by Open-Meteo's free geocoding API
// (https://open-meteo.com/en/docs/geocoding-api). No auth, no API key.
type OpenMeteo struct {
	http *http.Client
	base string
}

// NewOpenMeteo builds a forward geocoder with sensible defaults.
func NewOpenMeteo() *OpenMeteo {
	return &OpenMeteo{
		http: &http.Client{Timeout: 10 * time.Second},
		base: openMeteoDefaultBase,
	}
}

// WithBaseURL overrides the upstream URL (used by tests with httptest).
func (o *OpenMeteo) WithBaseURL(u string) *OpenMeteo { o.base = u; return o }

// WithHTTPClient swaps the HTTP client (used by tests).
func (o *OpenMeteo) WithHTTPClient(c *http.Client) *OpenMeteo { o.http = c; return o }

// Geocode implements Forward. The upstream API supports a `country` filter
// natively, but we also do a best-effort case-insensitive match on the
// returned country name to handle queries like "Greece" vs "GR".
func (o *OpenMeteo) Geocode(ctx context.Context, city, country string) (float64, float64, error) {
	city = strings.TrimSpace(city)
	country = strings.TrimSpace(country)
	if city == "" {
		return 0, 0, errors.New("geocode: city is required")
	}

	q := url.Values{}
	q.Set("name", city)
	q.Set("count", "10")
	q.Set("language", "en")
	q.Set("format", "json")
	if country != "" {
		q.Set("country", country)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.base+"/v1/search?"+q.Encode(), nil)
	if err != nil {
		return 0, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := o.http.Do(req)
	if err != nil {
		return 0, 0, fmt.Errorf("fetch geocode: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, 0, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("geocode returned %d", resp.StatusCode)
	}

	var out struct {
		Results []struct {
			Name      string  `json:"name"`
			Country   string  `json:"country"`
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, 0, fmt.Errorf("decode: %w", err)
	}
	if len(out.Results) == 0 {
		return 0, 0, ErrNoMatch
	}

	if country != "" {
		for _, r := range out.Results {
			if strings.EqualFold(strings.TrimSpace(r.Country), country) {
				return r.Latitude, r.Longitude, nil
			}
		}
	}

	r := out.Results[0]
	return r.Latitude, r.Longitude, nil
}
