package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
	"github.com/zxzharmlesszxz/puppet-forge/internal/proxy"
	"github.com/zxzharmlesszxz/puppet-forge/internal/service"
	"github.com/zxzharmlesszxz/puppet-forge/internal/storage"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
	"github.com/zxzharmlesszxz/puppet-forge/internal/testutil"
	"github.com/zxzharmlesszxz/puppet-forge/internal/webauth"
)

type testArtifactStorage struct{}

func (s testArtifactStorage) Upload(context.Context, string, string, []byte) error {
	return nil
}

func (s testArtifactStorage) Exists(context.Context, string) (bool, error) {
	return false, nil
}

func (s testArtifactStorage) Download(context.Context, string) (storage.Object, error) {
	return storage.Object{}, store.ErrNotFound
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

func (s fixedDownloadStorage) Upload(context.Context, string, string, []byte) error {
	return nil
}

func (s fixedDownloadStorage) Exists(context.Context, string) (bool, error) {
	return true, nil
}

func (s fixedDownloadStorage) Download(context.Context, string) (storage.Object, error) {
	return storage.Object{Body: s.body, ContentType: s.contentType}, nil
}

func (s fixedDownloadStorage) Stat(context.Context, string) (storage.ObjectAttrs, error) {
	return storage.ObjectAttrs{}, storage.ErrObjectNotFound
}

func (s fixedDownloadStorage) PublicURL(string) string {
	return ""
}

func newTestRouter(modules *service.ModuleService, forgeProxy http.Handler, publicBaseURL string, authorizer *auth.Authorizer, webAuth *webauth.OIDCAuth, adminToken string, publicModuleAccess bool, activeReleaseTTL time.Duration, opts ...RouterOption) http.Handler {
	return NewRouter(RouterConfig{
		Modules:            modules,
		ForgeProxy:         forgeProxy,
		PublicBaseURL:      publicBaseURL,
		Authorizer:         authorizer,
		WebAuth:            webAuth,
		AdminToken:         adminToken,
		PublicModuleAccess: publicModuleAccess,
		ActiveReleaseTTL:   activeReleaseTTL,
	}, opts...)
}

func TestRouterSecurityHeaders(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)
	moduleSvc := service.NewModuleService(st, testArtifactStorage{}, "modules", nil)
	handler := NewRouter(RouterConfig{
		Modules:             moduleSvc,
		AdminToken:          "admin-token",
		SecurityHSTSEnabled: true,
	})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rec.Header().Get("Strict-Transport-Security"); got != "max-age=31536000; includeSubDomains" {
		t.Fatalf("Strict-Transport-Security = %q", got)
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
		tt := tt
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
	body := getBody(t, client, server.URL+"/manage")
	if !strings.Contains(body, "Team: teamname") {
		t.Fatalf("expected teamname manage page, got body:\n%s", body)
	}

	postManageToken(t, client, server.URL, "admin-token")
	body = getBody(t, client, server.URL+"/manage")
	if !strings.Contains(body, "Team: bootstrap-admin") || !strings.Contains(body, "admin") {
		t.Fatalf("expected admin manage page, got body:\n%s", body)
	}
	if strings.Contains(body, "spaces: teamname") {
		t.Fatalf("admin manage page still shows teamname publish scope:\n%s", body)
	}
}

func TestManageTokenLoginStoresOpaqueEncryptedSession(t *testing.T) {
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

	body := getBody(t, client, server.URL+"/manage")
	if !strings.Contains(body, "Team: bootstrap-admin") {
		t.Fatalf("encrypted manage session was not accepted:\n%s", body)
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
	body := getBody(t, client, serverB.URL+"/manage")
	if !strings.Contains(body, "Team: bootstrap-admin") {
		t.Fatalf("manage token session was not accepted by another router instance:\n%s", body)
	}
}

func TestIndexFilterHiddenRowsStayHidden(t *testing.T) {
	t.Parallel()

	var page bytes.Buffer
	if err := indexPageTemplate.Execute(&page, indexPageData{}); err != nil {
		t.Fatalf("indexPageTemplate.Execute() error = %v", err)
	}
	if !strings.Contains(page.String(), ".row[hidden]") {
		t.Fatalf("index page does not force filtered rows hidden:\n%s", page.String())
	}
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
	body := getBody(t, client, server.URL+"/manage")
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

func TestDeleteModuleRejectsProtectedReleases(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)

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
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "module contains latest release teamname/apache 2.0.0 cannot be deleted") {
		t.Fatalf("expected module delete to get 409 protected error, got %d body=%s", resp.StatusCode, string(body))
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

	moduleSvc := service.NewModuleService(st, testArtifactStorage{}, "modules", nil)
	authorizer := newAdminAuthorizer(t)

	server := httptest.NewServer(newTestRouter(moduleSvc, nil, "http://example.test", authorizer, nil, "admin-token", true, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)

	downloadClient := server.Client()
	downloadClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := downloadClient.Get(server.URL + "/api/v1/modules/teamname/apache/versions/1.2.3/download")
	if err != nil {
		t.Fatalf("GET download error = %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("download status = %d, want %d", resp.StatusCode, http.StatusFound)
	}

	jar := newCookieJar(t)
	client := &http.Client{Transport: server.Client().Transport}
	client.Jar = jar
	postManageToken(t, client, server.URL, "admin-token")
	body := getBody(t, client, server.URL+"/manage")

	if strings.Contains(body, "/manage/modules/teamname/apache/versions/1.2.3/delete") {
		t.Fatalf("manage page exposes delete for active release:\n%s", body)
	}
	if strings.Contains(body, "/manage/modules/teamname/apache/versions/2.0.0/delete") {
		t.Fatalf("manage page exposes delete for latest release:\n%s", body)
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
	sha := sha256.Sum256(body)
	expectedSHA := hex.EncodeToString(sha[:])
	moduleSvc := service.NewModuleService(st, fixedDownloadStorage{body: body, contentType: "application/gzip"}, "modules", nil)
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
	body := getBody(t, client, server.URL+"/manage")

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
				"file_sha256":"upstream-sha256"
			}`))
		case "/v3/files/puppetlabs-concat-9.1.0.tar.gz":
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write([]byte("upstream archive"))
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(upstream.Close)

	forgeProxy, err := proxy.NewForgeProxy(upstream.URL, 0, 1024, testArtifactStorage{}, "upstream-cache")
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

	moduleSvc := service.NewModuleService(st, testArtifactStorage{}, "modules", forgeProxy)
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
	body := getBody(t, client, server.URL+"/manage")

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
			ID:          "release-9.1.0",
			ModuleID:    module.ID,
			Owner:       "puppetlabs",
			Name:        "concat",
			Source:      "upstream",
			Version:     "9.1.0",
			FileName:    "puppetlabs-concat-9.1.0.tar.gz",
			ContentType: "application/gzip",
			SHA256:      "upstream-sha256",
			Metadata:    map[string]any{},
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
	body := getBody(t, client, server.URL+"/manage")

	if !strings.Contains(body, "9.1.0") || !strings.Contains(body, "in use") {
		t.Fatalf("manage page does not mark requested release active:\n%s", body)
	}
	if strings.Contains(body, "/manage/modules/puppetlabs/concat/versions/9.1.0/delete") {
		t.Fatalf("manage page exposes delete for requested active release:\n%s", body)
	}
}

func TestV3ModuleRequestMarksLatestReleaseActive(t *testing.T) {
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
	if !strings.Contains(body, "in use") {
		t.Fatalf("manage page does not mark latest release active after module request:\n%s", body)
	}
	if strings.Contains(body, "/manage/modules/stm/debconf/versions/9.1.0/delete") {
		t.Fatalf("manage page exposes delete for latest release:\n%s", body)
	}
}

func TestUpstreamV3ModuleRequestIndexesAndMarksCurrentReleaseActive(t *testing.T) {
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

	forgeProxy, err := proxy.NewForgeProxy(upstream.URL, 0, 1024, testArtifactStorage{}, "upstream-cache")
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}
	st := newHTTPAPITestStore(t)

	moduleSvc := service.NewModuleService(st, testArtifactStorage{}, "modules", forgeProxy)
	forgeProxy.SetModuleObserver(func(ctx context.Context, module proxy.UpstreamModule) {
		if err := moduleSvc.IndexUpstreamModule(ctx, module); err != nil {
			t.Errorf("IndexUpstreamModule() error = %v", err)
			return
		}
		if err := moduleSvc.MarkUpstreamModuleCurrentReleaseUsed(ctx, module); err != nil {
			t.Errorf("MarkUpstreamModuleCurrentReleaseUsed() error = %v", err)
		}
	})
	server := newAdminServer(t, moduleSvc, forgeProxy.Handler())

	body := getV3ModulesAndManagePage(t, server)

	if !strings.Contains(body, "9.1.0") {
		t.Fatalf("manage page does not show indexed upstream current release:\n%s", body)
	}
	if !strings.Contains(body, "in use") {
		t.Fatalf("manage page does not mark upstream current release active after module request:\n%s", body)
	}
	if strings.Contains(body, "/manage/modules/stm/debconf/versions/9.1.0/delete") {
		t.Fatalf("manage page exposes delete for latest upstream current release:\n%s", body)
	}
}

func TestV3ReleaseRequestRestoresDeletedUpstreamReleaseOnDemand(t *testing.T) {
	t.Parallel()

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

	forgeProxy, err := proxy.NewForgeProxy(upstream.URL, 0, 1024, testArtifactStorage{}, "upstream-cache")
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}
	st := newHTTPAPITestStore(t)
	moduleSvc := service.NewModuleService(st, testArtifactStorage{}, "modules", forgeProxy)
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
	if !strings.Contains(body, "navigator.clipboard") || !strings.Contains(body, "document.querySelectorAll('.copy-button')") {
		t.Fatalf("module page does not include copy button handler:\n%s", body)
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
	if canDeleteInSpace(teamAdmin, "carbon") {
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
	if !canDeleteInSpace(admin, "carbon") {
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
		{name: "admin delete module protected", method: http.MethodDelete, path: "/api/v1/modules/teamname/apache", token: "admin-token", want: http.StatusConflict},
	}

	for _, tt := range tests {
		tt := tt
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
			"carbon-apache-1.2.3/metadata.json": `{"name":"carbon-apache","version":"1.2.3"}`,
		})
		if err != nil {
			t.Fatalf("testutil.BuildTarGz() error = %v", err)
		}
		body, contentType := buildPublishMultipart(t, "carbon", "apache", "1.2.3", archive, "")
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

	body := getBody(t, publishClient, server.URL+"/manage")
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
	if !strings.Contains(string(adminBody), "module contains latest release teamname/apache 1.2.3 cannot be deleted") {
		t.Fatalf("expected admin module delete to be blocked by latest release policy, got body:\n%s", string(adminBody))
	}
	if _, err := st.GetModule(ctx, "teamname", "apache"); err != nil {
		t.Fatalf("protected module was removed or lookup failed: %v", err)
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
	if !strings.Contains(string(body), "module contains latest release teamname/apache 1.2.3 cannot be deleted") {
		t.Fatalf("expected csrf-protected delete to reach latest release policy, got body:\n%s", string(body))
	}
	if _, err := st.GetModule(ctx, "teamname", "apache"); err != nil {
		t.Fatalf("protected module was removed or lookup failed: %v", err)
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

func TestManageAdminCanReplaceAccessConfig(t *testing.T) {
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
	body := getBody(t, client, server.URL+"/manage/access")
	if !strings.Contains(body, "platform-admin") {
		t.Fatalf("expected current access config in page, got body:\n%s", body)
	}

	nextConfig := `[
  {
    "team": "platform-admin",
    "oidc_admin_groups": ["forge-admins"]
  },
  {
    "team": "teamname",
    "publish_tokens": ["teamname-token"],
    "publish_owners": ["teamname"],
    "oidc_groups": ["teamname-devops"]
  }
]`
	resp, err := client.PostForm(server.URL+"/manage/access", manageFormValues(t, client, server.URL, url.Values{"config": {nextConfig}}))
	if err != nil {
		t.Fatalf("POST /manage/access error = %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected final save response 200 after redirect, got %d", resp.StatusCode)
	}

	configs, err := st.LoadTeamConfigs(ctx)
	if err != nil {
		t.Fatalf("LoadTeamConfigs() error = %v", err)
	}
	if len(configs) != 2 {
		t.Fatalf("expected two configs, got %#v", configs)
	}
	foundTeamname := false
	for _, cfg := range configs {
		if cfg.Team == "teamname" {
			foundTeamname = true
			if len(cfg.OIDCGroups) != 1 || cfg.OIDCGroups[0] != "teamname-devops" {
				t.Fatalf("unexpected teamname config: %#v", cfg)
			}
		}
	}
	if !foundTeamname {
		t.Fatalf("teamname config was not saved: %#v", configs)
	}

	resp, err = client.PostForm(server.URL+"/manage/access", manageFormValues(t, client, server.URL, url.Values{"config": {"[]"}}))
	if err != nil {
		t.Fatalf("POST empty /manage/access error = %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll(empty config response) error = %v", err)
	}
	if !strings.Contains(string(bodyBytes), "access config must contain at least one team") {
		t.Fatalf("expected empty config validation error, got body:\n%s", string(bodyBytes))
	}

	resp, err = client.PostForm(server.URL+"/manage/access", manageFormValues(t, client, server.URL, url.Values{
		"action":               {"save_team"},
		"team":                 {"carbon"},
		"publish_tokens":       {"carbon-token\n"},
		"extra_publish_spaces": {"shared"},
		"oidc_groups":          {"carbon-devops"},
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

	configs, err = st.LoadTeamConfigs(ctx)
	if err != nil {
		t.Fatalf("LoadTeamConfigs() after structured save error = %v", err)
	}
	foundCarbon := false
	for _, cfg := range configs {
		if cfg.Team == "carbon" {
			foundCarbon = true
			if len(cfg.PublishOwners) != 2 || cfg.PublishOwners[0] != "carbon" || cfg.PublishOwners[1] != "shared" {
				t.Fatalf("unexpected carbon owners: %#v", cfg.PublishOwners)
			}
			if len(cfg.PublishTokens) != 1 || cfg.PublishTokens[0] != "carbon-token" {
				t.Fatalf("unexpected carbon publish tokens: %#v", cfg.PublishTokens)
			}
			if len(cfg.OIDCGroups) != 1 || cfg.OIDCGroups[0] != "carbon-devops" {
				t.Fatalf("unexpected carbon oidc groups: %#v", cfg.OIDCGroups)
			}
		}
	}
	if !foundCarbon {
		t.Fatalf("carbon config was not saved: %#v", configs)
	}

	resp, err = client.PostForm(server.URL+"/manage/access", manageFormValues(t, client, server.URL, url.Values{
		"action": {"delete_team"},
		"team":   {"carbon"},
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
		if cfg.Team == "carbon" {
			t.Fatalf("carbon config was not deleted: %#v", configs)
		}
	}
}

func TestExtraPublishSpacesFormIgnoresLegacyOwnersField(t *testing.T) {
	t.Parallel()

	req := formRequest(url.Values{"extra_publish_owners": {"legacy"}})
	if got := extraPublishSpacesFromForm(req); len(got) != 0 {
		t.Fatalf("extraPublishSpacesFromForm() used legacy extra_publish_owners: %#v", got)
	}

	req = formRequest(url.Values{"extra_publish_spaces": {"shared\nplatform"}})
	got := extraPublishSpacesFromForm(req)
	if len(got) != 2 || got[0] != "shared" || got[1] != "platform" {
		t.Fatalf("extraPublishSpacesFromForm() = %#v", got)
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
	resp, err := client.Get(server.URL + "/manage/access")
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
			Team:          "carbon",
			ReadTokens:    []string{"carbon-read"},
			PublishTokens: []string{"carbon-publish"},
			PublishOwners: []string{"carbon"},
			OIDCGroups:    []string{"carbon-devops"},
		},
	}
	if err := st.ReplaceTeamConfigs(ctx, configs); err != nil {
		t.Fatalf("ReplaceTeamConfigs() error = %v", err)
	}

	router := &Router{modules: service.NewModuleService(st, testArtifactStorage{}, "modules", nil)}
	principal := auth.Principal{Team: "teamname", CanRead: true, CanPublish: true, CanManageTeam: true}

	rec := httptest.NewRecorder()
	router.renderManageAccess(rec, httptest.NewRequest(http.MethodGet, "/manage/access", nil), principal, "")
	body := rec.Body.String()
	if strings.Contains(body, "Global OIDC Admins") || strings.Contains(body, "Advanced JSON editor") || strings.Contains(body, "Delete Team") || strings.Contains(body, "carbon") || strings.Contains(body, "Extra publish spaces") {
		t.Fatalf("team admin page exposes forbidden controls or teams:\n%s", body)
	}
	if !strings.Contains(body, "teamname") || !strings.Contains(body, "OIDC team admins (emails)") || !strings.Contains(body, "OIDC team admins (groups)") {
		t.Fatalf("team admin page does not expose own team controls:\n%s", body)
	}

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
	if len(teamname.ReadTokens) != 1 || teamname.ReadTokens[0] != "new-read" {
		t.Fatalf("unexpected teamname read tokens: %#v", teamname.ReadTokens)
	}
	if len(teamname.PublishTokens) != 1 || teamname.PublishTokens[0] != "new-publish" {
		t.Fatalf("unexpected teamname publish tokens: %#v", teamname.PublishTokens)
	}
	if len(teamname.PublishOwners) != 2 || !containsString(teamname.PublishOwners, "teamname") || !containsString(teamname.PublishOwners, "shared") {
		t.Fatalf("team admin changed publish owners: %#v", teamname.PublishOwners)
	}
	if len(teamname.OIDCGroups) != 1 || teamname.OIDCGroups[0] != "teamname-publishers" {
		t.Fatalf("unexpected teamname oidc groups: %#v", teamname.OIDCGroups)
	}
	if len(teamname.OIDCTeamAdminEmails) != 2 || !containsString(teamname.OIDCTeamAdminEmails, "owner@example.com") || !containsString(teamname.OIDCTeamAdminEmails, "backup@example.com") {
		t.Fatalf("unexpected teamname team admin emails: %#v", teamname.OIDCTeamAdminEmails)
	}
	if len(teamname.OIDCTeamAdminGroups) != 2 || teamname.OIDCTeamAdminGroups[1] != "teamname-owners" {
		t.Fatalf("unexpected teamname team admin groups: %#v", teamname.OIDCTeamAdminGroups)
	}
	carbon := findTeamConfig(saved, "carbon")
	if carbon == nil || len(carbon.ReadTokens) != 1 || carbon.ReadTokens[0] != "carbon-read" {
		t.Fatalf("carbon config was changed: %#v", saved)
	}

	if _, _, err := router.accessConfigsFromForm(formRequest(url.Values{
		"action": {"replace_json"},
		"config": {"[]"},
	}), principal); err == nil || !strings.Contains(err.Error(), "global admin access required") {
		t.Fatalf("expected replace_json to require global admin, got %v", err)
	}
	if _, _, err := router.accessConfigsFromForm(formRequest(url.Values{
		"action":        {"save_team"},
		"original_team": {"carbon"},
		"team":          {"carbon"},
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
			Team:                "carbon",
			ReadTokens:          []string{"carbon-read"},
			PublishTokens:       []string{"carbon-publish"},
			PublishOwners:       []string{"carbon", "shared"},
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

	router := &Router{modules: service.NewModuleService(st, testArtifactStorage{}, "modules", nil)}
	rec := httptest.NewRecorder()
	router.renderManageAccess(rec, httptest.NewRequest(http.MethodGet, "/manage/access", nil), principal, "")
	body := rec.Body.String()
	if !strings.Contains(body, "teamname") || !strings.Contains(body, "carbon") {
		t.Fatalf("team admin page misses managed teams:\n%s", body)
	}
	if strings.Contains(body, "oxygen") {
		t.Fatalf("team admin page exposes unmanaged team:\n%s", body)
	}

	next, message, err := router.accessConfigsFromForm(formRequest(url.Values{
		"action":                 {"save_team"},
		"original_team":          {"carbon"},
		"team":                   {"carbon"},
		"read_tokens":            {"carbon-read-new"},
		"publish_tokens":         {"carbon-publish-new"},
		"extra_publish_spaces":   {"evil"},
		"oidc_groups":            {"carbon-devops"},
		"oidc_team_admin_groups": {"platform-owners"},
	}), principal)
	if err != nil {
		t.Fatalf("accessConfigsFromForm(carbon team admin) error = %v", err)
	}
	if message != "team access saved" {
		t.Fatalf("unexpected save message: %s", message)
	}
	carbon := findTeamConfig(next, "carbon")
	if carbon == nil {
		t.Fatalf("carbon config missing: %#v", next)
	}
	if len(carbon.ReadTokens) != 1 || carbon.ReadTokens[0] != "carbon-read-new" {
		t.Fatalf("unexpected carbon read tokens: %#v", carbon.ReadTokens)
	}
	if len(carbon.PublishOwners) != 2 || carbon.PublishOwners[0] != "carbon" || carbon.PublishOwners[1] != "shared" {
		t.Fatalf("team admin changed carbon publish owners: %#v", carbon.PublishOwners)
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
			PublishOwners: []string{"teamname"},
			OIDCGroups:    []string{"teamname-devops"},
		},
	}
	client, serverURL := newAccessManageClient(t, st, ctx, configs)
	resp, err := client.PostForm(serverURL+"/manage/access", manageFormValues(t, client, serverURL, url.Values{
		"action":               {"save_team"},
		"original_team":        {"teamname"},
		"team":                 {"teamname-platform"},
		"publish_tokens":       {"renamed-token"},
		"extra_publish_spaces": {"platform"},
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
	renamed := findTeamConfig(saved, "teamname-platform")
	if renamed == nil {
		t.Fatalf("renamed team was not saved: %#v", saved)
	}
	if len(renamed.PublishOwners) != 2 || !containsString(renamed.PublishOwners, "teamname-platform") || !containsString(renamed.PublishOwners, "platform") {
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
	body := getBody(t, client, serverURL+"/manage/access")
	if !strings.Contains(body, "Team Access") && !strings.Contains(body, "Access Config") {
		t.Fatalf("runtime admin token stopped working after save, got body:\n%s", body)
	}
	if strings.Contains(body, "OIDC publish emails") || strings.Contains(body, "OIDC publish subjects") || strings.Contains(body, "OIDC publish domains") {
		t.Fatalf("team OIDC email/subject/domain fields should not be rendered, got body:\n%s", body)
	}
	if !strings.Contains(body, "Global OIDC Admins") || !strings.Contains(body, "forge-owners") {
		t.Fatalf("global admin form was not rendered with saved groups, got body:\n%s", body)
	}
	if len(admin.OIDCAdminGroups) != 2 || admin.OIDCAdminGroups[1] != "forge-owners" {
		t.Fatalf("oidc admin groups were not updated: %#v", admin.OIDCAdminGroups)
	}
}

func TestManageAccessRemembersOpenSections(t *testing.T) {
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
	router.renderManageAccess(rec, httptest.NewRequest(http.MethodGet, "/manage/access", nil), auth.Principal{CanAdmin: true}, "")
	body := rec.Body.String()

	for _, want := range []string{
		`data-access-section="global-admins"`,
		`data-access-section="team:teamname"`,
		`data-access-section="advanced-json"`,
		`puppet-forge:manage-access:open-sections`,
		`window.localStorage.setItem(storageKey`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("manage access page missing persisted section state hook %q:\n%s", want, body)
		}
	}
}

func TestManageAccessRejectsDuplicateOIDCMapping(t *testing.T) {
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
		"team":        {"carbon"},
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
	if !strings.Contains(string(body), "OIDC group") || !strings.Contains(string(body), "teamname") || !strings.Contains(string(body), "carbon") {
		t.Fatalf("expected duplicate OIDC group validation error, got body:\n%s", string(body))
	}

	saved, err := st.LoadTeamConfigs(ctx)
	if err != nil {
		t.Fatalf("LoadTeamConfigs() error = %v", err)
	}
	if findTeamConfig(saved, "carbon") != nil {
		t.Fatalf("invalid carbon config was persisted: %#v", saved)
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

	body := getBody(t, client, server.URL+"/manage")
	if !strings.Contains(body, "/manage/upstream") || !strings.Contains(body, "Add Upstream Module") {
		t.Fatalf("manage page does not expose upstream add form for admin:\n%s", body)
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

	body := getBody(t, client, server.URL+"/manage")
	if !strings.Contains(body, `<a href="/">Public modules</a>`) {
		t.Fatalf("manage page does not link to public modules:\n%s", body)
	}
	if !strings.Contains(body, `<a href="/manage">Manage</a>`) {
		t.Fatalf("manage page modules nav does not point to /manage:\n%s", body)
	}
	if !strings.Contains(body, `<a href="/manage/access">Access</a>`) {
		t.Fatalf("manage page does not link to access config:\n%s", body)
	}
	if !strings.Contains(body, `<a href="/manage/access/add">Add team</a>`) {
		t.Fatalf("admin manage page does not link to add team:\n%s", body)
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

func TestManageAccessNavLinks(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)
	router := &Router{modules: service.NewModuleService(st, testArtifactStorage{}, "modules", nil)}
	rec := httptest.NewRecorder()
	router.renderManageAccess(rec, httptest.NewRequest(http.MethodGet, "/manage/access", nil), auth.Principal{CanAdmin: true}, "")
	body := rec.Body.String()

	for _, want := range []string{
		`<a href="/">Public modules</a>`,
		`<a href="/manage">Manage</a>`,
		`<a href="/manage/access">Access</a>`,
		`<a href="/manage/access/add">Add Team</a>`,
		`<form method="post" action="/manage/logout">`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("manage access page missing nav item %q:\n%s", want, body)
		}
	}
}

func TestManageAccessAddTeamNavLinks(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	err := manageAccessAddTeamTemplate.Execute(rec, manageAccessAddTeamData{CSRFToken: "csrf-token"})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	body := rec.Body.String()

	for _, want := range []string{
		`<a href="/">Public modules</a>`,
		`<a href="/manage">Manage</a>`,
		`<a href="/manage/access">Access</a>`,
		`<form method="post" action="/manage/logout">`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("add team page missing nav item %q:\n%s", want, body)
		}
	}
}

func TestManageModulesRemembersOpenSections(t *testing.T) {
	t.Parallel()

	_, server, client := setupManageTestWithModule(t)

	body := getBody(t, client, server.URL+"/manage")
	for _, want := range []string{
		`data-section-key="teamname/apache"`,
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

func TestParseMetadata(t *testing.T) {
	t.Parallel()

	metadata, err := parseMetadata([]string{`{"source":"test"}`})
	if err != nil {
		t.Fatalf("parseMetadata() error = %v", err)
	}
	if metadata["source"] != "test" {
		t.Fatalf("unexpected metadata: %#v", metadata)
	}

	metadata, err = parseMetadata(nil)
	if err != nil {
		t.Fatalf("parseMetadata(nil) error = %v", err)
	}
	if len(metadata) != 0 {
		t.Fatalf("expected empty metadata, got %#v", metadata)
	}

	if _, err := parseMetadata([]string{"{"}); err == nil {
		t.Fatal("expected invalid metadata error")
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

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func buildPublishMultipart(t *testing.T, owner, name, version string, archive []byte, csrfToken string) (*bytes.Buffer, string) {
	t.Helper()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	fields := map[string]string{
		"owner":   owner,
		"name":    name,
		"version": version,
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
	st, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	t.Cleanup(st.Close)
	return st
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
	authorizer, err := auth.NewAuthorizer(auth.AccessConfigsWithRuntimeAdmin(configs, "admin-token"))
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

	forgeProxy, err := proxy.NewForgeProxy(upstream.URL, 0, 1024, testArtifactStorage{}, "upstream-cache")
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
	return getBody(t, client, server.URL+"/manage")
}

func TestManageModulesReturns500WhenRandomGenerationFails(t *testing.T) {
	// Not parallel due to global testableRandomBase64URL mock
	st := newHTTPAPITestStore(t)
	server, client := setupManageTestWithTeamnameApacheModule(t, st, true)
	jar := client.Jar

	// Clear CSRF token cookie to force regeneration
	target, err := url.Parse(server.URL + "/manage")
	if err != nil {
		t.Fatalf("parse URL error = %v", err)
	}
	jar.SetCookies(target, []*http.Cookie{
		{
			Name:   manageCSRFCookie,
			Value:  "",
			MaxAge: -1,
			Path:   "/manage",
		},
	})

	// Mock random generation to fail
	oldRandom := testableRandomBase64URL
	testableRandomBase64URL = func(size int) (string, error) {
		return "", errors.New("simulated random generation failure")
	}
	t.Cleanup(func() {
		testableRandomBase64URL = oldRandom
	})

	resp, err := client.Get(server.URL + "/manage")
	if err != nil {
		t.Fatalf("GET /manage error = %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500 when random generation fails, got %d", resp.StatusCode)
	}
}

func TestRandomBase64URLRejectsNegativeSize(t *testing.T) {
	t.Parallel()

	if _, err := randomBase64URL(-1); err == nil {
		t.Fatal("expected error for negative random token size")
	}
}
