package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zxzharmlesszxz/puppet-forge/internal/service"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

func TestV3ModuleCollectionServesLocalCatalogWithoutUpstreamProxy(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)
	createV3CollectionRelease(t, st, "teamname", "apache", "1.0.0")
	createV3CollectionRelease(t, st, "teamname", "apache", "2.0.0")
	createV3CollectionRelease(t, st, "otherteam", "stdlib", "3.0.0")
	server := httptest.NewServer(newTestRouter(service.NewModuleService(st, testArtifactStorage{}, "modules", nil), nil, "http://example.test", nil, nil, "admin-token", true, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)

	resp, err := server.Client().Get(server.URL + "/v3/modules?query=apache&limit=1")
	if err != nil {
		t.Fatalf("GET /v3/modules error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /v3/modules status = %d, want %d: %s", resp.StatusCode, http.StatusOK, body)
	}

	var payload struct {
		Pagination struct {
			Limit  int     `json:"limit"`
			Offset int     `json:"offset"`
			Total  int     `json:"total"`
			Next   *string `json:"next"`
		} `json:"pagination"`
		Results []struct {
			Slug  string `json:"slug"`
			Owner struct {
				Username string `json:"username"`
			} `json:"owner"`
			CurrentRelease struct {
				Version  string         `json:"version"`
				Metadata map[string]any `json:"metadata"`
			} `json:"current_release"`
			Releases []v3ReleaseRef `json:"releases"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("Decode(response) error = %v", err)
	}
	if payload.Pagination.Limit != 1 || payload.Pagination.Offset != 0 || payload.Pagination.Total != 1 || payload.Pagination.Next != nil {
		t.Fatalf("pagination = %#v", payload.Pagination)
	}
	if len(payload.Results) != 1 || payload.Results[0].Slug != "teamname-apache" || payload.Results[0].Owner.Username != "teamname" {
		t.Fatalf("results = %#v", payload.Results)
	}
	if payload.Results[0].CurrentRelease.Version != "2.0.0" || payload.Results[0].CurrentRelease.Metadata["name"] != "teamname-apache" || len(payload.Results[0].Releases) != 2 {
		t.Fatalf("module releases = %#v", payload.Results[0])
	}
}

func TestV3ReleaseCollectionFiltersAndPaginatesLocalReleases(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)
	createV3CollectionRelease(t, st, "teamname", "apache", "1.0.0")
	createV3CollectionRelease(t, st, "teamname", "apache", "2.0.0")
	createV3CollectionRelease(t, st, "otherteam", "stdlib", "3.0.0")
	server := httptest.NewServer(newTestRouter(service.NewModuleService(st, testArtifactStorage{}, "modules", nil), nil, "http://example.test", nil, nil, "admin-token", true, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)

	first := getV3ReleaseCollection(t, server, "/v3/releases?module=teamname-apache&sort_by=version&limit=1")
	if first.Pagination.Total != 2 || first.Pagination.Offset != 0 || first.Pagination.Next == nil || len(first.Results) != 1 || first.Results[0].Version != "2.0.0" {
		t.Fatalf("first release page = %#v", first)
	}
	second := getV3ReleaseCollection(t, server, *first.Pagination.Next)
	if second.Pagination.Total != 2 || second.Pagination.Offset != 1 || second.Pagination.Next != nil || len(second.Results) != 1 || second.Results[0].Version != "1.0.0" {
		t.Fatalf("second release page = %#v", second)
	}
}

func TestV3CollectionRejectsInvalidPagination(t *testing.T) {
	t.Parallel()

	st := newHTTPAPITestStore(t)
	server := httptest.NewServer(newTestRouter(service.NewModuleService(st, testArtifactStorage{}, "modules", nil), nil, "http://example.test", nil, nil, "admin-token", true, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)

	resp, err := server.Client().Get(server.URL + "/v3/modules?limit=0")
	if err != nil {
		t.Fatalf("GET /v3/modules error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("GET /v3/modules status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

type v3ReleaseCollectionPayload struct {
	Pagination struct {
		Limit  int     `json:"limit"`
		Offset int     `json:"offset"`
		Total  int     `json:"total"`
		Next   *string `json:"next"`
	} `json:"pagination"`
	Results []struct {
		Slug     string         `json:"slug"`
		Version  string         `json:"version"`
		Metadata map[string]any `json:"metadata"`
		FileURI  string         `json:"file_uri"`
	} `json:"results"`
}

func getV3ReleaseCollection(t *testing.T, server *httptest.Server, requestPath string) v3ReleaseCollectionPayload {
	t.Helper()
	requestURL := requestPath
	if strings.HasPrefix(requestPath, "/") {
		requestURL = server.URL + requestPath
	}
	resp, err := server.Client().Get(requestURL)
	if err != nil {
		t.Fatalf("GET %s error = %v", requestPath, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s status = %d, want %d: %s", requestPath, resp.StatusCode, http.StatusOK, body)
	}
	var payload v3ReleaseCollectionPayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("Decode(%s) error = %v", requestPath, err)
	}
	return payload
}

func createV3CollectionRelease(t *testing.T, st *store.SQLiteStore, owner, name, version string) {
	t.Helper()
	module, err := st.UpsertModule(t.Context(), owner, name)
	if err != nil {
		t.Fatalf("UpsertModule(%s/%s) error = %v", owner, name, err)
	}
	metadata := map[string]any{
		"name":         owner + "-" + name,
		"version":      version,
		"summary":      name + " module",
		"tags":         []any{"test"},
		"dependencies": []any{},
	}
	release := store.NewRelease(module.ID, owner, name, version, "", "", owner+"-"+name+"-"+version+".tar.gz", "application/gzip", "md5", "sha256", "modules/path", 42, metadata)
	if _, err := st.CreateRelease(t.Context(), release); err != nil {
		t.Fatalf("CreateRelease(%s/%s %s) error = %v", owner, name, version, err)
	}
}
