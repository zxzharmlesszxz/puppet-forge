package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	artifactstorage "github.com/zxzharmlesszxz/puppet-forge/internal/storage"
)

type testArtifactStorage struct {
	mu            sync.Mutex
	existing      map[string]bool
	uploaded      map[string][]byte
	contentType   map[string]string
	downloads     int
	readerUploads int
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

func (s *testArtifactStorage) UploadIfAbsent(_ context.Context, objectPath string, contentType string, body []byte) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.existing[objectPath] {
		return false, nil
	}
	s.existing[objectPath] = true
	s.uploaded[objectPath] = append([]byte(nil), body...)
	s.contentType[objectPath] = contentType
	return true, nil
}

func (s *testArtifactStorage) UploadReaderIfAbsent(ctx context.Context, objectPath string, contentType string, body io.Reader) (bool, error) {
	s.mu.Lock()
	s.readerUploads++
	s.mu.Unlock()
	data, err := io.ReadAll(body)
	if err != nil {
		return false, err
	}
	return s.UploadIfAbsent(ctx, objectPath, contentType, data)
}

func (s *testArtifactStorage) readerUploadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readerUploads
}

func (s *testArtifactStorage) Delete(_ context.Context, objectPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.existing, objectPath)
	delete(s.uploaded, objectPath)
	delete(s.contentType, objectPath)
	return nil
}

func (s *testArtifactStorage) Exists(_ context.Context, objectPath string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.existing[objectPath], nil
}

func (s *testArtifactStorage) Open(_ context.Context, objectPath string) (artifactstorage.ObjectReader, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.downloads++
	body := append([]byte(nil), s.uploaded[objectPath]...)
	return artifactstorage.ObjectReader{
		Body:        io.NopCloser(bytes.NewReader(body)),
		ContentType: s.contentType[objectPath],
		Size:        int64(len(body)),
	}, nil
}

func (s *testArtifactStorage) OpenRange(_ context.Context, objectPath string, offset, length int64) (artifactstorage.ObjectReader, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body := s.uploaded[objectPath]
	end := offset + length
	if !s.existing[objectPath] || offset < 0 || length < 0 || end > int64(len(body)) {
		return artifactstorage.ObjectReader{}, artifactstorage.ErrObjectNotFound
	}
	selected := append([]byte(nil), body[offset:end]...)
	return artifactstorage.ObjectReader{
		Body:        io.NopCloser(bytes.NewReader(selected)),
		ContentType: s.contentType[objectPath],
		Size:        int64(len(selected)),
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
		ETag:        `"test-etag"`,
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

func newTestForgeProxy(upstream *httptest.Server, cacheTTL time.Duration, maxBodyBytes int64, artifacts artifactstorage.ArtifactStorage, artifactPrefix string, opts ...Option) (*ForgeProxy, error) {
	opts = append(opts, WithHTTPClient(upstream.Client()), WithPrivateNetworks())
	return NewForgeProxy(upstream.URL, cacheTTL, maxBodyBytes, artifacts, artifactPrefix, opts...)
}

func artifactProxyStatus(t *testing.T, upstream *httptest.Server, artifacts artifactstorage.ArtifactStorage, opts ...Option) int {
	t.Helper()

	forgeProxy, err := newTestForgeProxy(upstream, time.Minute, 1024, artifacts, "upstream-cache", opts...)
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}
	server := httptest.NewServer(forgeProxy.Handler())
	defer server.Close()

	resp, err := http.Get(server.URL + "/v3/files/puppetlabs-apache-1.0.0.tar.gz")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

type testLease struct {
	holder     string
	leaseUntil time.Time
}

type testLeaseStore struct {
	mu           sync.Mutex
	leases       map[string]testLease
	acquisitions map[string]int
	failAfter    int
}

func newTestLeaseStore() *testLeaseStore {
	return &testLeaseStore{
		leases:       make(map[string]testLease),
		acquisitions: make(map[string]int),
	}
}

func (s *testLeaseStore) AcquireLease(_ context.Context, name, holder string, duration time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failAfter > 0 && s.acquisitions[name] >= s.failAfter {
		return false, errors.New("lease backend unavailable")
	}
	current, exists := s.leases[name]
	if exists && current.holder != holder && current.leaseUntil.After(time.Now()) {
		return false, nil
	}
	s.leases[name] = testLease{holder: holder, leaseUntil: time.Now().Add(duration)}
	s.acquisitions[name]++
	return true, nil
}

func TestArtifactLeaseCancelsWorkWhenRenewalFails(t *testing.T) {
	t.Parallel()

	leases := newTestLeaseStore()
	leases.failAfter = 1
	forgeProxy := &ForgeProxy{
		leaseStore:       leases,
		artifactLeaseTTL: 30 * time.Millisecond,
	}
	leaseCtx, release, err := forgeProxy.acquireArtifactLease(context.Background(), "upstream-cache/v3/files/teamname-module-2.0.0.tar.gz")
	if err != nil {
		t.Fatalf("acquireArtifactLease() error = %v", err)
	}
	if release == nil {
		t.Fatal("acquireArtifactLease() returned nil release")
	}
	defer release()

	select {
	case <-leaseCtx.Done():
		if !errors.Is(leaseCtx.Err(), context.Canceled) {
			t.Fatalf("lease context error = %v, want context canceled", leaseCtx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("lease context was not canceled after renewal failure")
	}
}

func (s *testLeaseStore) ReleaseLease(_ context.Context, name, holder string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, exists := s.leases[name]; exists && current.holder == holder {
		delete(s.leases, name)
	}
	return nil
}

func (s *testLeaseStore) acquisitionCount(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acquisitions[name]
}

func TestArtifactLeaseRenewsUntilReleased(t *testing.T) {
	t.Parallel()

	leases := newTestLeaseStore()
	forgeProxy := &ForgeProxy{
		leaseStore:       leases,
		artifactLeaseTTL: 30 * time.Millisecond,
	}
	const objectPath = "upstream-cache/v3/files/teamname-module-1.0.0.tar.gz"
	leaseCtx, release, err := forgeProxy.acquireArtifactLease(context.Background(), objectPath)
	if err != nil {
		t.Fatalf("acquireArtifactLease() error = %v", err)
	}
	if release == nil {
		t.Fatal("acquireArtifactLease() returned nil release")
	}
	if err := leaseCtx.Err(); err != nil {
		t.Fatalf("lease context error = %v", err)
	}

	leaseDigest := sha256.Sum256([]byte(objectPath))
	leaseName := fmt.Sprintf("upstream-artifact-%x", leaseDigest[:])
	deadline := time.Now().Add(time.Second)
	for leases.acquisitionCount(leaseName) < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := leases.acquisitionCount(leaseName); got < 2 {
		t.Fatalf("lease acquisition count = %d, want heartbeat renewal", got)
	}
	if acquired, err := leases.AcquireLease(context.Background(), leaseName, "other-holder", 30*time.Millisecond); err != nil {
		t.Fatalf("competing AcquireLease() error = %v", err)
	} else if acquired {
		t.Fatal("competing holder acquired a renewed lease")
	}

	release()
	if acquired, err := leases.AcquireLease(context.Background(), leaseName, "other-holder", 30*time.Millisecond); err != nil {
		t.Fatalf("AcquireLease(after release) error = %v", err)
	} else if !acquired {
		t.Fatal("competing holder could not acquire released lease")
	}
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

	proxy, err := newTestForgeProxy(upstream, time.Minute, 1024, nil, "upstream-cache")
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}

	server := httptest.NewServer(proxy.Handler())
	defer server.Close()

	for range 2 {
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

	proxy, err := newTestForgeProxy(upstream, time.Minute, 1024, nil, "upstream-cache")
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

	proxy, err := newTestForgeProxy(upstream, 0, 1024, nil, "upstream-cache")
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

func TestParseUpstreamModuleAcceptsObjectOwner(t *testing.T) {
	t.Parallel()

	module, err := parseUpstreamModule([]byte(`{
		"slug":"puppetlabs-apt",
		"owner":{"slug":"puppetlabs","username":"Puppet Labs"},
		"name":"apt",
		"current_release":{"slug":"puppetlabs-apt-11.3.2","version":"11.3.2"}
	}`))
	if err != nil {
		t.Fatalf("parseUpstreamModule() error = %v", err)
	}
	if module.Owner != "puppetlabs" || module.Name != "apt" {
		t.Fatalf("module identity = %q/%q, want puppetlabs/apt", module.Owner, module.Name)
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

	proxy, err := newTestForgeProxy(upstream, time.Minute, 1024, nil, "upstream-cache")
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}

	var mu sync.Mutex
	var observed []UpstreamModule
	var freshness []bool
	proxy.SetModuleObserver(func(_ context.Context, module UpstreamModule, fresh bool) {
		mu.Lock()
		defer mu.Unlock()
		observed = append(observed, module)
		freshness = append(freshness, fresh)
	})

	server := httptest.NewServer(proxy.Handler())
	defer server.Close()

	client := server.Client()
	for range 2 {
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
	if !freshness[0] || freshness[1] {
		t.Fatalf("observer freshness = %v, want [true false]", freshness)
	}
	for _, module := range observed {
		if module.Owner != "stm" || module.Name != "debconf" || module.CurrentRelease.Version != "7.0.1" {
			t.Fatalf("unexpected observed module: %#v", module)
		}
	}
}

func TestForgeProxyObservesColdModuleBeforeWritingResponse(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"slug":"puppetlabs-stdlib",
			"owner":"puppetlabs",
			"name":"stdlib",
			"current_release":{"slug":"puppetlabs-stdlib-10.0.2","version":"10.0.2"}
		}`))
	}))
	defer upstream.Close()

	forgeProxy, err := newTestForgeProxy(upstream, time.Minute, 1024, nil, "upstream-cache")
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}

	var observerErr error
	forgeProxy.SetModuleObserver(func(ctx context.Context, _ UpstreamModule, _ bool) {
		observerErr = ctx.Err()
		if writerStarted(ctx) {
			t.Fatal("observer ran after the response write started")
		}
	})

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	writeStarted := &atomic.Bool{}
	requestCtx = context.WithValue(requestCtx, responseWriteStartedKey{}, writeStarted)
	req := httptest.NewRequest(http.MethodGet, "/v3/modules/puppetlabs-stdlib", nil).WithContext(requestCtx)
	recorder := httptest.NewRecorder()
	writer := &cancelOnWriteResponseWriter{ResponseWriter: recorder, cancel: cancelRequest, started: writeStarted}
	forgeProxy.Handler().ServeHTTP(writer, req)

	if !writer.canceled {
		t.Fatal("response writer did not cancel the request context")
	}
	if observerErr != nil {
		t.Fatalf("observer context error = %v, want nil", observerErr)
	}
}

type cancelOnWriteResponseWriter struct {
	http.ResponseWriter
	cancel   context.CancelFunc
	started  *atomic.Bool
	canceled bool
}

func (w *cancelOnWriteResponseWriter) Write(body []byte) (int, error) {
	w.started.Store(true)
	w.cancel()
	w.canceled = true
	return w.ResponseWriter.Write(body)
}

type responseWriteStartedKey struct{}

func writerStarted(ctx context.Context) bool {
	started, _ := ctx.Value(responseWriteStartedKey{}).(*atomic.Bool)
	return started != nil && started.Load()
}

func TestForgeProxyNormalizesJSONAcceptEncoding(t *testing.T) {
	t.Parallel()

	var upstreamAcceptEncoding string
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		upstreamAcceptEncoding = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	proxy, err := newTestForgeProxy(upstream, time.Minute, 1024, nil, "upstream-cache")
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}
	server := httptest.NewServer(proxy.Handler())
	defer server.Close()

	for _, encoding := range []string{"gzip", "br"} {
		req, err := http.NewRequest(http.MethodGet, server.URL+"/v3/modules/teamname-module", nil)
		if err != nil {
			t.Fatalf("NewRequest() error = %v", err)
		}
		req.Header.Set("Accept-Encoding", encoding)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("GET error = %v", err)
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil || string(body) != `{"ok":true}` {
			t.Fatalf("response body = %q error = %v", body, readErr)
		}
		if resp.Header.Get("Content-Encoding") != "" {
			t.Fatalf("response Content-Encoding = %q, want identity", resp.Header.Get("Content-Encoding"))
		}
	}
	if requests != 1 {
		t.Fatalf("upstream requests = %d, want one normalized cache entry", requests)
	}
	if upstreamAcceptEncoding != "identity" {
		t.Fatalf("upstream Accept-Encoding = %q, want identity", upstreamAcceptEncoding)
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

	proxy, err := newTestForgeProxy(upstream, 10*time.Millisecond, 1024, nil, "upstream-cache")
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

	proxy, err := newTestForgeProxy(upstream, 10*time.Millisecond, 1024, nil, "upstream-cache", WithMaxStaleAge(5*time.Millisecond))
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

	proxy, err := newTestForgeProxy(upstream, time.Minute, 1024, nil, "upstream-cache")
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}

	server := httptest.NewServer(proxy.Handler())
	defer server.Close()

	for range 2 {
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

func TestForgeProxyTreatsOversizedBypassArtifactAsUpstreamFailure(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write([]byte("too-large-tarball"))
	}))
	defer upstream.Close()

	if status := artifactProxyStatus(t, upstream, nil, WithMaxArtifactBytes(4)); status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", status)
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
	proxy, err := newTestForgeProxy(upstream, time.Minute, 1024, artifacts, "upstream-cache")
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
	if uploads := artifacts.readerUploadCount(); uploads != 1 {
		t.Fatalf("streaming uploads = %d, want 1", uploads)
	}
}

func TestEnsureArtifactIntegrityRepairsCorruptCachedObject(t *testing.T) {
	t.Parallel()

	archive := []byte("authoritative-tarball")
	digest := sha256.Sum256(archive)
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(archive)
	}))
	t.Cleanup(upstream.Close)

	artifacts := newTestArtifactStorage()
	const objectPath = "upstream-cache/v3/files/puppetlabs-apache-1.0.0.tar.gz"
	artifacts.existing[objectPath] = true
	artifacts.uploaded[objectPath] = []byte("corrupt")
	artifacts.contentType[objectPath] = "application/gzip"
	forgeProxy, err := newTestForgeProxy(upstream, time.Minute, 1024, artifacts, "upstream-cache")
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}

	err = forgeProxy.EnsureArtifactIntegrity(
		context.Background(),
		"/v3/files/puppetlabs-apache-1.0.0.tar.gz",
		hex.EncodeToString(digest[:]),
		int64(len(archive)),
	)
	if err != nil {
		t.Fatalf("EnsureArtifactIntegrity() error = %v", err)
	}
	if got := artifacts.uploadedBody(objectPath); !bytes.Equal(got, archive) {
		t.Fatalf("repaired body = %q, want %q", got, archive)
	}
	if requests.Load() != 1 {
		t.Fatalf("upstream requests = %d, want 1", requests.Load())
	}
}

func TestEnsureArtifactIntegrityCoalescesRepairAcrossReplicas(t *testing.T) {
	t.Parallel()

	archive := []byte("authoritative-tarball")
	digest := sha256.Sum256(archive)
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(archive)
	}))
	t.Cleanup(upstream.Close)

	artifacts := newTestArtifactStorage()
	leases := newTestLeaseStore()
	const objectPath = "upstream-cache/v3/files/puppetlabs-apache-1.0.0.tar.gz"
	artifacts.existing[objectPath] = true
	artifacts.uploaded[objectPath] = []byte("corrupt")
	artifacts.contentType[objectPath] = "application/gzip"
	proxies := make([]*ForgeProxy, 2)
	for i := range proxies {
		forgeProxy, err := newTestForgeProxy(upstream, time.Minute, 1024, artifacts, "upstream-cache", WithLeaseStore(leases))
		if err != nil {
			t.Fatalf("NewForgeProxy(%d) error = %v", i, err)
		}
		proxies[i] = forgeProxy
	}

	start := make(chan struct{})
	errorsByReplica := make(chan error, len(proxies))
	var wait sync.WaitGroup
	for _, forgeProxy := range proxies {
		wait.Go(func() {
			<-start
			errorsByReplica <- forgeProxy.EnsureArtifactIntegrity(
				context.Background(),
				"/v3/files/puppetlabs-apache-1.0.0.tar.gz",
				hex.EncodeToString(digest[:]),
				int64(len(archive)),
			)
		})
	}
	close(start)
	wait.Wait()
	close(errorsByReplica)
	for err := range errorsByReplica {
		if err != nil {
			t.Fatalf("EnsureArtifactIntegrity() error = %v", err)
		}
	}
	if got := artifacts.uploadedBody(objectPath); !bytes.Equal(got, archive) {
		t.Fatalf("repaired body = %q, want %q", got, archive)
	}
	if requests.Load() != 1 {
		t.Fatalf("upstream requests = %d, want 1", requests.Load())
	}
}

func TestArtifactUploadReaderValidatesLengthAndLimitWhileStreaming(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		body          string
		contentLength int64
		maxBytes      int64
		want          string
		wantError     error
	}{
		{name: "known exact", body: "artifact", contentLength: 8, maxBytes: 16, want: "artifact"},
		{name: "unknown exact limit", body: "artifact", contentLength: -1, maxBytes: 8, want: "artifact"},
		{name: "known short", body: "short", contentLength: 8, maxBytes: 16, wantError: io.ErrUnexpectedEOF},
		{name: "known long", body: "too-long", contentLength: 3, maxBytes: 16, wantError: io.ErrUnexpectedEOF},
		{name: "unknown over limit", body: "too-long", contentLength: -1, maxBytes: 3, wantError: errBodyTooLarge},
		{name: "declared over limit", body: "too-long", contentLength: 8, maxBytes: 3, wantError: errBodyTooLarge},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reader, err := newArtifactUploadReader(strings.NewReader(tc.body), tc.contentLength, tc.maxBytes)
			if err == nil {
				var body []byte
				body, err = io.ReadAll(reader)
				if string(body) != tc.want && tc.wantError == nil {
					t.Fatalf("body = %q, want %q", body, tc.want)
				}
			}
			if !errors.Is(err, tc.wantError) {
				t.Fatalf("error = %v, want %v", err, tc.wantError)
			}
		})
	}
}

func TestForgeProxyCachesFullArtifactAndServesRanges(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64
	var upstreamRange atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		upstreamRange.Store(r.Header.Get("Range"))
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write([]byte("tarball"))
	}))
	defer upstream.Close()

	artifacts := newTestArtifactStorage()
	forgeProxy, err := newTestForgeProxy(upstream, time.Minute, 1024, artifacts, "upstream-cache")
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}
	server := httptest.NewServer(forgeProxy.Handler())
	defer server.Close()
	artifactURL := server.URL + "/v3/files/teamname-module-1.0.0.tar.gz"

	requestRange := func(value string) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, artifactURL, nil)
		if err != nil {
			t.Fatalf("NewRequest() error = %v", err)
		}
		req.Header.Set("Range", value)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("Do() error = %v", err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("ReadAll() error = %v", err)
		}
		return resp, body
	}

	first, body := requestRange("bytes=1-3")
	if first.StatusCode != http.StatusPartialContent || string(body) != "arb" || first.Header.Get("Content-Range") != "bytes 1-3/7" {
		t.Fatalf("first range = %d %q headers=%#v", first.StatusCode, body, first.Header)
	}
	if got, _ := upstreamRange.Load().(string); got != "" {
		t.Fatalf("cache population forwarded client Range upstream: %q", got)
	}
	second, body := requestRange("bytes=-2")
	if second.StatusCode != http.StatusPartialContent || string(body) != "ll" || second.Header.Get("X-Forge-Artifact-Cache") != "HIT" {
		t.Fatalf("cached suffix range = %d %q headers=%#v", second.StatusCode, body, second.Header)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("upstream requests = %d, want 1", got)
	}
	if got := artifacts.downloadCount(); got != 0 {
		t.Fatalf("range requests downloaded full cached object %d times", got)
	}
}

func TestForgeProxyCoalescesConcurrentArtifactMissesAcrossReplicas(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write([]byte("tarball"))
	}))
	defer upstream.Close()

	artifacts := newTestArtifactStorage()
	leases := newTestLeaseStore()
	first, err := newTestForgeProxy(upstream, time.Minute, 1024, artifacts, "upstream-cache", WithLeaseStore(leases))
	if err != nil {
		t.Fatalf("NewForgeProxy(first) error = %v", err)
	}
	second, err := newTestForgeProxy(upstream, time.Minute, 1024, artifacts, "upstream-cache", WithLeaseStore(leases))
	if err != nil {
		t.Fatalf("NewForgeProxy(second) error = %v", err)
	}
	servers := []*httptest.Server{httptest.NewServer(first.Handler()), httptest.NewServer(second.Handler())}
	defer servers[0].Close()
	defer servers[1].Close()

	const clients = 20
	start := make(chan struct{})
	errorsByClient := make(chan error, clients)
	var wait sync.WaitGroup
	for i := range clients {
		wait.Go(func() {
			<-start
			resp, err := http.Get(servers[i%len(servers)].URL + "/v3/files/teamname-module-1.0.0.tar.gz")
			if err != nil {
				errorsByClient <- err
				return
			}
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				errorsByClient <- err
				return
			}
			if resp.StatusCode != http.StatusOK || string(body) != "tarball" {
				errorsByClient <- errors.New("unexpected artifact response")
			}
		})
	}
	close(start)
	wait.Wait()
	close(errorsByClient)
	for err := range errorsByClient {
		t.Fatal(err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("upstream requests = %d, want 1", got)
	}
}

func TestForgeProxyRetriesArtifactAfterLeaseHolderFailure(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			http.Error(w, "temporary failure", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write([]byte("tarball"))
	}))
	defer upstream.Close()

	artifacts := newTestArtifactStorage()
	leases := newTestLeaseStore()
	forgeProxy, err := newTestForgeProxy(upstream, time.Minute, 1024, artifacts, "upstream-cache", WithLeaseStore(leases))
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}
	server := httptest.NewServer(forgeProxy.Handler())
	defer server.Close()
	artifactURL := server.URL + "/v3/files/teamname-module-1.0.0.tar.gz"

	first, err := http.Get(artifactURL)
	if err != nil {
		t.Fatalf("first GET error = %v", err)
	}
	_ = first.Body.Close()
	if first.StatusCode != http.StatusBadGateway {
		t.Fatalf("first status = %d, want 502", first.StatusCode)
	}
	second, err := http.Get(artifactURL)
	if err != nil {
		t.Fatalf("second GET error = %v", err)
	}
	body, _ := io.ReadAll(second.Body)
	_ = second.Body.Close()
	if second.StatusCode != http.StatusOK || string(body) != "tarball" {
		t.Fatalf("second response = %d %q, want 200 tarball", second.StatusCode, body)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("upstream requests = %d, want 2", got)
	}
}

func TestEnsureArtifactCallerCancellationDoesNotCancelSharedDownload(t *testing.T) {
	digest := sha256.Sum256([]byte("tarball"))
	for _, tc := range []struct {
		name   string
		ensure func(context.Context, *ForgeProxy, string) error
	}{
		{name: "materialize", ensure: func(ctx context.Context, forgeProxy *ForgeProxy, fileURI string) error {
			return forgeProxy.EnsureArtifact(ctx, fileURI)
		}},
		{name: "integrity", ensure: func(ctx context.Context, forgeProxy *ForgeProxy, fileURI string) error {
			return forgeProxy.EnsureArtifactIntegrity(ctx, fileURI, hex.EncodeToString(digest[:]), int64(len("tarball")))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			started := make(chan struct{})
			release := make(chan struct{})
			var requests atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if requests.Add(1) == 1 {
					close(started)
				}
				<-release
				w.Header().Set("Content-Type", "application/gzip")
				_, _ = w.Write([]byte("tarball"))
			}))
			defer upstream.Close()

			artifacts := newTestArtifactStorage()
			forgeProxy, err := newTestForgeProxy(upstream, time.Minute, 1024, artifacts, "upstream-cache")
			if err != nil {
				t.Fatalf("NewForgeProxy() error = %v", err)
			}
			const fileURI = "/v3/files/teamname-module-1.0.0.tar.gz"
			firstCtx, cancelFirst := context.WithCancel(context.Background())
			firstDone := make(chan error, 1)
			go func() {
				firstDone <- tc.ensure(firstCtx, forgeProxy, fileURI)
			}()
			<-started
			cancelFirst()
			select {
			case err := <-firstDone:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("first ensure error = %v, want context.Canceled", err)
				}
			case <-time.After(time.Second):
				t.Fatal("canceled caller remained blocked on shared download")
			}

			secondDone := make(chan error, 1)
			go func() {
				secondDone <- tc.ensure(context.Background(), forgeProxy, fileURI)
			}()
			close(release)
			select {
			case err := <-secondDone:
				if err != nil {
					t.Fatalf("second ensure error = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("shared download did not complete")
			}
			if got := requests.Load(); got != 1 {
				t.Fatalf("upstream requests = %d, want 1", got)
			}
			if !artifacts.hasObject("upstream-cache/v3/files/teamname-module-1.0.0.tar.gz") {
				t.Fatal("shared download did not populate artifact cache")
			}
		})
	}
}

func TestForgeProxyDoesNotServeOrCachePartialUpstreamArtifact(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("partial"))
	}))
	defer upstream.Close()

	artifacts := newTestArtifactStorage()
	forgeProxy, err := newTestForgeProxy(upstream, time.Minute, 1024, artifacts, "upstream-cache")
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}
	server := httptest.NewServer(forgeProxy.Handler())
	defer server.Close()

	resp, err := http.Get(server.URL + "/v3/files/teamname-module-1.0.0.tar.gz")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d body=%q, want 502", resp.StatusCode, body)
	}
	if artifacts.uploadCount() != 0 {
		t.Fatal("partial upstream artifact was cached")
	}
}

func TestForgeProxyRejectsUnexpectedSuccessfulArtifactStatuses(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusNoContent, http.StatusPartialContent} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				if status != http.StatusNoContent {
					_, _ = w.Write([]byte("partial"))
				}
			}))
			defer upstream.Close()

			artifacts := newTestArtifactStorage()
			if got := artifactProxyStatus(t, upstream, artifacts); got != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502", got)
			}
			if uploads := artifacts.uploadCount(); uploads != 0 {
				t.Fatalf("unexpected upstream status was cached with %d uploads", uploads)
			}
		})
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
	if status := artifactProxyStatus(t, upstream, artifacts, WithMaxArtifactBytes(4)); status != http.StatusBadGateway {
		t.Fatalf("expected upstream artifact over limit to get 502, got %d", status)
	}
	if uploads := artifacts.uploadCount(); uploads != 0 {
		t.Fatalf("expected oversized artifact not to be cached, got %d uploads", uploads)
	}
}

func TestForgeProxyRejectsChunkedUpstreamArtifactOverConfiguredLimit(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte("too-large-tarball"))
	}))
	defer upstream.Close()

	artifacts := newTestArtifactStorage()
	if status := artifactProxyStatus(t, upstream, artifacts, WithMaxArtifactBytes(4)); status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", status)
	}
	if uploads := artifacts.uploadCount(); uploads != 0 {
		t.Fatalf("oversized chunked artifact created %d cached objects", uploads)
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
	proxy, err := newTestForgeProxy(upstream, time.Minute, 1024, artifacts, "upstream-cache")
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
	proxy, err := newTestForgeProxy(upstream, time.Minute, 1024, artifacts, "upstream-cache")
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

func TestWriteCachedResponseDropsHopByHopAndConnectionNamedHeaders(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	writeCachedResponse(recorder, CacheEntry{
		StatusCode: http.StatusOK,
		Header: map[string][]string{
			"Content-Type":      {"application/json"},
			"Connection":        {"close, X-Internal-Hop"},
			"Keep-Alive":        {"timeout=5"},
			"Trailer":           {"X-Checksum"},
			"Transfer-Encoding": {"chunked"},
			"X-Internal-Hop":    {"private"},
		},
		Body: []byte(`{"ok":true}`),
	}, "HIT", false)

	response := recorder.Result()
	defer closeResponseBody(response)
	if got := response.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	for _, name := range []string{"Connection", "Keep-Alive", "Trailer", "Transfer-Encoding", "X-Internal-Hop"} {
		if got := response.Header.Get(name); got != "" {
			t.Errorf("%s = %q, want empty", name, got)
		}
	}
}

func TestWriteCachedResponseRefreshesDateAndAge(t *testing.T) {
	t.Parallel()

	storedAt := time.Date(2026, time.August, 9, 10, 0, 0, 0, time.UTC)
	now := storedAt.Add(45 * time.Second)
	recorder := httptest.NewRecorder()
	writeCachedResponseAt(recorder, CacheEntry{
		StatusCode: http.StatusOK,
		Header: map[string][]string{
			"Content-Type": {"application/json"},
			"Date":         {storedAt.Add(-15 * time.Second).Format(http.TimeFormat)},
			"Age":          {"10"},
		},
		StoredAt: storedAt,
		Body:     []byte(`{"ok":true}`),
	}, "STALE", false, now)

	response := recorder.Result()
	defer closeResponseBody(response)
	if got := response.Header.Get("Date"); got != now.Format(http.TimeFormat) {
		t.Fatalf("Date = %q, want %q", got, now.Format(http.TimeFormat))
	}
	if got := response.Header.Get("Age"); got != "60" {
		t.Fatalf("Age = %q, want 60", got)
	}
}
