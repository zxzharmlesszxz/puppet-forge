package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/service"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
	"github.com/zxzharmlesszxz/puppet-forge/internal/testutil"
)

type readmePublishStorage struct {
	testArtifactStorage
	uploads atomic.Int32
}

func (s *readmePublishStorage) UploadReaderIfAbsent(_ context.Context, _, _ string, body io.Reader) (bool, error) {
	s.uploads.Add(1)
	_, err := io.Copy(io.Discard, body)
	return err == nil, err
}

func TestPublishRoutesRequireNonBlankRootReadme(t *testing.T) {
	t.Parallel()
	for _, route := range []struct {
		name, path string
		manage     bool
		async      bool
		validCode  int
	}{
		{"v1 multipart", "/api/v1/modules", false, false, http.StatusCreated},
		{"v3 JSON", "/v3/releases", false, false, http.StatusCreated},
		{"manage async", "/manage/modules", true, true, http.StatusNoContent},
		{"manage form", "/manage/modules", true, false, http.StatusFound},
	} {
		for _, tc := range []struct {
			name, filename, body string
			valid                bool
		}{
			{name: "missing"},
			{name: "empty", filename: "README.md"},
			{name: "whitespace", filename: "README.md", body: " \t\n"},
			{name: "nested only", filename: "docs/README.md", body: "# Nested"},
			{name: "valid mixed case", filename: "Readme.MD", body: " \n# Demo\n", valid: true},
		} {
			t.Run(route.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				st := newHTTPAPITestStore(t)
				artifacts := &readmePublishStorage{}
				authorizer := newAdminAuthorizer(t, auth.TeamConfig{Team: "teamname", PublishTokens: []string{"publish-token"}})
				svc := service.NewModuleService(st, artifacts, "modules", nil)
				server := httptest.NewServer(newTestRouter(svc, nil, "", authorizer, nil, "admin-token", false, defaultActiveReleaseTTL))
				t.Cleanup(server.Close)
				client := server.Client()
				csrf := ""
				if route.manage {
					client.Jar = newCookieJar(t)
					postManageToken(t, client, server.URL, "admin-token")
					csrf = manageCSRFToken(t, client, server.URL)
				}
				client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
				files := map[string]string{"teamname-demo-1.0.0/metadata.json": `{"name":"teamname-demo","version":"1.0.0"}`}
				if tc.filename != "" {
					files["teamname-demo-1.0.0/"+tc.filename] = tc.body
				}
				archive, err := testutil.BuildTarGz(files)
				if err != nil {
					t.Fatal(err)
				}
				var resp *http.Response
				if route.path == "/v3/releases" {
					resp = postV3Release(t, server, "publish-token", archive)
				} else {
					body, contentType := buildPublishMultipart(t, "teamname", "demo", "1.0.0", archive, csrf)
					req, err := http.NewRequest(http.MethodPost, server.URL+route.path, body)
					if err != nil {
						t.Fatal(err)
					}
					req.Header.Set("Content-Type", contentType)
					if !route.manage {
						req.Header.Set("Authorization", "Bearer publish-token")
					}
					if route.async {
						req.Header.Set("X-Puppet-Forge-Async-Mutation", "true")
					}
					resp, err = client.Do(req)
					if err != nil {
						t.Fatal(err)
					}
				}
				defer func() { _ = resp.Body.Close() }()
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatal(err)
				}
				wantCode := http.StatusBadRequest
				if tc.valid {
					wantCode = route.validCode
				} else if route.manage && !route.async {
					wantCode = http.StatusFound
				}
				if resp.StatusCode != wantCode {
					t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, wantCode, body)
				}
				if tc.valid {
					release, err := st.GetRelease(t.Context(), "teamname", "demo", "1.0.0")
					if err != nil {
						t.Fatal(err)
					}
					if release.Readme != tc.body || artifacts.uploads.Load() != 1 {
						t.Fatalf("valid README not published: %#v %v", release, err)
					}
					return
				}
				if route.manage && !route.async {
					if !strings.Contains(resp.Header.Get("Location"), "README") {
						t.Fatal("form redirect lacks the README validation error")
					}
				} else if !strings.Contains(string(body), "README") || !strings.Contains(string(body), `"code":"validation_error"`) {
					t.Fatalf("missing controlled validation response: %s", body)
				}
				if artifacts.uploads.Load() != 0 {
					t.Fatal("invalid publication wrote to artifact storage")
				}
				if _, err := st.GetModule(t.Context(), "teamname", "demo"); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("invalid publication created a module: %v", err)
				}
			})
		}
	}
}
