package httpapi

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
	"github.com/zxzharmlesszxz/puppet-forge/internal/proxy"
	"github.com/zxzharmlesszxz/puppet-forge/internal/service"
	"github.com/zxzharmlesszxz/puppet-forge/internal/storage"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
	"github.com/zxzharmlesszxz/puppet-forge/internal/testutil"
	"github.com/zxzharmlesszxz/puppet-forge/internal/throttle"
	"github.com/zxzharmlesszxz/puppet-forge/internal/webauth"
)

type testArtifactStorage struct{}

func (s testArtifactStorage) Upload(context.Context, string, string, []byte) error {
	return nil
}

func (s testArtifactStorage) UploadIfAbsent(context.Context, string, string, []byte) (bool, error) {
	return true, nil
}

func (s testArtifactStorage) UploadReaderIfAbsent(context.Context, string, string, io.Reader) (bool, error) {
	return true, nil
}

func (s testArtifactStorage) Delete(context.Context, string) error {
	return nil
}

func (s testArtifactStorage) Exists(context.Context, string) (bool, error) {
	return false, nil
}

func (s testArtifactStorage) Open(context.Context, string) (storage.ObjectReader, error) {
	return storage.ObjectReader{}, store.ErrNotFound
}

func (s testArtifactStorage) Stat(context.Context, string) (storage.ObjectAttrs, error) {
	return storage.ObjectAttrs{}, storage.ErrObjectNotFound
}

func (s testArtifactStorage) PublicURL(string) string {
	return ""
}

type fixedDownloadStorage struct {
	body        []byte
	contentType string
}

type countingDownloadStorage struct {
	fixedDownloadStorage
	downloads int
}

type trackingRangeStorage struct {
	fixedDownloadStorage
	mu         sync.Mutex
	fullOpens  int
	rangeOpens int
}

func (s *trackingRangeStorage) Open(ctx context.Context, objectPath string) (storage.ObjectReader, error) {
	s.mu.Lock()
	s.fullOpens++
	s.mu.Unlock()
	return s.fixedDownloadStorage.Open(ctx, objectPath)
}

func (s *trackingRangeStorage) OpenRange(ctx context.Context, objectPath string, offset, length int64) (storage.ObjectReader, error) {
	s.mu.Lock()
	s.rangeOpens++
	s.mu.Unlock()
	return s.fixedDownloadStorage.OpenRange(ctx, objectPath, offset, length)
}

func (s *trackingRangeStorage) openCounts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fullOpens, s.rangeOpens
}

func (s *countingDownloadStorage) Open(ctx context.Context, objectPath string) (storage.ObjectReader, error) {
	s.downloads++
	return s.fixedDownloadStorage.Open(ctx, objectPath)
}

func (s fixedDownloadStorage) Upload(context.Context, string, string, []byte) error {
	return nil
}

func (s fixedDownloadStorage) UploadIfAbsent(context.Context, string, string, []byte) (bool, error) {
	return true, nil
}

func (s fixedDownloadStorage) UploadReaderIfAbsent(context.Context, string, string, io.Reader) (bool, error) {
	return true, nil
}

func (s fixedDownloadStorage) Delete(context.Context, string) error {
	return nil
}

func (s fixedDownloadStorage) Exists(context.Context, string) (bool, error) {
	return true, nil
}

func (s fixedDownloadStorage) Open(context.Context, string) (storage.ObjectReader, error) {
	return storage.ObjectReader{
		Body:        io.NopCloser(bytes.NewReader(s.body)),
		ContentType: s.contentType,
		Size:        int64(len(s.body)),
	}, nil
}

func (s fixedDownloadStorage) OpenRange(_ context.Context, _ string, offset, length int64) (storage.ObjectReader, error) {
	end := offset + length
	if offset < 0 || length < 0 || end > int64(len(s.body)) {
		return storage.ObjectReader{}, errors.New("invalid test object range")
	}
	body := s.body[offset:end]
	return storage.ObjectReader{
		Body:        io.NopCloser(bytes.NewReader(body)),
		ContentType: s.contentType,
		Size:        int64(len(body)),
	}, nil
}

func (s fixedDownloadStorage) Stat(context.Context, string) (storage.ObjectAttrs, error) {
	return storage.ObjectAttrs{ContentType: s.contentType, Size: int64(len(s.body))}, nil
}

func (s fixedDownloadStorage) PublicURL(string) string {
	return ""
}

func newTestRouter(modules *service.ModuleService, forgeProxy http.Handler, publicBaseURL string, authorizer *auth.Authorizer, webAuth *webauth.OIDCAuth, adminToken string, publicModuleAccess bool, activeReleaseTTL time.Duration, opts ...RouterOption) http.Handler {
	handler, err := NewRouter(RouterConfig{
		Modules:             modules,
		ForgeProxy:          forgeProxy,
		PublicBaseURL:       publicBaseURL,
		Authorizer:          authorizer,
		TokenHasher:         httpAPITestTokenHasher(),
		WebAuth:             webAuth,
		AdminToken:          adminToken,
		ManageSessionSecret: "test-manage-session-secret-32-bytes",
		PublicModuleAccess:  publicModuleAccess,
		ActiveReleaseTTL:    activeReleaseTTL,
	}, opts...)
	if err != nil {
		panic(err)
	}
	return handler
}

func TestRouterSecurityHeaders(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)
	moduleSvc := service.NewModuleService(st, testArtifactStorage{}, "modules", nil)
	handler, err := NewRouter(RouterConfig{
		Modules:             moduleSvc,
		AdminToken:          "admin-token",
		ManageSessionSecret: "test-manage-session-secret-32-bytes",
		SecurityHSTSEnabled: true,
	})
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rec.Header().Get("Strict-Transport-Security"); got != "max-age=31536000; includeSubDomains" {
		t.Fatalf("Strict-Transport-Security = %q", got)
	}
	policy := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(policy, "frame-ancestors 'none'") {
		t.Fatalf("Content-Security-Policy = %q", policy)
	}
	if strings.Contains(policy, "unsafe-inline") {
		t.Fatalf("Content-Security-Policy permits unsafe inline content: %q", policy)
	}
	if strings.Contains(policy, "nonce-") || strings.Contains(policy, "script-src") || strings.Contains(policy, "style-src") {
		t.Fatalf("machine endpoint received browser-only CSP directives: %q", policy)
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("X-Frame-Options = %q", got)
	}

	manageReq := httptest.NewRequest(http.MethodGet, "/manage/login", nil)
	manageRec := httptest.NewRecorder()
	handler.ServeHTTP(manageRec, manageReq)
	if got := manageRec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("manage Cache-Control = %q, want no-store", got)
	}
	managePolicy := manageRec.Header().Get("Content-Security-Policy")
	nonceMarker := "'nonce-"
	nonceStart := strings.Index(managePolicy, nonceMarker)
	if nonceStart < 0 {
		t.Fatalf("manage Content-Security-Policy has no nonce: %q", managePolicy)
	}
	nonceStart += len(nonceMarker)
	nonceEnd := strings.Index(managePolicy[nonceStart:], "'")
	if nonceEnd < 0 {
		t.Fatalf("manage Content-Security-Policy has malformed nonce: %q", managePolicy)
	}
	nonce := managePolicy[nonceStart : nonceStart+nonceEnd]
	if !strings.Contains(manageRec.Body.String(), `nonce="`+nonce+`"`) {
		t.Fatalf("manage login body does not use CSP nonce %q", nonce)
	}
	if strings.Contains(policy, nonce) {
		t.Fatalf("CSP nonce was reused across requests: %q", nonce)
	}
}

func TestFormActionSourcesIncludesOnlyOIDCLogoutOrigin(t *testing.T) {
	t.Parallel()

	if got := formActionSources(""); got != "'self'" {
		t.Fatalf("formActionSources(empty) = %q", got)
	}
	if got := formActionSources("https://auth.example.com"); got != "'self' https://auth.example.com" {
		t.Fatalf("formActionSources(logout origin) = %q", got)
	}
}

func TestApplicationRouterDoesNotExposeMetrics(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)
	handler, err := NewRouter(RouterConfig{
		Modules:             service.NewModuleService(st, testArtifactStorage{}, "modules", nil),
		AdminToken:          "admin-token",
		ManageSessionSecret: "test-manage-session-secret-32-bytes",
	})
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /metrics on application listener status = %d, want 404", rec.Code)
	}
}

func TestValidModuleFilePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		want bool
	}{
		{name: "nested file", path: "manifests/init.pp", want: true},
		{name: "empty", path: "", want: false},
		{name: "parent traversal", path: "../metadata.json", want: false},
		{name: "normalized traversal", path: "manifests/../metadata.json", want: false},
		{name: "double slash", path: "manifests//init.pp", want: false},
		{name: "nul byte", path: "manifests/init.pp\x00", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := validModuleFilePath(tt.path); got != tt.want {
				t.Fatalf("validModuleFilePath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestManageTokenLoginCanSwitchFromPublisherToAdmin(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)

	moduleSvc := service.NewModuleService(st, testArtifactStorage{}, "modules", nil)
	authorizer := newAdminAuthorizer(t, auth.TeamConfig{
		Team:          "teamname",
		PublishTokens: []string{"teamname-token"},
		PublishOwners: []string{"teamname"},
	})

	server := httptest.NewServer(newTestRouter(moduleSvc, nil, "http://example.test", authorizer, nil, "admin-token", false, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)

	jar := newCookieJar(t)
	client := server.Client()
	client.CheckRedirect = nil
	client.Jar = jar

	postManageToken(t, client, server.URL, "teamname-token")
	body := getBody(t, client, server.URL+"/manage/modules")
	if !strings.Contains(body, "Team: teamname") {
		t.Fatalf("expected teamname manage page, got body:\n%s", body)
	}

	postManageToken(t, client, server.URL, "admin-token")
	body = getBody(t, client, server.URL+"/manage/modules")
	if !strings.Contains(body, "Global administrator") {
		t.Fatalf("expected admin manage page, got body:\n%s", body)
	}
	if strings.Contains(body, "spaces: teamname") {
		t.Fatalf("admin manage page still shows teamname publish scope:\n%s", body)
	}
}

func TestManageTokenLoginStoresOpaqueSessionID(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)
	moduleSvc := service.NewModuleService(st, testArtifactStorage{}, "modules", nil)
	authorizer := newAdminAuthorizer(t)

	server := httptest.NewServer(newTestRouter(moduleSvc, nil, "http://example.test", authorizer, nil, "admin-token", false, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)

	jar := newCookieJar(t)
	client := server.Client()
	client.CheckRedirect = nil
	client.Jar = jar

	postManageToken(t, client, server.URL, "admin-token")
	target, err := url.Parse(server.URL + "/manage")
	if err != nil {
		t.Fatalf("parse manage URL error = %v", err)
	}
	var sessionCookie *http.Cookie
	for _, cookie := range jar.Cookies(target) {
		if cookie.Name == manageTokenCookie {
			sessionCookie = cookie
			break
		}
	}
	if sessionCookie == nil {
		t.Fatal("manage session cookie missing")
	}
	if sessionCookie.Value == "admin-token" || strings.Contains(sessionCookie.Value, "admin-token") {
		t.Fatalf("manage session cookie leaked token: %q", sessionCookie.Value)
	}

	body := getBody(t, client, server.URL+"/manage/modules")
	if !strings.Contains(body, "Global administrator") {
		t.Fatalf("server-side manage session was not accepted:\n%s", body)
	}
}

func TestManageLogoutRevokesCopiedSessionCookie(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)
	moduleSvc := service.NewModuleService(st, testArtifactStorage{}, "modules", nil)
	server := httptest.NewServer(newTestRouter(moduleSvc, nil, "http://example.test", newAdminAuthorizer(t), nil, "admin-token", false, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)

	jar := newCookieJar(t)
	client := server.Client()
	client.Jar = jar
	postManageToken(t, client, server.URL, "admin-token")
	target, err := url.Parse(server.URL + "/manage")
	if err != nil {
		t.Fatalf("url.Parse() error = %v", err)
	}
	var copiedSession *http.Cookie
	for _, cookie := range jar.Cookies(target) {
		if cookie.Name == manageTokenCookie {
			sessionCookie := *cookie
			copiedSession = &sessionCookie
			break
		}
	}
	if copiedSession == nil {
		t.Fatal("manage session cookie missing")
	}
	resp, err := client.PostForm(server.URL+"/manage/logout", manageFormValues(t, client, server.URL, nil))
	if err != nil {
		t.Fatalf("POST /manage/logout error = %v", err)
	}
	_ = resp.Body.Close()

	noRedirect := server.Client()
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequest(http.MethodGet, server.URL+"/manage", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.AddCookie(copiedSession)
	resp, err = noRedirect.Do(req)
	if err != nil {
		t.Fatalf("GET /manage with copied cookie error = %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/manage/login" {
		t.Fatalf("revoked copied cookie status=%d location=%q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestManageTokenSessionWorksAcrossRouterInstances(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)
	moduleSvcA := service.NewModuleService(st, testArtifactStorage{}, "modules", nil)
	moduleSvcB := service.NewModuleService(st, testArtifactStorage{}, "modules", nil)
	authorizer := newAdminAuthorizer(t)

	serverA := httptest.NewServer(newTestRouter(moduleSvcA, nil, "http://example.test", authorizer, nil, "admin-token", false, defaultActiveReleaseTTL))
	t.Cleanup(serverA.Close)
	serverB := httptest.NewServer(newTestRouter(moduleSvcB, nil, "http://example.test", authorizer, nil, "admin-token", false, defaultActiveReleaseTTL))
	t.Cleanup(serverB.Close)

	jar := newCookieJar(t)
	client := serverA.Client()
	client.CheckRedirect = nil
	client.Jar = jar

	postManageToken(t, client, serverA.URL, "admin-token")
	body := getBody(t, client, serverB.URL+"/manage/modules")
	if !strings.Contains(body, "Global administrator") {
		t.Fatalf("manage token session was not accepted by another router instance:\n%s", body)
	}
}

func TestRevokedPublishTokenInvalidatesManageSession(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newHTTPAPITestStore(t)
	if err := st.ReplaceTeamConfigs(ctx, []auth.TeamConfig{{
		Team:          "teamname",
		PublishTokens: []string{"session-publish-token"},
		PublishOwners: []string{"teamname"},
	}}); err != nil {
		t.Fatalf("ReplaceTeamConfigs() error = %v", err)
	}
	configs, err := st.LoadTeamConfigs(ctx)
	if err != nil {
		t.Fatalf("LoadTeamConfigs() error = %v", err)
	}
	authorizer, err := auth.NewAuthorizerWithTokenHasher(configs, httpAPITestTokenHasher())
	if err != nil {
		t.Fatalf("NewAuthorizerWithTokenHasher() error = %v", err)
	}
	handler, err := NewRouter(RouterConfig{
		Modules:             service.NewModuleService(st, testArtifactStorage{}, "modules", nil),
		Authorizer:          authorizer,
		TokenHasher:         httpAPITestTokenHasher(),
		ManageSessionSecret: "shared-manage-session-secret-32-bytes",
		RefreshAccessConfig: true,
	})
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	jar := newCookieJar(t)
	client := server.Client()
	client.Jar = jar
	postManageToken(t, client, server.URL, "session-publish-token")

	revokedAt := time.Now().UTC()
	configs[0].PublishTokenRecords[0].RevokedAt = &revokedAt
	if err := st.ReplaceTeamConfigs(ctx, configs); err != nil {
		t.Fatalf("ReplaceTeamConfigs(revoke) error = %v", err)
	}
	noRedirect := server.Client()
	noRedirect.Jar = jar
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noRedirect.Get(server.URL + "/manage")
	if err != nil {
		t.Fatalf("GET /manage after token revoke error = %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/manage/login" {
		t.Fatalf("revoked-token session status=%d location=%q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestRecordAccessTokenUsedPrunesExpiredThrottleEntries(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)
	router := &Router{
		modules:    service.NewModuleService(st, testArtifactStorage{}, "modules", nil),
		tokenUsage: throttle.NewExpirySet(defaultTokenUsageMaxEntries, tokenUsageRecordInterval),
	}
	router.tokenUsage.Record("expired-one", time.Now().Add(-2*time.Minute))
	router.tokenUsage.Record("expired-two", time.Now().Add(-time.Hour))
	router.recordAccessTokenUsed(context.Background(), auth.Principal{TokenID: "current"})

	if router.tokenUsage.Len() != 1 || !router.tokenUsage.Contains("current") {
		t.Fatalf("token usage throttle did not retain only the current token")
	}
}

func TestTokenUsageThrottleEvictsOldestEntryAtCapacity(t *testing.T) {
	t.Parallel()

	usage := throttle.NewExpirySet(2, tokenUsageRecordInterval)
	now := time.Now()
	if !usage.Record("oldest", now) || !usage.Record("newer", now.Add(time.Second)) {
		t.Fatal("initial token usage was unexpectedly throttled")
	}
	if !usage.Record("newest", now.Add(2*time.Second)) {
		t.Fatal("new token usage was rejected at capacity")
	}
	if usage.Len() != 2 || usage.Contains("oldest") || !usage.Contains("newer") || !usage.Contains("newest") {
		t.Fatal("token usage throttle did not evict the oldest entry")
	}
}

func TestBearerAuthorizationRejectsRevokedTokenWithStaleAuthorizer(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newHTTPAPITestStore(t)
	const rawToken = "stale-read-token"
	if err := st.ReplaceTeamConfigs(ctx, []auth.TeamConfig{{
		Team:       "teamname",
		ReadTokens: []string{rawToken},
	}}); err != nil {
		t.Fatalf("ReplaceTeamConfigs() error = %v", err)
	}
	configs, err := st.LoadTeamConfigs(ctx)
	if err != nil {
		t.Fatalf("LoadTeamConfigs() error = %v", err)
	}
	authorizer, err := auth.NewAuthorizerWithTokenHasher(configs, httpAPITestTokenHasher())
	if err != nil {
		t.Fatalf("NewAuthorizerWithTokenHasher() error = %v", err)
	}

	revokedAt := time.Now().UTC()
	configs[0].ReadTokenRecords[0].RevokedAt = &revokedAt
	if err := st.ReplaceTeamConfigs(ctx, configs); err != nil {
		t.Fatalf("ReplaceTeamConfigs(revoke) error = %v", err)
	}

	router := &Router{
		modules:    service.NewModuleService(st, testArtifactStorage{}, "modules", nil),
		authorizer: authorizer,
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/modules", nil)
	req.Header.Set("Authorization", "Bearer "+rawToken)
	recorder := httptest.NewRecorder()
	if router.requireReadAccess(recorder, req) {
		t.Fatal("revoked token was accepted by stale authorizer snapshot")
	}
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
}

func TestManagePrincipalRefreshesAccessConfigFromStore(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newHTTPAPITestStore(t)
	if err := st.ReplaceTeamConfigs(ctx, []auth.TeamConfig{
		{
			Team:          "teamname",
			PublishTokens: []string{"session-token"},
		},
	}); err != nil {
		t.Fatalf("ReplaceTeamConfigs() error = %v", err)
	}

	staleAuthorizer, err := auth.NewAuthorizerWithTokenHasher([]auth.TeamConfig{
		{
			Team:        "platform-admin",
			AdminTokens: []string{"session-token"},
		},
	}, httpAPITestTokenHasher())
	if err != nil {
		t.Fatalf("NewAuthorizer() error = %v", err)
	}
	router := &Router{
		modules:             service.NewModuleService(st, testArtifactStorage{}, "modules", nil),
		authorizer:          staleAuthorizer,
		tokenHasher:         httpAPITestTokenHasher(),
		adminToken:          "runtime-admin-token",
		manageSessions:      newManageSessionStore(service.NewModuleService(st, testArtifactStorage{}, "modules", nil), "shared-session-secret"),
		tokenUsage:          throttle.NewExpirySet(defaultTokenUsageMaxEntries, tokenUsageRecordInterval),
		refreshAccessConfig: true,
	}
	sessionID, _, err := router.manageSessions.Create(ctx, httpAPITestTokenHasher().Digest("session-token"), "", manageSessionTTL)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/manage", nil)
	req.AddCookie(&http.Cookie{Name: manageTokenCookie, Value: sessionID})

	principal, ok, err := router.managePrincipal(req)
	if err != nil {
		t.Fatalf("managePrincipal() error = %v", err)
	}
	if !ok {
		t.Fatal("managePrincipal() denied refreshed publish token")
	}
	if principal.CanAdmin || principal.Team != "teamname" || !principal.CanPublish {
		t.Fatalf("managePrincipal() used stale admin authorizer: %#v", principal)
	}
}

func TestManageAccessPostRefreshesStaleAdminBeforeSaving(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newHTTPAPITestStore(t)
	if err := st.ReplaceTeamConfigs(ctx, []auth.TeamConfig{
		{
			Team:          "teamname",
			PublishTokens: []string{"session-token"},
			PublishOwners: []string{"teamname"},
		},
	}); err != nil {
		t.Fatalf("ReplaceTeamConfigs() error = %v", err)
	}
	staleAuthorizer, err := auth.NewAuthorizerWithTokenHasher([]auth.TeamConfig{
		{
			Team:        "platform-admin",
			AdminTokens: []string{"session-token"},
		},
	}, httpAPITestTokenHasher())
	if err != nil {
		t.Fatalf("NewAuthorizer() error = %v", err)
	}
	router, err := NewRouter(RouterConfig{
		Modules:             service.NewModuleService(st, testArtifactStorage{}, "modules", nil),
		Authorizer:          staleAuthorizer,
		TokenHasher:         httpAPITestTokenHasher(),
		AdminToken:          "runtime-admin-token",
		ManageSessionSecret: "shared-manage-session-secret-32-bytes",
		RefreshAccessConfig: true,
	})
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	jar := newCookieJar(t)
	client := server.Client()
	client.Jar = jar
	postManageToken(t, client, server.URL, "session-token")
	resp, err := client.PostForm(server.URL+"/manage/access", manageFormValues(t, client, server.URL, url.Values{
		"action": {"save_team"},
		"team":   {"alpha"},
	}))
	if err != nil {
		t.Fatalf("POST /manage/access error = %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected refreshed non-admin session to get 403, got %d body:\n%s", resp.StatusCode, string(body))
	}
	configs, err := st.LoadTeamConfigs(ctx)
	if err != nil {
		t.Fatalf("LoadTeamConfigs() error = %v", err)
	}
	if findAccessConfig(configs, "alpha") != nil {
		t.Fatalf("stale admin authorizer created a new team: %#v", configs)
	}
}

func TestIndexFilterRefreshesPaginatedResultsWithoutReload(t *testing.T) {
	t.Parallel()

	var page bytes.Buffer
	if err := indexPageTemplate.Execute(&page, indexPageData{Filter: listFilterData{
		ID: "module-filter-form", InputID: "module-filter", Action: "/", Target: "module-list",
		Label: "Filter modules", Name: "q", Placeholder: "Filter by owner or module name",
	}}); err != nil {
		t.Fatalf("indexPageTemplate.Execute() error = %v", err)
	}
	if !strings.Contains(page.String(), ".list {\n      padding: 10px 0;\n      overflow: hidden;") {
		t.Fatalf("index page does not clip module rows to the rounded list boundary:\n%s", page.String())
	}
	for _, want := range []string{
		`id="module-filter-form"`,
		`data-async-filter="module-list"`,
		`id="module-list" data-async-list`,
		`name="q" type="search"`,
		`const refreshList = async (targetID, requestURL, options = {}) =>`,
		`const url = requestURL.toString();`,
		`const replacement = nextPage.getElementById(targetID)`,
		`current.replaceWith(replacement)`,
		`window.setTimeout(() =>`,
	} {
		if !strings.Contains(page.String(), want) {
			t.Fatalf("index page misses paginated live filtering %q:\n%s", want, page.String())
		}
	}
	listStart := strings.Index(page.String(), `id="module-list" data-async-list`)
	filterStart := strings.Index(page.String(), `id="module-filter-form"`)
	if listStart < 0 || filterStart < listStart {
		t.Fatalf("public filter must be rendered inside its async-list target:\n%s", page.String())
	}
}

func TestIndexOwnerFilterDeepLink(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)
	createTeamnameApacheModuleAndRelease(t, st)
	ctx := context.Background()
	other, err := st.UpsertModule(ctx, "other", "nginx")
	if err != nil {
		t.Fatalf("UpsertModule(other/nginx) error = %v", err)
	}
	if _, err := st.CreateRelease(ctx, domain.Release{
		ID:          "release-other-nginx",
		ModuleID:    other.ID,
		Owner:       other.Owner,
		Name:        other.Name,
		Source:      "local",
		Version:     "1.0.0",
		FileName:    "other-nginx-1.0.0.tar.gz",
		ContentType: "application/gzip",
		SizeBytes:   1,
		SHA256:      "other-nginx-sha256",
		StoragePath: "modules/other/nginx/1.0.0/other-nginx-1.0.0.tar.gz",
	}); err != nil {
		t.Fatalf("CreateRelease(other/nginx) error = %v", err)
	}

	handler := newTestRouter(service.NewModuleService(st, testArtifactStorage{}, "modules", nil), nil, "", nil, nil, "admin-token", true, defaultActiveReleaseTTL)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/?owner=teamname", nil))
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /?owner=teamname status = %d body:\n%s", recorder.Code, body)
	}
	for _, want := range []string{
		`<span><a href="/">Modules</a></span>`,
		`<span aria-current="page">teamname</span>`,
		`<h1>teamname Modules</h1>`,
		`Modules published in the teamname space.`,
		`placeholder="Filter teamname modules"`,
		`href="/modules/teamname/apache"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("owner-filtered index misses %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `href="/modules/other/nginx"`) {
		t.Fatalf("owner-filtered index contains another owner's module:\n%s", body)
	}
}

func TestManageAccessTokenCreateRotateAndRevoke(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newHTTPAPITestStore(t)
	client, baseURL := newAccessManageClient(t, st, ctx, []auth.TeamConfig{{
		Team:          "teamname",
		PublishOwners: []string{"teamname"},
		OIDCGroups:    []string{"teamname-publishers"},
	}})
	unnamed := manageFormValues(t, client, baseURL, url.Values{
		"action": {"create"}, "team": {"teamname"}, "role": {"read"},
	})
	unnamedResp, err := client.PostForm(baseURL+"/manage/access/token", unnamed)
	if err != nil {
		t.Fatalf("unnamed access token request error = %v", err)
	}
	_ = unnamedResp.Body.Close()
	if unnamedResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unnamed access token status = %d, want %d", unnamedResp.StatusCode, http.StatusBadRequest)
	}

	create := manageFormValues(t, client, baseURL, url.Values{
		"action":       {"create"},
		"team":         {"teamname"},
		"role":         {"read"},
		"name":         {"production r10k"},
		"expires_days": {"30"},
		"next":         {"/manage/teams/teamname/tokens"},
	})
	createReq, err := http.NewRequest(http.MethodPost, baseURL+"/manage/access/token", strings.NewReader(create.Encode()))
	if err != nil {
		t.Fatalf("build create access token request: %v", err)
	}
	createReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	createReq.Header.Set("Origin", "null")
	resp, err := client.Do(createReq)
	if err != nil {
		t.Fatalf("create access token error = %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read create response error = %v", err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("create response status=%d cache-control=%q body=%s", resp.StatusCode, resp.Header.Get("Cache-Control"), body)
	}
	oldRaw := accessTokenFromHTML(t, string(body), "pf_read_")
	configs, err := st.LoadTeamConfigs(ctx)
	if err != nil {
		t.Fatalf("LoadTeamConfigs() error = %v", err)
	}
	oldRecord := configs[0].ReadTokenRecords[0]
	if oldRecord.Digest != httpAPITestTokenHasher().Digest(oldRaw) || oldRecord.Description != "production r10k" || oldRecord.ExpiresAt == nil {
		t.Fatalf("unexpected created token record: %#v", oldRecord)
	}
	duplicate := manageFormValues(t, client, baseURL, url.Values{
		"action": {"create"}, "team": {"teamname"}, "role": {"read"}, "name": {"Production R10K"},
	})
	duplicateResp, err := client.PostForm(baseURL+"/manage/access/token", duplicate)
	if err != nil {
		t.Fatalf("duplicate access token request error = %v", err)
	}
	_ = duplicateResp.Body.Close()
	if duplicateResp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate access token status = %d, want %d", duplicateResp.StatusCode, http.StatusConflict)
	}
	authorizer, err := auth.NewAuthorizerWithTokenHasher(configs, httpAPITestTokenHasher())
	if err != nil {
		t.Fatalf("NewAuthorizerWithTokenHasher() error = %v", err)
	}
	usageServer := httptest.NewServer(newTestRouter(service.NewModuleService(st, testArtifactStorage{}, "modules", nil), nil, "http://example.test", authorizer, nil, "admin-token", false, defaultActiveReleaseTTL))
	t.Cleanup(usageServer.Close)
	usageReq, err := http.NewRequest(http.MethodGet, usageServer.URL+"/api/v1/modules", nil)
	if err != nil {
		t.Fatalf("NewRequest(token usage) error = %v", err)
	}
	usageReq.Header.Set("Authorization", "Bearer "+oldRaw)
	usageResp, err := usageServer.Client().Do(usageReq)
	if err != nil {
		t.Fatalf("GET modules with managed token error = %v", err)
	}
	_ = usageResp.Body.Close()
	if usageResp.StatusCode != http.StatusOK {
		t.Fatalf("GET modules with managed token status = %d", usageResp.StatusCode)
	}
	configs, err = st.LoadTeamConfigs(ctx)
	if err != nil {
		t.Fatalf("LoadTeamConfigs() after token use error = %v", err)
	}
	if configs[0].ReadTokenRecords[0].LastUsedAt == nil {
		t.Fatal("successful read request did not update last_used_at")
	}

	pageResp, err := client.Get(baseURL + "/manage/teams/teamname/tokens")
	if err != nil {
		t.Fatalf("GET team page error = %v", err)
	}
	pageBody, _ := io.ReadAll(pageResp.Body)
	_ = pageResp.Body.Close()
	if strings.Contains(string(pageBody), oldRaw) || !strings.Contains(string(pageBody), oldRecord.Prefix) {
		t.Fatalf("team page leaked raw token or omitted prefix:\n%s", pageBody)
	}

	rotate := manageFormValues(t, client, baseURL, url.Values{
		"action":   {"rotate"},
		"team":     {"teamname"},
		"token_id": {oldRecord.ID},
		"next":     {"/manage/teams/teamname/tokens"},
	})
	resp, err = client.PostForm(baseURL+"/manage/access/token", rotate)
	if err != nil {
		t.Fatalf("rotate access token error = %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	newRaw := accessTokenFromHTML(t, string(body), "pf_read_")
	configs, err = st.LoadTeamConfigs(ctx)
	if err != nil {
		t.Fatalf("LoadTeamConfigs() after rotate error = %v", err)
	}
	var rotatedOld, newRecord *auth.AccessTokenRecord
	for i := range configs[0].ReadTokenRecords {
		record := &configs[0].ReadTokenRecords[i]
		if record.ID == oldRecord.ID {
			rotatedOld = record
		} else {
			newRecord = record
		}
	}
	if len(configs[0].ReadTokenRecords) != 2 || rotatedOld == nil || rotatedOld.RevokedAt == nil || newRecord == nil {
		t.Fatalf("rotation did not revoke old and create new token: %#v", configs[0].ReadTokenRecords)
	}
	authorizer, err = auth.NewAuthorizerWithTokenHasher(configs, httpAPITestTokenHasher())
	if err != nil {
		t.Fatalf("NewAuthorizerWithTokenHasher() error = %v", err)
	}
	if _, ok := authorizer.AuthenticateToken(oldRaw); ok {
		t.Fatal("rotated token still authenticates")
	}
	if _, ok := authorizer.AuthenticateToken(newRaw); !ok {
		t.Fatal("replacement token does not authenticate")
	}

	revoke := manageFormValues(t, client, baseURL, url.Values{
		"action":   {"revoke"},
		"team":     {"teamname"},
		"token_id": {newRecord.ID},
		"next":     {"/manage/teams/teamname/tokens"},
	})
	if _, err := client.PostForm(baseURL+"/manage/access/token", revoke); err != nil {
		t.Fatalf("revoke access token error = %v", err)
	}
	configs, err = st.LoadTeamConfigs(ctx)
	if err != nil {
		t.Fatalf("LoadTeamConfigs() after revoke error = %v", err)
	}
	authorizer, err = auth.NewAuthorizerWithTokenHasher(configs, httpAPITestTokenHasher())
	if err != nil {
		t.Fatalf("NewAuthorizerWithTokenHasher() after revoke error = %v", err)
	}
	if _, ok := authorizer.AuthenticateToken(newRaw); ok {
		t.Fatal("revoked replacement token still authenticates")
	}
}

func TestManageAccessTokenNormalizesRoleBeforeStorage(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newHTTPAPITestStore(t)
	client, baseURL := newAccessManageClient(t, st, ctx, []auth.TeamConfig{{
		Team:          "teamname",
		PublishOwners: []string{"teamname"},
	}})
	form := manageFormValues(t, client, baseURL, url.Values{
		"action": {"create"}, "team": {"teamname"}, "role": {"READ"}, "name": {"deployment"},
	})
	resp, err := client.PostForm(baseURL+"/manage/access/token", form)
	if err != nil {
		t.Fatalf("create access token error = %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create access token status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	configs, err := st.LoadTeamConfigs(ctx)
	if err != nil {
		t.Fatalf("LoadTeamConfigs() error = %v", err)
	}
	if len(configs[0].ReadTokenRecords) != 1 || len(configs[0].PublishTokenRecords) != 0 {
		t.Fatalf("token records stored under wrong role: read=%d publish=%d", len(configs[0].ReadTokenRecords), len(configs[0].PublishTokenRecords))
	}
}

func accessTokenFromHTML(t *testing.T, body, prefix string) string {
	t.Helper()
	const marker = `<code id="created-token">`
	markerStart := strings.Index(body, marker)
	if markerStart < 0 {
		t.Fatalf("created token element not found in response:\n%s", body)
	}
	start := markerStart + len(marker)
	if !strings.HasPrefix(body[start:], prefix) {
		t.Fatalf("token prefix %q not found in created token element:\n%s", prefix, body)
	}
	end := strings.IndexByte(body[start:], '<')
	if end < 0 {
		t.Fatalf("token terminator not found in response:\n%s", body)
	}
	return body[start : start+end]
}

func TestManageAdminCanDeleteVersion(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)

	ctx := context.Background()
	module := createTeamnameApacheModuleAndRelease(t, st)
	createTeamnameApacheRelease(t, st, module, "2.0.0")

	moduleSvc := service.NewModuleService(st, testArtifactStorage{}, "modules", nil)
	authorizer := newAdminAuthorizer(t, auth.TeamConfig{
		Team:          "teamname",
		PublishTokens: []string{"teamname-token"},
		PublishOwners: []string{"teamname"},
	})

	server := httptest.NewServer(newTestRouter(moduleSvc, nil, "http://example.test", authorizer, nil, "admin-token", false, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)

	jar := newCookieJar(t)
	client := &http.Client{Transport: server.Client().Transport}
	client.Jar = jar

	postManageToken(t, client, server.URL, "teamname-token")
	body := getBody(t, client, server.URL+"/manage/modules")
	if strings.Contains(body, "/manage/modules/teamname/apache/versions/1.2.3/delete") || strings.Contains(body, "delete module") {
		t.Fatalf("publisher manage page exposes delete actions:\n%s", body)
	}

	resp, err := client.PostForm(server.URL+"/manage/modules/teamname/apache/versions/1.2.3/delete", manageFormValues(t, client, server.URL, nil))
	if err != nil {
		t.Fatalf("publisher delete version POST error = %v", err)
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("ReadAll(publisher delete response) error = %v", err)
	}
	if !strings.Contains(string(bodyBytes), "admin or team admin access required") {
		t.Fatalf("expected publisher delete to require admin or team admin, got body:\n%s", string(bodyBytes))
	}
	if _, err := st.GetRelease(ctx, "teamname", "apache", "1.2.3"); err != nil {
		t.Fatalf("publisher delete removed release or lookup failed: %v", err)
	}

	postManageToken(t, client, server.URL, "admin-token")
	resp, err = client.PostForm(server.URL+"/manage/modules/teamname/apache/versions/1.2.3/delete", manageFormValues(t, client, server.URL, nil))
	if err != nil {
		t.Fatalf("delete version POST error = %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected final delete response 200 after redirect, got %d", resp.StatusCode)
	}

	if _, err := st.GetRelease(ctx, "teamname", "apache", "1.2.3"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRelease() error = %v, want ErrNotFound", err)
	}
}

func TestDeleteReleaseRejectsLatestAndActiveVersions(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)

	ctx := context.Background()
	module := createTeamnameApacheModuleAndRelease(t, st)
	createTeamnameApacheRelease(t, st, module, "1.5.0")
	createTeamnameApacheRelease(t, st, module, "2.0.0")
	if err := st.MarkReleaseUsed(ctx, "teamname", "apache", "1.5.0"); err != nil {
		t.Fatalf("MarkReleaseUsed() error = %v", err)
	}

	server, client := setupManageTest(t, st, defaultActiveReleaseTTL)

	for _, tt := range []struct {
		version string
		want    string
	}{
		{version: "1.5.0", want: "active release teamname/apache 1.5.0 cannot be deleted"},
		{version: "2.0.0", want: "latest release teamname/apache 2.0.0 cannot be deleted"},
	} {
		resp, err := client.PostForm(server.URL+"/manage/modules/teamname/apache/versions/"+tt.version+"/delete", manageFormValues(t, client, server.URL, nil))
		if err != nil {
			t.Fatalf("POST delete %s error = %v", tt.version, err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("ReadAll(delete %s response) error = %v", tt.version, err)
		}
		if !strings.Contains(string(body), tt.want) {
			t.Fatalf("expected protected delete message %q, got body:\n%s", tt.want, string(body))
		}
		if _, err := st.GetRelease(ctx, "teamname", "apache", tt.version); err != nil {
			t.Fatalf("protected release %s was removed or lookup failed: %v", tt.version, err)
		}
	}

	req, err := http.NewRequest(http.MethodDelete, server.URL+"/api/v1/modules/teamname/apache/versions/2.0.0", nil)
	if err != nil {
		t.Fatalf("NewRequest(api delete latest) error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer admin-token")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("DELETE latest release error = %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("ReadAll(api delete latest response) error = %v", err)
	}
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "latest release teamname/apache 2.0.0 cannot be deleted") {
		t.Fatalf("expected API latest delete to get 409 protected error, got %d body=%s", resp.StatusCode, string(body))
	}
}

func TestReleaseAPIResponseHidesStorageDetails(t *testing.T) {
	t.Parallel()

	response := newReleaseAPIResponse(domain.Release{
		ID:              "release-id",
		ModuleID:        "module-id",
		Owner:           "teamname",
		Name:            "apache",
		Version:         "1.2.3",
		StoragePath:     "modules/teamname/apache/private.tar.gz",
		UpstreamFileURI: "https://upstream.example/v3/files/private.tar.gz",
		DownloadURL:     "https://bucket.example/private.tar.gz",
	})
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	encoded := string(body)
	for _, forbidden := range []string{"storage_path", "upstream_file_uri", "bucket.example", "upstream.example", "private.tar.gz"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("release API response exposes %q: %s", forbidden, encoded)
		}
	}
	if response.DownloadURL != "/api/v1/modules/teamname/apache/versions/1.2.3/download" {
		t.Fatalf("download URL = %q", response.DownloadURL)
	}
}

func TestDeleteModuleRejectsActiveReleases(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)
	ctx := context.Background()

	module := createTeamnameApacheModuleAndRelease(t, st)
	createTeamnameApacheRelease(t, st, module, "2.0.0")
	if err := st.MarkReleaseUsed(ctx, "teamname", "apache", "1.2.3"); err != nil {
		t.Fatalf("MarkReleaseUsed() error = %v", err)
	}

	moduleSvc := service.NewModuleService(st, testArtifactStorage{}, "modules", nil)
	authorizer := newAdminAuthorizer(t)
	server := httptest.NewServer(newTestRouter(moduleSvc, nil, "http://example.test", authorizer, nil, "admin-token", false, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)

	req, err := http.NewRequest(http.MethodDelete, server.URL+"/api/v1/modules/teamname/apache", nil)
	if err != nil {
		t.Fatalf("NewRequest(api delete module) error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer admin-token")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("DELETE module error = %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("ReadAll(api delete module response) error = %v", err)
	}
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "module contains active release teamname/apache 1.2.3 cannot be deleted") {
		t.Fatalf("expected module delete to get 409 active-release error, got %d body=%s", resp.StatusCode, string(body))
	}
	if _, err := st.GetModule(ctx, "teamname", "apache"); err != nil {
		t.Fatalf("active module was removed or lookup failed: %v", err)
	}
}

func TestDeleteModuleAllowsLatestReleaseWhenNoReleaseIsActive(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)
	ctx := context.Background()

	module := createTeamnameApacheModuleAndRelease(t, st)
	createTeamnameApacheRelease(t, st, module, "2.0.0")

	moduleSvc := service.NewModuleService(st, testArtifactStorage{}, "modules", nil)
	authorizer := newAdminAuthorizer(t)
	server := httptest.NewServer(newTestRouter(moduleSvc, nil, "http://example.test", authorizer, nil, "admin-token", false, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)

	req, err := http.NewRequest(http.MethodDelete, server.URL+"/api/v1/modules/teamname/apache", nil)
	if err != nil {
		t.Fatalf("NewRequest(api delete module) error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer admin-token")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("DELETE module error = %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("ReadAll(api delete module response) error = %v", err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"status":"deleted"`) {
		t.Fatalf("expected module delete to succeed, got %d body=%s", resp.StatusCode, string(body))
	}
	if _, err := st.GetModule(ctx, "teamname", "apache"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetModule() error = %v, want ErrNotFound", err)
	}
}

func TestDownloadMarksReleaseUsedAndManageHidesDelete(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)

	ctx := context.Background()
	module, err := st.UpsertModule(ctx, "teamname", "apache")
	if err != nil {
		t.Fatalf("UpsertModule() error = %v", err)
	}
	for _, version := range []string{"1.2.3", "1.5.0", "2.0.0"} {
		_, err = st.CreateRelease(ctx, domain.Release{
			ID:          "release-" + version,
			ModuleID:    module.ID,
			Owner:       "teamname",
			Name:        "apache",
			Source:      "local",
			Version:     version,
			FileName:    "teamname-apache-" + version + ".tar.gz",
			ContentType: "application/gzip",
			SizeBytes:   123,
			SHA256:      "deadbeef",
			StoragePath: "modules/teamname/apache/" + version + "/teamname-apache-" + version + ".tar.gz",
			Metadata:    map[string]any{},
		})
		if err != nil {
			t.Fatalf("CreateRelease(%s) error = %v", version, err)
		}
	}

	archiveBody := []byte("local archive bytes")
	md5Sum := md5.Sum(archiveBody)
	expectedMD5 := hex.EncodeToString(md5Sum[:])
	sha256Sum := sha256.Sum256(archiveBody)
	expectedSHA256 := hex.EncodeToString(sha256Sum[:])
	artifacts := &countingDownloadStorage{fixedDownloadStorage: fixedDownloadStorage{body: archiveBody, contentType: "application/gzip"}}
	moduleSvc := service.NewModuleService(st, artifacts, "modules", nil)
	authorizer := newAdminAuthorizer(t)

	server := httptest.NewServer(newTestRouter(moduleSvc, nil, "http://example.test", authorizer, nil, "admin-token", true, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)

	resp, err := server.Client().Get(server.URL + "/api/v1/modules/teamname/apache/versions/1.2.3/download")
	if err != nil {
		t.Fatalf("GET download error = %v", err)
	}
	downloadBody, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("ReadAll(download body) error = %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download status = %d, want %d body=%s", resp.StatusCode, http.StatusOK, string(downloadBody))
	}
	if got := resp.Header.Get("Location"); got != "" {
		t.Fatalf("download redirected to %q instead of serving the archive", got)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/gzip" {
		t.Fatalf("download content type = %q, want application/gzip", got)
	}
	if got := resp.Header.Get("Content-Length"); got != "19" {
		t.Fatalf("download content length = %q, want 19", got)
	}
	if got := resp.Header.Get("ETag"); got != `"`+expectedSHA256+`"` {
		t.Fatalf("download ETag = %q, want SHA-256 validator", got)
	}
	if !bytes.Equal(downloadBody, archiveBody) {
		t.Fatalf("download body = %q, want %q", downloadBody, archiveBody)
	}
	stored, err := st.GetRelease(ctx, "teamname", "apache", "1.2.3")
	if err != nil {
		t.Fatalf("GetRelease() error = %v", err)
	}
	if stored.MD5 != expectedMD5 || stored.SHA256 != expectedSHA256 || stored.SizeBytes != int64(len(archiveBody)) {
		t.Fatalf("stored checksums = md5:%q sha256:%q size:%d", stored.MD5, stored.SHA256, stored.SizeBytes)
	}
	if artifacts.downloads != 2 {
		t.Fatalf("archive opens after first download = %d, want checksum plus response", artifacts.downloads)
	}

	headReq, err := http.NewRequest(http.MethodHead, server.URL+"/api/v1/modules/teamname/apache/versions/1.2.3/download", nil)
	if err != nil {
		t.Fatalf("NewRequest(HEAD) error = %v", err)
	}
	headResp, err := server.Client().Do(headReq)
	if err != nil {
		t.Fatalf("HEAD download error = %v", err)
	}
	_ = headResp.Body.Close()
	if headResp.StatusCode != http.StatusOK || headResp.Header.Get("ETag") != `"`+expectedSHA256+`"` {
		t.Fatalf("HEAD download status=%d ETag=%q", headResp.StatusCode, headResp.Header.Get("ETag"))
	}
	if artifacts.downloads != 2 {
		t.Fatalf("archive opens after cached-checksum HEAD = %d, want 2", artifacts.downloads)
	}

	jar := newCookieJar(t)
	client := &http.Client{Transport: server.Client().Transport}
	client.Jar = jar
	postManageToken(t, client, server.URL, "admin-token")
	pageBody := getBody(t, client, server.URL+"/manage/modules")
	if !strings.Contains(pageBody, `data-module-card-url="/manage/modules/teamname/apache/card"`) {
		t.Fatalf("manage page does not expose lazy module card endpoint:\n%s", pageBody)
	}
	if strings.Contains(pageBody, "in use") {
		t.Fatalf("manage page eagerly renders release usage:\n%s", pageBody)
	}
	body := getBody(t, client, server.URL+"/manage/modules/teamname/apache/card")

	if strings.Contains(body, "/manage/modules/teamname/apache/versions/1.2.3/delete") {
		t.Fatalf("manage page exposes delete for active release:\n%s", body)
	}
	if strings.Contains(body, "/manage/modules/teamname/apache/versions/2.0.0/delete") {
		t.Fatalf("manage page exposes delete for latest release:\n%s", body)
	}
	if !strings.Contains(body, "/manage/modules/teamname/apache/delete") {
		t.Fatalf("manage page hides module delete action for admin:\n%s", body)
	}
	if !strings.Contains(body, "in use") {
		t.Fatalf("manage page does not mark active release:\n%s", body)
	}
	if !strings.Contains(body, `class="release-version is-active"`) {
		t.Fatalf("manage page does not style active release:\n%s", body)
	}
	if !strings.Contains(body, `class="release-version is-latest"`) {
		t.Fatalf("manage page does not style latest release:\n%s", body)
	}
	if strings.Contains(body, `version-deletable`) {
		t.Fatalf("manage page should not emit unused deletable release class:\n%s", body)
	}
	if !strings.Contains(body, "/manage/modules/teamname/apache/versions/1.5.0/delete") {
		t.Fatalf("manage page hides delete for inactive release:\n%s", body)
	}
	if !strings.Contains(body, `class="release-danger-button" type="submit">Delete</button>`) {
		t.Fatalf("manage page does not render release delete as a highlighted danger button:\n%s", body)
	}
	for _, wantStyle := range []string{
		`.release-version:hover`,
		`.release-version:focus-within`,
		`.release-version.is-latest:hover`,
		`.release-version.is-active:hover`,
		`.release-version:has(.release-danger-button:hover)`,
		`.release-version:has(.release-danger-button:focus-visible)`,
	} {
		if !strings.Contains(pageBody, wantStyle) {
			t.Fatalf("manage page does not render release highlight style %q:\n%s", wantStyle, pageBody)
		}
	}
	if strings.Contains(body, `class="link-button danger" type="submit">delete</button>`) {
		t.Fatalf("manage page renders legacy release delete link:\n%s", body)
	}
}

func TestReleaseDownloadHTTPRangeAndConditionalSemantics(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)
	ctx := context.Background()
	module, err := st.UpsertModule(ctx, "platform-core", "reverse-proxy")
	if err != nil {
		t.Fatalf("UpsertModule() error = %v", err)
	}
	body := []byte("0123456789")
	_, err = st.CreateRelease(ctx, domain.Release{
		ID: "range-release", ModuleID: module.ID, Owner: module.Owner, Name: module.Name,
		Source: "local", Version: "1.2.3-rc.1", FileName: "archive.tar.gz",
		ContentType: "application/gzip", SizeBytes: int64(len(body)), MD5: "md5", SHA256: "sha256-range",
		StoragePath: "modules/archive.tar.gz", Metadata: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CreateRelease() error = %v", err)
	}
	artifacts := &trackingRangeStorage{fixedDownloadStorage: fixedDownloadStorage{body: body, contentType: "application/gzip"}}
	moduleSvc := service.NewModuleService(st, artifacts, "modules", nil)
	server := httptest.NewServer(newTestRouter(moduleSvc, http.NotFoundHandler(), "http://example.test", nil, nil, "", true, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)
	downloadURL := server.URL + "/api/v1/modules/platform-core/reverse-proxy/versions/1.2.3-rc.1/download"

	request := func(method, byteRange, ifNoneMatch, ifRange string) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, downloadURL, nil)
		if err != nil {
			t.Fatalf("NewRequest() error = %v", err)
		}
		if byteRange != "" {
			req.Header.Set("Range", byteRange)
		}
		if ifNoneMatch != "" {
			req.Header.Set("If-None-Match", ifNoneMatch)
		}
		if ifRange != "" {
			req.Header.Set("If-Range", ifRange)
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("Do() error = %v", err)
		}
		responseBody, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("ReadAll() error = %v", err)
		}
		return resp, responseBody
	}

	resp, got := request(http.MethodGet, "", "", "")
	if resp.StatusCode != http.StatusOK || string(got) != string(body) || resp.Header.Get("ETag") != `"sha256-range"` {
		t.Fatalf("full download = %d %q ETag=%q", resp.StatusCode, got, resp.Header.Get("ETag"))
	}
	if resp.Header.Get("Accept-Ranges") != "bytes" || resp.Header.Get("Content-Length") != "10" {
		t.Fatalf("full download headers = %#v", resp.Header)
	}

	resp, got = request(http.MethodHead, "", "", "")
	if resp.StatusCode != http.StatusOK || len(got) != 0 || resp.Header.Get("Content-Length") != "10" {
		t.Fatalf("HEAD = %d %q headers=%#v", resp.StatusCode, got, resp.Header)
	}

	resp, got = request(http.MethodGet, "bytes=2-5", "", "")
	if resp.StatusCode != http.StatusPartialContent || string(got) != "2345" || resp.Header.Get("Content-Range") != "bytes 2-5/10" {
		t.Fatalf("range = %d %q headers=%#v", resp.StatusCode, got, resp.Header)
	}
	resp, got = request(http.MethodGet, "bytes=-3", "", "")
	if resp.StatusCode != http.StatusPartialContent || string(got) != "789" || resp.Header.Get("Content-Range") != "bytes 7-9/10" {
		t.Fatalf("suffix range = %d %q headers=%#v", resp.StatusCode, got, resp.Header)
	}

	for _, invalidRange := range []string{"bytes=1-2,4-5", "bytes=99-100", "items=0-1"} {
		resp, _ = request(http.MethodGet, invalidRange, "", "")
		if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable || resp.Header.Get("Content-Range") != "bytes */10" {
			t.Fatalf("invalid range %q = %d headers=%#v", invalidRange, resp.StatusCode, resp.Header)
		}
	}

	resp, got = request(http.MethodGet, "", `"sha256-range"`, "")
	if resp.StatusCode != http.StatusNotModified || len(got) != 0 {
		t.Fatalf("conditional GET = %d %q", resp.StatusCode, got)
	}
	resp, got = request(http.MethodGet, "bytes=0-1", "", `"other"`)
	if resp.StatusCode != http.StatusOK || string(got) != string(body) {
		t.Fatalf("If-Range mismatch = %d %q", resp.StatusCode, got)
	}
	fullOpens, rangeOpens := artifacts.openCounts()
	if fullOpens != 2 || rangeOpens != 2 {
		t.Fatalf("object opens full=%d range=%d, want 2 and 2", fullOpens, rangeOpens)
	}
}

func TestReleaseV3FileURIUsesForwardedHost(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "http://internal/v3/releases/puppetlabs-concat-9.1.0", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "forge.example.com")

	got := releaseV3FileURI(req, domain.Release{
		Owner:   "puppetlabs",
		Name:    "concat",
		Version: "9.1.0",
	})

	if got != "https://forge.example.com/v3/files/puppetlabs-concat-9.1.0.tar.gz" {
		t.Fatalf("releaseV3FileURI() = %q", got)
	}
}

func TestLocalV3ModuleWithoutReleasesHasNullCurrentRelease(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)
	if _, err := st.UpsertModule(context.Background(), "teamname", "empty"); err != nil {
		t.Fatalf("UpsertModule() error = %v", err)
	}

	proxyCalls := 0
	forgeProxy := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		proxyCalls++
		http.NotFound(w, req)
	})
	moduleSvc := service.NewModuleService(st, testArtifactStorage{}, "modules", nil)
	server := httptest.NewServer(newTestRouter(moduleSvc, forgeProxy, "http://example.test", nil, nil, "", true, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)

	resp, err := server.Client().Get(server.URL + "/v3/modules/teamname-empty")
	if err != nil {
		t.Fatalf("GET /v3/modules error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v3/modules status = %d", resp.StatusCode)
	}

	var module map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&module); err != nil {
		t.Fatalf("decode module JSON error = %v", err)
	}
	if module["current_release"] != nil {
		t.Fatalf("current_release = %#v, want null", module["current_release"])
	}
	releases, ok := module["releases"].([]any)
	if !ok || len(releases) != 0 {
		t.Fatalf("releases = %#v, want empty array", module["releases"])
	}
	if proxyCalls != 0 {
		t.Fatalf("Forge proxy calls = %d, want zero", proxyCalls)
	}
}

func TestV3ReleaseChecksumsComeFromServedArchive(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)

	ctx := context.Background()
	module, err := st.UpsertModule(ctx, "teamname", "apache")
	if err != nil {
		t.Fatalf("UpsertModule() error = %v", err)
	}
	_, err = st.CreateRelease(ctx, domain.Release{
		ID:          "release-1",
		ModuleID:    module.ID,
		Owner:       "teamname",
		Name:        "apache",
		Source:      "local",
		Version:     "1.2.3",
		FileName:    "teamname-apache-1.2.3.tar.gz",
		ContentType: "application/gzip",
		SizeBytes:   1,
		SHA256:      "stale-sha256",
		StoragePath: "modules/teamname/apache/1.2.3/teamname-apache-1.2.3.tar.gz",
		Metadata:    map[string]any{},
	})
	if err != nil {
		t.Fatalf("CreateRelease() error = %v", err)
	}

	body := []byte("real archive bytes")
	md5Sum := md5.Sum(body)
	expectedMD5 := hex.EncodeToString(md5Sum[:])
	sha := sha256.Sum256(body)
	expectedSHA := hex.EncodeToString(sha[:])
	artifacts := &countingDownloadStorage{
		fixedDownloadStorage: fixedDownloadStorage{body: body, contentType: "application/gzip"},
	}
	moduleSvc := service.NewModuleService(st, artifacts, "modules", nil)
	server := httptest.NewServer(newTestRouter(moduleSvc, http.NotFoundHandler(), "http://example.test", nil, nil, "", true, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)

	resp, err := server.Client().Get(server.URL + "/v3/releases/teamname-apache-1.2.3")
	if err != nil {
		t.Fatalf("GET /v3/releases error = %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v3/releases status = %d", resp.StatusCode)
	}

	var release map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		t.Fatalf("decode release JSON error = %v", err)
	}
	if release["file_sha256"] != expectedSHA {
		t.Fatalf("file_sha256 = %q, want %q", release["file_sha256"], expectedSHA)
	}
	if release["file_sha256"] == "stale-sha256" {
		t.Fatal("v3 release used stale stored checksum")
	}
	if release["file_md5"] != expectedMD5 {
		t.Fatalf("file_md5 = %q, want %q", release["file_md5"], expectedMD5)
	}

	resp, err = server.Client().Get(server.URL + "/v3/releases/teamname-apache-1.2.3")
	if err != nil {
		t.Fatalf("second GET /v3/releases error = %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second GET /v3/releases status = %d", resp.StatusCode)
	}
	if artifacts.downloads != 1 {
		t.Fatalf("release archive downloads = %d, want one lazy checksum backfill", artifacts.downloads)
	}
}

func TestIncompleteUpstreamV3ReleaseDoesNotReturnMalformedSuccess(t *testing.T) {
	t.Parallel()

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(upstream.Close)

	artifacts := testArtifactStorage{}
	forgeProxy, err := proxy.NewForgeProxy(
		upstream.URL,
		0,
		1024,
		artifacts,
		"upstream-cache",
		proxy.WithHTTPClient(upstream.Client()),
		proxy.WithPrivateNetworks(),
	)
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}

	st := newHTTPAPITestStore(t)
	ctx := context.Background()
	module, err := st.UpsertModule(ctx, "puppetlabs", "concat")
	if err != nil {
		t.Fatalf("UpsertModule() error = %v", err)
	}
	if _, err := st.CreateRelease(ctx, domain.Release{
		ID:           "release-incomplete",
		ModuleID:     module.ID,
		Owner:        "puppetlabs",
		Name:         "concat",
		Source:       "upstream",
		Version:      "9.1.0",
		FileName:     "puppetlabs-concat-9.1.0.tar.gz",
		ContentType:  "application/gzip",
		UpstreamSlug: "puppetlabs-concat-9.1.0",
		Metadata:     map[string]any{},
	}); err != nil {
		t.Fatalf("CreateRelease() error = %v", err)
	}

	moduleSvc := service.NewModuleService(st, artifacts, "modules", forgeProxy)
	server := httptest.NewServer(newTestRouter(
		moduleSvc,
		forgeProxy.Handler(),
		"http://example.test",
		nil,
		nil,
		"",
		true,
		defaultActiveReleaseTTL,
	))
	t.Cleanup(server.Close)

	resp, err := server.Client().Get(server.URL + "/v3/releases/puppetlabs-concat-9.1.0")
	if err != nil {
		t.Fatalf("GET /v3/releases error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /v3/releases returned malformed success: %s", body)
	}
	if upstreamCalls.Load() < 2 {
		t.Fatalf("upstream calls = %d, want hydration attempt and proxy fallback", upstreamCalls.Load())
	}
}

func TestLocalV3ReadOnlyRoutesRejectWriteMethodsWithoutMarkingUsage(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)

	ctx := context.Background()
	createTeamnameApacheModuleAndRelease(t, st)

	moduleSvc := service.NewModuleService(st, fixedDownloadStorage{body: []byte("archive"), contentType: "application/gzip"}, "modules", nil)
	server := httptest.NewServer(newTestRouter(moduleSvc, http.NotFoundHandler(), "http://example.test", nil, nil, "", true, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)

	for _, path := range []string{
		"/v3/modules/teamname-apache",
		"/v3/releases/teamname-apache-1.2.3",
		"/v3/files/teamname-apache-1.2.3.tar.gz",
	} {
		req, err := http.NewRequest(http.MethodPost, server.URL+path, nil)
		if err != nil {
			t.Fatalf("NewRequest(%s) error = %v", path, err)
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("POST %s error = %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s status = %d, want %d", path, resp.StatusCode, http.StatusMethodNotAllowed)
		}
	}

	active, err := st.IsReleaseActive(ctx, "teamname", "apache", "1.2.3", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("IsReleaseActive() error = %v", err)
	}
	if active {
		t.Fatal("write methods marked release as active")
	}
}

func TestUpstreamV3FileDownloadMarksReleaseUsed(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)

	ctx := context.Background()
	module, err := st.UpsertModule(ctx, "teamname", "apache")
	if err != nil {
		t.Fatalf("UpsertModule() error = %v", err)
	}
	for _, release := range []domain.Release{
		{
			ID:              "release-1",
			ModuleID:        module.ID,
			Owner:           "teamname",
			Name:            "apache",
			Source:          "upstream",
			Version:         "1.2.3",
			FileName:        "teamname-apache-1.2.3.tar.gz",
			ContentType:     "application/gzip",
			SizeBytes:       0,
			StoragePath:     "",
			UpstreamSlug:    "teamname-apache-1.2.3",
			UpstreamFileURI: "https://forge.example/v3/files/teamname-apache-1.2.3.tar.gz",
			Metadata:        map[string]any{},
		},
		{
			ID:          "release-2",
			ModuleID:    module.ID,
			Owner:       "teamname",
			Name:        "apache",
			Source:      "local",
			Version:     "2.0.0",
			FileName:    "teamname-apache-2.0.0.tar.gz",
			ContentType: "application/gzip",
			SizeBytes:   123,
			SHA256:      "deadbeef",
			StoragePath: "modules/teamname/apache/2.0.0/teamname-apache-2.0.0.tar.gz",
			Metadata:    map[string]any{},
		},
	} {
		if _, err := st.CreateRelease(ctx, release); err != nil {
			t.Fatalf("CreateRelease(%s) error = %v", release.Version, err)
		}
	}

	moduleSvc := service.NewModuleService(st, testArtifactStorage{}, "modules", nil)
	authorizer := newAdminAuthorizer(t)
	forgeProxy := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write([]byte("upstream archive"))
	})
	server := httptest.NewServer(newTestRouter(moduleSvc, forgeProxy, "http://example.test", authorizer, nil, "admin-token", true, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)

	resp, err := server.Client().Get(server.URL + "/v3/files/teamname-apache-1.2.3.tar.gz")
	if err != nil {
		t.Fatalf("GET /v3/files error = %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v3/files status = %d", resp.StatusCode)
	}

	jar := newCookieJar(t)
	client := &http.Client{Transport: server.Client().Transport}
	client.Jar = jar
	postManageToken(t, client, server.URL, "admin-token")
	body := getBody(t, client, server.URL+"/manage/modules/teamname/apache/card")

	if !strings.Contains(body, "in use") {
		t.Fatalf("manage page does not mark upstream v3 download as active:\n%s", body)
	}
	if strings.Contains(body, "/manage/modules/teamname/apache/versions/1.2.3/delete") {
		t.Fatalf("manage page exposes delete for active upstream release:\n%s", body)
	}
}

func TestUpstreamV3ReleaseUsesLocalFileURIAndMarksSelectedVersionActive(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/v3/releases/puppetlabs-concat-9.1.0":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"slug":"puppetlabs-concat-9.1.0",
				"version":"9.1.0",
				"file_uri":"https://forgeapi.puppetlabs.com/v3/files/puppetlabs-concat-9.1.0.tar.gz",
				"file_name":"puppetlabs-concat-9.1.0.tar.gz",
				"file_sha256":"9f844ff2df372c25823534f525bb2b31e853c5a7c6b8b8641a67ef03e83a460d"
			}`))
		case "/v3/files/puppetlabs-concat-9.1.0.tar.gz":
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write([]byte("upstream archive"))
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(upstream.Close)

	artifacts := fixedDownloadStorage{body: []byte("upstream archive"), contentType: "application/gzip"}
	forgeProxy, err := proxy.NewForgeProxy(upstream.URL, 0, 1024, artifacts, "upstream-cache", proxy.WithHTTPClient(upstream.Client()), proxy.WithPrivateNetworks())
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}
	st := newHTTPAPITestStore(t)

	ctx := context.Background()
	module, err := st.UpsertModule(ctx, "puppetlabs", "concat")
	if err != nil {
		t.Fatalf("UpsertModule() error = %v", err)
	}
	for _, release := range []domain.Release{
		{
			ID:           "release-9.1.0",
			ModuleID:     module.ID,
			Owner:        "puppetlabs",
			Name:         "concat",
			Source:       "upstream",
			Version:      "9.1.0",
			FileName:     "puppetlabs-concat-9.1.0.tar.gz",
			ContentType:  "application/gzip",
			UpstreamSlug: "puppetlabs-concat-9.1.0",
			Metadata:     map[string]any{},
		},
		{
			ID:           "release-10.0.0",
			ModuleID:     module.ID,
			Owner:        "puppetlabs",
			Name:         "concat",
			Source:       "upstream",
			Version:      "10.0.0",
			FileName:     "puppetlabs-concat-10.0.0.tar.gz",
			ContentType:  "application/gzip",
			UpstreamSlug: "puppetlabs-concat-10.0.0",
			Metadata:     map[string]any{},
		},
	} {
		if _, err := st.CreateRelease(ctx, release); err != nil {
			t.Fatalf("CreateRelease(%s) error = %v", release.Version, err)
		}
	}

	moduleSvc := service.NewModuleService(st, artifacts, "modules", forgeProxy)
	server := newAdminServer(t, moduleSvc, forgeProxy.Handler())
	resp := getV3Release(t, server)
	var releaseJSON map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&releaseJSON); err != nil {
		_ = resp.Body.Close()
		t.Fatalf("decode release JSON error = %v", err)
	}
	_ = resp.Body.Close()
	fileURI, ok := releaseJSON["file_uri"].(string)
	if !ok {
		t.Fatalf("file_uri missing from release JSON: %#v", releaseJSON)
	}
	if !strings.HasPrefix(fileURI, server.URL+"/v3/files/") {
		t.Fatalf("file_uri = %q, want local server URL %q", fileURI, server.URL)
	}
	archiveBody := []byte("upstream archive")
	md5Sum := md5.Sum(archiveBody)
	sha256Sum := sha256.Sum256(archiveBody)
	if releaseJSON["file_md5"] != hex.EncodeToString(md5Sum[:]) {
		t.Fatalf("file_md5 = %q, want checksum of served archive", releaseJSON["file_md5"])
	}
	if releaseJSON["file_sha256"] != hex.EncodeToString(sha256Sum[:]) {
		t.Fatalf("file_sha256 = %q, want checksum of served archive", releaseJSON["file_sha256"])
	}

	resp, err = server.Client().Get(fileURI)
	if err != nil {
		t.Fatalf("GET local file_uri error = %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET local file_uri status = %d", resp.StatusCode)
	}

	jar := newCookieJar(t)
	client := &http.Client{Transport: server.Client().Transport}
	client.Jar = jar
	postManageToken(t, client, server.URL, "admin-token")
	body := getBody(t, client, server.URL+"/manage/modules/puppetlabs/concat/card")

	if !strings.Contains(body, "9.1.0") || !strings.Contains(body, "in use") {
		t.Fatalf("manage page does not mark selected upstream release active:\n%s", body)
	}
	if strings.Contains(body, "/manage/modules/puppetlabs/concat/versions/9.1.0/delete") {
		t.Fatalf("manage page exposes delete for active upstream release:\n%s", body)
	}
	if !strings.Contains(body, `class="release-version is-latest"`) {
		t.Fatalf("manage page does not mark latest release:\n%s", body)
	}
}

func TestV3ReleaseRequestMarksSelectedVersionActive(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)

	ctx := context.Background()
	module, err := st.UpsertModule(ctx, "puppetlabs", "concat")
	if err != nil {
		t.Fatalf("UpsertModule() error = %v", err)
	}
	for _, release := range []domain.Release{
		{
			ID:              "release-9.1.0",
			ModuleID:        module.ID,
			Owner:           "puppetlabs",
			Name:            "concat",
			Source:          "upstream",
			Version:         "9.1.0",
			FileName:        "puppetlabs-concat-9.1.0.tar.gz",
			ContentType:     "application/gzip",
			SizeBytes:       16,
			MD5:             "c57d31f96ef80516f1e4807278b21da6",
			SHA256:          "9f844ff2df372c25823534f525bb2b31e853c5a7c6b8b8641a67ef03e83a460d",
			StoragePath:     "upstream-cache/v3/files/puppetlabs-concat-9.1.0.tar.gz",
			UpstreamFileURI: "https://forgeapi.puppetlabs.com/v3/files/puppetlabs-concat-9.1.0.tar.gz",
			Metadata:        map[string]any{},
		},
		{
			ID:          "release-10.0.0",
			ModuleID:    module.ID,
			Owner:       "puppetlabs",
			Name:        "concat",
			Source:      "upstream",
			Version:     "10.0.0",
			FileName:    "puppetlabs-concat-10.0.0.tar.gz",
			ContentType: "application/gzip",
			SHA256:      "latest-sha256",
			Metadata:    map[string]any{},
		},
	} {
		if _, err := st.CreateRelease(ctx, release); err != nil {
			t.Fatalf("CreateRelease(%s) error = %v", release.Version, err)
		}
	}

	moduleSvc := service.NewModuleService(st, testArtifactStorage{}, "modules", nil)
	server := newAdminServer(t, moduleSvc, http.NotFoundHandler())
	resp := getV3Release(t, server)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v3/releases status = %d", resp.StatusCode)
	}

	jar := newCookieJar(t)
	client := &http.Client{Transport: server.Client().Transport}
	client.Jar = jar
	postManageToken(t, client, server.URL, "admin-token")
	body := getBody(t, client, server.URL+"/manage/modules/puppetlabs/concat/card")

	if !strings.Contains(body, "9.1.0") || !strings.Contains(body, "in use") {
		t.Fatalf("manage page does not mark requested release active:\n%s", body)
	}
	if strings.Contains(body, "/manage/modules/puppetlabs/concat/versions/9.1.0/delete") {
		t.Fatalf("manage page exposes delete for requested active release:\n%s", body)
	}
}

func TestV3ModuleRequestDoesNotMarkLatestReleaseActive(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)

	ctx := context.Background()
	module, err := st.UpsertModule(ctx, "stm", "debconf")
	if err != nil {
		t.Fatalf("UpsertModule() error = %v", err)
	}
	for _, release := range []domain.Release{
		{
			ID:          "release-8.0.0",
			ModuleID:    module.ID,
			Owner:       "stm",
			Name:        "debconf",
			Source:      "local",
			Version:     "8.0.0",
			FileName:    "stm-debconf-8.0.0.tar.gz",
			ContentType: "application/gzip",
			SHA256:      "old-sha256",
			StoragePath: "modules/stm/debconf/8.0.0/stm-debconf-8.0.0.tar.gz",
			Metadata:    map[string]any{},
		},
		{
			ID:          "release-9.1.0",
			ModuleID:    module.ID,
			Owner:       "stm",
			Name:        "debconf",
			Source:      "local",
			Version:     "9.1.0",
			FileName:    "stm-debconf-9.1.0.tar.gz",
			ContentType: "application/gzip",
			SHA256:      "latest-sha256",
			StoragePath: "modules/stm/debconf/9.1.0/stm-debconf-9.1.0.tar.gz",
			Metadata:    map[string]any{},
		},
	} {
		if _, err := st.CreateRelease(ctx, release); err != nil {
			t.Fatalf("CreateRelease(%s) error = %v", release.Version, err)
		}
	}

	moduleSvc := service.NewModuleService(st, testArtifactStorage{}, "modules", nil)
	server := newAdminServer(t, moduleSvc, http.NotFoundHandler())

	body := getV3ModulesAndManagePage(t, server)

	if !strings.Contains(body, "9.1.0") {
		t.Fatalf("manage page does not show latest release after module request:\n%s", body)
	}
	if strings.Contains(body, "in use") {
		t.Fatalf("module metadata request incorrectly marks latest release active:\n%s", body)
	}
	if strings.Contains(body, "/manage/modules/stm/debconf/versions/9.1.0/delete") {
		t.Fatalf("manage page exposes delete for latest release:\n%s", body)
	}
}

func TestUpstreamV3ModuleRequestIndexesWithoutMarkingCurrentReleaseActive(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v3/modules/stm-debconf" {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"slug":"stm-debconf",
			"owner":"stm",
			"name":"debconf",
			"current_release":{"slug":"stm-debconf-9.1.0"},
			"releases":[{"slug":"stm-debconf-9.1.0"}]
		}`))
	}))
	t.Cleanup(upstream.Close)

	forgeProxy, err := proxy.NewForgeProxy(upstream.URL, 0, 1024, testArtifactStorage{}, "upstream-cache", proxy.WithHTTPClient(upstream.Client()), proxy.WithPrivateNetworks())
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}
	st := newHTTPAPITestStore(t)

	moduleSvc := service.NewModuleService(st, testArtifactStorage{}, "modules", forgeProxy)
	forgeProxy.SetModuleObserver(func(ctx context.Context, module proxy.UpstreamModule, fresh bool) {
		if fresh {
			if err := moduleSvc.IndexUpstreamModule(ctx, module); err != nil {
				t.Errorf("IndexUpstreamModule() error = %v", err)
				return
			}
		}
	})
	server := newAdminServer(t, moduleSvc, forgeProxy.Handler())

	body := getV3ModulesAndManagePage(t, server)

	if !strings.Contains(body, "9.1.0") {
		t.Fatalf("manage page does not show indexed upstream current release:\n%s", body)
	}
	if strings.Contains(body, "in use") {
		t.Fatalf("upstream module metadata request incorrectly marks current release active:\n%s", body)
	}
	if strings.Contains(body, "/manage/modules/stm/debconf/versions/9.1.0/delete") {
		t.Fatalf("manage page exposes delete for latest upstream current release:\n%s", body)
	}
}

func TestV3ReleaseRequestRestoresDeletedUpstreamReleaseOnDemand(t *testing.T) {
	t.Parallel()
	archive := []byte("restored upstream archive")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v3/releases/puppetlabs-stdlib-1.0.0" {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"slug":"puppetlabs-stdlib-1.0.0",
			"version":"1.0.0",
			"description":"stdlib 1.0.0",
			"readme":"# stdlib",
			"file_uri":"https://forge.example/v3/files/puppetlabs-stdlib-1.0.0.tar.gz",
			"file_name":"puppetlabs-stdlib-1.0.0.tar.gz",
			"file_sha256":"abc123"
		}`))
	}))
	t.Cleanup(upstream.Close)

	artifacts := fixedDownloadStorage{body: archive, contentType: "application/gzip"}
	forgeProxy, err := proxy.NewForgeProxy(upstream.URL, 0, 1024, artifacts, "upstream-cache", proxy.WithHTTPClient(upstream.Client()), proxy.WithPrivateNetworks())
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}
	st := newHTTPAPITestStore(t)
	moduleSvc := service.NewModuleService(st, artifacts, "modules", forgeProxy)
	upstreamModule := proxy.UpstreamModule{
		Slug:  "puppetlabs-stdlib",
		Owner: "puppetlabs",
		Name:  "stdlib",
		CurrentRelease: proxy.UpstreamReleaseRef{
			Slug:    "puppetlabs-stdlib-2.0.0",
			Version: "2.0.0",
		},
		Releases: []proxy.UpstreamReleaseRef{
			{Slug: "puppetlabs-stdlib-1.0.0", Version: "1.0.0"},
			{Slug: "puppetlabs-stdlib-2.0.0", Version: "2.0.0"},
		},
	}

	ctx := context.Background()
	if err := moduleSvc.IndexUpstreamModule(ctx, upstreamModule); err != nil {
		t.Fatalf("IndexUpstreamModule() error = %v", err)
	}
	if err := st.DeleteRelease(ctx, "puppetlabs", "stdlib", "1.0.0"); err != nil {
		t.Fatalf("DeleteRelease() error = %v", err)
	}
	if _, err := st.GetRelease(ctx, "puppetlabs", "stdlib", "1.0.0"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRelease(deleted) error = %v, want ErrNotFound", err)
	}

	server := newAdminServer(t, moduleSvc, forgeProxy.Handler())
	resp, err := server.Client().Get(server.URL + "/v3/releases/puppetlabs-stdlib-1.0.0")
	if err != nil {
		t.Fatalf("GET /v3/releases deleted upstream release error = %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /v3/releases deleted upstream release status = %d body = %s", resp.StatusCode, string(body))
	}
	if _, err := st.GetRelease(ctx, "puppetlabs", "stdlib", "1.0.0"); err != nil {
		t.Fatalf("GetRelease(restored local) error = %v", err)
	}
}

func TestModulePageDoesNotExposeVersionDeleteAction(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)

	ctx := context.Background()
	module, err := st.UpsertModule(ctx, "teamname", "apache")
	if err != nil {
		t.Fatalf("UpsertModule() error = %v", err)
	}
	_, err = st.CreateRelease(ctx, domain.Release{
		ID:          "release-1",
		ModuleID:    module.ID,
		Owner:       "teamname",
		Name:        "apache",
		Source:      "local",
		Version:     "1.2.3",
		FileName:    "teamname-apache-1.2.3.tar.gz",
		ContentType: "application/gzip",
		SizeBytes:   123,
		SHA256:      "deadbeef",
		StoragePath: "modules/teamname/apache/1.2.3/teamname-apache-1.2.3.tar.gz",
		Readme:      "# Apache\n",
		Metadata:    map[string]any{},
	})
	if err != nil {
		t.Fatalf("CreateRelease() error = %v", err)
	}

	server := httptest.NewServer(newTestRouter(service.NewModuleService(st, testArtifactStorage{}, "modules", nil), nil, "http://example.test", nil, nil, "", false, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)

	body := getBody(t, server.Client(), server.URL+"/modules/teamname/apache")
	if !strings.Contains(body, "teamname/apache") {
		t.Fatalf("expected module page, got body:\n%s", body)
	}
	if strings.Contains(body, "Delete This Version") || strings.Contains(body, "delete-version") || strings.Contains(body, "method: 'DELETE'") {
		t.Fatalf("module page exposes version delete action:\n%s", body)
	}
	if count := strings.Count(body, `class="code-window"`); count != 5 {
		t.Fatalf("expected five install code windows, got %d", count)
	}
	if count := strings.Count(body, `class="copy-button"`); count != 5 {
		t.Fatalf("expected five copy buttons, got %d", count)
	}
	if !strings.Contains(body, "navigator.clipboard") || !strings.Contains(body, "document.querySelectorAll('button.copy-button')") {
		t.Fatalf("module page does not include copy button handler:\n%s", body)
	}
	for _, want := range []string{
		`id="copy-version-link"`,
		`title="Copy link to this version"`,
		`url.searchParams.set('version', versionSelect.value)`,
		`await copyText(url.toString())`,
		`document.execCommand('copy')`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("module page does not include version permalink control %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "pe_r10k::forge_settings") {
		t.Fatalf("module page renders old r10k forge_settings snippet:\n%s", body)
	}
	expectedR10KBlock := "forge:\n  baseurl: '" + server.URL + "'\n  authorization_token: 'Bearer &lt;READ_TOKEN&gt;'"
	if !strings.Contains(body, expectedR10KBlock) {
		t.Fatalf("module page does not render expected r10k forge block:\n%s", body)
	}
}

func TestModuleFileRouteRequiresReadAccessWhenPrivate(t *testing.T) {
	t.Parallel()

	archive, err := testutil.BuildTarGz(map[string]string{
		"teamname-apache-1.2.3/metadata.json": `{"name":"teamname-apache","version":"1.2.3"}`,
	})
	if err != nil {
		t.Fatalf("testutil.BuildTarGz() error = %v", err)
	}

	st := newHTTPAPITestStore(t)

	ctx := context.Background()
	module, err := st.UpsertModule(ctx, "teamname", "apache")
	if err != nil {
		t.Fatalf("UpsertModule() error = %v", err)
	}
	_, err = st.CreateRelease(ctx, domain.Release{
		ID:          "release-1",
		ModuleID:    module.ID,
		Owner:       "teamname",
		Name:        "apache",
		Source:      "local",
		Version:     "1.2.3",
		FileName:    "teamname-apache-1.2.3.tar.gz",
		ContentType: "application/gzip",
		SizeBytes:   int64(len(archive)),
		SHA256:      "deadbeef",
		StoragePath: "modules/teamname/apache/1.2.3/teamname-apache-1.2.3.tar.gz",
		Metadata:    map[string]any{},
	})
	if err != nil {
		t.Fatalf("CreateRelease() error = %v", err)
	}

	moduleSvc := service.NewModuleService(st, fixedDownloadStorage{body: archive, contentType: "application/gzip"}, "modules", nil)
	authorizer := newAdminAuthorizer(t, auth.TeamConfig{Team: "teamname", ReadTokens: []string{"read-token"}})

	privateServer := httptest.NewServer(newTestRouter(moduleSvc, nil, "http://example.test", authorizer, nil, "admin-token", false, defaultActiveReleaseTTL))
	t.Cleanup(privateServer.Close)
	publicServer := httptest.NewServer(newTestRouter(moduleSvc, nil, "http://example.test", authorizer, nil, "admin-token", true, defaultActiveReleaseTTL))
	t.Cleanup(publicServer.Close)

	path := "/modules/teamname/apache/versions/1.2.3/files/metadata.json"

	resp, err := privateServer.Client().Get(privateServer.URL + path)
	if err != nil {
		t.Fatalf("private GET module file without token error = %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected private module file without token to get 401, got %d", resp.StatusCode)
	}

	req, err := http.NewRequest(http.MethodGet, privateServer.URL+path, nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer read-token")
	resp, err = privateServer.Client().Do(req)
	if err != nil {
		t.Fatalf("private GET module file with token error = %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("ReadAll(private module file) error = %v", err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"version":"1.2.3"`) {
		t.Fatalf("expected private module file with token to get 200 metadata, got %d body=%s", resp.StatusCode, string(body))
	}

	resp, err = publicServer.Client().Get(publicServer.URL + path)
	if err != nil {
		t.Fatalf("public GET module file without token error = %v", err)
	}
	body, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("ReadAll(public module file) error = %v", err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"version":"1.2.3"`) {
		t.Fatalf("expected public module file without token to get 200 metadata, got %d body=%s", resp.StatusCode, string(body))
	}
}

func TestCanDeleteInSpaceAllowsOnlyAdminsAndTeamAdmins(t *testing.T) {
	t.Parallel()

	teamAdmin := auth.Principal{
		Team:          "teamname",
		CanPublish:    true,
		CanManageTeam: true,
		PublishOwners: map[string]struct{}{"teamname": {}, "shared": {}},
		ManagedTeams:  map[string]struct{}{"teamname": {}},
	}
	if !canDeleteInSpace(teamAdmin, "teamname") {
		t.Fatalf("team admin cannot delete in primary team space")
	}
	if canDeleteInSpace(teamAdmin, "shared") {
		t.Fatalf("team admin can delete in extra publish space")
	}
	if canDeleteInSpace(teamAdmin, "alpha") {
		t.Fatalf("team admin can delete outside own spaces")
	}

	publisher := auth.Principal{
		Team:          "teamname",
		CanPublish:    true,
		PublishOwners: map[string]struct{}{"teamname": {}},
	}
	if canDeleteInSpace(publisher, "teamname") {
		t.Fatalf("publisher can delete in own space")
	}

	admin := auth.Principal{Team: "platform-admin", CanAdmin: true}
	if !canDeleteInSpace(admin, "alpha") {
		t.Fatalf("global admin cannot delete across spaces")
	}
}

func TestPublicModuleAccessControlsReadEndpoints(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)

	authorizer := newAdminAuthorizer(t, auth.TeamConfig{
		Team:       "teamname",
		ReadTokens: []string{"read-token"},
	})

	moduleSvc := service.NewModuleService(st, testArtifactStorage{}, "modules", nil)
	privateServer := httptest.NewServer(newTestRouter(moduleSvc, nil, "http://example.test", authorizer, nil, "admin-token", false, defaultActiveReleaseTTL))
	t.Cleanup(privateServer.Close)
	publicServer := httptest.NewServer(newTestRouter(moduleSvc, nil, "http://example.test", authorizer, nil, "admin-token", true, defaultActiveReleaseTTL))
	t.Cleanup(publicServer.Close)

	resp, err := privateServer.Client().Get(privateServer.URL + "/api/v1/modules")
	if err != nil {
		t.Fatalf("GET private /api/v1/modules error = %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected private read without token to get 401, got %d", resp.StatusCode)
	}

	resp, err = publicServer.Client().Get(publicServer.URL + "/api/v1/modules")
	if err != nil {
		t.Fatalf("GET public /api/v1/modules error = %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected public read without token to get 200, got %d", resp.StatusCode)
	}
}

func TestHTTPAPIAccessMatrix(t *testing.T) {
	t.Parallel()

	authorizer := newAdminAuthorizer(t, auth.TeamConfig{
		Team:          "teamname",
		ReadTokens:    []string{"read-token"},
		PublishTokens: []string{"publish-token"},
		PublishOwners: []string{"teamname"},
	})

	tests := []struct {
		name   string
		method string
		path   string
		token  string
		want   int
	}{
		{name: "guest list private", method: http.MethodGet, path: "/api/v1/modules", want: http.StatusUnauthorized},
		{name: "read list private", method: http.MethodGet, path: "/api/v1/modules", token: "read-token", want: http.StatusOK},
		{name: "publish list private", method: http.MethodGet, path: "/api/v1/modules", token: "publish-token", want: http.StatusOK},
		{name: "admin list private", method: http.MethodGet, path: "/api/v1/modules", token: "admin-token", want: http.StatusOK},

		{name: "guest get module private", method: http.MethodGet, path: "/api/v1/modules/teamname/apache", want: http.StatusUnauthorized},
		{name: "read get module private", method: http.MethodGet, path: "/api/v1/modules/teamname/apache", token: "read-token", want: http.StatusOK},
		{name: "publish get module private", method: http.MethodGet, path: "/api/v1/modules/teamname/apache", token: "publish-token", want: http.StatusOK},
		{name: "admin get module private", method: http.MethodGet, path: "/api/v1/modules/teamname/apache", token: "admin-token", want: http.StatusOK},

		{name: "guest get release private", method: http.MethodGet, path: "/api/v1/modules/teamname/apache/versions/1.2.3", want: http.StatusUnauthorized},
		{name: "read get release private", method: http.MethodGet, path: "/api/v1/modules/teamname/apache/versions/1.2.3", token: "read-token", want: http.StatusOK},
		{name: "publish get release private", method: http.MethodGet, path: "/api/v1/modules/teamname/apache/versions/1.2.3", token: "publish-token", want: http.StatusOK},
		{name: "admin get release private", method: http.MethodGet, path: "/api/v1/modules/teamname/apache/versions/1.2.3", token: "admin-token", want: http.StatusOK},

		{name: "guest delete release", method: http.MethodDelete, path: "/api/v1/modules/teamname/apache/versions/1.2.3", want: http.StatusUnauthorized},
		{name: "read delete release", method: http.MethodDelete, path: "/api/v1/modules/teamname/apache/versions/1.2.3", token: "read-token", want: http.StatusForbidden},
		{name: "publish delete release", method: http.MethodDelete, path: "/api/v1/modules/teamname/apache/versions/1.2.3", token: "publish-token", want: http.StatusForbidden},
		{name: "admin delete release", method: http.MethodDelete, path: "/api/v1/modules/teamname/apache/versions/1.2.3", token: "admin-token", want: http.StatusOK},

		{name: "guest delete module", method: http.MethodDelete, path: "/api/v1/modules/teamname/apache", want: http.StatusUnauthorized},
		{name: "read delete module", method: http.MethodDelete, path: "/api/v1/modules/teamname/apache", token: "read-token", want: http.StatusForbidden},
		{name: "publish delete module", method: http.MethodDelete, path: "/api/v1/modules/teamname/apache", token: "publish-token", want: http.StatusForbidden},
		{name: "admin delete module", method: http.MethodDelete, path: "/api/v1/modules/teamname/apache", token: "admin-token", want: http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			st, server := newHTTPAPIAccessMatrixServer(t, authorizer)
			req, err := http.NewRequest(tt.method, server.URL+tt.path, nil)
			if err != nil {
				t.Fatalf("NewRequest() error = %v", err)
			}
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}

			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatalf("%s %s error = %v", tt.method, tt.path, err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tt.want {
				t.Fatalf("%s %s status = %d, want %d", tt.method, tt.path, resp.StatusCode, tt.want)
			}

			if tt.token != "admin-token" && (tt.method == http.MethodDelete) {
				if _, err := st.GetModule(context.Background(), "teamname", "apache"); err != nil {
					t.Fatalf("non-admin delete changed module: %v", err)
				}
			}
		})
	}

	t.Run("publish token cannot publish outside owner", func(t *testing.T) {
		t.Parallel()

		_, server := newHTTPAPIAccessMatrixServer(t, authorizer)
		archive, err := testutil.BuildTarGz(map[string]string{
			"alpha-apache-1.2.3/metadata.json": `{"name":"alpha-apache","version":"1.2.3"}`,
		})
		if err != nil {
			t.Fatalf("testutil.BuildTarGz() error = %v", err)
		}
		body, contentType := buildPublishMultipart(t, "alpha", "apache", "1.2.3", archive, "")
		req, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/modules", body)
		if err != nil {
			t.Fatalf("NewRequest(publish) error = %v", err)
		}
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Authorization", "Bearer publish-token")

		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("POST publish outside owner error = %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("publish outside owner status = %d, want %d", resp.StatusCode, http.StatusForbidden)
		}
	})
}

func TestManageActionsEnforceRoleBoundary(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)

	ctx := context.Background()
	createTeamnameApacheModuleAndRelease(t, st)

	authorizer := newAdminAuthorizer(t, auth.TeamConfig{
		Team:          "teamname",
		ReadTokens:    []string{"read-token"},
		PublishTokens: []string{"publish-token"},
		PublishOwners: []string{"teamname"},
	})

	moduleSvc := service.NewModuleService(st, testArtifactStorage{}, "modules", nil)
	server := httptest.NewServer(newTestRouter(moduleSvc, nil, "http://example.test", authorizer, nil, "admin-token", true, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)

	transport := server.Client().Transport
	noRedirectClient := &http.Client{Transport: transport}
	noRedirectClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	resp, err := noRedirectClient.PostForm(server.URL+"/manage/modules/teamname/apache/delete", nil)
	if err != nil {
		t.Fatalf("guest POST manage delete error = %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/manage/login" {
		t.Fatalf("expected guest manage delete to redirect to login, got %d location=%s", resp.StatusCode, resp.Header.Get("Location"))
	}

	readClient := &http.Client{Transport: transport}
	readJar := newCookieJar(t)
	readClient.Jar = readJar
	resp, err = readClient.PostForm(server.URL+"/manage/login", url.Values{"token": {"read-token"}})
	if err != nil {
		t.Fatalf("read token POST /manage/login error = %v", err)
	}
	readBody, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("ReadAll(read login response) error = %v", err)
	}
	if !strings.Contains(string(readBody), "publish or admin token required") {
		t.Fatalf("expected read token login to be rejected, got body:\n%s", string(readBody))
	}

	publishClient := &http.Client{Transport: transport}
	publishJar := newCookieJar(t)
	publishClient.Jar = publishJar
	postManageToken(t, publishClient, server.URL, "publish-token")

	body := getBody(t, publishClient, server.URL+"/manage/modules")
	if strings.Contains(body, "/manage/modules/teamname/apache/delete") || strings.Contains(body, "/manage/modules/teamname/apache/versions/1.2.3/delete") {
		t.Fatalf("publisher manage page exposes delete actions:\n%s", body)
	}

	resp, err = publishClient.PostForm(server.URL+"/manage/modules/teamname/apache/delete", manageFormValues(t, publishClient, server.URL, nil))
	if err != nil {
		t.Fatalf("publisher POST manage module delete error = %v", err)
	}
	publishBody, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("ReadAll(publisher delete response) error = %v", err)
	}
	if !strings.Contains(string(publishBody), "admin or team admin access required") {
		t.Fatalf("expected publisher module delete to be rejected, got body:\n%s", string(publishBody))
	}
	if _, err := st.GetModule(ctx, "teamname", "apache"); err != nil {
		t.Fatalf("publisher delete removed module or lookup failed: %v", err)
	}

	resp, err = publishClient.PostForm(server.URL+"/manage/upstream", manageFormValues(t, publishClient, server.URL, url.Values{"module": {"puppetlabs/apache"}}))
	if err != nil {
		t.Fatalf("publisher POST manage upstream error = %v", err)
	}
	upstreamBody, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("ReadAll(publisher upstream response) error = %v", err)
	}
	if !strings.Contains(string(upstreamBody), "global admin access required") {
		t.Fatalf("expected publisher upstream add to be rejected, got body:\n%s", string(upstreamBody))
	}

	archive, err := testutil.BuildTarGz(map[string]string{
		"teamname-nginx-1.0.0/metadata.json": `{"name":"teamname-nginx","version":"1.0.0"}`,
	})
	if err != nil {
		t.Fatalf("testutil.BuildTarGz() error = %v", err)
	}
	csrfToken := manageCSRFToken(t, publishClient, server.URL)
	publishBodyBuffer, contentType := buildPublishMultipart(t, "teamname", "nginx", "1.0.0", archive, csrfToken)
	req, err := http.NewRequest(http.MethodPost, server.URL+"/manage/modules", publishBodyBuffer)
	if err != nil {
		t.Fatalf("NewRequest(manage publish) error = %v", err)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err = publishClient.Do(req)
	if err != nil {
		t.Fatalf("publisher POST manage publish error = %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected publisher manage publish final response 200, got %d", resp.StatusCode)
	}
	if _, err := st.GetModule(ctx, "teamname", "nginx"); err != nil {
		t.Fatalf("publisher manage publish did not create module: %v", err)
	}

	adminClient := &http.Client{Transport: transport}
	adminJar := newCookieJar(t)
	adminClient.Jar = adminJar
	postManageToken(t, adminClient, server.URL, "admin-token")
	resp, err = adminClient.PostForm(server.URL+"/manage/modules/teamname/apache/delete", manageFormValues(t, adminClient, server.URL, nil))
	if err != nil {
		t.Fatalf("admin POST manage module delete error = %v", err)
	}
	adminBody, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("ReadAll(admin delete response) error = %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected admin manage module delete final response 200, got %d", resp.StatusCode)
	}
	if !strings.Contains(string(adminBody), "module deleted") {
		t.Fatalf("expected admin module delete to succeed, got body:\n%s", string(adminBody))
	}
	if _, err := st.GetModule(ctx, "teamname", "apache"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetModule() error = %v, want ErrNotFound", err)
	}
}

func TestListModulesPaginationMetadata(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newHTTPAPITestStore(t)
	createModuleRelease(t, st, "teamname", "alpha", "1.0.0")
	createModuleRelease(t, st, "teamname", "bravo", "1.0.0")
	createModuleRelease(t, st, "teamname", "charlie", "1.0.0")

	authorizer := newAdminAuthorizer(t, auth.TeamConfig{
		Team:       "teamname",
		ReadTokens: []string{"read-token"},
	})
	server := httptest.NewServer(newTestRouter(service.NewModuleService(st, testArtifactStorage{}, "modules", nil), nil, "http://example.test", authorizer, nil, "admin-token", false, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/v1/modules?limit=2&offset=1", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer read-token")

	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/modules error = %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /api/v1/modules status = %d, want 200: %s", resp.StatusCode, string(body))
	}

	var payload struct {
		Items  []domain.Module `json:"items"`
		Limit  int             `json:"limit"`
		Offset int             `json:"offset"`
		Total  int             `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if payload.Limit != 2 || payload.Offset != 1 || payload.Total != 3 {
		t.Fatalf("unexpected pagination metadata: %#v", payload)
	}
	if len(payload.Items) != 2 {
		t.Fatalf("items len = %d, want 2", len(payload.Items))
	}
}

func TestListModulesRejectsUnsafePagination(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)
	authorizer := newAdminAuthorizer(t, auth.TeamConfig{
		Team:       "teamname",
		ReadTokens: []string{"read-token"},
	})
	server := httptest.NewServer(newTestRouter(service.NewModuleService(st, testArtifactStorage{}, "modules", nil), nil, "http://example.test", authorizer, nil, "admin-token", false, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)

	for _, query := range []string{
		"limit=101",
		"limit=0",
		"offset=-1",
		"offset=100001",
		"offset=999999999999999999999999",
	} {
		req, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/modules?"+query, nil)
		if err != nil {
			t.Fatalf("NewRequest(%q) error = %v", query, err)
		}
		req.Header.Set("Authorization", "Bearer read-token")
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("GET modules with %q error = %v", query, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET modules with %q status = %d, want 400", query, resp.StatusCode)
		}
	}
}

func TestRequestedManagePageRejectsUnsafeValues(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"0", "-1", "2002", "999999999999999999999999"} {
		req := httptest.NewRequest(http.MethodGet, "/manage?page="+raw, nil)
		if _, err := requestedManagePageWithSize(req, manageModulePageSize); err == nil {
			t.Errorf("requestedManagePageWithSize(%q) error = nil", raw)
		}
	}
}

func TestRequestedManageModulePageUsesListSpecificPageSizeCookie(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		action   string
		target   string
		pageSize string
		want     int
	}{
		{name: "global module list", action: "/manage/modules", target: manageModuleListTarget, pageSize: "20", want: 20},
		{name: "team module list", action: "/manage/teams/teamname/modules", target: manageTeamModuleListTarget, pageSize: "100", want: 100},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodGet, tc.action, nil)
			storageKey := pageSizeStorageKey(tc.action, "per_page", tc.target)
			req.AddCookie(&http.Cookie{Name: pageSizeCookieName(storageKey), Value: tc.pageSize})
			page, pageSize, err := requestedManageModulePage(req, tc.action, tc.target)
			if err != nil {
				t.Fatalf("requestedManageModulePage() error = %v", err)
			}
			if page != 1 || pageSize != tc.want {
				t.Fatalf("requestedManageModulePage() = (%d, %d), want (1, %d)", page, pageSize, tc.want)
			}
		})
	}
}

func TestLoadManageModuleRowsPaginatesFilteredStoreResults(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newHTTPAPITestStore(t)
	for i := range 55 {
		name := fmt.Sprintf("module-%02d", i)
		module, err := st.UpsertModule(ctx, "teamname", name)
		if err != nil {
			t.Fatalf("UpsertModule(%s) error = %v", name, err)
		}
		if _, err := st.CreateRelease(ctx, domain.Release{
			ID:          module.ID + ":1.0.0",
			ModuleID:    module.ID,
			Owner:       "teamname",
			Name:        name,
			Source:      "local",
			Version:     "1.0.0",
			FileName:    "teamname-" + name + "-1.0.0.tar.gz",
			ContentType: "application/gzip",
			Metadata:    map[string]any{},
		}); err != nil {
			t.Fatalf("CreateRelease(%s) error = %v", name, err)
		}
	}

	router := &Router{
		modules:          service.NewModuleService(st, testArtifactStorage{}, "modules", nil),
		activeReleaseTTL: defaultActiveReleaseTTL,
	}
	principal := auth.Principal{
		Team:          "teamname",
		CanRead:       true,
		CanPublish:    true,
		PublishOwners: map[string]struct{}{"teamname": {}},
	}

	first, total, err := router.loadManageModuleRows(ctx, principal, nil, "teamname/module-", 1, manageModulePageSize)
	if err != nil {
		t.Fatalf("loadManageModuleRows(first) error = %v", err)
	}
	if total != 55 || len(first) != manageModulePageSize {
		t.Fatalf("first manage page rows = %d, total = %d", len(first), total)
	}
	if first[0].ReleaseCount != 1 || len(first[0].Versions) != 0 || first[0].VersionsLoaded {
		t.Fatalf("initial manage row eagerly loaded releases: %#v", first[0])
	}
	card, err := router.loadManageModuleCard(ctx, principal, first[0].Module.Owner, first[0].Module.Name)
	if err != nil {
		t.Fatalf("loadManageModuleCard() error = %v", err)
	}
	if !card.VersionsLoaded || card.ReleaseCount != 1 || len(card.Versions) != 1 {
		t.Fatalf("loaded manage module card = %#v", card)
	}

	second, total, err := router.loadManageModuleRows(ctx, principal, nil, "teamname/module-", 2, manageModulePageSize)
	if err != nil {
		t.Fatalf("loadManageModuleRows(second) error = %v", err)
	}
	if total != 55 || len(second) != 5 {
		t.Fatalf("second manage page rows = %d, total = %d", len(second), total)
	}
}

func TestManagePaginationPreservesSearchQuery(t *testing.T) {
	t.Parallel()

	query := url.Values{"q": {"teamname/web server"}}
	pagination := managePaginationForRequest("/manage/modules", query, "", "", 2, manageModulePageSize, 120)
	if !pagination.HasPrev || !pagination.HasNext || pagination.TotalPages != 3 {
		t.Fatalf("unexpected pagination metadata: %#v", pagination)
	}
	if pagination.PrevURL != "/manage/modules?q=teamname%2Fweb+server" {
		t.Fatalf("PrevURL = %q", pagination.PrevURL)
	}
	if pagination.NextURL != "/manage/modules?page=3&q=teamname%2Fweb+server" {
		t.Fatalf("NextURL = %q", pagination.NextURL)
	}
}

func TestAsyncManageMutationSuccessReturnsRefreshContract(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodPost, "/manage/modules", nil)
	req.Header.Set("X-Puppet-Forge-Async-Mutation", "true")
	rec := httptest.NewRecorder()
	respondManageMutationSuccess(rec, req, "/manage/modules", "module published", manageModuleListTarget, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if got := rec.Header().Get("X-Puppet-Forge-Refresh"); got != manageModuleListTarget {
		t.Fatalf("X-Puppet-Forge-Refresh = %q", got)
	}
	if got := rec.Header().Get("X-Puppet-Forge-Message"); got != "module published" {
		t.Fatalf("X-Puppet-Forge-Message = %q", got)
	}
}

func TestAsyncManageMutationErrorReturnsJSONInsteadOfRedirect(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodPost, "/manage/modules", nil)
	req.Header.Set("X-Puppet-Forge-Async-Mutation", "true")
	rec := httptest.NewRecorder()
	respondManageMutationHTTPError(rec, req, http.StatusForbidden, errors.New("space access required"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if location := rec.Header().Get("Location"); location != "" {
		t.Fatalf("unexpected redirect location %q", location)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"error":"space access required"`) {
		t.Fatalf("response body = %s", body)
	}
}

func TestManagePostRequiresCSRFToken(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)

	ctx := context.Background()
	server, client := setupManageTestWithTeamnameApacheModule(t, st, true)

	resp, err := client.PostForm(server.URL+"/manage/modules/teamname/apache/delete", nil)
	if err != nil {
		t.Fatalf("POST manage delete without csrf error = %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("ReadAll(csrf response) error = %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected missing csrf to get 403, got %d body=%s", resp.StatusCode, string(body))
	}
	if _, err := st.GetModule(ctx, "teamname", "apache"); err != nil {
		t.Fatalf("module was changed after csrf rejection: %v", err)
	}

	resp, err = client.PostForm(server.URL+"/manage/modules/teamname/apache/delete", url.Values{"csrf_token": {"wrong-token"}})
	if err != nil {
		t.Fatalf("POST manage delete with invalid csrf error = %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected invalid csrf to get 403, got %d", resp.StatusCode)
	}

	form := manageFormValues(t, client, server.URL, nil)
	crossOriginReq, err := http.NewRequest(http.MethodPost, server.URL+"/manage/modules/teamname/apache/delete", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("NewRequest(cross-origin) error = %v", err)
	}
	crossOriginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	crossOriginReq.Header.Set("Origin", "https://attacker.example.com")
	crossOriginReq.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err = client.Do(crossOriginReq)
	if err != nil {
		t.Fatalf("POST cross-origin manage delete error = %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected cross-origin request to get 403, got %d", resp.StatusCode)
	}
	if _, err := st.GetModule(ctx, "teamname", "apache"); err != nil {
		t.Fatalf("module was changed after cross-origin rejection: %v", err)
	}

	resp, err = client.PostForm(server.URL+"/manage/modules/teamname/apache/delete", manageFormValues(t, client, server.URL, nil))
	if err != nil {
		t.Fatalf("POST manage delete with csrf error = %v", err)
	}
	body, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("ReadAll(csrf protected delete response) error = %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected csrf-protected delete final response 200, got %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "module deleted") {
		t.Fatalf("expected csrf-protected delete to succeed, got body:\n%s", string(body))
	}
	if _, err := st.GetModule(ctx, "teamname", "apache"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetModule() error = %v, want ErrNotFound", err)
	}
}

func TestManageRequestSourceCompatibleWithCSRF(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		target     string
		host       string
		origin     string
		fetchSite  string
		publicBase string
		allowed    []string
		want       bool
	}{
		{name: "direct HTTP", target: "http://forge.example/manage", origin: "http://forge.example", want: true},
		{name: "direct HTTPS", target: "https://forge.example/manage", origin: "https://forge.example", want: true},
		{name: "TLS terminated before service", target: "http://forge.example/manage", origin: "https://forge.example", want: true},
		{name: "HTTPS downgrade rejected", target: "https://forge.example/manage", origin: "http://forge.example", want: false},
		{name: "ingress alias accepted when public host is allowed", target: "http://internal:8080/manage", origin: "https://forge.example", allowed: []string{"forge.example"}, want: true},
		{name: "unconfigured ingress alias rejected", target: "http://internal:8080/manage", origin: "https://forge.example", want: false},
		{name: "browser confirmed same origin", target: "http://internal:8080/manage", origin: "http://forge.example", fetchSite: "same-origin", want: true},
		{name: "browser reported cross site", target: "http://forge.example/manage", origin: "http://forge.example", fetchSite: "cross-site", want: false},
		{name: "opaque browser origin relies on csrf token", target: "http://forge.example/manage", origin: "null", want: true},
		{name: "opaque cross-site origin rejected", target: "http://forge.example/manage", origin: "null", fetchSite: "cross-site", want: false},
		{name: "browser same origin survives proxy scheme mismatch", target: "https://internal/manage", origin: "http://forge.example", fetchSite: "same-origin", want: true},
		{name: "public port alias accepted when public host is allowed", target: "http://forge.example:8080/manage", origin: "https://forge.example", allowed: []string{"forge.example"}, want: true},
		{name: "non HTTP origin rejected", target: "http://forge.example/manage", origin: "file://forge.example", want: false},
		{name: "non HTTP origin rejected despite fetch metadata", target: "http://internal/manage", origin: "file://forge.example", fetchSite: "same-origin", want: false},
		{name: "preserved ingress host", target: "http://service.namespace.svc/manage", host: "forge.example", origin: "https://forge.example", want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodPost, tc.target, nil)
			if tc.host != "" {
				req.Host = tc.host
			}
			req.Header.Set("Origin", tc.origin)
			if tc.fetchSite != "" {
				req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			}
			if got := manageRequestSourceCompatibleWithCSRF(req, tc.publicBase, tc.allowed); got != tc.want {
				t.Fatalf("manageRequestSourceCompatibleWithCSRF() = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestCheckManageRequestSourceForCSRFReportsSafeReason(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodPost, "https://forge.example/manage/access/token", nil)
	req.Header.Set("Origin", "http://forge.example/private/path?token=secret")

	allowed, reason := checkManageRequestSourceForCSRF(req, "")
	if allowed || reason != "source_scheme_downgrade" {
		t.Fatalf("checkManageRequestSourceForCSRF() = (%t, %q), want (false, %q)", allowed, reason, "source_scheme_downgrade")
	}
	if got := csrfLogOrigin(req.Header.Get("Origin")); got != "http://forge.example" {
		t.Fatalf("csrfLogOrigin() = %q, want origin without path or query", got)
	}
	if got := csrfLogOrigin("null"); got != "opaque" {
		t.Fatalf("csrfLogOrigin(null) = %q, want opaque", got)
	}
}

func TestReadOnlyPrincipalsCannotWriteOrDeleteHTTPAPI(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)

	authorizer := newAdminAuthorizer(t, auth.TeamConfig{
		Team:          "teamname",
		ReadTokens:    []string{"read-token"},
		PublishTokens: []string{"publish-token"},
		PublishOwners: []string{"teamname"},
	})

	archive, err := testutil.BuildTarGz(map[string]string{
		"teamname-apache-1.2.3/metadata.json": `{"name":"teamname-apache","version":"1.2.3"}`,
	})
	if err != nil {
		t.Fatalf("testutil.BuildTarGz() error = %v", err)
	}

	moduleSvc := service.NewModuleService(st, testArtifactStorage{}, "modules", nil)
	server := httptest.NewServer(newTestRouter(moduleSvc, nil, "http://example.test", authorizer, nil, "admin-token", true, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)

	postPublish := func(t *testing.T, token string, archive []byte) *http.Response {
		t.Helper()

		body, contentType := buildPublishMultipart(t, "teamname", "apache", "1.2.3", archive, "")
		req, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/modules", body)
		if err != nil {
			t.Fatalf("NewRequest(publish) error = %v", err)
		}
		req.Header.Set("Content-Type", contentType)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}

		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("POST /api/v1/modules error = %v", err)
		}
		return resp
	}

	resp := postPublish(t, "", archive)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected guest publish to get 401, got %d", resp.StatusCode)
	}

	resp = postPublish(t, "read-token", archive)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected read token publish to get 401, got %d", resp.StatusCode)
	}

	resp = postPublish(t, "publish-token", archive)
	bodyBytes, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("ReadAll(publish response) error = %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected publish token publish to get 201, got %d body=%s", resp.StatusCode, string(bodyBytes))
	}
	conflictingArchive, err := testutil.BuildTarGz(map[string]string{
		"teamname-apache-1.2.3/metadata.json": `{"name":"teamname-apache","version":"1.2.3"}`,
		"teamname-apache-1.2.3/changed.txt":   "different release bytes",
	})
	if err != nil {
		t.Fatalf("testutil.BuildTarGz(conflict) error = %v", err)
	}
	resp = postPublish(t, "publish-token", conflictingArchive)
	bodyBytes, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("ReadAll(conflicting publish response) error = %v", err)
	}
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected conflicting publish to get 409, got %d body=%s", resp.StatusCode, string(bodyBytes))
	}
	module, err := st.GetModule(context.Background(), "teamname", "apache")
	if err != nil {
		t.Fatalf("GetModule(after publish) error = %v", err)
	}
	createTeamnameApacheRelease(t, st, module, "2.0.0")

	deletePaths := []string{
		"/api/v1/modules/teamname/apache/versions/1.2.3",
		"/api/v1/modules/teamname/apache",
	}
	for _, path := range deletePaths {
		req, err := http.NewRequest(http.MethodDelete, server.URL+path, nil)
		if err != nil {
			t.Fatalf("NewRequest(guest delete %s) error = %v", path, err)
		}
		resp, err = server.Client().Do(req)
		if err != nil {
			t.Fatalf("guest DELETE %s error = %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected guest DELETE %s to get 401, got %d", path, resp.StatusCode)
		}

		req, err = http.NewRequest(http.MethodDelete, server.URL+path, nil)
		if err != nil {
			t.Fatalf("NewRequest(read delete %s) error = %v", path, err)
		}
		req.Header.Set("Authorization", "Bearer read-token")
		resp, err = server.Client().Do(req)
		if err != nil {
			t.Fatalf("read token DELETE %s error = %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("expected read token DELETE %s to get 403, got %d", path, resp.StatusCode)
		}

		req, err = http.NewRequest(http.MethodDelete, server.URL+path, nil)
		if err != nil {
			t.Fatalf("NewRequest(publish delete %s) error = %v", path, err)
		}
		req.Header.Set("Authorization", "Bearer publish-token")
		resp, err = server.Client().Do(req)
		if err != nil {
			t.Fatalf("publish token DELETE %s error = %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("expected publish token DELETE %s to get 403, got %d", path, resp.StatusCode)
		}
	}

	req, err := http.NewRequest(http.MethodDelete, server.URL+"/api/v1/modules/teamname/apache/versions/1.2.3", nil)
	if err != nil {
		t.Fatalf("NewRequest(admin delete release) error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer admin-token")
	resp, err = server.Client().Do(req)
	if err != nil {
		t.Fatalf("admin DELETE release error = %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected admin DELETE release to get 200, got %d", resp.StatusCode)
	}
}

func TestPublishRejectsUploadOverConfiguredLimit(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)

	authorizer := newAdminAuthorizer(t, auth.TeamConfig{
		Team:          "teamname",
		PublishTokens: []string{"publish-token"},
		PublishOwners: []string{"teamname"},
	})

	archive, err := testutil.BuildTarGz(map[string]string{
		"teamname-apache-1.2.3/metadata.json": `{"name":"teamname-apache","version":"1.2.3"}`,
	})
	if err != nil {
		t.Fatalf("testutil.BuildTarGz() error = %v", err)
	}

	moduleSvc := service.NewModuleService(st, testArtifactStorage{}, "modules", nil)
	server := httptest.NewServer(newTestRouter(
		moduleSvc,
		nil,
		"http://example.test",
		authorizer,
		nil,
		"admin-token",
		true,
		defaultActiveReleaseTTL,
		WithModuleUploadMaxBytes(64),
	))
	t.Cleanup(server.Close)

	body, contentType := buildPublishMultipart(t, "teamname", "apache", "1.2.3", archive, "")
	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/modules", body)
	if err != nil {
		t.Fatalf("NewRequest(publish) error = %v", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer publish-token")

	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /api/v1/modules error = %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected upload over limit to get 413, got %d", resp.StatusCode)
	}
}

func TestOIDCDoesNotProtectPublicPages(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)

	authorizer := newAdminAuthorizer(t)
	moduleSvc := service.NewModuleService(st, testArtifactStorage{}, "modules", nil)
	server := httptest.NewServer(newTestRouter(moduleSvc, nil, "http://example.test", authorizer, &webauth.OIDCAuth{}, "admin-token", false, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)

	noRedirectClient := server.Client()
	noRedirectClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	resp, err := noRedirectClient.Get(server.URL + "/")
	if err != nil {
		t.Fatalf("GET / error = %v", err)
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("ReadAll(/ response) error = %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected public / to get 200, got %d location=%s body=%s", resp.StatusCode, resp.Header.Get("Location"), string(bodyBytes))
	}
	if !strings.Contains(string(bodyBytes), "Manage") {
		t.Fatalf("expected public index page, got body:\n%s", string(bodyBytes))
	}

	resp, err = noRedirectClient.Get(server.URL + "/manage")
	if err != nil {
		t.Fatalf("GET /manage error = %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/manage/login" {
		t.Fatalf("expected /manage to redirect to manage login, got %d location=%s", resp.StatusCode, resp.Header.Get("Location"))
	}

	resp, err = noRedirectClient.Get(server.URL + "/manage/login")
	if err != nil {
		t.Fatalf("GET /manage/login error = %v", err)
	}
	bodyBytes, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("ReadAll(/manage/login response) error = %v", err)
	}
	body := string(bodyBytes)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Sign in with OIDC") || !strings.Contains(body, "Publish or admin token") {
		t.Fatalf("expected manage login page with oidc and token options, got %d body:\n%s", resp.StatusCode, body)
	}
}

func TestPublicModuleAccessControlsV3Proxy(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)

	authorizer := newAdminAuthorizer(t)
	forgeProxy := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"source": "proxy"})
	})
	moduleSvc := service.NewModuleService(st, testArtifactStorage{}, "modules", nil)
	privateServer := httptest.NewServer(newTestRouter(moduleSvc, forgeProxy, "http://example.test", authorizer, nil, "admin-token", false, defaultActiveReleaseTTL))
	t.Cleanup(privateServer.Close)
	publicServer := httptest.NewServer(newTestRouter(moduleSvc, forgeProxy, "http://example.test", authorizer, nil, "admin-token", true, defaultActiveReleaseTTL))
	t.Cleanup(publicServer.Close)

	resp, err := privateServer.Client().Get(privateServer.URL + "/v3/modules/puppetlabs-stdlib")
	if err != nil {
		t.Fatalf("GET private /v3/modules error = %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected private /v3 without token to get 401, got %d", resp.StatusCode)
	}

	resp, err = publicServer.Client().Get(publicServer.URL + "/v3/modules/puppetlabs-stdlib")
	if err != nil {
		t.Fatalf("GET public /v3/modules error = %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected public /v3 without token to get 200, got %d", resp.StatusCode)
	}
}

func TestPrivateModuleAccessKeepsHTMLCatalogPublicButProtectsInstallRoutes(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)

	createTeamnameApacheModuleAndRelease(t, st)

	authorizer := newAdminAuthorizer(t, auth.TeamConfig{
		Team:       "teamname",
		ReadTokens: []string{"read-token"},
	})
	forgeProxy := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"source": "proxy"})
	})
	moduleSvc := service.NewModuleService(st, fixedDownloadStorage{body: []byte("archive"), contentType: "application/gzip"}, "modules", nil)
	server := httptest.NewServer(newTestRouter(moduleSvc, forgeProxy, "http://example.test", authorizer, nil, "admin-token", false, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)

	for _, path := range []string{"/", "/modules/teamname/apache"} {
		resp, err := server.Client().Get(server.URL + path)
		if err != nil {
			t.Fatalf("GET %s error = %v", path, err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("ReadAll(%s) error = %v", path, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected informational HTML %s without token to get 200, got %d body=%s", path, resp.StatusCode, string(body))
		}
	}

	for _, path := range []string{
		"/api/v1/modules",
		"/api/v1/modules/teamname/apache",
		"/api/v1/modules/teamname/apache/versions/1.2.3/download",
		"/modules/teamname/apache/versions/1.2.3/files/metadata.json",
		"/v3/modules/teamname-apache",
	} {
		resp, err := server.Client().Get(server.URL + path)
		if err != nil {
			t.Fatalf("GET private install route %s without token error = %v", path, err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("ReadAll(%s) error = %v", path, err)
		}
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected private install route %s without token to get 401, got %d body=%s", path, resp.StatusCode, string(body))
		}
	}
}

func TestManageAdminCanManageStructuredAccessConfig(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)

	ctx := context.Background()
	if err := st.ReplaceTeamConfigs(ctx, []auth.TeamConfig{
		{
			Team:            "platform-admin",
			OIDCAdminGroups: []string{"forge-admins"},
		},
	}); err != nil {
		t.Fatalf("ReplaceTeamConfigs(seed) error = %v", err)
	}

	moduleSvc := service.NewModuleService(st, testArtifactStorage{}, "modules", nil)
	authorizer := newAdminAuthorizer(t, auth.TeamConfig{
		Team:            "platform-admin",
		OIDCAdminGroups: []string{"forge-admins"},
	})

	server := httptest.NewServer(newTestRouter(moduleSvc, nil, "http://example.test", authorizer, nil, "admin-token", false, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)

	jar := newCookieJar(t)
	client := server.Client()
	client.Jar = jar

	postManageToken(t, client, server.URL, "admin-token")
	body := getBody(t, client, server.URL+"/manage/admin/access")
	if !strings.Contains(body, "forge-admins") {
		t.Fatalf("expected current access config in page, got body:\n%s", body)
	}
	removedReplace, err := client.PostForm(server.URL+"/manage/access", manageFormValues(t, client, server.URL, url.Values{
		"action": {"replace_json"},
		"config": {`[{"team":"unexpected"}]`},
	}))
	if err != nil {
		t.Fatalf("POST removed JSON replacement action error = %v", err)
	}
	removedBody, err := io.ReadAll(removedReplace.Body)
	_ = removedReplace.Body.Close()
	if err != nil {
		t.Fatalf("read removed JSON replacement response: %v", err)
	}
	if !strings.Contains(string(removedBody), "unknown access form action") {
		t.Fatalf("removed JSON replacement action was not rejected:\n%s", removedBody)
	}
	unchanged, err := st.LoadTeamConfigs(ctx)
	if err != nil {
		t.Fatalf("LoadTeamConfigs() after removed JSON replacement: %v", err)
	}
	if len(unchanged) != 1 || unchanged[0].Team != "platform-admin" {
		t.Fatalf("removed JSON replacement changed access config: %#v", unchanged)
	}

	resp, err := client.PostForm(server.URL+"/manage/access", manageFormValues(t, client, server.URL, url.Values{
		"action":               {"save_team"},
		"team":                 {"alpha"},
		"publish_tokens":       {"ignored-manual-token\n"},
		"extra_publish_spaces": {"ignored-space"},
		"oidc_groups":          {"alpha-devops"},
	}))
	if err != nil {
		t.Fatalf("POST structured save /manage/access error = %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected final structured save response 200 after redirect, got %d", resp.StatusCode)
	}

	configs, err := st.LoadTeamConfigs(ctx)
	if err != nil {
		t.Fatalf("LoadTeamConfigs() after structured save error = %v", err)
	}
	foundAlpha := false
	for _, cfg := range configs {
		if cfg.Team == "alpha" {
			foundAlpha = true
			if len(cfg.PublishOwners) != 1 || cfg.PublishOwners[0] != "alpha" {
				t.Fatalf("unexpected alpha owners: %#v", cfg.PublishOwners)
			}
			if len(cfg.PublishTokenRecords) != 0 {
				t.Fatalf("structured team form created a token record: %#v", cfg.PublishTokenRecords)
			}
			if len(cfg.OIDCGroups) != 1 || cfg.OIDCGroups[0] != "alpha-devops" {
				t.Fatalf("unexpected alpha oidc groups: %#v", cfg.OIDCGroups)
			}
		}
	}
	if !foundAlpha {
		t.Fatalf("alpha config was not saved: %#v", configs)
	}

	resp, err = client.PostForm(server.URL+"/manage/access", manageFormValues(t, client, server.URL, url.Values{
		"action": {"delete_team"},
		"team":   {"alpha"},
	}))
	if err != nil {
		t.Fatalf("POST structured delete /manage/access error = %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected final structured delete response 200 after redirect, got %d", resp.StatusCode)
	}

	configs, err = st.LoadTeamConfigs(ctx)
	if err != nil {
		t.Fatalf("LoadTeamConfigs() after structured delete error = %v", err)
	}
	for _, cfg := range configs {
		if cfg.Team == "alpha" {
			t.Fatalf("alpha config was not deleted: %#v", configs)
		}
	}
}

func TestManageAccessRequiresAdmin(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)

	configs := []auth.TeamConfig{
		{
			Team:          "teamname",
			PublishTokens: []string{"teamname-token"},
			PublishOwners: []string{"teamname"},
		},
	}
	if err := st.ReplaceTeamConfigs(context.Background(), configs); err != nil {
		t.Fatalf("ReplaceTeamConfigs() error = %v", err)
	}
	authorizer := newAdminAuthorizer(t, configs...)

	server := httptest.NewServer(newTestRouter(service.NewModuleService(st, testArtifactStorage{}, "modules", nil), nil, "http://example.test", authorizer, nil, "admin-token", false, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)

	jar := newCookieJar(t)
	client := server.Client()
	client.Jar = jar

	postManageToken(t, client, server.URL, "teamname-token")
	resp, err := client.Get(server.URL + "/manage/admin/access")
	if err != nil {
		t.Fatalf("GET /manage/access error = %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected publisher to get 403, got %d", resp.StatusCode)
	}

}

func TestTeamAdminCanManageOnlyOwnTeamAccess(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)

	ctx := context.Background()
	configs := []auth.TeamConfig{
		{
			Team:                "teamname",
			ReadTokens:          []string{"old-read"},
			PublishTokens:       []string{"old-publish"},
			PublishOwners:       []string{"teamname", "shared"},
			OIDCGroups:          []string{"teamname-devops"},
			OIDCTeamAdminEmails: []string{"old-owner@example.com"},
			OIDCTeamAdminGroups: []string{"teamname-admins"},
		},
		{
			Team:          "alpha",
			ReadTokens:    []string{"alpha-read"},
			PublishTokens: []string{"alpha-publish"},
			PublishOwners: []string{"alpha"},
			OIDCGroups:    []string{"alpha-devops"},
		},
	}
	if err := st.ReplaceTeamConfigs(ctx, configs); err != nil {
		t.Fatalf("ReplaceTeamConfigs() error = %v", err)
	}

	router := &Router{modules: service.NewModuleService(st, testArtifactStorage{}, "modules", nil), tokenHasher: httpAPITestTokenHasher()}
	principal := auth.Principal{Team: "teamname", CanRead: true, CanPublish: true, CanManageTeam: true}

	req := formRequest(url.Values{
		"action":                 {"save_team"},
		"original_team":          {"teamname"},
		"team":                   {"teamname"},
		"read_tokens":            {"new-read"},
		"publish_tokens":         {"new-publish"},
		"extra_publish_spaces":   {"evil"},
		"oidc_groups":            {"teamname-publishers"},
		"oidc_team_admin_emails": {"owner@example.com\nbackup@example.com"},
		"oidc_team_admin_groups": {"teamname-admins\nteamname-owners"},
	})
	next, message, err := router.accessConfigsFromForm(req, principal)
	if err != nil {
		t.Fatalf("accessConfigsFromForm(team admin) error = %v", err)
	}
	if message != "team access saved" {
		t.Fatalf("unexpected save message: %s", message)
	}
	if err := router.saveAccessConfigs(ctx, next); err != nil {
		t.Fatalf("saveAccessConfigs() error = %v", err)
	}

	saved, err := st.LoadTeamConfigs(ctx)
	if err != nil {
		t.Fatalf("LoadTeamConfigs() error = %v", err)
	}
	teamname := findTeamConfig(saved, "teamname")
	if teamname == nil {
		t.Fatalf("teamname config missing: %#v", saved)
	}
	if len(teamname.ReadTokenRecords) != 1 || !strings.HasPrefix(teamname.ReadTokenRecords[0].Prefix, "legacy_read_") {
		t.Fatalf("teamname read token records were not preserved: %#v", teamname.ReadTokenRecords)
	}
	if len(teamname.PublishTokenRecords) != 1 || !strings.HasPrefix(teamname.PublishTokenRecords[0].Prefix, "legacy_publish_") {
		t.Fatalf("teamname publish token records were not preserved: %#v", teamname.PublishTokenRecords)
	}
	if len(teamname.PublishOwners) != 2 || !slices.Contains(teamname.PublishOwners, "teamname") || !slices.Contains(teamname.PublishOwners, "shared") {
		t.Fatalf("team admin changed publish owners: %#v", teamname.PublishOwners)
	}
	if len(teamname.OIDCGroups) != 1 || teamname.OIDCGroups[0] != "teamname-publishers" {
		t.Fatalf("unexpected teamname oidc groups: %#v", teamname.OIDCGroups)
	}
	if len(teamname.OIDCTeamAdminEmails) != 2 || !slices.Contains(teamname.OIDCTeamAdminEmails, "owner@example.com") || !slices.Contains(teamname.OIDCTeamAdminEmails, "backup@example.com") {
		t.Fatalf("unexpected teamname team admin emails: %#v", teamname.OIDCTeamAdminEmails)
	}
	if len(teamname.OIDCTeamAdminGroups) != 2 || teamname.OIDCTeamAdminGroups[1] != "teamname-owners" {
		t.Fatalf("unexpected teamname team admin groups: %#v", teamname.OIDCTeamAdminGroups)
	}
	alpha := findTeamConfig(saved, "alpha")
	if alpha == nil || len(alpha.ReadTokenRecords) != 1 || !strings.HasPrefix(alpha.ReadTokenRecords[0].Prefix, "legacy_read_") {
		t.Fatalf("alpha config was changed: %#v", saved)
	}

	if _, _, err := router.accessConfigsFromForm(formRequest(url.Values{
		"action": {"replace_json"},
		"config": {"[]"},
	}), principal); err == nil || !strings.Contains(err.Error(), "unknown access form action") {
		t.Fatalf("expected removed replace_json action to be rejected, got %v", err)
	}
	if _, _, err := router.accessConfigsFromForm(formRequest(url.Values{
		"action":        {"save_team"},
		"original_team": {"alpha"},
		"team":          {"alpha"},
	}), principal); err == nil || !strings.Contains(err.Error(), "team admins can edit only their own team") {
		t.Fatalf("expected foreign team save to be rejected, got %v", err)
	}
}

func TestTeamAdminCanManageMultipleOwnTeams(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)

	ctx := context.Background()
	configs := []auth.TeamConfig{
		{
			Team:                "teamname",
			ReadTokens:          []string{"teamname-read"},
			PublishTokens:       []string{"teamname-publish"},
			PublishOwners:       []string{"teamname"},
			OIDCTeamAdminGroups: []string{"platform-owners"},
		},
		{
			Team:                "alpha",
			ReadTokens:          []string{"alpha-read"},
			PublishTokens:       []string{"alpha-publish"},
			PublishOwners:       []string{"alpha", "shared"},
			OIDCTeamAdminGroups: []string{"platform-owners"},
		},
		{
			Team:          "oxygen",
			ReadTokens:    []string{"oxygen-read"},
			PublishTokens: []string{"oxygen-publish"},
			PublishOwners: []string{"oxygen"},
		},
	}
	if err := st.ReplaceTeamConfigs(ctx, configs); err != nil {
		t.Fatalf("ReplaceTeamConfigs() error = %v", err)
	}

	authorizer, err := auth.NewAuthorizer(configs)
	if err != nil {
		t.Fatalf("NewAuthorizer() error = %v", err)
	}
	principal, ok := authorizer.AuthenticateOIDC("", "", []string{"platform-owners"})
	if !ok {
		t.Fatal("expected platform-owners principal")
	}

	router := &Router{modules: service.NewModuleService(st, testArtifactStorage{}, "modules", nil), tokenHasher: httpAPITestTokenHasher()}

	next, message, err := router.accessConfigsFromForm(formRequest(url.Values{
		"action":                 {"save_team"},
		"original_team":          {"alpha"},
		"team":                   {"alpha"},
		"read_tokens":            {"alpha-read-new"},
		"publish_tokens":         {"alpha-publish-new"},
		"extra_publish_spaces":   {"evil"},
		"oidc_groups":            {"alpha-devops"},
		"oidc_team_admin_groups": {"platform-owners"},
	}), principal)
	if err != nil {
		t.Fatalf("accessConfigsFromForm(alpha team admin) error = %v", err)
	}
	if message != "team access saved" {
		t.Fatalf("unexpected save message: %s", message)
	}
	alpha := findTeamConfig(next, "alpha")
	if alpha == nil {
		t.Fatalf("alpha config missing: %#v", next)
	}
	if len(alpha.ReadTokenRecords) != 1 || !strings.HasPrefix(alpha.ReadTokenRecords[0].Prefix, "legacy_read_") {
		t.Fatalf("alpha read token record was not preserved: %#v", alpha.ReadTokenRecords)
	}
	if len(alpha.PublishOwners) != 2 || alpha.PublishOwners[0] != "alpha" || alpha.PublishOwners[1] != "shared" {
		t.Fatalf("team admin changed alpha publish owners: %#v", alpha.PublishOwners)
	}

	if _, _, err := router.accessConfigsFromForm(formRequest(url.Values{
		"action":        {"save_team"},
		"original_team": {"oxygen"},
		"team":          {"oxygen"},
	}), principal); err == nil || !strings.Contains(err.Error(), "team admins can edit only their own team") {
		t.Fatalf("expected unmanaged team save to be rejected, got %v", err)
	}
}

func TestManageAccessStructuredRenameTeam(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)

	ctx := context.Background()
	configs := []auth.TeamConfig{
		{
			Team:          "teamname",
			PublishTokens: []string{"teamname-token"},
			PublishOwners: []string{"teamname", "platform"},
			OIDCGroups:    []string{"teamname-devops"},
		},
	}
	client, serverURL := newAccessManageClient(t, st, ctx, configs)
	resp, err := client.PostForm(serverURL+"/manage/access", manageFormValues(t, client, serverURL, url.Values{
		"action":               {"save_team"},
		"original_team":        {"teamname"},
		"team":                 {"teamnameplatform"},
		"publish_tokens":       {"renamed-token"},
		"extra_publish_spaces": {"ignored-space"},
		"oidc_groups":          {"teamname-platform-devops"},
	}))
	if err != nil {
		t.Fatalf("POST rename /manage/access error = %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected final rename response 200 after redirect, got %d", resp.StatusCode)
	}

	saved, err := st.LoadTeamConfigs(ctx)
	if err != nil {
		t.Fatalf("LoadTeamConfigs() error = %v", err)
	}
	for _, cfg := range saved {
		if cfg.Team == "teamname" {
			t.Fatalf("old team name was not removed: %#v", saved)
		}
	}
	renamed := findTeamConfig(saved, "teamnameplatform")
	if renamed == nil {
		t.Fatalf("renamed team was not saved: %#v", saved)
	}
	if len(renamed.PublishOwners) != 2 || !slices.Contains(renamed.PublishOwners, "teamnameplatform") || !slices.Contains(renamed.PublishOwners, "platform") {
		t.Fatalf("unexpected renamed owners: %#v", renamed.PublishOwners)
	}
	if len(renamed.OIDCGroups) != 1 || renamed.OIDCGroups[0] != "teamname-platform-devops" {
		t.Fatalf("unexpected renamed oidc groups: %#v", renamed.OIDCGroups)
	}
}

func TestManageAccessStructuredSavePreservesRuntimeAdminToken(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)

	ctx := context.Background()
	configs := []auth.TeamConfig{
		{
			Team:            "platform-admin",
			OIDCAdminGroups: []string{"forge-admins"},
		},
	}
	client, serverURL := newAccessManageClient(t, st, ctx, configs)
	resp, err := client.PostForm(serverURL+"/manage/access", manageFormValues(t, client, serverURL, url.Values{
		"action":            {"save_global_admins"},
		"oidc_admin_groups": {"forge-admins\nforge-owners"},
	}))
	if err != nil {
		t.Fatalf("POST global admins /manage/access error = %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected final platform-admin save response 200 after redirect, got %d", resp.StatusCode)
	}

	saved, err := st.LoadTeamConfigs(ctx)
	if err != nil {
		t.Fatalf("LoadTeamConfigs() error = %v", err)
	}
	admin := findTeamConfig(saved, "platform-admin")
	if admin == nil {
		t.Fatalf("platform-admin config was not saved: %#v", saved)
	}
	postManageToken(t, client, serverURL, "admin-token")
	body := getBody(t, client, serverURL+"/manage/admin/access")
	if !strings.Contains(body, "Global Access") {
		t.Fatalf("runtime admin token stopped working after save, got body:\n%s", body)
	}
	if strings.Contains(body, "OIDC publish emails") || strings.Contains(body, "OIDC publish subjects") || strings.Contains(body, "OIDC publish domains") {
		t.Fatalf("team OIDC email/subject/domain fields should not be rendered, got body:\n%s", body)
	}
	if !strings.Contains(body, "Global Access") || !strings.Contains(body, "forge-owners") {
		t.Fatalf("global admin form was not rendered with saved groups, got body:\n%s", body)
	}
	if len(admin.OIDCAdminGroups) != 2 || admin.OIDCAdminGroups[1] != "forge-owners" {
		t.Fatalf("oidc admin groups were not updated: %#v", admin.OIDCAdminGroups)
	}
}

func TestManageAccessContainsOnlyGlobalControls(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)
	ctx := context.Background()
	configs := []auth.TeamConfig{
		{
			Team:          "teamname",
			ReadTokens:    []string{"teamname-read"},
			PublishTokens: []string{"teamname-publish"},
			PublishOwners: []string{"teamname"},
		},
	}
	if err := st.ReplaceTeamConfigs(ctx, configs); err != nil {
		t.Fatalf("ReplaceTeamConfigs() error = %v", err)
	}

	router := &Router{modules: service.NewModuleService(st, testArtifactStorage{}, "modules", nil)}
	rec := httptest.NewRecorder()
	router.renderManageAccess(rec, httptest.NewRequest(http.MethodGet, "/manage/admin/access", nil), auth.Principal{CanAdmin: true}, "")
	body := rec.Body.String()
	if !strings.Contains(body, "Global Access") || !strings.Contains(body, `href="/manage/admin/spaces"`) {
		t.Fatalf("global access page misses its navigation:\n%s", body)
	}
	for _, forbidden := range []string{`id="access-config-json"`, `team:teamname`, `OIDC team admins (emails)`, `>Expert</a>`} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("global access page exposes team or expert control %q:\n%s", forbidden, body)
		}
	}
}

func TestManageTeamSummariesRespectPrincipalScope(t *testing.T) {
	t.Parallel()

	configs := []auth.TeamConfig{
		{Team: "platform-admin", OIDCAdminGroups: []string{"forge-admins"}},
		{
			Team:                "teamname",
			ReadTokens:          []string{"read-token"},
			PublishTokens:       []string{"publish-token"},
			PublishOwners:       []string{"teamname", "shared"},
			OIDCGroups:          []string{"teamname-publishers"},
			OIDCTeamAdminEmails: []string{"owner@example.com"},
			OIDCTeamAdminGroups: []string{"teamname-admins"},
		},
		{Team: "alpha", PublishOwners: []string{"alpha"}},
	}
	moduleCounts := map[string]int{"teamname": 1, "shared": 1, "alpha": 1}
	principal := auth.Principal{
		Team:          "teamname",
		CanManageTeam: true,
		ManagedTeams:  map[string]struct{}{"teamname": {}},
	}

	rows := manageTeamSummaries(configs, moduleCounts, principal)
	if len(rows) != 1 || rows[0].Team != "teamname" {
		t.Fatalf("team admin sees unexpected teams: %#v", rows)
	}
	row := rows[0]
	if row.ModuleCount != 2 || row.ReadTokens != 1 || row.PublishTokens != 1 {
		t.Fatalf("unexpected team summary counts: %#v", row)
	}
	if row.PublishGroups != 1 || row.TeamAdminUsers != 1 || row.TeamAdminGroups != 1 {
		t.Fatalf("unexpected team OIDC summary counts: %#v", row)
	}
	if len(row.Spaces) != 2 || row.Spaces[0] != "shared" || row.Spaces[1] != "teamname" {
		t.Fatalf("unexpected sorted team spaces: %#v", row.Spaces)
	}
}

func TestManageTeamsPagesGroupAccessAndModules(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newHTTPAPITestStore(t)
	createModuleRelease(t, st, "teamname", "apache", "1.2.3")
	createModuleRelease(t, st, "shared", "stdlib", "2.0.0")
	configs := []auth.TeamConfig{
		{Team: "platform-admin", OIDCAdminGroups: []string{"forge-admins"}},
		{
			Team:          "teamname",
			ReadTokens:    []string{"read-token"},
			PublishTokens: []string{"publish-token"},
			PublishOwners: []string{"teamname", "shared"},
		},
	}
	client, serverURL := newAccessManageClient(t, st, ctx, configs)

	listBody := getBody(t, client, serverURL+"/manage/teams")
	if !strings.Contains(listBody, `href="/manage/teams/teamname/access"`) || !strings.Contains(listBody, "2</strong><span>modules") {
		t.Fatalf("teams page misses team summary or module count:\n%s", listBody)
	}
	if strings.Contains(listBody, `href="/manage/teams/platform-admin"`) {
		t.Fatalf("teams page exposes global admin pseudo-team:\n%s", listBody)
	}

	redirectClient := *client
	redirectClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	rootResponse, err := redirectClient.Get(serverURL + "/manage/teams/teamname")
	if err != nil {
		t.Fatalf("GET legacy team root: %v", err)
	}
	_ = rootResponse.Body.Close()
	if rootResponse.StatusCode != http.StatusFound || rootResponse.Header.Get("Location") != "/manage/teams/teamname/access" {
		t.Fatalf("legacy team root response = %d Location %q", rootResponse.StatusCode, rootResponse.Header.Get("Location"))
	}

	accessBody := getBody(t, client, serverURL+"/manage/teams/teamname/access")
	for _, want := range []string{
		`name="next" value="/manage/teams/teamname/access"`,
		`<span aria-current="page">Access</span>`,
		`id="team-access"`,
	} {
		if !strings.Contains(accessBody, want) {
			t.Fatalf("team access page misses %q:\n%s", want, accessBody)
		}
	}
	if strings.Contains(accessBody, `id="team-tokens"`) || strings.Contains(accessBody, `id="team-modules"`) {
		t.Fatalf("team access page exposes another section:\n%s", accessBody)
	}

	tokensBody := getBody(t, client, serverURL+"/manage/teams/teamname/tokens")
	for _, want := range []string{
		`action="/manage/access/token"`,
		`name="name" required`,
		`name="role" required`,
		`<option value="read">Read</option>`,
		`<option value="publish">Publish</option>`,
		`name="next" value="/manage/teams/teamname/tokens"`,
		`<span aria-current="page">Tokens</span>`,
	} {
		if !strings.Contains(tokensBody, want) {
			t.Fatalf("team tokens page misses %q:\n%s", want, tokensBody)
		}
	}
	if strings.Count(tokensBody, `class="token-create"`) != 1 {
		t.Fatalf("team tokens page must contain one token creation form:\n%s", tokensBody)
	}
	if strings.Contains(tokensBody, `id="team-access"`) || strings.Contains(tokensBody, `id="team-modules"`) {
		t.Fatalf("team tokens page exposes another section:\n%s", tokensBody)
	}

	modulesBody := getBody(t, client, serverURL+"/manage/teams/teamname/modules")
	for _, want := range []string{
		`teamname/apache`,
		`shared/stdlib`,
		`<span aria-current="page">Modules</span>`,
		`aria-label="Management context navigation"`,
	} {
		if !strings.Contains(modulesBody, want) {
			t.Fatalf("team modules page misses %q:\n%s", want, modulesBody)
		}
	}
	if strings.Contains(modulesBody, "Upload or Update") {
		t.Fatalf("global admin token without publish permission sees upload form:\n%s", modulesBody)
	}
	if strings.Contains(modulesBody, `id="team-access"`) || strings.Contains(modulesBody, `id="team-tokens"`) {
		t.Fatalf("team modules page exposes another section:\n%s", modulesBody)
	}
}

func TestManageReturnPathAllowsOnlyTeamManagementPages(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		next     string
		fallback string
		want     string
	}{
		{name: "team list", next: "/manage/teams", fallback: "/manage", want: "/manage/teams"},
		{name: "team detail", next: "/manage/teams/teamname/modules?q=apache", fallback: "/manage", want: "/manage/teams/teamname/modules?q=apache"},
		{name: "modules", next: "/manage/modules", fallback: "/manage", want: "/manage/modules"},
		{name: "global access", next: "/manage/admin/access", fallback: "/manage", want: "/manage/admin/access"},
		{name: "removed expert editor", next: "/manage/admin/expert", fallback: "/manage", want: "/manage"},
		{name: "external URL", next: "https://example.com/manage/teams/teamname", fallback: "/manage", want: "/manage"},
		{name: "scheme relative URL", next: "//example.com/manage/teams/teamname", fallback: "/manage", want: "/manage"},
		{name: "unrelated manage page", next: "/manage/access", fallback: "/manage", want: "/manage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := formRequest(url.Values{"next": {tc.next}})
			if got := manageReturnPath(req, tc.fallback); got != tc.want {
				t.Fatalf("manageReturnPath() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestManageAccessAllowsSharedOIDCGroupMapping(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)

	ctx := context.Background()
	configs := []auth.TeamConfig{
		{
			Team:       "teamname",
			OIDCGroups: []string{"teamname-devops"},
		},
	}
	client, serverURL := newAccessManageClient(t, st, ctx, configs)
	resp, err := client.PostForm(serverURL+"/manage/access", manageFormValues(t, client, serverURL, url.Values{
		"action":      {"save_team"},
		"team":        {"alpha"},
		"oidc_groups": {"TEAMNAME-DEVOPS"},
	}))
	if err != nil {
		t.Fatalf("POST duplicate oidc /manage/access error = %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if !strings.Contains(string(body), "team access saved") {
		t.Fatalf("expected shared OIDC group mapping to be saved, got body:\n%s", string(body))
	}

	saved, err := st.LoadTeamConfigs(ctx)
	if err != nil {
		t.Fatalf("LoadTeamConfigs() error = %v", err)
	}
	if findTeamConfig(saved, "alpha") == nil {
		t.Fatalf("alpha config was not persisted: %#v", saved)
	}
	authorizer, err := auth.NewAuthorizer(saved)
	if err != nil {
		t.Fatalf("NewAuthorizer() error = %v", err)
	}
	principal, ok := authorizer.AuthenticateOIDC("", "", []string{"teamname-devops"})
	if !ok || !principal.CanPublishOwner("teamname") || !principal.CanPublishOwner("alpha") {
		t.Fatalf("shared OIDC mapping did not grant both spaces: %#v ok=%v", principal, ok)
	}
}

func TestPublishSpacesEndpointReturnsOnlyPrincipalSpaces(t *testing.T) {
	t.Parallel()

	authorizer := newAdminAuthorizer(t, auth.TeamConfig{
		Team:          "teamname",
		PublishTokens: []string{"publish-token"},
		PublishOwners: []string{"shared"},
	})
	_, server := newHTTPAPIAccessMatrixServer(t, authorizer)
	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/manage/publish-spaces", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer publish-token")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET publish spaces error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("publish spaces status = %d", resp.StatusCode)
	}
	var payload struct {
		Spaces []string `json:"spaces"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if len(payload.Spaces) != 2 || payload.Spaces[0] != "shared" || payload.Spaces[1] != "teamname" {
		t.Fatalf("unexpected publish spaces: %#v", payload.Spaces)
	}
}

func TestIndexMenuUsesManageLabel(t *testing.T) {
	t.Parallel()

	router := &Router{}
	if label := router.indexAuthLabel(); label != "Manage" {
		t.Fatalf("unexpected auth label: %s", label)
	}
	if link := router.indexAuthLink(); link != "/manage" {
		t.Fatalf("unexpected auth link: %s", link)
	}

	router.webAuth = &webauth.OIDCAuth{}
	if link := router.indexAuthLink(); link != "/manage" {
		t.Fatalf("unexpected oidc auth link: %s", link)
	}
}

func TestIndexPageDoesNotExposeManualUpstreamLookup(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)

	moduleSvc := service.NewModuleService(st, testArtifactStorage{}, "modules", nil)
	server := httptest.NewServer(newTestRouter(moduleSvc, nil, "http://example.test", nil, nil, "", true, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)

	body := getBody(t, server.Client(), server.URL+"/")
	if strings.Contains(body, "Open upstream module") || strings.Contains(body, "upstream-lookup") || strings.Contains(body, "/manage/upstream") {
		t.Fatalf("index page exposes manual upstream lookup:\n%s", body)
	}
}

func TestModulePageDoesNotSyncMissingUpstreamModuleForGuest(t *testing.T) {
	t.Parallel()

	fx := newUpstreamPuppetlabsApache(t)

	server := httptest.NewServer(newTestRouter(fx.moduleSvc, fx.proxy.Handler(), "http://example.test", nil, nil, "", true, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)

	resp, err := server.Client().Get(server.URL + "/modules/puppetlabs/apache")
	if err != nil {
		t.Fatalf("GET /modules/puppetlabs/apache error = %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected guest module page to return 404, got %d", resp.StatusCode)
	}

	if _, err := fx.st.GetModule(context.Background(), "puppetlabs", "apache"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("guest module page created upstream module, err=%v", err)
	}
}

func TestManageAdminCanAddUpstreamModule(t *testing.T) {
	t.Parallel()

	fx := newUpstreamPuppetlabsApache(t)

	authorizer := newAdminAuthorizer(t)
	server := httptest.NewServer(newTestRouter(fx.moduleSvc, fx.proxy.Handler(), "http://example.test", authorizer, nil, "admin-token", true, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)

	jar := newCookieJar(t)
	client := &http.Client{Transport: server.Client().Transport, Jar: jar}
	postManageToken(t, client, server.URL, "admin-token")

	body := getBody(t, client, server.URL+"/manage/modules")
	if !strings.Contains(body, "/manage/upstream") || !strings.Contains(body, "Add Upstream Module") {
		t.Fatalf("manage page does not expose upstream add form for admin:\n%s", body)
	}
	if !strings.Contains(body, `<details class="panel-disclosure" data-section-key="add-upstream-module">`) {
		t.Fatalf("manage page upstream form is not collapsed by default:\n%s", body)
	}

	resp, err := client.PostForm(server.URL+"/manage/upstream", manageFormValues(t, client, server.URL, url.Values{"module": {"puppetlabs/apache"}}))
	if err != nil {
		t.Fatalf("POST /manage/upstream error = %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected final upstream add response 200 after redirect, got %d", resp.StatusCode)
	}

	module, err := fx.st.GetModule(context.Background(), "puppetlabs", "apache")
	if err != nil {
		t.Fatalf("GetModule(upstream) error = %v", err)
	}
	if module.LatestVersion != "1.0.0" {
		t.Fatalf("unexpected upstream latest version: %s", module.LatestVersion)
	}
}

func setupManageTest(t *testing.T, store store.Store, activeReleaseTTL time.Duration) (*httptest.Server, *http.Client) {
	t.Helper()

	return setupManageTestWithPublicAccess(t, store, activeReleaseTTL, false)
}

func setupManageTestWithPublicAccess(t *testing.T, store store.Store, activeReleaseTTL time.Duration, publicModuleAccess bool) (*httptest.Server, *http.Client) {
	t.Helper()

	moduleSvc := service.NewModuleService(store, testArtifactStorage{}, "modules", nil)
	authorizer := newAdminAuthorizer(t)
	server := httptest.NewServer(newTestRouter(moduleSvc, nil, "http://example.test", authorizer, nil, "admin-token", publicModuleAccess, activeReleaseTTL))
	t.Cleanup(server.Close)

	jar := newCookieJar(t)
	client := &http.Client{Transport: server.Client().Transport, Jar: jar}
	postManageToken(t, client, server.URL, "admin-token")

	return server, client
}

func setupManageTestWithModule(t *testing.T) (store.Store, *httptest.Server, *http.Client) {
	t.Helper()
	st := newHTTPAPITestStore(t)
	server, client := setupManageTestWithTeamnameApacheModule(t, st, false)
	return st, server, client
}

func setupManageTestWithTeamnameApacheModule(t *testing.T, st *store.SQLiteStore, publicModuleAccess bool) (*httptest.Server, *http.Client) {
	t.Helper()
	createTeamnameApacheModuleAndRelease(t, st)

	return setupManageTestWithPublicAccess(t, st, defaultActiveReleaseTTL, publicModuleAccess)
}

func TestManageModulesNavStaysInManage(t *testing.T) {
	t.Parallel()

	_, server, client := setupManageTestWithModule(t)

	body := getBody(t, client, server.URL+"/manage/modules")
	if !strings.Contains(body, `class="public-link" href="/">Public modules</a>`) {
		t.Fatalf("manage page does not link to public modules:\n%s", body)
	}
	if !strings.Contains(body, `<a href="/manage/modules" aria-current="page">Modules</a>`) {
		t.Fatalf("manage page modules nav does not point to /manage/modules:\n%s", body)
	}
	if !strings.Contains(body, `<a href="/manage/teams">Teams</a>`) {
		t.Fatalf("manage page does not link to teams:\n%s", body)
	}
	if !strings.Contains(body, `<summary>Administration</summary>`) || !strings.Contains(body, `href="/manage/admin/access"`) {
		t.Fatalf("manage page does not expose the separate administration group:\n%s", body)
	}
	if strings.Contains(body, `<a href="/">Modules</a>`) {
		t.Fatalf("manage page modules nav points to public index:\n%s", body)
	}
	if !strings.Contains(body, `id="item-teamname-apache"`) {
		t.Fatalf("manage module row does not stay in manage context:\n%s", body)
	}
	if strings.Contains(body, `href="/modules/teamname/apache"`) {
		t.Fatalf("manage module row points to public module page:\n%s", body)
	}
}

func TestManageOverviewIsReadOnly(t *testing.T) {
	t.Parallel()

	_, server, client := setupManageTestWithModule(t)
	body := getBody(t, client, server.URL+"/manage")
	for _, want := range []string{
		`<a href="/manage" aria-current="page">Overview</a>`,
		`<title>Overview</title>`,
		`<h1>Overview</h1>`,
		`href="/manage/modules">Manage modules</a>`,
		`href="/modules/teamname/apache"`,
		`href="/manage/teams"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("manage overview misses %q:\n%s", want, body)
		}
	}
	for _, forbidden := range []string{
		`aria-label="Breadcrumb"`,
		`action="/manage/modules"`,
		`/manage/modules/teamname/apache/delete`,
		`Add Upstream Module`,
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("manage overview exposes mutation control %q:\n%s", forbidden, body)
		}
	}
}

func TestManageOverviewLinksCountersTeamsAndSpaces(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newHTTPAPITestStore(t)
	createTeamnameApacheModuleAndRelease(t, st)
	client, serverURL := newAccessManageClient(t, st, ctx, []auth.TeamConfig{{
		Team:          "teamname",
		PublishOwners: []string{"shared"},
	}})

	body := getBody(t, client, serverURL+"/manage")
	for _, want := range []string{
		`grid-template-columns: repeat(2, minmax(0, 1fr));`,
		`class="summary-item" href="/manage/modules"`,
		`class="summary-item" href="/manage/admin/spaces"`,
		`class="summary-item" href="/manage/teams"`,
		`<span class="summary-value">2</span><span class="summary-label">Available spaces</span>`,
		`<a class="resource-link" href="/manage/teams/teamname/access">teamname</a>`,
		`<a class="count-link" href="/manage/teams/teamname/modules">1 module</a>`,
		`<section class="panel spaces-panel" id="spaces" data-async-list`,
		`<a class="resource-link" href="/manage/modules?q=shared%2F">shared</a>`,
		`<a class="count-link" href="/manage/modules?q=teamname%2F">1 module</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("manage overview misses linked resource %q:\n%s", want, body)
		}
	}
}

func TestManageOverviewPaginatesModulesWithTeamModulesFirst(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newHTTPAPITestStore(t)
	createTeamnameApacheModuleAndRelease(t, st)
	for i := range manageOverviewModulePageSize {
		createModuleRelease(t, st, "puppetlabs", fmt.Sprintf("upstream-%02d", i), "1.0.0")
	}
	client, serverURL := newAccessManageClient(t, st, ctx, []auth.TeamConfig{{Team: "teamname"}})

	body := getBody(t, client, serverURL+"/manage")
	if !strings.Contains(body, `href="/modules/teamname/apache"`) {
		t.Fatalf("first overview page omits the prioritized team-owned module:\n%s", body)
	}
	if strings.Count(body, `class="item-link"`) != manageOverviewModulePageSize {
		t.Fatalf("first overview page has an unexpected module count:\n%s", body)
	}
	if !strings.Contains(body, `21 modules · page 1 of 2`) || !strings.Contains(body, `href="/manage?modules_page=2#modules">Next</a>`) {
		t.Fatalf("first overview page misses module pagination:\n%s", body)
	}

	secondPage := getBody(t, client, serverURL+"/manage?modules_page=2")
	if strings.Contains(secondPage, `href="/modules/teamname/apache"`) || strings.Count(secondPage, `class="item-link"`) != 1 {
		t.Fatalf("second overview page has unexpected modules:\n%s", secondPage)
	}
	if !strings.Contains(secondPage, `href="/manage#modules">Previous</a>`) {
		t.Fatalf("second overview page misses its previous link:\n%s", secondPage)
	}
	for _, want := range []string{
		`id="modules" data-async-list`,
		`headers: { Accept: "text/html", "X-Puppet-Forge-Fragment": targetID }`,
		`const replacement = nextPage.getElementById(targetID)`,
		`event.stopImmediatePropagation()`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("overview misses asynchronous panel behavior %q:\n%s", want, body)
		}
	}
}

func TestManageOverviewPaginatesTeamsAndSpacesIndependently(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newHTTPAPITestStore(t)
	configs := make([]auth.TeamConfig, 0, manageOverviewTeamPageSize+1)
	for i := range manageOverviewTeamPageSize + 1 {
		configs = append(configs, auth.TeamConfig{Team: fmt.Sprintf("team%02d", i)})
	}
	client, serverURL := newAccessManageClient(t, st, ctx, configs)

	body := getBody(t, client, serverURL+"/manage?teams_page=2&spaces_page=2")
	for _, want := range []string{
		`11 teams · page 2 of 2`,
		`11 spaces · page 2 of 2`,
		`href="/manage?spaces_page=2#teams">Previous</a>`,
		`href="/manage?teams_page=2#spaces">Previous</a>`,
		`href="/manage/teams/team10/access">team10</a>`,
		`href="/manage/modules?q=team10%2F">team10</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("independent overview pagination misses %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `href="/manage/teams/team00/access">team00</a>`) {
		t.Fatalf("second team page still contains a first-page team:\n%s", body)
	}
}

func TestManageOverviewRedirectsOutOfRangePanelPage(t *testing.T) {
	t.Parallel()

	_, server, client := setupManageTestWithModule(t)
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	req, err := http.NewRequest(http.MethodGet, server.URL+"/manage?modules_page=2", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusSeeOther)
	}
	if location := resp.Header.Get("Location"); location != "/manage#modules" {
		t.Fatalf("Location = %q, want %q", location, "/manage#modules")
	}
}

func TestManageOverviewRejectsMalformedPanelPage(t *testing.T) {
	t.Parallel()

	_, server, client := setupManageTestWithModule(t)
	resp, err := client.Get(server.URL + "/manage?modules_page=invalid")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestManagePublishFormAcceptsOnlySpaceAndArchive(t *testing.T) {
	t.Parallel()

	var rec bytes.Buffer
	principal := auth.Principal{CanAdmin: true, CanPublish: true, CanManageTeam: true, PublishOwners: map[string]struct{}{"teamname": {}}}
	err := managePageTemplate.Execute(&rec, managePageData{
		Navigation: newManageNavigation(principal, "csrf", "modules", ""),
		Principal:  principal,
		Owners:     []string{"teamname"},
		CSRFToken:  "csrf",
	})
	if err != nil {
		t.Fatalf("managePageTemplate.Execute() error = %v", err)
	}
	body := rec.String()
	for _, want := range []string{
		`<details class="panel-disclosure" data-section-key="upload-or-update">`,
		`<label for="publish-space-select">Space</label>`,
		`<select id="publish-space-select" name="space" required>`,
		`<option value="teamname">teamname</option>`,
		`id="module-archive-input" name="file" type="file" accept=".gz,.tgz,.tar.gz" required`,
		`Module identity and metadata are read from metadata.json.`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("manage publish form missing expected helper %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `<details class="panel-disclosure" data-section-key="upload-or-update" open>`) {
		t.Fatalf("manage page upload form is open by default:\n%s", body)
	}
	for _, forbidden := range []string{
		`name="owner"`,
		`name="name"`,
		`name="version"`,
		`name="description"`,
		`name="metadata"`,
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("manage publish form marks optional field as browser-required %q:\n%s", forbidden, body)
		}
	}
}

func TestManageModuleDeleteButtonVisibleForDeletePrincipals(t *testing.T) {
	t.Parallel()

	module := domain.Module{Owner: "teamname", Name: "apache", LatestVersion: "2.0.0"}
	rows := []manageModuleRow{{
		Module: module,
		Versions: []manageVersionRow{
			{Version: "1.2.3", Active: true},
			{Version: "2.0.0", Latest: true},
		},
		CanDelete: true,
	}}
	for _, principal := range []auth.Principal{
		{CanAdmin: true},
		{Team: "teamname", CanPublish: true, CanManageTeam: true, ManagedTeams: map[string]struct{}{"teamname": {}}},
	} {
		var rec bytes.Buffer
		err := managePageTemplate.Execute(&rec, managePageData{
			Principal: principal,
			Modules:   rows,
			CSRFToken: "csrf",
		})
		if err != nil {
			t.Fatalf("managePageTemplate.Execute() error = %v", err)
		}
		body := rec.String()
		if !strings.Contains(body, `/manage/modules/teamname/apache/delete`) {
			t.Fatalf("manage page hides module delete action for %#v:\n%s", principal, body)
		}
		if !strings.Contains(body, `class="danger-button" type="submit" form="delete-module-teamname-apache">Delete module</button>`) {
			t.Fatalf("manage page does not render module delete as a danger button for %#v:\n%s", principal, body)
		}
		if !strings.Contains(body, `<form id="delete-module-teamname-apache" method="post" action="/manage/modules/teamname/apache/delete" hidden`) {
			t.Fatalf("manage page does not associate module delete button with an external form for %#v:\n%s", principal, body)
		}
		for _, wantStyle := range []string{
			`.module-details:hover`,
			`.module-details:focus-within`,
			`.module-details:has(.danger-button:hover)`,
			`.module-details:has(.danger-button:focus-visible)`,
			`.module-details > .release-list`,
		} {
			if !strings.Contains(body, wantStyle) {
				t.Fatalf("manage page does not render module block highlight style %q for %#v:\n%s", wantStyle, principal, body)
			}
		}
		if strings.Contains(body, `class="delete-form"`) || strings.Contains(body, `>delete module</button>`) {
			t.Fatalf("manage page renders legacy module delete control for %#v:\n%s", principal, body)
		}
		if strings.Contains(body, `/manage/modules/teamname/apache/versions/1.2.3/delete`) {
			t.Fatalf("manage page exposes active release delete action for %#v:\n%s", principal, body)
		}
		if strings.Contains(body, `/manage/modules/teamname/apache/versions/2.0.0/delete`) {
			t.Fatalf("manage page exposes latest release delete action for %#v:\n%s", principal, body)
		}
	}
}

func TestReadPublishInputReportsMissingArchive(t *testing.T) {
	t.Parallel()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("space", "teamname"); err != nil {
		t.Fatalf("WriteField(space) error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("multipart Close() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/modules", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	_, _, err := readPublishInput(httptest.NewRecorder(), req, 0)
	if err == nil || err.Error() != "artifact file is required" {
		t.Fatalf("readPublishInput() error = %v, want artifact file is required", err)
	}
}

func TestReadPublishInputStreamsLargeMultipartFileAndCleansItUp(t *testing.T) {
	t.Parallel()

	archive := bytes.Repeat([]byte("a"), (1<<20)+1)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("space", "teamname"); err != nil {
		t.Fatalf("WriteField(space) error = %v", err)
	}
	part, err := writer.CreateFormFile("file", "module.tar.gz")
	if err != nil {
		t.Fatalf("CreateFormFile() error = %v", err)
	}
	if _, err := part.Write(archive); err != nil {
		t.Fatalf("file Write() error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("multipart Close() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/modules", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	input, cleanup, err := readPublishInput(httptest.NewRecorder(), req, 0)
	if err != nil {
		t.Fatalf("readPublishInput() error = %v", err)
	}
	if len(input.FileBytes) != 0 || input.File == nil {
		t.Fatal("publish input buffered the multipart archive instead of returning its reader")
	}
	cleanup()
	if _, err := input.File.Seek(0, io.SeekStart); err == nil {
		t.Fatal("multipart file remained usable after cleanup")
	}
}

func TestReadPublishInputAcceptsArchiveAtConfiguredLimit(t *testing.T) {
	t.Parallel()

	archive := bytes.Repeat([]byte("a"), 1024)
	body, contentType := buildPublishMultipart(t, "teamname", "module", "1.0.0", archive, "")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/modules", body)
	req.ContentLength = -1
	req.Header.Set("Content-Type", contentType)

	input, cleanup, err := readPublishInput(httptest.NewRecorder(), req, int64(len(archive)))
	if err != nil {
		t.Fatalf("readPublishInput() error = %v", err)
	}
	defer cleanup()
	if input.SizeBytes != int64(len(archive)) {
		t.Fatalf("publish input size = %d, want %d", input.SizeBytes, len(archive))
	}
}

func TestReadPublishInputRejectsArchiveOneByteOverConfiguredLimit(t *testing.T) {
	t.Parallel()

	archive := bytes.Repeat([]byte("a"), 1025)
	body, contentType := buildPublishMultipart(t, "teamname", "module", "1.0.0", archive, "")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/modules", body)
	req.Header.Set("Content-Type", contentType)

	_, _, err := readPublishInput(httptest.NewRecorder(), req, 1024)
	if err == nil || !isRequestTooLarge(err) {
		t.Fatalf("readPublishInput() error = %v, want request-too-large error", err)
	}
}

func TestManageAccessNavLinks(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)
	router := &Router{modules: service.NewModuleService(st, testArtifactStorage{}, "modules", nil)}
	rec := httptest.NewRecorder()
	router.renderManageAccess(rec, httptest.NewRequest(http.MethodGet, "/manage/admin/access", nil), auth.Principal{CanAdmin: true}, "")
	body := rec.Body.String()

	for _, want := range []string{
		`class="public-link" href="/">Public modules</a>`,
		`<a href="/manage">Overview</a>`,
		`<a href="/manage/modules">Modules</a>`,
		`<a href="/manage/teams">Teams</a>`,
		`<details class="administration-menu active">`,
		`<summary>Administration</summary>`,
		`<a href="/manage/admin/spaces">Publish spaces</a>`,
		`<a href="/manage/admin/access" aria-current="page">Global access</a>`,
		`<form method="post" action="/manage/logout">`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("manage access page missing nav item %q:\n%s", want, body)
		}
	}
}

func TestGlobalAdminManagesPublishSpaceAssignments(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newHTTPAPITestStore(t)
	configs := []auth.TeamConfig{
		{
			Team:          "teamname",
			PublishTokens: []string{"team-publish-token"},
			PublishOwners: []string{"teamname"},
		},
		{
			Team:       "alpha",
			ReadTokens: []string{"alpha-read-token"},
		},
	}
	adminClient, serverURL := newAccessManageClient(t, st, ctx, configs)

	body := getBody(t, adminClient, serverURL+"/manage/admin/spaces")
	for _, want := range []string{
		`<a href="/manage/admin/spaces" aria-current="page">Publish spaces</a>`,
		`<option value="teamname">teamname</option>`,
		`<td><a href="/manage/teams/teamname/access">teamname</a></td>`,
		`<span class="badge primary">Primary</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("publish spaces page misses %q:\n%s", want, body)
		}
	}

	resp, err := adminClient.PostForm(serverURL+"/manage/admin/spaces", manageFormValues(t, adminClient, serverURL, url.Values{
		"action": {"assign"},
		"team":   {"teamname"},
		"space":  {"shared"},
	}))
	if err != nil {
		t.Fatalf("assign publish space error = %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("assign publish space final status = %d", resp.StatusCode)
	}

	saved, err := st.LoadTeamConfigs(ctx)
	if err != nil {
		t.Fatalf("LoadTeamConfigs(assign) error = %v", err)
	}
	team := findTeamConfig(saved, "teamname")
	if team == nil || !slices.Contains(team.PublishOwners, "shared") {
		t.Fatalf("shared publish space was not assigned: %#v", saved)
	}
	body = getBody(t, adminClient, serverURL+"/manage/admin/spaces")
	if !strings.Contains(body, `<input type="hidden" name="space" value="shared"><button class="remove-button" type="submit">Unassign</button>`) {
		t.Fatalf("assigned publish space has no unassign action:\n%s", body)
	}

	resp, err = adminClient.PostForm(serverURL+"/manage/admin/spaces", manageFormValues(t, adminClient, serverURL, url.Values{
		"action": {"unassign"},
		"team":   {"teamname"},
		"space":  {"shared"},
	}))
	if err != nil {
		t.Fatalf("unassign publish space error = %v", err)
	}
	_ = resp.Body.Close()
	saved, err = st.LoadTeamConfigs(ctx)
	if err != nil {
		t.Fatalf("LoadTeamConfigs(unassign) error = %v", err)
	}
	team = findTeamConfig(saved, "teamname")
	if team == nil || slices.Contains(team.PublishOwners, "shared") {
		t.Fatalf("shared publish space was not unassigned: %#v", saved)
	}

	publishClient := &http.Client{Transport: adminClient.Transport, Jar: newCookieJar(t)}
	postManageToken(t, publishClient, serverURL, "team-publish-token")
	resp, err = publishClient.Get(serverURL + "/manage/admin/spaces")
	if err != nil {
		t.Fatalf("publisher GET publish spaces error = %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("publisher GET publish spaces status = %d, want 403", resp.StatusCode)
	}

	router := &Router{modules: service.NewModuleService(st, testArtifactStorage{}, "modules", nil)}
	_, _, err = router.publishSpaceConfigsFromForm(formRequest(url.Values{
		"action": {"assign"},
		"team":   {"teamname"},
		"space":  {"shared-space"},
	}))
	if err == nil || !strings.Contains(err.Error(), "only ASCII letters and digits") {
		t.Fatalf("assigning invalid publish space error = %v", err)
	}

	_, _, err = router.publishSpaceConfigsFromForm(formRequest(url.Values{
		"action": {"assign"},
		"team":   {"teamname"},
		"space":  {"alpha"},
	}))
	if err == nil || !strings.Contains(err.Error(), "primary space of another team") {
		t.Fatalf("assigning another team's primary space error = %v", err)
	}

	if err := router.modules.IndexUpstreamModule(ctx, proxy.UpstreamModule{
		Slug:  "puppetlabs-stdlib",
		Owner: "puppetlabs",
		Name:  "stdlib",
		CurrentRelease: proxy.UpstreamReleaseRef{
			Slug:    "puppetlabs-stdlib-9.0.0",
			Version: "9.0.0",
		},
	}); err != nil {
		t.Fatalf("IndexUpstreamModule() error = %v", err)
	}
	_, _, err = router.publishSpaceConfigsFromForm(formRequest(url.Values{
		"action": {"assign"},
		"team":   {"teamname"},
		"space":  {"puppetlabs"},
	}))
	if err == nil || !strings.Contains(err.Error(), "belongs to Official Forge and is read only") {
		t.Fatalf("assigning Official Forge space error = %v", err)
	}
}

func TestSaveTeamCannotChangePublishSpaces(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)
	ctx := context.Background()
	configs := []auth.TeamConfig{{
		Team:          "teamname",
		PublishTokens: []string{"team-publish-token"},
		PublishOwners: []string{"teamname", "shared"},
	}}
	adminClient, serverURL := newAccessManageClient(t, st, ctx, configs)

	resp, err := adminClient.PostForm(serverURL+"/manage/access", manageFormValues(t, adminClient, serverURL, url.Values{
		"action":               {"save_team"},
		"original_team":        {"teamname"},
		"team":                 {"teamname"},
		"extra_publish_spaces": {"injected"},
	}))
	if err != nil {
		t.Fatalf("save team error = %v", err)
	}
	_ = resp.Body.Close()

	saved, err := st.LoadTeamConfigs(ctx)
	if err != nil {
		t.Fatalf("LoadTeamConfigs() error = %v", err)
	}
	team := findTeamConfig(saved, "teamname")
	if team == nil || !slices.Contains(team.PublishOwners, "shared") || slices.Contains(team.PublishOwners, "injected") {
		t.Fatalf("save_team changed publish spaces: %#v", saved)
	}

	body := getBody(t, adminClient, serverURL+"/manage/teams/teamname/access")
	if strings.Contains(body, `name="extra_publish_spaces"`) {
		t.Fatalf("team access page exposes editable extra publish spaces:\n%s", body)
	}
	if !strings.Contains(body, `href="/manage/admin/spaces">Manage publish spaces</a>`) {
		t.Fatalf("global admin team page misses publish-space management link:\n%s", body)
	}
}

func TestSaveTeamRejectsDuplicateCreateAndRename(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)
	ctx := context.Background()
	configs := []auth.TeamConfig{
		{Team: "teamname", PublishTokens: []string{"team-publish-token"}, PublishOwners: []string{"teamname", "shared"}},
		{Team: "alpha", ReadTokens: []string{"alpha-read-token"}, PublishOwners: []string{"alpha"}},
	}
	if err := st.ReplaceTeamConfigs(ctx, configs); err != nil {
		t.Fatalf("ReplaceTeamConfigs() error = %v", err)
	}
	router := &Router{modules: service.NewModuleService(st, testArtifactStorage{}, "modules", nil)}
	principal := auth.Principal{CanAdmin: true}

	for _, values := range []url.Values{
		{"action": {"save_team"}, "team": {"teamname"}},
		{"action": {"save_team"}, "original_team": {"teamname"}, "team": {"alpha"}},
	} {
		if _, _, err := router.accessConfigsFromForm(formRequest(values), principal); err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("duplicate team update error = %v", err)
		}
	}

	saved, err := st.LoadTeamConfigs(ctx)
	if err != nil {
		t.Fatalf("LoadTeamConfigs() error = %v", err)
	}
	team := findTeamConfig(saved, "teamname")
	if team == nil || !slices.Contains(team.PublishOwners, "shared") {
		t.Fatalf("duplicate update changed existing team: %#v", saved)
	}
}

func TestLegacyManagePageRoutesRedirectToCanonicalPages(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)
	server, client := setupManageTest(t, st, defaultActiveReleaseTTL)
	noRedirect := *client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	for oldPath, canonicalPath := range map[string]string{
		"/manage/access":     "/manage/admin/access",
		"/manage/access/add": "/manage/teams/new",
	} {
		resp, err := noRedirect.Get(server.URL + oldPath)
		if err != nil {
			t.Fatalf("GET %s error = %v", oldPath, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != canonicalPath {
			t.Fatalf("GET %s status=%d location=%q, want 302 to %s", oldPath, resp.StatusCode, resp.Header.Get("Location"), canonicalPath)
		}
	}
	for _, removedPath := range []string{"/manage/access/expert", "/manage/admin/expert"} {
		resp, err := noRedirect.Get(server.URL + removedPath)
		if err != nil {
			t.Fatalf("GET removed path %s error = %v", removedPath, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET removed path %s status=%d, want 404", removedPath, resp.StatusCode)
		}
	}
}

func TestManageAccessAddTeamNavLinks(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	principal := auth.Principal{CanAdmin: true}
	err := manageAccessAddTeamTemplate.Execute(rec, manageAccessAddTeamData{
		Navigation: newManageNavigation(principal, "csrf-token", "add-team", ""),
		CSRFToken:  "csrf-token",
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	body := rec.Body.String()

	for _, want := range []string{
		`class="public-link" href="/">Public modules</a>`,
		`<a href="/manage">Overview</a>`,
		`<a href="/manage/modules">Modules</a>`,
		`<a href="/manage/teams" aria-current="page">Teams</a>`,
		`<a href="/manage/teams/new" aria-current="page">Add team</a>`,
		`<form method="post" action="/manage/teams/new">`,
		`<input id="new-team" name="team" placeholder="Platform" pattern="[A-Za-z0-9]+" required>`,
		`<form method="post" action="/manage/logout">`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("add team page missing nav item %q:\n%s", want, body)
		}
	}
}

func TestManageAccessAddTeamKeepsValidationAndSuccessInTeamContext(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newHTTPAPITestStore(t)
	server, client := setupManageTest(t, st, defaultActiveReleaseTTL)
	noRedirect := *client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	resp, err := noRedirect.PostForm(server.URL+"/manage/teams/new", manageFormValues(t, client, server.URL, url.Values{
		"action": {"save_team"},
	}))
	if err != nil {
		t.Fatalf("POST empty add-team form error = %v", err)
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read empty add-team response: %v", readErr)
	}
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "<h1>Add Team</h1>") || !strings.Contains(string(body), "team is required") {
		t.Fatalf("empty add-team response status=%d body=%s", resp.StatusCode, string(body))
	}
	configs, err := st.LoadTeamConfigs(ctx)
	if err != nil {
		t.Fatalf("LoadTeamConfigs() after rejected create error = %v", err)
	}
	if len(configs) != 0 {
		t.Fatalf("rejected add-team form persisted configs: %#v", configs)
	}

	resp, err = noRedirect.PostForm(server.URL+"/manage/teams/new", manageFormValues(t, client, server.URL, url.Values{
		"action": {"save_team"},
		"team":   {"team-name"},
	}))
	if err != nil {
		t.Fatalf("POST invalid add-team form error = %v", err)
	}
	body, readErr = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read invalid add-team response: %v", readErr)
	}
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "team must contain only ASCII letters and digits") {
		t.Fatalf("invalid add-team response status=%d body=%s", resp.StatusCode, string(body))
	}

	resp, err = noRedirect.PostForm(server.URL+"/manage/teams/new", manageFormValues(t, client, server.URL, url.Values{
		"action": {"save_team"},
		"team":   {"teamname"},
	}))
	if err != nil {
		t.Fatalf("POST valid add-team form error = %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/manage/teams/teamname/access?message=team+access+saved" {
		t.Fatalf("valid add-team response status=%d location=%q", resp.StatusCode, resp.Header.Get("Location"))
	}
	configs, err = st.LoadTeamConfigs(ctx)
	if err != nil {
		t.Fatalf("LoadTeamConfigs() after create error = %v", err)
	}
	if len(configs) != 1 || configs[0].Team != "teamname" {
		t.Fatalf("valid add-team form persisted configs = %#v", configs)
	}

	resp, err = noRedirect.PostForm(server.URL+"/manage/teams/new", manageFormValues(t, client, server.URL, url.Values{
		"action": {"save_team"},
		"team":   {"TeamName"},
	}))
	if err != nil {
		t.Fatalf("POST case-duplicate add-team form error = %v", err)
	}
	body, readErr = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read case-duplicate add-team response: %v", readErr)
	}
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "teamname") || !strings.Contains(string(body), "already exists") {
		t.Fatalf("case-duplicate add-team response status=%d body=%s", resp.StatusCode, string(body))
	}
}

func TestManageModulesRemembersOpenSections(t *testing.T) {
	t.Parallel()

	_, server, client := setupManageTestWithModule(t)

	body := getBody(t, client, server.URL+"/manage/modules")
	for _, want := range []string{
		`data-section-key="module:teamname/apache"`,
		`puppet-forge:manage:open-sections`,
		`window.localStorage.setItem(storageKey`,
		`document.addEventListener("submit", saveOpenSections)`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("manage modules page missing persisted section state hook %q:\n%s", want, body)
		}
	}
}

func TestParseUpstreamModuleFormValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw       string
		wantOwner string
		wantName  string
		wantErr   bool
	}{
		{raw: "puppetlabs/apache", wantOwner: "puppetlabs", wantName: "apache"},
		{raw: "puppetlabs-apache", wantOwner: "puppetlabs", wantName: "apache"},
		{raw: " /puppetlabs/apache/ ", wantOwner: "puppetlabs", wantName: "apache"},
		{raw: "", wantErr: true},
		{raw: "puppetlabs", wantErr: true},
		{raw: "too/many/parts", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			owner, name, err := parseUpstreamModuleFormValue(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseUpstreamModuleFormValue() error = %v", err)
			}
			if owner != tt.wantOwner || name != tt.wantName {
				t.Fatalf("unexpected module identity: %s/%s", owner, name)
			}
		})
	}
}

func TestHTTPAPIHelpers(t *testing.T) {
	t.Parallel()

	if got := downloadPath("teamname", "apache", "1.2.3"); got != "/api/v1/modules/teamname/apache/versions/1.2.3/download" {
		t.Fatalf("unexpected download path: %s", got)
	}
	if got := downloadPath("teamname", "apache", ""); got != "" {
		t.Fatalf("expected empty download path, got %s", got)
	}
	if got := releaseAPIPath("teamname", "apache", "1.2.3"); got != "/api/v1/modules/teamname/apache/versions/1.2.3" {
		t.Fatalf("unexpected release API path: %s", got)
	}
	if got := releaseV3FileName(domain.Release{Owner: "teamname", Name: "apache", Version: "1.2.3"}); got != "teamname-apache-1.2.3.tar.gz" {
		t.Fatalf("unexpected release file name: %s", got)
	}
	if got := readmeBaseHref("teamname", "apache", "1.2.3"); got != "/modules/teamname/apache/versions/1.2.3/files/" {
		t.Fatalf("unexpected readme base href: %s", got)
	}
	if got := auth.EmailDomain("Dev@Example.COM"); got != "example.com" {
		t.Fatalf("unexpected email domain: %s", got)
	}
}

func TestRenderMarkdownWrapsReadmeCodeBlocks(t *testing.T) {
	t.Parallel()

	body := string(renderMarkdown("Inline `code`.\n\n```yaml\nforge:\n  baseurl: http://example.test\n```\n\n```\n# Managed by Puppet -- do not edit!\nDEFAULT -m root -M daily\nDEVICESCAN\n```\n\n```\nsmartd => /usr/sbin/smartd\n```\n\n```pp\nclass apache {}\n```\n", ""))
	if !strings.Contains(body, `class="code-window"`) {
		t.Fatalf("README code block is not wrapped in code window:\n%s", body)
	}
	if !strings.Contains(body, `class="copy-button"`) {
		t.Fatalf("README code block does not include copy button:\n%s", body)
	}
	if !strings.Contains(body, `<span class="code-title">yaml</span>`) {
		t.Fatalf("README code block does not keep language title:\n%s", body)
	}
	if !strings.Contains(body, `<span class="code-title">Code</span>`) {
		t.Fatalf("README code block without language does not use neutral Code title:\n%s", body)
	}
	if !strings.Contains(body, `<span class="code-title">Puppet</span>`) {
		t.Fatalf("README Puppet code block does not use Puppet title:\n%s", body)
	}
	if strings.Count(body, `<span class="code-title">Puppet</span>`) != 1 {
		t.Fatalf("README Puppet code block detection should match pp language hint only:\n%s", body)
	}
	if strings.Contains(body, `<span class="code-title">README</span>`) {
		t.Fatalf("README code block title leaked into UI:\n%s", body)
	}
	if strings.Count(body, `class="code-window"`) != 4 {
		t.Fatalf("inline code was wrapped as code window too:\n%s", body)
	}
	if !strings.Contains(body, "<p>Inline <code>code</code>.</p>") {
		t.Fatalf("inline code rendering changed unexpectedly:\n%s", body)
	}
}

func TestRenderMarkdownEscapesRawHTML(t *testing.T) {
	t.Parallel()

	body := string(renderMarkdown("# Unsafe\n"+
		"<script>alert(\"xss\")</script>\n"+
		"<img src=x onerror=\"alert(1)\">\n\n"+
		"```html\n"+
		"<script>alert(\"code\")</script>\n"+
		"```\n", ""))

	if strings.Contains(body, "<script>") || strings.Contains(body, `onerror=`) {
		t.Fatalf("raw HTML was rendered unsafely:\n%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;alert") {
		t.Fatalf("code block HTML was not preserved as escaped code:\n%s", body)
	}
}

func TestRenderMarkdownSanitizesUnsafeLinks(t *testing.T) {
	t.Parallel()

	body := string(renderMarkdown("[unsafe](javascript:alert(1))\n\n![unsafe](javascript:alert(2))", ""))

	if strings.Contains(body, "javascript:") {
		t.Fatalf("unsafe README URL was preserved:\n%s", body)
	}
}

func TestReadPublishInputRejectsManualMetadataFields(t *testing.T) {
	t.Parallel()

	for _, field := range []string{"owner", "name", "version", "summary", "description", "metadata"} {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		if err := writer.WriteField("space", "teamname"); err != nil {
			t.Fatalf("WriteField(space) error = %v", err)
		}
		if err := writer.WriteField(field, "override"); err != nil {
			t.Fatalf("WriteField(%s) error = %v", field, err)
		}
		if err := writer.Close(); err != nil {
			t.Fatalf("multipart Close() error = %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/modules", &body)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		_, _, err := readPublishInput(httptest.NewRecorder(), req, 0)
		if err == nil || !strings.Contains(err.Error(), `manual field "`+field+`" is not allowed`) {
			t.Fatalf("field %s error = %v", field, err)
		}
	}
}

func TestRewriteRelativeReadmeLinks(t *testing.T) {
	t.Parallel()

	source := `<a href="docs/usage.md">Usage</a><img src="images/logo.png"><a href="https://example.com/x">External</a>`
	got := rewriteRelativeReadmeLinks(source, "/modules/teamname/apache/versions/1.2.3/files/")
	if !strings.Contains(got, `href="/modules/teamname/apache/versions/1.2.3/files/docs/usage.md"`) {
		t.Fatalf("relative href was not rewritten: %s", got)
	}
	if !strings.Contains(got, `src="/modules/teamname/apache/versions/1.2.3/files/images/logo.png"`) {
		t.Fatalf("relative src was not rewritten: %s", got)
	}
	if !strings.Contains(got, `href="https://example.com/x"`) {
		t.Fatalf("absolute href should not be rewritten: %s", got)
	}
}

func postManageToken(t *testing.T, client *http.Client, baseURL string, token string) {
	t.Helper()

	resp, err := client.PostForm(baseURL+"/manage/login", url.Values{"token": {token}})
	if err != nil {
		t.Fatalf("POST /manage/login error = %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected final login response 200 after redirect, got %d", resp.StatusCode)
	}
}

func manageFormValues(t *testing.T, client *http.Client, baseURL string, values url.Values) url.Values {
	t.Helper()

	next := make(url.Values, len(values)+1)
	for key, vals := range values {
		next[key] = append([]string(nil), vals...)
	}
	next.Set("csrf_token", manageCSRFToken(t, client, baseURL))
	return next
}

func manageCSRFToken(t *testing.T, client *http.Client, baseURL string) string {
	t.Helper()

	if client.Jar == nil {
		t.Fatal("client has no cookie jar")
	}
	target, err := url.Parse(baseURL + "/manage")
	if err != nil {
		t.Fatalf("parse manage URL error = %v", err)
	}
	for _, cookie := range client.Jar.Cookies(target) {
		if cookie.Name == manageCSRFCookie && cookie.Value != "" {
			return cookie.Value
		}
	}

	_ = getBody(t, client, baseURL+"/manage")
	for _, cookie := range client.Jar.Cookies(target) {
		if cookie.Name == manageCSRFCookie && cookie.Value != "" {
			return cookie.Value
		}
	}
	t.Fatal("manage csrf cookie missing")
	return ""
}

func getBody(t *testing.T, client *http.Client, target string) string {
	t.Helper()

	resp, err := client.Get(target)
	if err != nil {
		t.Fatalf("GET %s error = %v", target, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	return string(body)
}

func formRequest(values url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/manage/access", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

func findTeamConfig(configs []auth.TeamConfig, team string) *auth.TeamConfig {
	for i := range configs {
		if configs[i].Team == team {
			return &configs[i]
		}
	}
	return nil
}

func buildPublishMultipart(t *testing.T, owner, name, version string, archive []byte, csrfToken string) (*bytes.Buffer, string) {
	t.Helper()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	fields := map[string]string{
		"space": owner,
	}
	if csrfToken != "" {
		fields["csrf_token"] = csrfToken
	}
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatalf("WriteField(%s) error = %v", key, err)
		}
	}
	part, err := writer.CreateFormFile("file", owner+"-"+name+"-"+version+".tar.gz")
	if err != nil {
		t.Fatalf("CreateFormFile() error = %v", err)
	}
	if _, err := part.Write(archive); err != nil {
		t.Fatalf("Write(archive) error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("multipart Close() error = %v", err)
	}

	return &body, writer.FormDataContentType()
}

func newHTTPAPIAccessMatrixServer(t *testing.T, authorizer *auth.Authorizer) (*store.SQLiteStore, *httptest.Server) {
	t.Helper()

	st := newHTTPAPITestStore(t)

	module := createTeamnameApacheModuleAndRelease(t, st)
	createTeamnameApacheRelease(t, st, module, "2.0.0")

	server := httptest.NewServer(newTestRouter(service.NewModuleService(st, testArtifactStorage{}, "modules", nil), nil, "http://example.test", authorizer, nil, "admin-token", false, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)
	return st, server
}

func newHTTPAPITestStore(t *testing.T) *store.SQLiteStore {
	t.Helper()
	st, err := store.NewSQLiteStore("sqlite://:memory:", httpAPITestTokenHasher())
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

func httpAPITestTokenHasher() *auth.TokenHasher {
	tokenHasher, err := auth.NewTokenHasher(strings.Repeat("test-pepper-", 3))
	if err != nil {
		panic(err)
	}
	return tokenHasher
}

func newCookieJar(t *testing.T) *cookiejar.Jar {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New() error = %v", err)
	}
	return jar
}

func newAdminAuthorizer(t *testing.T, configs ...auth.TeamConfig) *auth.Authorizer {
	t.Helper()
	authorizer, err := auth.NewAuthorizerWithTokenHasher(auth.AccessConfigsWithRuntimeAdmin(configs, "admin-token"), httpAPITestTokenHasher())
	if err != nil {
		t.Fatalf("NewAuthorizer() error = %v", err)
	}
	return authorizer
}

func createTeamnameApacheModuleAndRelease(t *testing.T, st *store.SQLiteStore) domain.Module {
	t.Helper()
	ctx := context.Background()
	module, err := st.UpsertModule(ctx, "teamname", "apache")
	if err != nil {
		t.Fatalf("UpsertModule() error = %v", err)
	}
	createTeamnameApacheRelease(t, st, module, "1.2.3")
	return module
}

func createTeamnameApacheRelease(t *testing.T, st *store.SQLiteStore, module domain.Module, version string) {
	t.Helper()
	ctx := context.Background()
	_, err := st.CreateRelease(ctx, domain.Release{
		ID:          "release-" + version,
		ModuleID:    module.ID,
		Owner:       "teamname",
		Name:        "apache",
		Source:      "local",
		Version:     version,
		FileName:    "teamname-apache-" + version + ".tar.gz",
		ContentType: "application/gzip",
		SizeBytes:   123,
		SHA256:      "deadbeef",
		StoragePath: "modules/teamname/apache/" + version + "/teamname-apache-" + version + ".tar.gz",
		Metadata:    map[string]any{},
	})
	if err != nil {
		t.Fatalf("CreateRelease(%s) error = %v", version, err)
	}
}

func createModuleRelease(t *testing.T, st *store.SQLiteStore, owner, name, version string) domain.Module {
	t.Helper()
	ctx := context.Background()
	module, err := st.UpsertModule(ctx, owner, name)
	if err != nil {
		t.Fatalf("UpsertModule(%s/%s) error = %v", owner, name, err)
	}
	_, err = st.CreateRelease(ctx, domain.Release{
		ID:          "release-" + owner + "-" + name + "-" + version,
		ModuleID:    module.ID,
		Owner:       owner,
		Name:        name,
		Source:      "local",
		Version:     version,
		FileName:    owner + "-" + name + "-" + version + ".tar.gz",
		ContentType: "application/gzip",
		SizeBytes:   123,
		SHA256:      "deadbeef",
		StoragePath: "modules/" + owner + "/" + name + "/" + version + "/" + owner + "-" + name + "-" + version + ".tar.gz",
		Metadata:    map[string]any{},
	})
	if err != nil {
		t.Fatalf("CreateRelease(%s/%s %s) error = %v", owner, name, version, err)
	}
	return module
}

func newAccessManageClient(t *testing.T, st *store.SQLiteStore, ctx context.Context, configs []auth.TeamConfig) (*http.Client, string) {
	t.Helper()
	if err := st.ReplaceTeamConfigs(ctx, configs); err != nil {
		t.Fatalf("ReplaceTeamConfigs() error = %v", err)
	}
	authorizer := newAdminAuthorizer(t, configs...)
	server := httptest.NewServer(newTestRouter(service.NewModuleService(st, testArtifactStorage{}, "modules", nil), nil, "http://example.test", authorizer, nil, "admin-token", false, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)
	jar := newCookieJar(t)
	client := server.Client()
	client.Jar = jar
	postManageToken(t, client, server.URL, "admin-token")
	return client, server.URL
}

type upstreamPuppetlabsApache struct {
	st        *store.SQLiteStore
	moduleSvc *service.ModuleService
	proxy     *proxy.ForgeProxy
}

func newUpstreamPuppetlabsApache(t *testing.T) upstreamPuppetlabsApache {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v3/modules/puppetlabs-apache" {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"slug":"puppetlabs-apache",
			"owner":"puppetlabs",
			"name":"apache",
			"current_release":{"slug":"puppetlabs-apache-1.0.0"},
			"releases":[{"slug":"puppetlabs-apache-1.0.0"}]
		}`))
	}))
	t.Cleanup(upstream.Close)

	forgeProxy, err := proxy.NewForgeProxy(upstream.URL, 0, 1024, testArtifactStorage{}, "upstream-cache", proxy.WithHTTPClient(upstream.Client()), proxy.WithPrivateNetworks())
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}
	st := newHTTPAPITestStore(t)
	return upstreamPuppetlabsApache{
		st:        st,
		proxy:     forgeProxy,
		moduleSvc: service.NewModuleService(st, testArtifactStorage{}, "modules", forgeProxy),
	}
}

func newAdminServer(t *testing.T, moduleSvc *service.ModuleService, handler http.Handler) *httptest.Server {
	t.Helper()
	authorizer := newAdminAuthorizer(t)
	server := httptest.NewServer(newTestRouter(moduleSvc, handler, "http://example.test", authorizer, nil, "admin-token", true, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)
	return server
}

func getV3Release(t *testing.T, server *httptest.Server) *http.Response {
	t.Helper()
	resp, err := server.Client().Get(server.URL + "/v3/releases/puppetlabs-concat-9.1.0")
	if err != nil {
		t.Fatalf("GET /v3/releases error = %v", err)
	}
	return resp
}

func getV3ModulesAndManagePage(t *testing.T, server *httptest.Server) string {
	t.Helper()
	resp, err := server.Client().Get(server.URL + "/v3/modules/stm-debconf")
	if err != nil {
		t.Fatalf("GET /v3/modules error = %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v3/modules status = %d", resp.StatusCode)
	}
	jar := newCookieJar(t)
	client := &http.Client{Transport: server.Client().Transport}
	client.Jar = jar
	postManageToken(t, client, server.URL, "admin-token")
	return getBody(t, client, server.URL+"/manage/modules/stm/debconf/card")
}

func TestEnsureManageCSRFTokenReturnsErrorWhenRandomGenerationFails(t *testing.T) {
	// Not parallel due to global testableRandomBase64URL mock
	oldRandom := testableRandomBase64URL
	testableRandomBase64URL = func(size int) (string, error) {
		return "", errors.New("simulated random generation failure")
	}
	t.Cleanup(func() {
		testableRandomBase64URL = oldRandom
	})

	router := &Router{manageSessions: newManageSessionStore(nil, "test-secret")}
	_, err := router.ensureManageCSRFToken(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/manage", nil))
	if err == nil || !strings.Contains(err.Error(), "simulated random generation failure") {
		t.Fatalf("ensureManageCSRFToken() error = %v", err)
	}
}

func TestNewRouterRejectsMissingManageSessionSecret(t *testing.T) {
	t.Parallel()

	handler, err := NewRouter(RouterConfig{})
	if handler != nil {
		t.Fatal("NewRouter() returned a handler without a manage session secret")
	}
	if err == nil || !strings.Contains(err.Error(), "manage session secret must contain at least 32 bytes") {
		t.Fatalf("NewRouter() error = %v", err)
	}
}

func TestRandomBase64URLRejectsNegativeSize(t *testing.T) {
	t.Parallel()

	if _, err := randomBase64URL(-1); err == nil {
		t.Fatal("expected error for negative random token size")
	}
}
