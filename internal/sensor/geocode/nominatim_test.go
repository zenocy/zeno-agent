package geocode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNominatim_Reverse_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/reverse", r.URL.Path)
		require.NotEmpty(t, r.URL.Query().Get("lat"))
		require.NotEmpty(t, r.URL.Query().Get("lon"))
		require.Contains(t, r.Header.Get("User-Agent"), "zeno-v2")
		_, _ = w.Write([]byte(`{"name":"San Francisco","address":{"city":"San Francisco"}}`))
	}))
	defer srv.Close()

	n := NewNominatim().WithBaseURL(srv.URL)
	got, err := n.Reverse(context.Background(), 37.7749, -122.4194)
	require.NoError(t, err)
	require.Equal(t, "San Francisco", got)
}

func TestNominatim_Reverse_PrefersTownWhenNoCity(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"address":{"town":"Mountain View","village":"Should not pick"}}`))
	}))
	defer s.Close()

	got, err := NewNominatim().WithBaseURL(s.URL).Reverse(context.Background(), 1, 1)
	require.NoError(t, err)
	require.Equal(t, "Mountain View", got)
}

func TestNominatim_Reverse_Caches(t *testing.T) {
	var calls int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{"address":{"city":"Athens"}}`))
	}))
	defer s.Close()

	n := NewNominatim().WithBaseURL(s.URL)
	for range 5 {
		_, err := n.Reverse(context.Background(), 37.9838, 23.7275)
		require.NoError(t, err)
	}
	require.Equal(t, int32(1), atomic.LoadInt32(&calls), "cache should suppress repeat calls at the same cell")
}

func TestNominatim_Reverse_HTTPError(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(429)
	}))
	defer s.Close()

	got, err := NewNominatim().WithBaseURL(s.URL).Reverse(context.Background(), 1, 1)
	require.Error(t, err)
	require.Empty(t, got)
}

func TestNominatim_Reverse_EmptyResult(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer s.Close()

	got, err := NewNominatim().WithBaseURL(s.URL).Reverse(context.Background(), 1, 1)
	require.NoError(t, err)
	require.Equal(t, "", got, "no error, just no name")
}
