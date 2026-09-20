package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/service"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
	"github.com/zxzharmlesszxz/puppet-forge/internal/testutil"
)

func TestV3ReleasePublishUsesArchiveNamespace(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)
	authorizer := newAdminAuthorizer(t, auth.TeamConfig{
		Team:          "teamname",
		PublishTokens: []string{"publish-token"},
	})
	server := httptest.NewServer(newTestRouter(service.NewModuleService(st, testArtifactStorage{}, "modules", nil), nil, "http://example.test", authorizer, nil, "admin-token", false, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)

	archive := buildV3PublishArchive(t, "teamname", "pdk", "1.2.3")
	resp := postV3Release(t, server, "publish-token", archive)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /v3/releases status = %d, want %d: %s", resp.StatusCode, http.StatusCreated, body)
	}

	var response releaseAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		t.Fatalf("Decode(response) error = %v", err)
	}
	if response.Owner != "teamname" || response.Name != "pdk" || response.Version != "1.2.3" {
		t.Fatalf("release identity = %s/%s %s, want teamname/pdk 1.2.3", response.Owner, response.Name, response.Version)
	}
	if _, err := st.GetRelease(context.Background(), "teamname", "pdk", "1.2.3"); err != nil {
		t.Fatalf("GetRelease() error = %v", err)
	}
}

func TestV3ReleasePublishRejectsForbiddenArchiveNamespace(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)
	authorizer := newAdminAuthorizer(t, auth.TeamConfig{
		Team:          "teamname",
		PublishTokens: []string{"publish-token"},
	})
	server := httptest.NewServer(newTestRouter(service.NewModuleService(st, testArtifactStorage{}, "modules", nil), nil, "http://example.test", authorizer, nil, "admin-token", false, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)

	resp := postV3Release(t, server, "publish-token", buildV3PublishArchive(t, "otherteam", "pdk", "1.2.3"))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /v3/releases status = %d, want %d: %s", resp.StatusCode, http.StatusForbidden, body)
	}
	if _, err := st.GetModule(context.Background(), "otherteam", "pdk"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("forbidden publish changed module store: %v", err)
	}
}

func TestV3ReleasePublishRejectsInvalidRequests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		token      string
		body       string
		maxBytes   int64
		wantStatus int
	}{
		{name: "missing token", body: `{"file":"YQ=="}`, wantStatus: http.StatusUnauthorized},
		{name: "invalid base64", token: "publish-token", body: `{"file":"!!!"}`, wantStatus: http.StatusBadRequest},
		{name: "unknown field", token: "publish-token", body: `{"file":"YQ==","space":"teamname"}`, wantStatus: http.StatusBadRequest},
		{name: "decoded archive too large", token: "publish-token", body: `{"file":"YWJjZA=="}`, maxBytes: 3, wantStatus: http.StatusRequestEntityTooLarge},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			st := newHTTPAPITestStore(t)
			authorizer := newAdminAuthorizer(t, auth.TeamConfig{Team: "teamname", PublishTokens: []string{"publish-token"}})
			options := []RouterOption(nil)
			if testCase.maxBytes > 0 {
				options = append(options, WithModuleUploadMaxBytes(testCase.maxBytes))
			}
			server := httptest.NewServer(newTestRouter(service.NewModuleService(st, testArtifactStorage{}, "modules", nil), nil, "http://example.test", authorizer, nil, "admin-token", false, defaultActiveReleaseTTL, options...))
			t.Cleanup(server.Close)

			req, err := http.NewRequest(http.MethodPost, server.URL+"/v3/releases", bytes.NewBufferString(testCase.body))
			if err != nil {
				t.Fatalf("NewRequest() error = %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			if testCase.token != "" {
				req.Header.Set("Authorization", "Bearer "+testCase.token)
			}
			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatalf("POST /v3/releases error = %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != testCase.wantStatus {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("POST /v3/releases status = %d, want %d: %s", resp.StatusCode, testCase.wantStatus, body)
			}
		})
	}
}

func buildV3PublishArchive(t *testing.T, owner, name, version string) []byte {
	t.Helper()
	root := owner + "-" + name + "-" + version
	archive, err := testutil.BuildTarGz(map[string]string{
		root + "/metadata.json": `{"name":"` + owner + `-` + name + `","version":"` + version + `"}`,
	})
	if err != nil {
		t.Fatalf("testutil.BuildTarGz() error = %v", err)
	}
	return archive
}

func postV3Release(t *testing.T, server *httptest.Server, token string, archive []byte) *http.Response {
	t.Helper()
	body, err := json.Marshal(struct {
		File []byte `json:"file"`
	}{File: archive})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v3/releases", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /v3/releases error = %v", err)
	}
	return resp
}
