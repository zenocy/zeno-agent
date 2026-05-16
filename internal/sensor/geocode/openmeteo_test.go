package geocode

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenMeteo_Geocode_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/search", r.URL.Path)
		require.Equal(t, "Athens", r.URL.Query().Get("name"))
		require.Equal(t, "Greece", r.URL.Query().Get("country"))
		_, _ = w.Write([]byte(`{"results":[{"name":"Athens","country":"Greece","latitude":37.9838,"longitude":23.7275}]}`))
	}))
	defer srv.Close()

	g := NewOpenMeteo().WithBaseURL(srv.URL)
	lat, lon, err := g.Geocode(context.Background(), "Athens", "Greece")
	require.NoError(t, err)
	require.InDelta(t, 37.9838, lat, 1e-6)
	require.InDelta(t, 23.7275, lon, 1e-6)
}

func TestOpenMeteo_Geocode_NoMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	_, _, err := NewOpenMeteo().WithBaseURL(srv.URL).Geocode(context.Background(), "Atlantis", "Mu")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNoMatch), "expected ErrNoMatch, got %v", err)
}

func TestOpenMeteo_Geocode_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, _, err := NewOpenMeteo().WithBaseURL(srv.URL).Geocode(context.Background(), "Athens", "")
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrNoMatch))
}

// When the country filter passes through the wire, but the upstream
// returns multiple cities sharing the same name in different countries,
// the geocoder should pick the one matching the requested country rather
// than the first row.
func TestOpenMeteo_Geocode_CountryFilterPicksMatchingResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"results":[
			{"name":"Athens","country":"United States","latitude":33.95,"longitude":-83.38},
			{"name":"Athens","country":"Greece","latitude":37.9838,"longitude":23.7275}
		]}`))
	}))
	defer srv.Close()

	lat, lon, err := NewOpenMeteo().WithBaseURL(srv.URL).Geocode(context.Background(), "Athens", "Greece")
	require.NoError(t, err)
	require.InDelta(t, 37.9838, lat, 1e-6)
	require.InDelta(t, 23.7275, lon, 1e-6)
}

func TestOpenMeteo_Geocode_RejectsEmptyCity(t *testing.T) {
	_, _, err := NewOpenMeteo().Geocode(context.Background(), "  ", "Greece")
	require.Error(t, err)
}
