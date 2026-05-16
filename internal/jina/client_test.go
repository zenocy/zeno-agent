package jina

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRead_Wrapped200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("auth header missing or wrong: %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("accept header wrong: %q", got)
		}
		// Confirm the prefix-style URL: /<full target URL>
		if !strings.HasPrefix(r.URL.Path, "/https://example.com") {
			t.Errorf("unexpected path: %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":200,"status":20000,"data":{"url":"https://example.com/x","title":"Hello","content":"# Hello\n\nWorld. This is a long-enough body to clear the auth-wall heuristic threshold of 200 characters so we go down the happy path. We add some extra padding here just to be safe — at least three hundred characters of plain markdown body so the regex never sees a login phrase and the length check passes."}}`))
	}))
	defer srv.Close()

	c := NewClient(Config{APIKey: "test-key", BaseURL: srv.URL, SearchBaseURL: srv.URL}, srv.Client())
	doc, err := c.Read(context.Background(), "https://example.com/x", ReadOpts{})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if doc.Title != "Hello" {
		t.Errorf("title=%q", doc.Title)
	}
	if !strings.Contains(doc.Content, "World") {
		t.Errorf("content=%q", doc.Content)
	}
	if doc.LooksAuthWalled {
		t.Errorf("LooksAuthWalled=true on real content")
	}
	if doc.FetchedAt.IsZero() {
		t.Errorf("FetchedAt unset")
	}
}

func TestRead_RejectsRelativeURL(t *testing.T) {
	c := NewClient(Config{APIKey: "k", BaseURL: "http://localhost", SearchBaseURL: "http://localhost"}, nil)
	if _, err := c.Read(context.Background(), "/not-absolute", ReadOpts{}); err == nil {
		t.Fatalf("expected error on relative URL")
	}
}

func TestRead_AuthWalledHeuristic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"url":"https://example.com","title":"Notion","content":"Please sign in to view this document. Notion login required to continue."}}`))
	}))
	defer srv.Close()
	c := NewClient(Config{BaseURL: srv.URL}, srv.Client())
	doc, err := c.Read(context.Background(), "https://example.com/p", ReadOpts{})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !doc.LooksAuthWalled {
		t.Errorf("expected LooksAuthWalled=true on login-page content")
	}
}

func TestDo_401MapsToErrAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad token", http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := NewClient(Config{BaseURL: srv.URL, SearchBaseURL: srv.URL}, srv.Client())
	_, err := c.Read(context.Background(), "https://example.com", ReadOpts{})
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("want ErrAuth, got %v", err)
	}
}

func TestDo_429MapsToErrRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := NewClient(Config{BaseURL: srv.URL, SearchBaseURL: srv.URL}, srv.Client())
	_, err := c.Search(context.Background(), "anything", SearchOpts{})
	if !errors.Is(err, ErrRateLimit) {
		t.Fatalf("want ErrRateLimit, got %v", err)
	}
}

func TestDo_500MapsToErrUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := NewClient(Config{BaseURL: srv.URL, SearchBaseURL: srv.URL}, srv.Client())
	_, err := c.Read(context.Background(), "https://example.com", ReadOpts{})
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("want ErrUpstream, got %v", err)
	}
}

func TestDo_TimeoutMapsToErrTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()
	hc := srv.Client()
	hc.Timeout = 50 * time.Millisecond
	c := NewClient(Config{BaseURL: srv.URL, SearchBaseURL: srv.URL}, hc)
	_, err := c.Read(context.Background(), "https://example.com", ReadOpts{})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
}

func TestSearch_WrappedDataArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/") {
			t.Errorf("path=%q", r.URL.Path)
		}
		if site := r.Header.Get("X-Site"); site != "go.dev" {
			t.Errorf("X-Site=%q", site)
		}
		_, _ = w.Write([]byte(`{"code":200,"data":[{"title":"R1","url":"https://go.dev/a","description":"d1","content":"c1"},{"title":"R2","url":"https://go.dev/b","description":"d2","content":"c2"}]}`))
	}))
	defer srv.Close()
	c := NewClient(Config{SearchBaseURL: srv.URL}, srv.Client())
	results, err := c.Search(context.Background(), "generics", SearchOpts{Site: "go.dev", MaxResults: 2})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 2 || results[0].Title != "R1" || results[1].URL != "https://go.dev/b" {
		t.Fatalf("results=%+v", results)
	}
}

func TestSearch_EmptyQuery(t *testing.T) {
	c := NewClient(Config{}, nil)
	if _, err := c.Search(context.Background(), "   ", SearchOpts{}); err == nil {
		t.Fatalf("expected error on empty query")
	}
}

func TestNormalizeURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"https://Example.COM/Path/", "https://example.com/Path"},
		{"https://example.com/x?utm_source=foo&keep=1", "https://example.com/x?keep=1"},
		{"https://example.com/x#frag", "https://example.com/x"},
		{"https://example.com/", "https://example.com/"},
		{"https://example.com/x?fbclid=xyz&gclid=abc", "https://example.com/x"},
	}
	for _, c := range cases {
		got := NormalizeURL(c.in)
		if got != c.want {
			t.Errorf("NormalizeURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLooksAuthWalled(t *testing.T) {
	if !looksAuthWalled("tiny") {
		t.Errorf("short body should be flagged")
	}
	long := strings.Repeat("hello world. ", 80)
	if looksAuthWalled(long) {
		t.Errorf("long benign body should not be flagged")
	}
	loginish := "Please sign in to continue. " + strings.Repeat("more text ", 50)
	if !looksAuthWalled(loginish) {
		t.Errorf("login page should be flagged")
	}
}
