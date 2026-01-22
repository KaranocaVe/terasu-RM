package mirror

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestMirror(t *testing.T, routes []RouteConfig) *httptest.Server {
	t.Helper()
	cfg := DefaultConfig()
	cfg.AccessLog = false
	cfg.Routes = routes
	return newTestMirrorWithConfig(t, cfg)
}

func newTestMirrorWithConfig(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	runtime, err := cfg.Runtime()
	if err != nil {
		t.Fatalf("runtime config: %v", err)
	}
	transport := NewTransport(runtime.Transport)
	m, err := New(runtime, transport)
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}
	return httptest.NewServer(m.Handler())
}

func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func TestRouteSelectionLongestPrefix(t *testing.T) {
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream", "auth")
		w.WriteHeader(http.StatusOK)
	}))
	defer auth.Close()

	root := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream", "root")
		w.WriteHeader(http.StatusOK)
	}))
	defer root.Close()

	mirror := newTestMirror(t, []RouteConfig{
		{Name: "auth", PublicPrefix: "/_auth", Upstream: auth.URL},
		{Name: "root", PublicPrefix: "/", Upstream: root.URL},
	})
	defer mirror.Close()

	resp, err := http.Get(mirror.URL + "/_auth/token")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("X-Upstream"); got != "auth" {
		t.Fatalf("expected auth upstream, got %q", got)
	}
}

func TestRequestRewriteWithUpstreamBasePath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Path", r.URL.Path)
		w.Header().Set("X-Query", r.URL.RawQuery)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	mirror := newTestMirror(t, []RouteConfig{
		{Name: "api", PublicPrefix: "/api", Upstream: upstream.URL + "/v1"},
	})
	defer mirror.Close()

	resp, err := http.Get(mirror.URL + "/api/users?id=42")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("X-Path"); got != "/v1/users" {
		t.Fatalf("unexpected upstream path: %q", got)
	}
	if got := resp.Header.Get("X-Query"); got != "id=42" {
		t.Fatalf("unexpected upstream query: %q", got)
	}
}

func TestLocationRewrite(t *testing.T) {
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer blob.Close()

	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", blob.URL+"/data")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer registry.Close()

	mirror := newTestMirror(t, []RouteConfig{
		{Name: "registry", PublicPrefix: "/", Upstream: registry.URL},
		{Name: "blob", PublicPrefix: "/_blob", Upstream: blob.URL},
	})
	defer mirror.Close()

	client := noRedirectClient()
	resp, err := client.Get(mirror.URL + "/v2/test")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	got := resp.Header.Get("Location")
	want := mirror.URL + "/_blob/data"
	if got != want {
		t.Fatalf("unexpected location: %q (want %q)", got, want)
	}
}

func TestWWWAuthenticateRewrite(t *testing.T) {
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer auth.Close()

	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", "Bearer realm=\""+auth.URL+"/token\",service=\"registry\"")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer registry.Close()

	mirror := newTestMirror(t, []RouteConfig{
		{Name: "registry", PublicPrefix: "/", Upstream: registry.URL},
		{Name: "auth", PublicPrefix: "/_auth", Upstream: auth.URL},
	})
	defer mirror.Close()

	resp, err := http.Get(mirror.URL + "/v2/")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	value := resp.Header.Get("WWW-Authenticate")
	if !strings.Contains(value, "realm=\""+mirror.URL+"/_auth/token\"") {
		t.Fatalf("unexpected WWW-Authenticate: %q", value)
	}
}

func TestUnknownLocationPreserved(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://example.com/path")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer upstream.Close()

	mirror := newTestMirror(t, []RouteConfig{
		{Name: "root", PublicPrefix: "/", Upstream: upstream.URL},
	})
	defer mirror.Close()

	client := noRedirectClient()
	resp, err := client.Get(mirror.URL + "/anything")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Location"); got != "https://example.com/path" {
		t.Fatalf("unexpected location: %q", got)
	}
}

func TestConcurrentRequests(t *testing.T) {
	var count int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&count, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	mirror := newTestMirror(t, []RouteConfig{
		{Name: "root", PublicPrefix: "/", Upstream: upstream.URL},
	})
	defer mirror.Close()

	const total = 25
	var wg sync.WaitGroup
	wg.Add(total)
	for i := 0; i < total; i++ {
		go func() {
			defer wg.Done()
			resp, err := http.Get(mirror.URL + "/ping")
			if err != nil {
				t.Errorf("request failed: %v", err)
				return
			}
			resp.Body.Close()
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt64(&count); got != total {
		t.Fatalf("expected %d requests, got %d", total, got)
	}
}

func TestHealthEndpoints(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfg := DefaultConfig()
	cfg.AccessLog = false
	cfg.Routes = []RouteConfig{{Name: "root", PublicPrefix: "/", Upstream: upstream.URL}}
	mirror := newTestMirrorWithConfig(t, cfg)
	defer mirror.Close()

	client := &http.Client{Timeout: 2 * time.Second}
	for _, path := range []string{"/_rmirror/healthz", "/_rmirror/readyz"} {
		resp, err := client.Get(mirror.URL + path)
		if err != nil {
			t.Fatalf("request %s failed: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("unexpected status for %s: %d", path, resp.StatusCode)
		}
	}
}

func TestMaxInflightLimit(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfg := DefaultConfig()
	cfg.AccessLog = false
	cfg.Routes = []RouteConfig{{Name: "root", PublicPrefix: "/", Upstream: upstream.URL}}
	cfg.Limits.MaxInflight = 1
	cfg.Limits.MaxInflightWait = "0s"
	mirror := newTestMirrorWithConfig(t, cfg)
	defer mirror.Close()

	client := &http.Client{Timeout: 2 * time.Second}
	firstErr := make(chan error, 1)
	go func() {
		resp, err := client.Get(mirror.URL + "/slow")
		if err == nil {
			resp.Body.Close()
		}
		firstErr <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for upstream to start")
	}

	resp, err := client.Get(mirror.URL + "/busy")
	if err != nil {
		t.Fatalf("second request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}

	close(release)
	if err := <-firstErr; err != nil {
		t.Fatalf("first request failed: %v", err)
	}
}
