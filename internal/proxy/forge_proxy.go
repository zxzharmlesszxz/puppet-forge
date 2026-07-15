package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"puppet-forge/internal/httputil"
	"puppet-forge/internal/metrics"
	artifactstorage "puppet-forge/internal/storage"
)

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
	moduleObserver   func(context.Context, UpstreamModule)
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
	FileSHA256  string `json:"file_sha256"`
	Description string `json:"description"`
}

const defaultUpstreamArtifactMaxBytes = 128 << 20
const defaultMaxStaleAge = time.Hour
const maxErrorBodyBytes = 64 << 10

var errBodyTooLarge = errors.New("body exceeds maximum size")

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

func NewForgeProxy(upstreamURL string, cacheTTL time.Duration, maxBodyBytes int64, artifacts artifactstorage.ArtifactStorage, artifactPrefix string, opts ...Option) (*ForgeProxy, error) {
	upstream, err := url.Parse(upstreamURL)
	if err != nil {
		return nil, fmt.Errorf("parse forge upstream url: %w", err)
	}

	p := &ForgeProxy{
		upstream: upstream,
		client: &http.Client{
			Timeout: 60 * time.Second,
		},
		cache:            NewResponseCache(),
		cacheTTL:         cacheTTL,
		maxStaleAge:      defaultMaxStaleAge,
		maxBodyBytes:     maxBodyBytes,
		maxArtifactBytes: defaultUpstreamArtifactMaxBytes,
		artifacts:        artifacts,
		artifactPrefix:   artifactPrefix,
	}
	for _, opt := range opts {
		opt(p)
	}

	return p, nil
}

func (p *ForgeProxy) Handler() http.Handler {
	return http.HandlerFunc(p.handle)
}

func (p *ForgeProxy) SetModuleObserver(fn func(context.Context, UpstreamModule)) {
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
		p.observeModuleResponse(req.Context(), req.URL.Path, entry.StatusCode, entry.Header, entry.Body)
		writeCachedResponse(w, entry, "HIT", isHead)
		return
	}
	metrics.ObserveUpstreamCache("json", "miss")
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
		status := http.StatusBadGateway
		message := err.Error()
		if errors.Is(err, errBodyTooLarge) {
			status = http.StatusRequestEntityTooLarge
			message = "response body exceeds maximum size"
		}
		writeProxyError(w, status, message)
		return
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if p.serveStaleJSONFallback(w, req, staleEntry, hasStaleEntry, isHead) {
			return
		}
		writeUpstreamResponse(w, resp, body, isHead)
		return
	}

	writeUpstreamResponse(w, resp, body, isHead)

	p.observeModuleResponse(req.Context(), req.URL.Path, resp.StatusCode, resp.Header, body)

	if p.shouldCache(resp, body) {
		p.cache.Set(cacheKey, CacheEntry{
			StatusCode: resp.StatusCode,
			Header:     cloneHeader(resp.Header),
			Body:       append([]byte(nil), body...),
			ExpiresAt:  time.Now().Add(p.cacheTTL),
		})
	}
}

func (p *ForgeProxy) serveStaleJSONFallback(w http.ResponseWriter, req *http.Request, entry CacheEntry, ok bool, isHead bool) bool {
	if !ok {
		return false
	}
	metrics.ObserveUpstreamCache("json", "stale")
	p.observeModuleResponse(req.Context(), req.URL.Path, entry.StatusCode, entry.Header, entry.Body)
	writeCachedResponse(w, entry, "STALE", isHead)
	return true
}

func (p *ForgeProxy) observeModuleResponse(ctx context.Context, requestPath string, statusCode int, header http.Header, body []byte) {
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
	p.moduleObserver(ctx, module)
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
		return nil, errors.New("upstream not found")
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
		Owner: stringValue(raw["owner"]),
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
	upstreamReq, err := http.NewRequestWithContext(req.Context(), method, upstreamURL.String(), nil)
	if err != nil {
		return nil, err
	}
	copyRequestHeaders(upstreamReq.Header, req.Header)
	upstreamReq.Header.Set("X-Forwarded-Host", req.Host)
	upstreamReq.Header.Set("X-Forwarded-Proto", httputil.ForwardedScheme(req))
	return p.client.Do(upstreamReq)
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
		resp, err := p.upstreamRequest(req, req.Method)
		if err != nil {
			writeProxyError(w, http.StatusBadGateway, err.Error())
			return
		}
		defer closeResponseBody(resp)
		isHead := req.Method == http.MethodHead
		body, relayed, err := p.readOrRelayArtifactUpstream(w, resp, "BYPASS", isHead)
		if err != nil {
			writeProxyError(w, http.StatusRequestEntityTooLarge, err.Error())
			return
		}
		if relayed {
			return
		}
		relayUpstreamResponse(w, resp, body, "X-Forge-Artifact-Cache", "BYPASS", isHead)
		return
	}

	objectPath := p.cachedArtifactPath(req.URL.Path)

	if req.Method == http.MethodHead {
		attrs, err := p.artifacts.Stat(req.Context(), objectPath)
		if err != nil {
			if errors.Is(err, artifactstorage.ErrObjectNotFound) {
				metrics.ObserveUpstreamCache("artifact", "miss")
				resp, err := p.upstreamRequest(req, http.MethodHead)
				if err != nil {
					writeProxyError(w, http.StatusBadGateway, err.Error())
					return
				}
				defer closeResponseBody(resp)
				relayUpstreamResponse(w, resp, nil, "X-Forge-Artifact-Cache", "MISS", true)
				return
			}
			writeProxyError(w, http.StatusBadGateway, err.Error())
			return
		}
		metrics.ObserveUpstreamCache("artifact", "hit")
		if attrs.ContentType != "" {
			w.Header().Set("Content-Type", attrs.ContentType)
		}
		w.Header().Set("Content-Length", strconv.FormatInt(attrs.Size, 10))
		w.Header().Set("X-Forge-Artifact-Cache", "HIT")
		w.WriteHeader(http.StatusOK)
		return
	}

	exists, err := p.artifacts.Exists(req.Context(), objectPath)
	if err != nil {
		writeProxyError(w, http.StatusBadGateway, err.Error())
		return
	}
	if exists {
		metrics.ObserveUpstreamCache("artifact", "hit")
		object, err := p.artifacts.Download(req.Context(), objectPath)
		if err != nil {
			writeProxyError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeArtifactResponse(w, http.StatusOK, object.ContentType, object.Body, true, false)
		return
	}
	metrics.ObserveUpstreamCache("artifact", "miss")

	resp, err := p.upstreamRequest(req, req.Method)
	if err != nil {
		writeProxyError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer closeResponseBody(resp)

	isHead := req.Method == http.MethodHead
	body, relayed, err := p.readOrRelayArtifactUpstream(w, resp, "MISS", isHead)
	if err != nil {
		writeProxyError(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	if relayed {
		return
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	if err := p.artifacts.Upload(req.Context(), objectPath, contentType, body); err != nil {
		writeProxyError(w, http.StatusBadGateway, err.Error())
		return
	}

	writeArtifactResponse(w, http.StatusOK, contentType, body, false, false)
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
		return nil, fmt.Errorf("upstream artifact exceeds maximum size of %d bytes", maxBytes)
	}

	overflowMessage := fmt.Sprintf("upstream artifact exceeds maximum size of %d bytes", maxBytes)
	body, err := readBoundedBody(reader, maxBytes, overflowMessage)
	if errors.Is(err, errBodyTooLarge) {
		return nil, fmt.Errorf("upstream artifact exceeds maximum size of %d bytes", maxBytes)
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
	header := w.Header()
	for key, values := range entry.Header {
		for _, value := range values {
			header.Add(key, value)
		}
	}
	header.Set("X-Forge-Cache", cacheHeader)
	w.WriteHeader(entry.StatusCode)
	if !isHead {
		_, _ = io.Copy(w, bytes.NewReader(entry.Body))
	}
}

func writeUpstreamResponse(w http.ResponseWriter, resp *http.Response, body []byte, isHead bool) {
	relayUpstreamResponse(w, resp, body, "X-Forge-Cache", "MISS", isHead)
}

func relayUpstreamResponse(w http.ResponseWriter, resp *http.Response, body []byte, cacheHeader, cacheStatus string, isHead bool) {
	header := w.Header()
	for key, values := range resp.Header {
		if isHopByHopHeader(key) {
			continue
		}
		for _, value := range values {
			header.Add(key, value)
		}
	}
	header.Set(cacheHeader, cacheStatus)
	w.WriteHeader(resp.StatusCode)
	if !isHead {
		_, _ = io.Copy(w, bytes.NewReader(body))
	}
}

func writeProxyError(w http.ResponseWriter, status int, message string) {
	http.Error(w, message, status)
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

func cloneHeader(header http.Header) map[string][]string {
	cloned := make(map[string][]string, len(header))
	for key, values := range header {
		if isHopByHopHeader(key) {
			continue
		}
		cloned[key] = append([]string(nil), values...)
	}
	return cloned
}

func isHopByHopHeader(key string) bool {
	switch strings.ToLower(key) {
	case "connection", "proxy-connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailers", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func (p *ForgeProxy) cachedArtifactPath(requestPath string) string {
	trimmed := strings.TrimPrefix(requestPath, "/")
	return path.Join(p.artifactPrefix, trimmed)
}

func writeArtifactResponse(w http.ResponseWriter, status int, contentType string, body []byte, hit bool, isHead bool) {
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	cacheStatus := "MISS"
	if hit {
		cacheStatus = "HIT"
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("X-Forge-Artifact-Cache", cacheStatus)
	w.WriteHeader(status)
	if !isHead {
		_, _ = io.Copy(w, bytes.NewReader(body))
	}
}
