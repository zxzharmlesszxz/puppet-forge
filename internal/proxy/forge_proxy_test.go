package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	artifactstorage "puppet-forge/internal/storage"
)

type testArtifactStorage struct {
	mu          sync.Mutex
	existing    map[string]bool
	uploaded    map[string][]byte
	contentType map[string]string
	downloads   int
}

func newTestArtifactStorage() *testArtifactStorage {
	return &testArtifactStorage{
		existing:    make(map[string]bool),
		uploaded:    make(map[string][]byte),
		contentType: make(map[string]string),
	}
}

func (s *testArtifactStorage) Upload(_ context.Context, objectPath string, contentType string, body []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.existing[objectPath] = true
	s.uploaded[objectPath] = append([]byte(nil), body...)
	s.contentType[objectPath] = contentType
	return nil
}

func (s *testArtifactStorage) Exists(_ context.Context, objectPath string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.existing[objectPath], nil
}

func (s *testArtifactStorage) Download(_ context.Context, objectPath string) (artifactstorage.Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.downloads++
	return artifactstorage.Object{
		Body:        append([]byte(nil), s.uploaded[objectPath]...),
		ContentType: s.contentType[objectPath],
	}, nil
}

func (s *testArtifactStorage) Stat(_ context.Context, objectPath string) (artifactstorage.ObjectAttrs, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.existing[objectPath] {
		return artifactstorage.ObjectAttrs{}, artifactstorage.ErrObjectNotFound
	}
	return artifactstorage.ObjectAttrs{
		ContentType: s.contentType[objectPath],
		Size:        int64(len(s.uploaded[objectPath])),
	}, nil
}

func (s *testArtifactStorage) PublicURL(objectPath string) string {
	return "https://storage.googleapis.com/test-bucket/" + objectPath
}

func (s *testArtifactStorage) hasObject(objectPath string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.existing[objectPath]
}

func (s *testArtifactStorage) uploadedBody(objectPath string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]byte(nil), s.uploaded[objectPath]...)
}

func (s *testArtifactStorage) uploadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.uploaded)
}

func (s *testArtifactStorage) downloadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.downloads
}

func TestForgeProxyCachesJSONResponses(t *testing.T) {
	t.Parallel()

	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	proxy, err := NewForgeProxy(upstream.URL, time.Minute, 1024, nil, "upstream-cache")
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}

	server := httptest.NewServer(proxy.Handler())
	defer server.Close()

	for i := 0; i < 2; i++ {
		resp, err := http.Get(server.URL + "/v3/modules/puppetlabs-apache")
		if err != nil {
			t.Fatalf("GET error = %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if string(body) != `{"ok":true}` {
			t.Fatalf("unexpected body: %s", string(body))
		}
	}

	if requests != 1 {
		t.Fatalf("expected one upstream request, got %d", requests)
	}
}

func TestForgeProxyForwardsRFCForwardedProto(t *testing.T) {
	t.Parallel()

	var gotProto string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotProto = r.Header.Get("X-Forwarded-Proto")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	proxy, err := NewForgeProxy(upstream.URL, time.Minute, 1024, nil, "upstream-cache")
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}

	server := httptest.NewServer(proxy.Handler())
	defer server.Close()

	req, err := http.NewRequest(http.MethodGet, server.URL+"/v3/modules/puppetlabs-apache", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Forwarded", `for=192.0.2.10;proto=https;host=forge.example.com`)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	_ = resp.Body.Close()

	if gotProto != "https" {
		t.Fatalf("upstream X-Forwarded-Proto = %q, want https", gotProto)
	}
}

func TestForgeProxyOnlyForwardsAllowedHeadersUpstream(t *testing.T) {
	t.Parallel()

	seen := make(http.Header)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	proxy, err := NewForgeProxy(upstream.URL, 0, 1024, nil, "upstream-cache")
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}

	server := httptest.NewServer(proxy.Handler())
	defer server.Close()

	req, err := http.NewRequest(http.MethodGet, server.URL+"/v3/modules/puppetlabs-apache", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Cookie", "session=secret")
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("X-Internal-Trace", "secret-trace")
	req.Header.Set("User-Agent", "forge-test")
	req.Header.Set("Accept", "application/json")

	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	_ = resp.Body.Close()

	if seen.Get("Cookie") != "" || seen.Get("Authorization") != "" || seen.Get("X-Internal-Trace") != "" {
		t.Fatalf("sensitive headers leaked upstream: %#v", seen)
	}
	if seen.Get("User-Agent") != "forge-test" || seen.Get("Accept") != "application/json" {
		t.Fatalf("allowed headers were not forwarded upstream: %#v", seen)
	}
}

func TestParseUpstreamModuleDerivesReleaseVersionsFromSlugs(t *testing.T) {
	t.Parallel()

	module, err := parseUpstreamModule([]byte(`{
		"slug":"stm-debconf",
		"owner":"stm",
		"name":"debconf",
		"current_release":{"slug":"stm-debconf-9.1.0"},
		"releases":[
			{"slug":"stm-debconf-9.1.0"},
			"/v3/releases/stm-debconf-8.0.0"
		]
	}`))
	if err != nil {
		t.Fatalf("parseUpstreamModule() error = %v", err)
	}
	if module.CurrentRelease.Version != "9.1.0" {
		t.Fatalf("current version = %q, want 9.1.0", module.CurrentRelease.Version)
	}
	if len(module.Releases) != 2 {
		t.Fatalf("release count = %d, want 2", len(module.Releases))
	}
	if module.Releases[0].Version != "9.1.0" || module.Releases[1].Version != "8.0.0" {
		t.Fatalf("unexpected release versions: %#v", module.Releases)
	}
}

func TestForgeProxyObservesCachedGzipModuleResponses(t *testing.T) {
	t.Parallel()

	var gzipBody bytes.Buffer
	gzipWriter := gzip.NewWriter(&gzipBody)
	if _, err := gzipWriter.Write([]byte(`{
		"slug":"stm-debconf",
		"owner":"stm",
		"name":"debconf",
		"current_release":{"slug":"stm-debconf-7.0.1"}
	}`)); err != nil {
		t.Fatalf("gzip Write() error = %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("gzip Close() error = %v", err)
	}

	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(gzipBody.Bytes())
	}))
	defer upstream.Close()

	proxy, err := NewForgeProxy(upstream.URL, time.Minute, 1024, nil, "upstream-cache")
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}

	var mu sync.Mutex
	var observed []UpstreamModule
	proxy.SetModuleObserver(func(_ context.Context, module UpstreamModule) {
		mu.Lock()
		defer mu.Unlock()
		observed = append(observed, module)
	})

	server := httptest.NewServer(proxy.Handler())
	defer server.Close()

	client := server.Client()
	for i := 0; i < 2; i++ {
		req, err := http.NewRequest(http.MethodGet, server.URL+"/v3/modules/stm-debconf", nil)
		if err != nil {
			t.Fatalf("NewRequest() error = %v", err)
		}
		req.Header.Set("Accept-Encoding", "gzip")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET error = %v", err)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}

	if requests != 1 {
		t.Fatalf("expected one upstream request, got %d", requests)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(observed) != 2 {
		t.Fatalf("expected observer to run for upstream response and cache hit, got %d", len(observed))
	}
	for _, module := range observed {
		if module.Owner != "stm" || module.Name != "debconf" || module.CurrentRelease.Version != "7.0.1" {
			t.Fatalf("unexpected observed module: %#v", module)
		}
	}
}

func TestObserverJSONBodyRejectsOversizedGzipPayload(t *testing.T) {
	t.Parallel()

	var gzipBody bytes.Buffer
	gzipWriter := gzip.NewWriter(&gzipBody)
	if _, err := gzipWriter.Write(bytes.Repeat([]byte("a"), 1024)); err != nil {
		t.Fatalf("gzip Write() error = %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("gzip Close() error = %v", err)
	}

	header := http.Header{"Content-Encoding": []string{"gzip"}}
	if _, err := observerJSONBody(header, gzipBody.Bytes(), 64); !errors.Is(err, errBodyTooLarge) {
		t.Fatalf("observerJSONBody() error = %v, want errBodyTooLarge", err)
	}
}

func TestForgeProxyServesStaleJSONOnUpstreamError(t *testing.T) {
	t.Parallel()

	requests := 0
	upstreamHealthy := true
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if !upstreamHealthy {
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	proxy, err := NewForgeProxy(upstream.URL, 10*time.Millisecond, 1024, nil, "upstream-cache")
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}

	server := httptest.NewServer(proxy.Handler())
	defer server.Close()

	resp1, err := http.Get(server.URL + "/v3/modules/puppetlabs-apache")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	body1, _ := io.ReadAll(resp1.Body)
	_ = resp1.Body.Close()

	if string(body1) != `{"ok":true}` {
		t.Fatalf("unexpected initial body: %s", string(body1))
	}
	if resp1.Header.Get("X-Forge-Cache") != "MISS" {
		t.Fatalf("expected initial cache header MISS, got %q", resp1.Header.Get("X-Forge-Cache"))
	}

	time.Sleep(20 * time.Millisecond)
	upstreamHealthy = false

	resp2, err := http.Get(server.URL + "/v3/modules/puppetlabs-apache")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected stale response status 200, got %d", resp2.StatusCode)
	}
	if string(body2) != `{"ok":true}` {
		t.Fatalf("unexpected stale body: %s", string(body2))
	}
	if resp2.Header.Get("X-Forge-Cache") != "STALE" {
		t.Fatalf("expected stale cache header STALE, got %q", resp2.Header.Get("X-Forge-Cache"))
	}
	if requests != 2 {
		t.Fatalf("expected two upstream requests, got %d", requests)
	}
}

func TestForgeProxyRejectsTooOldStaleJSONOnUpstreamError(t *testing.T) {
	t.Parallel()

	requests := 0
	upstreamHealthy := true
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if !upstreamHealthy {
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	proxy, err := NewForgeProxy(upstream.URL, 10*time.Millisecond, 1024, nil, "upstream-cache", WithMaxStaleAge(5*time.Millisecond))
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}

	server := httptest.NewServer(proxy.Handler())
	defer server.Close()

	resp1, err := http.Get(server.URL + "/v3/modules/puppetlabs-apache")
	if err != nil {
		t.Fatalf("GET initial error = %v", err)
	}
	_ = resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("expected initial status 200, got %d", resp1.StatusCode)
	}

	time.Sleep(30 * time.Millisecond)
	upstreamHealthy = false

	resp2, err := http.Get(server.URL + "/v3/modules/puppetlabs-apache")
	if err != nil {
		t.Fatalf("GET stale error = %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()

	if resp2.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected upstream error after stale TTL, got %d body=%s", resp2.StatusCode, string(body2))
	}
	if resp2.Header.Get("X-Forge-Cache") == "STALE" {
		t.Fatalf("too-old stale entry was served: body=%s", string(body2))
	}
	if requests != 2 {
		t.Fatalf("expected two upstream requests, got %d", requests)
	}
}

func TestForgeProxyDoesNotCacheFiles(t *testing.T) {
	t.Parallel()

	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write([]byte("tarball"))
	}))
	defer upstream.Close()

	proxy, err := NewForgeProxy(upstream.URL, time.Minute, 1024, nil, "upstream-cache")
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}

	server := httptest.NewServer(proxy.Handler())
	defer server.Close()

	for i := 0; i < 2; i++ {
		resp, err := http.Get(server.URL + "/v3/files/puppetlabs-apache-1.0.0.tar.gz")
		if err != nil {
			t.Fatalf("GET error = %v", err)
		}
		if resp.Header.Get("X-Forge-Artifact-Cache") != "BYPASS" {
			t.Fatalf("bypass path should set X-Forge-Artifact-Cache: BYPASS, got %q", resp.Header.Get("X-Forge-Artifact-Cache"))
		}
		if resp.Header.Get("X-Forge-Cache") != "" {
			t.Fatalf("bypass path should not set X-Forge-Cache, got %q", resp.Header.Get("X-Forge-Cache"))
		}
		_ = resp.Body.Close()
	}

	if requests != 2 {
		t.Fatalf("expected two upstream requests, got %d", requests)
	}
}

func TestForgeProxyCachesArtifactsInStorage(t *testing.T) {
	t.Parallel()

	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write([]byte("tarball"))
	}))
	defer upstream.Close()

	artifacts := newTestArtifactStorage()
	proxy, err := NewForgeProxy(upstream.URL, time.Minute, 1024, artifacts, "upstream-cache")
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}

	server := httptest.NewServer(proxy.Handler())
	defer server.Close()

	resp1, err := http.Get(server.URL + "/v3/files/puppetlabs-apache-1.0.0.tar.gz")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	body1, _ := io.ReadAll(resp1.Body)
	_ = resp1.Body.Close()

	resp2, err := http.Get(server.URL + "/v3/files/puppetlabs-apache-1.0.0.tar.gz")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()

	objectPath := "upstream-cache/v3/files/puppetlabs-apache-1.0.0.tar.gz"
	if requests != 1 {
		t.Fatalf("expected one upstream request, got %d", requests)
	}
	if resp1.StatusCode != http.StatusOK || resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 responses, got %d and %d", resp1.StatusCode, resp2.StatusCode)
	}
	if string(body1) != "tarball" || string(body2) != "tarball" {
		t.Fatalf("unexpected response bodies: %q and %q", string(body1), string(body2))
	}
	if resp1.Header.Get("X-Forge-Artifact-Cache") != "MISS" || resp2.Header.Get("X-Forge-Artifact-Cache") != "HIT" {
		t.Fatalf("unexpected artifact cache headers: %q and %q", resp1.Header.Get("X-Forge-Artifact-Cache"), resp2.Header.Get("X-Forge-Artifact-Cache"))
	}
	if !artifacts.hasObject(objectPath) {
		t.Fatalf("expected object %s to be cached", objectPath)
	}
	if body := string(artifacts.uploadedBody(objectPath)); body != "tarball" {
		t.Fatalf("unexpected cached body: %s", body)
	}
}

func TestForgeProxyRejectsUpstreamArtifactsOverConfiguredLimit(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Length", "17")
		_, _ = w.Write([]byte("too-large-tarball"))
	}))
	defer upstream.Close()

	artifacts := newTestArtifactStorage()
	proxy, err := NewForgeProxy(upstream.URL, time.Minute, 1024, artifacts, "upstream-cache", WithMaxArtifactBytes(4))
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}

	server := httptest.NewServer(proxy.Handler())
	defer server.Close()

	resp, err := http.Get(server.URL + "/v3/files/puppetlabs-apache-1.0.0.tar.gz")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected upstream artifact over limit to get 413, got %d", resp.StatusCode)
	}
	if uploads := artifacts.uploadCount(); uploads != 0 {
		t.Fatalf("expected oversized artifact not to be cached, got %d uploads", uploads)
	}
}

func TestForgeProxyHeadUsesStatNotDownloadCachedArtifact(t *testing.T) {
	t.Parallel()

	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Length", "7")
		_, _ = w.Write([]byte("tarball"))
	}))
	defer upstream.Close()

	artifacts := newTestArtifactStorage()
	proxy, err := NewForgeProxy(upstream.URL, time.Minute, 1024, artifacts, "upstream-cache")
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}

	server := httptest.NewServer(proxy.Handler())
	defer server.Close()
	client := server.Client()

	path := "/v3/files/puppetlabs-apache-1.0.0.tar.gz"

	// Prime cache with GET
	resp, err := client.Get(server.URL + path)
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if upstreamCalls != 1 {
		t.Fatalf("expected 1 upstream call for GET, got %d", upstreamCalls)
	}

	prevDownloads := artifacts.downloadCount()

	// HEAD should use Stat, not Download
	req, err := http.NewRequest(http.MethodHead, server.URL+path, nil)
	if err != nil {
		t.Fatalf("NewRequest error = %v", err)
	}
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("HEAD error = %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if upstreamCalls != 1 {
		t.Fatalf("expected 0 upstream calls for HEAD, got %d", upstreamCalls-1)
	}
	if downloads := artifacts.downloadCount(); downloads != prevDownloads {
		t.Fatalf("HEAD triggered a Download call (downloads=%d, want %d)", downloads, prevDownloads)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("Content-Type") != "application/gzip" {
		t.Fatalf("HEAD Content-Type = %q, want application/gzip", resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("Content-Length") != "7" {
		t.Fatalf("HEAD Content-Length = %q, want 7", resp.Header.Get("Content-Length"))
	}
	if resp.Header.Get("X-Forge-Artifact-Cache") != "HIT" {
		t.Fatalf("HEAD X-Forge-Artifact-Cache = %q, want HIT", resp.Header.Get("X-Forge-Artifact-Cache"))
	}
	if len(body) != 0 {
		t.Fatalf("HEAD returned body with length %d, want 0", len(body))
	}
}

func TestForgeProxyHeadFallsBackToUpstreamWhenNotCached(t *testing.T) {
	t.Parallel()

	upstreamCalls := 0
	var upstreamMethod string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		upstreamMethod = r.Method
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Length", "7")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	artifacts := newTestArtifactStorage()
	proxy, err := NewForgeProxy(upstream.URL, time.Minute, 1024, artifacts, "upstream-cache")
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}

	server := httptest.NewServer(proxy.Handler())
	defer server.Close()
	client := server.Client()

	path := "/v3/files/puppetlabs-apache-1.0.0.tar.gz"
	req, err := http.NewRequest(http.MethodHead, server.URL+path, nil)
	if err != nil {
		t.Fatalf("NewRequest error = %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("HEAD error = %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if upstreamCalls != 1 {
		t.Fatalf("expected 1 upstream call for uncached HEAD, got %d", upstreamCalls)
	}
	if upstreamMethod != http.MethodHead {
		t.Fatalf("expected upstream method HEAD, got %s", upstreamMethod)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("Content-Type") != "application/gzip" {
		t.Fatalf("HEAD Content-Type = %q, want application/gzip", resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("X-Forge-Artifact-Cache") != "MISS" {
		t.Fatalf("HEAD X-Forge-Artifact-Cache = %q, want MISS", resp.Header.Get("X-Forge-Artifact-Cache"))
	}
	if resp.Header.Get("X-Forge-Cache") != "" {
		t.Fatalf("HEAD X-Forge-Cache should be empty, got %q", resp.Header.Get("X-Forge-Cache"))
	}
	if len(body) != 0 {
		t.Fatalf("HEAD returned body with length %d, want 0", len(body))
	}
}
