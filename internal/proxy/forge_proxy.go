package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/httputil"
	"github.com/zxzharmlesszxz/puppet-forge/internal/metrics"
	artifactstorage "github.com/zxzharmlesszxz/puppet-forge/internal/storage"
	"golang.org/x/sync/singleflight"
)

type LeaseStore interface {
	AcquireLease(ctx context.Context, name, holder string, duration time.Duration) (bool, error)
	ReleaseLease(ctx context.Context, name, holder string) error
}

type ForgeProxy struct {
	upstream         *url.URL
	client           *http.Client
	cache            *ResponseCache
	cacheTTL         time.Duration
	maxStaleAge      time.Duration
	maxBodyBytes     int64
	maxArtifactBytes int64
	artifacts        artifactstorage.ArtifactStorage
	artifactPrefix   string
	moduleObserver   func(context.Context, UpstreamModule, bool)
	leaseStore       LeaseStore
	allowPrivateNet  bool
	artifactGroup    singleflight.Group
	artifactLeaseTTL time.Duration
	artifactWorkTTL  time.Duration
}

type UpstreamModule struct {
	Slug           string
	Owner          string
	Name           string
	CurrentRelease UpstreamReleaseRef
	Releases       []UpstreamReleaseRef
}

type UpstreamReleaseRef struct {
	Slug    string `json:"slug"`
	Version string `json:"version"`
}

type UpstreamRelease struct {
	Slug        string `json:"slug"`
	Version     string `json:"version"`
	Readme      string `json:"readme"`
	FileURI     string `json:"file_uri"`
	FileName    string `json:"file_name"`
	FileSize    int64  `json:"file_size"`
	FileMD5     string `json:"file_md5"`
	FileSHA256  string `json:"file_sha256"`
	Description string `json:"description"`
}

const defaultUpstreamArtifactMaxBytes = 128 << 20
const defaultMaxStaleAge = time.Hour
const maxErrorBodyBytes = 64 << 10
const defaultArtifactLeaseDuration = 75 * time.Second
const defaultArtifactWorkTimeout = 5 * time.Minute
const artifactLeasePollInterval = 100 * time.Millisecond
const moduleObserverTimeout = 30 * time.Second

var errBodyTooLarge = errors.New("body exceeds maximum size")
var ErrUpstreamNotFound = errors.New("upstream resource not found")

type Option func(*ForgeProxy)

func WithMaxArtifactBytes(maxBytes int64) Option {
	return func(p *ForgeProxy) {
		if maxBytes > 0 {
			p.maxArtifactBytes = maxBytes
		}
	}
}

func WithMaxStaleAge(maxStaleAge time.Duration) Option {
	return func(p *ForgeProxy) {
		p.maxStaleAge = maxStaleAge
	}
}

func WithLeaseStore(leaseStore LeaseStore) Option {
	return func(p *ForgeProxy) {
		p.leaseStore = leaseStore
	}
}

// WithHTTPClient replaces the hardened default client. It is intended for tests
// and controlled integrations that provide an equivalently restricted client.
func WithHTTPClient(client *http.Client) Option {
	return func(p *ForgeProxy) {
		p.client = client
	}
}

// WithPrivateNetworks permits private and special-purpose upstream addresses.
// Callers remain responsible for constraining any custom HTTP transport.
func WithPrivateNetworks() Option {
	return func(p *ForgeProxy) {
		p.allowPrivateNet = true
	}
}

func NewForgeProxy(upstreamURL string, cacheTTL time.Duration, maxBodyBytes int64, artifacts artifactstorage.ArtifactStorage, artifactPrefix string, opts ...Option) (*ForgeProxy, error) {
	upstream, err := url.Parse(upstreamURL)
	if err != nil {
		return nil, fmt.Errorf("parse forge upstream url: %w", err)
	}
	if err := validateUpstreamURL(upstream); err != nil {
		return nil, fmt.Errorf("validate forge upstream url: %w", err)
	}

	p := &ForgeProxy{
		upstream:         upstream,
		cache:            NewResponseCache(),
		cacheTTL:         cacheTTL,
		maxStaleAge:      defaultMaxStaleAge,
		maxBodyBytes:     maxBodyBytes,
		maxArtifactBytes: defaultUpstreamArtifactMaxBytes,
		artifacts:        artifacts,
		artifactPrefix:   artifactPrefix,
		artifactLeaseTTL: defaultArtifactLeaseDuration,
		artifactWorkTTL:  defaultArtifactWorkTimeout,
	}
	for _, opt := range opts {
		opt(p)
	}
	if err := validateLiteralUpstreamHost(upstream, p.allowPrivateNet); err != nil {
		return nil, fmt.Errorf("validate forge upstream url: %w", err)
	}
	if p.client == nil {
		p.client = newUpstreamHTTPClient(p.allowPrivateNet)
	}

	return p, nil
}

func (p *ForgeProxy) Handler() http.Handler {
	return http.HandlerFunc(p.handle)
}

func (p *ForgeProxy) SetModuleObserver(fn func(context.Context, UpstreamModule, bool)) {
	p.moduleObserver = fn
}

func (p *ForgeProxy) FetchModule(ctx context.Context, slug string) (UpstreamModule, error) {
	body, err := p.fetchBody(ctx, "/v3/modules/"+slug)
	if err != nil {
		return UpstreamModule{}, err
	}
	return parseUpstreamModule(body)
}

func (p *ForgeProxy) FetchRelease(ctx context.Context, slug string) (UpstreamRelease, error) {
	var release UpstreamRelease
	if err := p.fetchJSON(ctx, "/v3/releases/"+slug, &release); err != nil {
		return UpstreamRelease{}, err
	}
	return release, nil
}

func (p *ForgeProxy) EnsureArtifact(ctx context.Context, fileURI string) error {
	req, objectPath, err := p.artifactRequest(fileURI)
	if err != nil {
		return err
	}
	loaded, err := p.loadArtifactShared(ctx, req, objectPath)
	if err != nil {
		return err
	}
	if loaded.statusCode < 200 || loaded.statusCode >= 300 {
		return fmt.Errorf("upstream artifact status %d", loaded.statusCode)
	}
	return nil
}

func (p *ForgeProxy) WithCachedArtifactLease(ctx context.Context, objectPath string, fn func(context.Context) error) error {
	if p == nil || fn == nil {
		return errors.New("cached artifact lease callback is required")
	}
	waitCtx, cancelWait := context.WithTimeout(ctx, p.artifactLeaseTTL+10*time.Second)
	leaseCtx, releaseLease, err := p.acquireArtifactLeaseExclusive(waitCtx, objectPath)
	cancelWait()
	if err != nil {
		return err
	}
	if releaseLease != nil {
		defer releaseLease()
	}
	workCtx, cancelWork := context.WithTimeout(leaseCtx, p.artifactWorkTTL)
	defer cancelWork()
	return fn(workCtx)
}

func (p *ForgeProxy) EnsureArtifactIntegrity(ctx context.Context, fileURI, expectedSHA256 string, expectedSize int64) error {
	req, objectPath, err := p.artifactRequest(fileURI)
	if err != nil {
		metrics.ObserveUpstreamArtifactIntegrity("error")
		return err
	}
	result := p.artifactGroup.DoChan(objectPath, func() (any, error) {
		result := "error"
		defer func() { metrics.ObserveUpstreamArtifactIntegrity(result) }()
		waitCtx, cancelWait := context.WithTimeout(context.WithoutCancel(ctx), p.artifactLeaseTTL+10*time.Second)
		leaseCtx, releaseLease, err := p.acquireArtifactLeaseExclusive(waitCtx, objectPath)
		cancelWait()
		if err != nil {
			return artifactLoadResult{}, err
		}
		if releaseLease != nil {
			defer releaseLease()
		}
		workCtx, cancelWork := context.WithTimeout(leaseCtx, p.artifactWorkTTL)
		defer cancelWork()
		exists, valid, err := p.verifyCachedArtifact(workCtx, objectPath, expectedSHA256, expectedSize)
		if err != nil {
			return artifactLoadResult{}, err
		}
		if exists && valid {
			result = "valid"
			return artifactLoadResult{statusCode: http.StatusOK, cacheHit: true}, nil
		}
		if exists {
			slog.Default().Warn("removing corrupt upstream artifact cache", "path", objectPath)
			if err := p.artifacts.Delete(workCtx, objectPath); err != nil {
				return artifactLoadResult{}, fmt.Errorf("delete corrupt upstream artifact cache: %w", err)
			}
		}
		loaded, err := p.fetchAndCacheArtifact(workCtx, req, objectPath)
		if err != nil {
			return artifactLoadResult{}, err
		}
		if loaded.statusCode < 200 || loaded.statusCode >= 300 {
			return loaded, fmt.Errorf("upstream artifact status %d", loaded.statusCode)
		}
		_, valid, err = p.verifyCachedArtifact(workCtx, objectPath, expectedSHA256, expectedSize)
		if err != nil {
			return artifactLoadResult{}, err
		}
		if !valid {
			_ = p.artifacts.Delete(workCtx, objectPath)
			return artifactLoadResult{}, errors.New("refetched upstream artifact does not match release checksum or size")
		}
		result = "repaired"
		return loaded, nil
	})
	_, err = waitForSharedArtifact(ctx, result)
	return err
}

func (p *ForgeProxy) artifactRequest(fileURI string) (*http.Request, string, error) {
	if p.artifacts == nil {
		return nil, "", errors.New("upstream artifact cache is not configured")
	}
	parsed, err := url.Parse(fileURI)
	if err != nil {
		return nil, "", fmt.Errorf("parse upstream artifact uri: %w", err)
	}
	if !strings.HasPrefix(parsed.Path, "/v3/files/") {
		return nil, "", errors.New("upstream artifact uri is not a Forge file path")
	}
	req := &http.Request{
		Method: http.MethodGet,
		URL: &url.URL{
			Path:     parsed.Path,
			RawQuery: parsed.RawQuery,
		},
		Header: make(http.Header),
	}
	return req, p.cachedArtifactPath(parsed.Path), nil
}

func (p *ForgeProxy) handle(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		writeProxyError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	if strings.HasPrefix(req.URL.Path, "/v3/files/") {
		p.handleArtifactRequest(w, req)
		return
	}

	cacheKey := req.Method + " " + req.URL.RequestURI()
	isHead := req.Method == http.MethodHead

	if entry, ok := p.cache.Get(cacheKey, time.Now()); ok {
		metrics.ObserveUpstreamCache("json", "hit")
		slog.Debug("upstream json cache hit", "request_id", req.Header.Get("X-Request-ID"), "method", req.Method, "path", req.URL.Path)
		p.observeModuleResponse(req.Context(), req.URL.Path, entry.StatusCode, entry.Header, entry.Body, false)
		writeCachedResponse(w, entry, "HIT", isHead)
		return
	}
	metrics.ObserveUpstreamCache("json", "miss")
	slog.Debug("upstream json cache miss", "request_id", req.Header.Get("X-Request-ID"), "method", req.Method, "path", req.URL.Path)
	now := time.Now()
	staleEntry, hasStaleEntry := p.cache.GetStale(cacheKey, now, p.maxStaleAge)

	resp, err := p.upstreamRequest(req, req.Method)
	if err != nil {
		if p.serveStaleJSONFallback(w, req, staleEntry, hasStaleEntry, isHead) {
			return
		}
		writeProxyError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer closeResponseBody(resp)

	body, err := readBoundedBody(resp.Body, p.maxBodyBytes, "response body exceeds maximum size")
	if err != nil {
		if p.serveStaleJSONFallback(w, req, staleEntry, hasStaleEntry, isHead) {
			return
		}
		message := err.Error()
		if errors.Is(err, errBodyTooLarge) {
			message = "response body exceeds maximum size"
		}
		writeProxyError(w, http.StatusBadGateway, message)
		return
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if p.serveStaleJSONFallback(w, req, staleEntry, hasStaleEntry, isHead) {
			return
		}
		writeUpstreamStatusError(w, resp.StatusCode)
		return
	}

	p.observeModuleResponse(req.Context(), req.URL.Path, resp.StatusCode, resp.Header, body, true)
	writeUpstreamResponse(w, resp, body, isHead)

	if p.shouldCache(resp, body) {
		storedAt := time.Now()
		p.cache.Set(cacheKey, CacheEntry{
			StatusCode: resp.StatusCode,
			Header:     cloneHeader(resp.Header),
			Body:       append([]byte(nil), body...),
			StoredAt:   storedAt,
			ExpiresAt:  storedAt.Add(p.cacheTTL),
			StaleUntil: storedAt.Add(p.cacheTTL + p.maxStaleAge),
		})
		slog.Debug("upstream json response cached",
			"request_id", req.Header.Get("X-Request-ID"),
			"method", req.Method,
			"path", req.URL.Path,
			"status", resp.StatusCode,
			"bytes", len(body),
			"ttl", p.cacheTTL,
		)
	} else {
		slog.Debug("upstream json response not cached",
			"request_id", req.Header.Get("X-Request-ID"),
			"method", req.Method,
			"path", req.URL.Path,
			"status", resp.StatusCode,
			"bytes", len(body),
		)
	}
}

func (p *ForgeProxy) serveStaleJSONFallback(w http.ResponseWriter, req *http.Request, entry CacheEntry, ok bool, isHead bool) bool {
	if !ok {
		return false
	}
	metrics.ObserveUpstreamCache("json", "stale")
	slog.Debug("serving stale upstream json",
		"request_id", req.Header.Get("X-Request-ID"),
		"method", req.Method,
		"path", req.URL.Path,
	)
	p.observeModuleResponse(req.Context(), req.URL.Path, entry.StatusCode, entry.Header, entry.Body, false)
	writeCachedResponse(w, entry, "STALE", isHead)
	return true
}

func (p *ForgeProxy) observeModuleResponse(ctx context.Context, requestPath string, statusCode int, header http.Header, body []byte, fresh bool) {
	if p.moduleObserver == nil || statusCode < 200 || statusCode >= 300 {
		return
	}
	if !strings.HasPrefix(requestPath, "/v3/modules/") {
		return
	}
	if strings.Count(strings.Trim(requestPath, "/"), "/") != 2 {
		return
	}

	moduleBody, err := observerJSONBody(header, body, p.maxBodyBytes)
	if err != nil {
		return
	}
	module, err := parseUpstreamModule(moduleBody)
	if err != nil {
		return
	}
	observerCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), moduleObserverTimeout)
	defer cancel()
	p.moduleObserver(observerCtx, module, fresh)
}

func observerJSONBody(header http.Header, body []byte, maxBytes int64) ([]byte, error) {
	if !isGzipEncoded(header, body) {
		return body, nil
	}
	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = reader.Close()
	}()
	overflowMessage := fmt.Sprintf("upstream observer json exceeds maximum size of %d bytes", maxBytes)
	return readBoundedBody(reader, maxBytes, overflowMessage)
}

func isGzipEncoded(header http.Header, body []byte) bool {
	if strings.EqualFold(header.Get("Content-Encoding"), "gzip") {
		return true
	}
	return len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b
}

func (p *ForgeProxy) fetchJSON(ctx context.Context, requestPath string, target any) error {
	body, err := p.fetchBody(ctx, requestPath)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("decode upstream json: %w", err)
	}
	return nil
}

func (p *ForgeProxy) fetchBody(ctx context.Context, requestPath string) ([]byte, error) {
	upstreamURL := p.upstreamURL(requestPath, "")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call upstream: %w", err)
	}
	defer closeResponseBody(resp)

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrUpstreamNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := readBoundedBody(resp.Body, maxErrorBodyBytes, "")
		return nil, fmt.Errorf("upstream status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	body, err := readBoundedBody(resp.Body, p.maxBodyBytes, "upstream response body exceeds maximum size")
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			return nil, errors.New("upstream response body exceeds maximum size")
		}
		return nil, fmt.Errorf("read upstream body: %w", err)
	}

	return body, nil
}

func parseUpstreamModule(body []byte) (UpstreamModule, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return UpstreamModule{}, fmt.Errorf("decode upstream module: %w", err)
	}

	module := UpstreamModule{
		Slug:  stringValue(raw["slug"]),
		Owner: upstreamOwner(raw["owner"]),
		Name:  stringValue(raw["name"]),
	}

	switch current := raw["current_release"].(type) {
	case map[string]any:
		module.CurrentRelease = UpstreamReleaseRef{
			Slug:    stringValue(current["slug"]),
			Version: stringValue(current["version"]),
		}
		module.CurrentRelease = normalizeReleaseRef(module.Slug, module.CurrentRelease)
	case string:
		module.CurrentRelease = normalizeReleaseRef(module.Slug, UpstreamReleaseRef{Slug: current})
	}

	switch releases := raw["releases"].(type) {
	case []any:
		for _, item := range releases {
			switch typed := item.(type) {
			case map[string]any:
				module.Releases = append(module.Releases, UpstreamReleaseRef{
					Slug:    stringValue(typed["slug"]),
					Version: stringValue(typed["version"]),
				})
				module.Releases[len(module.Releases)-1] = normalizeReleaseRef(module.Slug, module.Releases[len(module.Releases)-1])
			case string:
				module.Releases = append(module.Releases, normalizeReleaseRef(module.Slug, UpstreamReleaseRef{Slug: typed}))
			}
		}
	case map[string]any:
		if results, ok := releases["results"].([]any); ok {
			for _, item := range results {
				if typed, ok := item.(map[string]any); ok {
					module.Releases = append(module.Releases, UpstreamReleaseRef{
						Slug:    stringValue(typed["slug"]),
						Version: stringValue(typed["version"]),
					})
					module.Releases[len(module.Releases)-1] = normalizeReleaseRef(module.Slug, module.Releases[len(module.Releases)-1])
				}
			}
		}
	}

	return module, nil
}

func upstreamOwner(value any) string {
	if owner := stringValue(value); owner != "" {
		return owner
	}
	owner, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	if slug := stringValue(owner["slug"]); slug != "" {
		return slug
	}
	return stringValue(owner["username"])
}

func normalizeReleaseRef(moduleSlug string, ref UpstreamReleaseRef) UpstreamReleaseRef {
	if ref.Version != "" {
		return ref
	}
	ref.Version = httputil.ReleaseVersionFromSlug(moduleSlug, ref.Slug)
	return ref
}

func stringValue(value any) string {
	if typed, ok := value.(string); ok {
		return typed
	}
	return ""
}

func (p *ForgeProxy) upstreamRequest(req *http.Request, method string) (*http.Response, error) {
	upstreamURL := p.upstreamURL(req.URL.Path, req.URL.RawQuery)
	// #nosec G704 -- the configured base URL is validated at construction and the transport revalidates every resolved IP and redirect.
	upstreamReq, err := http.NewRequestWithContext(req.Context(), method, upstreamURL.String(), nil)
	if err != nil {
		return nil, err
	}
	copyRequestHeaders(upstreamReq.Header, req.Header)
	if !strings.HasPrefix(req.URL.Path, "/v3/files/") {
		upstreamReq.Header.Set("Accept-Encoding", "identity")
	}
	upstreamReq.Header.Set("X-Forwarded-Host", req.Host)
	upstreamReq.Header.Set("X-Forwarded-Proto", httputil.ForwardedScheme(req))
	// #nosec G704 -- the hardened client blocks private/link-local/metadata targets at dial time.
	started := time.Now()
	slog.Debug("upstream request started",
		"request_id", req.Header.Get("X-Request-ID"),
		"method", method,
		"path", req.URL.Path,
		"upstream_host", upstreamURL.Host,
	)
	resp, err := p.client.Do(upstreamReq) // #nosec G704 -- the hardened client revalidates resolved IPs and redirects.
	if err != nil {
		slog.Debug("upstream request failed",
			"request_id", req.Header.Get("X-Request-ID"),
			"method", method,
			"path", req.URL.Path,
			"duration", time.Since(started).Round(time.Millisecond),
			"err", err,
		)
		return nil, err
	}
	slog.Debug("upstream request finished",
		"request_id", req.Header.Get("X-Request-ID"),
		"method", method,
		"path", req.URL.Path,
		"status", resp.StatusCode,
		"content_length", resp.ContentLength,
		"duration", time.Since(started).Round(time.Millisecond),
	)
	return resp, nil
}

func (p *ForgeProxy) upstreamURL(requestPath, rawQuery string) url.URL {
	upstreamURL := *p.upstream
	upstreamURL.Path = httputil.SingleJoiningSlash(p.upstream.Path, requestPath)
	upstreamURL.RawQuery = rawQuery
	return upstreamURL
}

func (p *ForgeProxy) handleArtifactRequest(w http.ResponseWriter, req *http.Request) {
	if p.artifacts == nil {
		metrics.ObserveUpstreamCache("artifact", "bypass")
		slog.Debug("upstream artifact cache bypass", "request_id", req.Header.Get("X-Request-ID"), "method", req.Method, "path", req.URL.Path)
		resp, err := p.upstreamRequest(req, req.Method)
		if err != nil {
			writeProxyError(w, http.StatusBadGateway, err.Error())
			return
		}
		defer closeResponseBody(resp)
		isHead := req.Method == http.MethodHead
		body, relayed, err := p.readOrRelayArtifactUpstream(w, resp, "BYPASS", isHead)
		if err != nil {
			writeProxyError(w, http.StatusBadGateway, err.Error())
			return
		}
		if relayed {
			return
		}
		relayUpstreamResponse(w, resp, body, "X-Forge-Artifact-Cache", "BYPASS", isHead)
		return
	}

	objectPath := p.cachedArtifactPath(req.URL.Path)
	attrs, err := p.artifacts.Stat(req.Context(), objectPath)
	if err == nil {
		metrics.ObserveUpstreamCache("artifact", "hit")
		slog.Debug("upstream artifact cache hit", "request_id", req.Header.Get("X-Request-ID"), "method", req.Method, "path", req.URL.Path, "bytes", attrs.Size)
		p.serveStoredArtifact(w, req, objectPath, attrs, "HIT")
		return
	}
	if !errors.Is(err, artifactstorage.ErrObjectNotFound) {
		writeProxyError(w, http.StatusBadGateway, err.Error())
		return
	}
	metrics.ObserveUpstreamCache("artifact", "miss")
	slog.Debug("upstream artifact cache miss", "request_id", req.Header.Get("X-Request-ID"), "method", req.Method, "path", req.URL.Path)
	if req.Method == http.MethodHead {
		resp, err := p.upstreamRequest(req, http.MethodHead)
		if err != nil {
			writeProxyError(w, http.StatusBadGateway, err.Error())
			return
		}
		defer closeResponseBody(resp)
		relayUpstreamResponse(w, resp, nil, "X-Forge-Artifact-Cache", "MISS", true)
		return
	}

	loaded, err := p.loadArtifactShared(req.Context(), req, objectPath)
	if err != nil {
		writeProxyError(w, http.StatusBadGateway, err.Error())
		return
	}
	if loaded.statusCode < 200 || loaded.statusCode >= 300 {
		w.Header().Set("X-Forge-Artifact-Cache", "MISS")
		writeUpstreamStatusError(w, loaded.statusCode)
		return
	}
	attrs, err = p.artifacts.Stat(req.Context(), objectPath)
	if err != nil {
		writeProxyError(w, http.StatusBadGateway, err.Error())
		return
	}
	cacheStatus := "MISS"
	if loaded.cacheHit {
		cacheStatus = "HIT"
	}
	p.serveStoredArtifact(w, req, objectPath, attrs, cacheStatus)
}

type artifactLoadResult struct {
	statusCode int
	cacheHit   bool
}

func (p *ForgeProxy) loadArtifactShared(ctx context.Context, req *http.Request, objectPath string) (artifactLoadResult, error) {
	result := p.artifactGroup.DoChan(objectPath, func() (any, error) {
		return p.loadArtifact(context.WithoutCancel(ctx), req, objectPath)
	})
	loadedValue, err := waitForSharedArtifact(ctx, result)
	if err != nil {
		return artifactLoadResult{}, err
	}
	loadedResult, ok := loadedValue.(artifactLoadResult)
	if !ok {
		return artifactLoadResult{}, errors.New("unexpected shared artifact result")
	}
	return loadedResult, nil
}

func waitForSharedArtifact(ctx context.Context, result <-chan singleflight.Result) (any, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case loaded := <-result:
		if loaded.Err != nil {
			return nil, loaded.Err
		}
		return loaded.Val, nil
	}
}

func (p *ForgeProxy) loadArtifact(ctx context.Context, req *http.Request, objectPath string) (artifactLoadResult, error) {
	waitCtx, cancelWait := context.WithTimeout(ctx, p.artifactLeaseTTL+10*time.Second)
	if cached, ok, err := p.cachedArtifactResult(waitCtx, objectPath); err != nil {
		cancelWait()
		return artifactLoadResult{}, err
	} else if ok {
		cancelWait()
		return cached, nil
	}

	leaseCtx, releaseLease, err := p.acquireArtifactLease(waitCtx, objectPath)
	cancelWait()
	if err != nil {
		return artifactLoadResult{}, err
	}
	if releaseLease != nil {
		defer releaseLease()
	}
	workCtx, cancelWork := context.WithTimeout(leaseCtx, p.artifactWorkTTL)
	defer cancelWork()
	if cached, ok, err := p.cachedArtifactResult(workCtx, objectPath); err != nil {
		return artifactLoadResult{}, err
	} else if ok {
		return cached, nil
	}
	return p.fetchAndCacheArtifact(workCtx, req, objectPath)
}

func (p *ForgeProxy) fetchAndCacheArtifact(ctx context.Context, req *http.Request, objectPath string) (artifactLoadResult, error) {
	upstreamReq := req.Clone(ctx)
	upstreamReq.Header.Del("Range")
	upstreamReq.Header.Del("If-Range")
	resp, err := p.upstreamRequest(upstreamReq, http.MethodGet)
	if err != nil {
		return artifactLoadResult{}, err
	}
	defer closeResponseBody(resp)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBodyBytes+1))
		return artifactLoadResult{
			statusCode: resp.StatusCode,
		}, nil
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBodyBytes+1))
		return artifactLoadResult{}, fmt.Errorf("upstream artifact returned unexpected success status %d", resp.StatusCode)
	}
	body, err := newArtifactUploadReader(resp.Body, resp.ContentLength, p.maxArtifactBytes)
	if err != nil {
		return artifactLoadResult{}, err
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	created, err := p.artifacts.UploadReaderIfAbsent(ctx, objectPath, contentType, body)
	if err != nil {
		return artifactLoadResult{}, fmt.Errorf("cache upstream artifact: %w", err)
	}
	if !created {
		if cached, ok, err := p.cachedArtifactResult(ctx, objectPath); err != nil {
			return artifactLoadResult{}, err
		} else if ok {
			return cached, nil
		}
		return artifactLoadResult{}, errors.New("cached upstream artifact disappeared after concurrent upload")
	}
	return artifactLoadResult{
		statusCode: http.StatusOK,
	}, nil
}

func (p *ForgeProxy) verifyCachedArtifact(ctx context.Context, objectPath, expectedSHA256 string, expectedSize int64) (bool, bool, error) {
	object, err := p.artifacts.Open(ctx, objectPath)
	if errors.Is(err, artifactstorage.ErrObjectNotFound) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("open cached upstream artifact: %w", err)
	}
	hash := sha256.New()
	size, copyErr := io.Copy(hash, object.Body)
	closeErr := object.Body.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return true, false, fmt.Errorf("verify cached upstream artifact: %w", err)
	}
	actualSHA256 := fmt.Sprintf("%x", hash.Sum(nil))
	validSize := expectedSize <= 0 || size == expectedSize
	validSHA256 := expectedSHA256 == "" || strings.EqualFold(actualSHA256, expectedSHA256)
	return true, validSize && validSHA256, nil
}

type artifactUploadReader struct {
	source        io.Reader
	contentLength int64
	maxBytes      int64
	readBytes     int64
}

func newArtifactUploadReader(source io.Reader, contentLength, maxBytes int64) (io.Reader, error) {
	if maxBytes <= 0 {
		return nil, errors.New("upstream artifact maximum size must be greater than 0")
	}
	if contentLength > maxBytes {
		return nil, fmt.Errorf("upstream artifact exceeds maximum size of %d bytes: %w", maxBytes, errBodyTooLarge)
	}
	return &artifactUploadReader{source: source, contentLength: contentLength, maxBytes: maxBytes}, nil
}

func (r *artifactUploadReader) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	limit := r.maxBytes
	if r.contentLength >= 0 {
		limit = r.contentLength
	}
	if r.readBytes == limit {
		var extra [1]byte
		n, err := io.ReadFull(r.source, extra[:])
		if n > 0 {
			if r.contentLength >= 0 {
				return 0, fmt.Errorf("upstream artifact body exceeds Content-Length %d: %w", r.contentLength, io.ErrUnexpectedEOF)
			}
			return 0, fmt.Errorf("upstream artifact exceeds maximum size of %d bytes: %w", r.maxBytes, errBodyTooLarge)
		}
		return 0, err
	}

	remaining := limit - r.readBytes
	if int64(len(buffer)) > remaining {
		buffer = buffer[:remaining]
	}
	n, err := r.source.Read(buffer)
	r.readBytes += int64(n)
	if errors.Is(err, io.EOF) && r.contentLength >= 0 && r.readBytes != r.contentLength {
		return n, fmt.Errorf(
			"upstream artifact body length %d does not match Content-Length %d: %w",
			r.readBytes,
			r.contentLength,
			io.ErrUnexpectedEOF,
		)
	}
	return n, err
}

func (p *ForgeProxy) cachedArtifactResult(ctx context.Context, objectPath string) (artifactLoadResult, bool, error) {
	exists, err := p.artifacts.Exists(ctx, objectPath)
	if err != nil {
		return artifactLoadResult{}, false, fmt.Errorf("check cached upstream artifact: %w", err)
	}
	if !exists {
		return artifactLoadResult{}, false, nil
	}
	return artifactLoadResult{
		statusCode: http.StatusOK,
		cacheHit:   true,
	}, true, nil
}

func (p *ForgeProxy) acquireArtifactLease(ctx context.Context, objectPath string) (context.Context, func(), error) {
	return p.acquireArtifactLeaseWithCacheShortcut(ctx, objectPath, true)
}

func (p *ForgeProxy) acquireArtifactLeaseExclusive(ctx context.Context, objectPath string) (context.Context, func(), error) {
	return p.acquireArtifactLeaseWithCacheShortcut(ctx, objectPath, false)
}

func (p *ForgeProxy) acquireArtifactLeaseWithCacheShortcut(ctx context.Context, objectPath string, cacheShortcut bool) (context.Context, func(), error) {
	if p.leaseStore == nil {
		return context.WithoutCancel(ctx), nil, nil
	}
	holderBytes := make([]byte, 16)
	if _, err := rand.Read(holderBytes); err != nil {
		return nil, nil, fmt.Errorf("generate artifact lease holder: %w", err)
	}
	leaseDigest := sha256.Sum256([]byte(objectPath))
	leaseName := fmt.Sprintf("upstream-artifact-%x", leaseDigest[:])
	holder := fmt.Sprintf("%x", holderBytes)
	ticker := time.NewTicker(artifactLeasePollInterval)
	defer ticker.Stop()

	for {
		acquired, err := p.leaseStore.AcquireLease(ctx, leaseName, holder, p.artifactLeaseTTL)
		if err != nil {
			return nil, nil, fmt.Errorf("acquire upstream artifact lease: %w", err)
		}
		if acquired {
			leaseCtx, cancelLease := context.WithCancel(context.WithoutCancel(ctx))
			stopRenewal := make(chan struct{})
			renewalDone := make(chan struct{})
			go p.renewArtifactLease(leaseCtx, cancelLease, leaseName, holder, stopRenewal, renewalDone)
			return leaseCtx, func() {
				close(stopRenewal)
				<-renewalDone
				cancelLease()
				releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				_ = p.leaseStore.ReleaseLease(releaseCtx, leaseName, holder)
			}, nil
		}
		if cacheShortcut {
			if exists, err := p.artifacts.Exists(ctx, objectPath); err != nil {
				return nil, nil, fmt.Errorf("check artifact while waiting for lease: %w", err)
			} else if exists {
				return context.WithoutCancel(ctx), nil, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (p *ForgeProxy) renewArtifactLease(ctx context.Context, cancel context.CancelFunc, leaseName, holder string, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	interval := p.artifactLeaseTTL / 3
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			renewCtx, cancelRenewal := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			acquired, err := p.leaseStore.AcquireLease(renewCtx, leaseName, holder, p.artifactLeaseTTL)
			cancelRenewal()
			if err != nil || !acquired {
				slog.Error("renew upstream artifact lease failed", "lease", leaseName, "err", err)
				cancel()
				return
			}
		}
	}
}

func (p *ForgeProxy) readOrRelayArtifactUpstream(w http.ResponseWriter, resp *http.Response, cacheStatus string, isHead bool) ([]byte, bool, error) {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := readBoundedBody(resp.Body, maxErrorBodyBytes, "")
		relayUpstreamResponse(w, resp, body, "X-Forge-Artifact-Cache", cacheStatus, isHead)
		return nil, true, nil
	}
	if isHead {
		relayUpstreamResponse(w, resp, nil, "X-Forge-Artifact-Cache", cacheStatus, true)
		return nil, true, nil
	}
	body, err := readLimitedBody(resp.Body, resp.ContentLength, p.maxArtifactBytes)
	return body, false, err
}

func readLimitedBody(reader io.Reader, contentLength int64, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, errors.New("upstream artifact maximum size must be greater than 0")
	}
	if contentLength > maxBytes {
		return nil, fmt.Errorf("upstream artifact exceeds maximum size of %d bytes: %w", maxBytes, errBodyTooLarge)
	}

	overflowMessage := fmt.Sprintf("upstream artifact exceeds maximum size of %d bytes", maxBytes)
	body, err := readBoundedBody(reader, maxBytes, overflowMessage)
	if err == nil && contentLength >= 0 && int64(len(body)) != contentLength {
		return nil, fmt.Errorf("upstream artifact body length %d does not match Content-Length %d: %w", len(body), contentLength, io.ErrUnexpectedEOF)
	}
	if errors.Is(err, errBodyTooLarge) {
		return nil, fmt.Errorf("upstream artifact exceeds maximum size of %d bytes: %w", maxBytes, errBodyTooLarge)
	}
	return body, err
}

func readBoundedBody(reader io.Reader, maxBytes int64, overflowMessage string) ([]byte, error) {
	if maxBytes <= 0 {
		return io.ReadAll(reader)
	}

	body, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) <= maxBytes {
		return body, nil
	}
	if overflowMessage == "" {
		return body[:maxBytes], errBodyTooLarge
	}
	return nil, fmt.Errorf("%s: %w", overflowMessage, errBodyTooLarge)
}

func (p *ForgeProxy) shouldCache(resp *http.Response, body []byte) bool {
	if p.cacheTTL <= 0 {
		return false
	}
	if resp.Request != nil && strings.HasPrefix(resp.Request.URL.Path, "/v3/files/") {
		return false
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}
	if int64(len(body)) > p.maxBodyBytes {
		return false
	}

	contentType := resp.Header.Get("Content-Type")
	return strings.Contains(contentType, "application/json") || strings.Contains(contentType, "text/json")
}

func writeCachedResponse(w http.ResponseWriter, entry CacheEntry, cacheHeader string, isHead bool) {
	writeCachedResponseAt(w, entry, cacheHeader, isHead, time.Now())
}

func writeCachedResponseAt(w http.ResponseWriter, entry CacheEntry, cacheHeader string, isHead bool, now time.Time) {
	header := w.Header()
	copyEndToEndHeaders(header, entry.Header)
	header.Set("Date", now.UTC().Format(http.TimeFormat))
	header.Set("Age", strconv.FormatInt(cachedResponseAge(entry, now), 10))
	header.Set("X-Forge-Cache", cacheHeader)
	w.WriteHeader(entry.StatusCode)
	if !isHead {
		_, _ = io.Copy(w, bytes.NewReader(entry.Body))
	}
}

func cachedResponseAge(entry CacheEntry, now time.Time) int64 {
	storedAt := entry.StoredAt
	if storedAt.IsZero() || storedAt.After(now) {
		storedAt = now
	}
	var age int64
	if value := entry.Header.Get("Age"); value != "" {
		if parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && parsed > age {
			age = parsed
		}
	}
	if value := entry.Header.Get("Date"); value != "" {
		if date, err := http.ParseTime(value); err == nil {
			apparentAge := int64(max(0, storedAt.Sub(date)).Seconds())
			age = max(age, apparentAge)
		}
	}
	return age + int64(max(0, now.Sub(storedAt)).Seconds())
}

func writeUpstreamResponse(w http.ResponseWriter, resp *http.Response, body []byte, isHead bool) {
	relayUpstreamResponse(w, resp, body, "X-Forge-Cache", "MISS", isHead)
}

func relayUpstreamResponse(w http.ResponseWriter, resp *http.Response, body []byte, cacheHeader, cacheStatus string, isHead bool) {
	header := w.Header()
	copyEndToEndHeaders(header, resp.Header)
	header.Set(cacheHeader, cacheStatus)
	w.WriteHeader(resp.StatusCode)
	if !isHead {
		_, _ = io.Copy(w, bytes.NewReader(body))
	}
}

func writeProxyError(w http.ResponseWriter, status int, message string) {
	if status >= http.StatusInternalServerError {
		slog.Error("upstream proxy request failed", "status", status, "err", message)
		message = "upstream service is unavailable"
	}
	http.Error(w, message, status)
}

func writeUpstreamStatusError(w http.ResponseWriter, status int) {
	message := "upstream request failed"
	if status == http.StatusNotFound {
		message = "upstream resource not found"
	}
	writeProxyError(w, status, message)
}

func closeResponseBody(resp *http.Response) {
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
}

func copyRequestHeaders(dst, src http.Header) {
	for key, values := range src {
		if !shouldForwardUpstreamHeader(key) {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func shouldForwardUpstreamHeader(key string) bool {
	switch strings.ToLower(key) {
	case "accept", "accept-encoding", "accept-language", "cache-control", "if-modified-since", "if-none-match", "range", "user-agent":
		return true
	default:
		return false
	}
}

func cloneHeader(header http.Header) http.Header {
	cloned := make(http.Header, len(header))
	connectionHeaders := connectionHeaderNames(header)
	for key, values := range header {
		if isHopByHopHeader(key) || connectionHeaders[http.CanonicalHeaderKey(key)] {
			continue
		}
		cloned[key] = append([]string(nil), values...)
	}
	return cloned
}

func copyEndToEndHeaders(dst, src http.Header) {
	connectionHeaders := connectionHeaderNames(src)
	for key, values := range src {
		if isHopByHopHeader(key) || connectionHeaders[http.CanonicalHeaderKey(key)] {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func connectionHeaderNames(header http.Header) map[string]bool {
	names := make(map[string]bool)
	for _, value := range header.Values("Connection") {
		for name := range strings.SplitSeq(value, ",") {
			if canonical := http.CanonicalHeaderKey(strings.TrimSpace(name)); canonical != "" {
				names[canonical] = true
			}
		}
	}
	return names
}

func isHopByHopHeader(key string) bool {
	switch strings.ToLower(key) {
	case "connection", "proxy-connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "trailers", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func (p *ForgeProxy) cachedArtifactPath(requestPath string) string {
	trimmed := strings.TrimPrefix(requestPath, "/")
	return path.Join(p.artifactPrefix, trimmed)
}

func (p *ForgeProxy) serveStoredArtifact(w http.ResponseWriter, req *http.Request, objectPath string, attrs artifactstorage.ObjectAttrs, cacheStatus string) {
	etag := normalizeETag(attrs.ETag)
	response, ok := prepareArtifactResponse(w, req, attrs.Size, etag, attrs.ContentType)
	if !ok {
		return
	}
	w.Header().Set("X-Forge-Artifact-Cache", cacheStatus)
	if req.Method == http.MethodHead {
		w.WriteHeader(response.Status)
		return
	}

	var (
		object artifactstorage.ObjectReader
		err    error
	)
	if response.Partial {
		rangeStorage, ok := p.artifacts.(artifactstorage.RangeArtifactStorage)
		if !ok {
			httputil.ClearByteRangeResponseHeaders(w.Header())
			writeProxyError(w, http.StatusBadGateway, "artifact storage does not support range reads")
			return
		}
		object, err = rangeStorage.OpenRange(req.Context(), objectPath, response.Range.Start, response.Range.Length)
	} else {
		object, err = p.artifacts.Open(req.Context(), objectPath)
	}
	if err != nil {
		httputil.ClearByteRangeResponseHeaders(w.Header())
		writeProxyError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer func() { _ = object.Body.Close() }()
	w.WriteHeader(response.Status)
	written, err := io.Copy(w, object.Body)
	result := "success"
	if err != nil {
		result = "error"
		if req.Context().Err() != nil {
			result = "client_cancel"
		}
		slog.Warn("stream cached upstream artifact", "err", err, "object_path", objectPath)
	}
	metrics.ObserveArtifactStream("upstream_cache", result, written)
}

func prepareArtifactResponse(w http.ResponseWriter, req *http.Request, size int64, etag, contentType string) (httputil.ByteRangeResponse, bool) {
	response, err := httputil.PrepareByteRangeResponse(w.Header(), req.Header, size, etag, contentType)
	if err != nil {
		writeProxyError(w, http.StatusRequestedRangeNotSatisfiable, err.Error())
		return httputil.ByteRangeResponse{}, false
	}
	if response.NotModified {
		w.WriteHeader(response.Status)
		return httputil.ByteRangeResponse{}, false
	}
	return response, true
}

func normalizeETag(etag string) string {
	etag = strings.TrimSpace(etag)
	if etag == "" || strings.HasPrefix(etag, `"`) || strings.HasPrefix(etag, `W/"`) {
		return etag
	}
	return `"` + etag + `"`
}
