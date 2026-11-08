package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/proxy"
	"github.com/zxzharmlesszxz/puppet-forge/internal/service"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

func installerMetadata(name, version string) map[string]any {
	return map[string]any{"name": name, "version": version,
		"dependencies": []any{map[string]any{"name": "puppetlabs/stdlib", "version_requirement": ">= 9.0.0 < 11.0.0"}},
		"requirements": []any{map[string]any{"name": "puppet", "version_requirement": ">= 7.0.0 < 9.0.0"}},
		"custom":       map[string]any{"preserved": true},
	}
}

type metadataV3Fixture struct {
	handler   http.Handler
	st        *store.SQLiteStore
	calls     atomic.Int32
	artifacts atomic.Int32
}

func newMetadataV3Fixture(t *testing.T, response func(http.ResponseWriter, *http.Request), versions ...string) *metadataV3Fixture {
	t.Helper()
	fixture := &metadataV3Fixture{st: newHTTPAPITestStore(t)}
	if len(versions) == 0 {
		versions = []string{"11.2.0", "11.3.0", "11.4.0"}
	}
	for _, version := range versions {
		module, err := fixture.st.UpsertModule(t.Context(), "puppetlabs", "apt")
		if err != nil {
			t.Fatal(err)
		}
		release := store.NewRelease(module.ID, module.Owner, module.Name, version, "", "", "puppetlabs-apt-"+version+".tar.gz", "application/gzip", "cached-md5", "cached-sha", "cached-object", 7, nil)
		release.Source, release.UpstreamFileURI = "upstream", "/v3/files/puppetlabs-apt-"+version+".tar.gz"
		if _, err := fixture.st.CreateRelease(t.Context(), release); err != nil {
			t.Fatal(err)
		}
	}
	if err := fixture.st.SaveUpstreamCatalog(t.Context(), "puppetlabs", "apt", versions, store.CatalogLease{}); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasPrefix(req.URL.Path, "/v3/releases/") {
			fixture.artifacts.Add(1)
			http.Error(w, "unexpected artifact request", http.StatusInternalServerError)
			return
		}
		fixture.calls.Add(1)
		if req.Header.Get("Authorization") != "" {
			t.Error("Forge bearer token leaked upstream")
		}
		if response != nil {
			response(w, req)
			return
		}
		version := strings.TrimPrefix(req.URL.Path, "/v3/releases/puppetlabs-apt-")
		if err := json.NewEncoder(w).Encode(map[string]any{"slug": "puppetlabs-apt-" + version, "version": version, "metadata": installerMetadata("puppetlabs-apt", version), "file_uri": "/v3/files/puppetlabs-apt-" + version + ".tar.gz"}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(upstream.Close)
	artifacts := fixedDownloadStorage{body: []byte("archive"), contentType: "application/gzip"}
	forge, err := proxy.NewForgeProxy(upstream.URL, time.Minute, 1<<20, artifacts, "upstream-cache", proxy.WithHTTPClient(upstream.Client()), proxy.WithPrivateNetworks())
	if err != nil {
		t.Fatal(err)
	}
	fixture.handler = newTestRouter(service.NewModuleService(fixture.st, artifacts, "modules", forge), forge.Handler(), "https://external.example", newAdminAuthorizer(t), nil, "admin-token", false, defaultActiveReleaseTTL)
	return fixture
}

func metadataV3Request(handler http.Handler, path string, authorized bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if authorized {
		req.Header.Set("Authorization", "Bearer admin-token")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestUpstreamV3MetadataRepairAndRelativePagination(t *testing.T) {
	fixture := newMetadataV3Fixture(t, nil)
	path := "/v3/releases?module=puppetlabs-apt&limit=1&sort_by=version&extra=preserve"
	if rec := metadataV3Request(fixture.handler, path, false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous collection status=%d", rec.Code)
	}
	for page := range 3 {
		rec := metadataV3Request(fixture.handler, path, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("page=%d status=%d body=%s", page, rec.Code, rec.Body.String())
		}
		var payload struct {
			Results []struct {
				Metadata map[string]any
				FileURI  string `json:"file_uri"`
			}
			Pagination map[string]any
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		version := fmt.Sprintf("11.%d.0", 4-page)
		if len(payload.Results) != 1 || !reflect.DeepEqual(payload.Results[0].Metadata, installerMetadata("puppetlabs-apt", version)) || payload.Results[0].FileURI != "/v3/files/puppetlabs-apt-"+version+".tar.gz" {
			t.Fatalf("incomplete release: %s", rec.Body.String())
		}
		for _, key := range []string{"first", "current", "last", "previous", "next"} {
			value := payload.Pagination[key]
			if value == nil {
				continue
			}
			uri, err := url.Parse(value.(string))
			if err != nil || uri.IsAbs() || uri.Host != "" || uri.Path != "/v3/releases" || uri.Query().Get("module") != "puppetlabs-apt" || uri.Query().Get("extra") != "preserve" || uri.Query().Get("sort_by") != "version" {
				t.Fatalf("invalid %s: %v %v", key, value, err)
			}
		}
		if (page == 0) != (payload.Pagination["previous"] == nil) || (page == 2) != (payload.Pagination["next"] == nil) {
			t.Fatalf("wrong absent pages: %#v", payload.Pagination)
		}
		if page < 2 {
			path = payload.Pagination["next"].(string)
		}
	}
	if fixture.calls.Load() != 3 || fixture.artifacts.Load() != 0 {
		t.Fatalf("collection fetched too much: metadata=%d artifacts=%d", fixture.calls.Load(), fixture.artifacts.Load())
	}
	for _, path := range []string{"/v3/releases?module=puppetlabs-apt&version=11.4.0&limit=1", "/v3/releases/puppetlabs-apt-11.4.0"} {
		rec := metadataV3Request(fixture.handler, path, true)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"requirements"`) || strings.Contains(rec.Body.String(), "https://external.example/v3/") {
			t.Fatalf("warm release: %d %s", rec.Code, rec.Body.String())
		}
	}
	if fixture.calls.Load() != 3 || fixture.artifacts.Load() != 0 {
		t.Fatal("warm metadata refetched upstream or downloaded archives")
	}
	stored, err := fixture.st.GetRelease(t.Context(), "puppetlabs", "apt", "11.4.0")
	if err != nil || !reflect.DeepEqual(stored.Metadata, installerMetadata("puppetlabs-apt", "11.4.0")) || stored.StoragePath != "cached-object" || stored.SHA256 != "cached-sha" || stored.MD5 != "cached-md5" {
		t.Fatalf("cache or artifact changed: %#v %v", stored, err)
	}
}

func TestAptOfficialMetadataCompatibilityInCollectionAndSingleRelease(t *testing.T) {
	versions := []string{"0.0.1", "0.0.2", "10.0.0"}
	metadataFor := func(version string) map[string]any {
		metadata := installerMetadata("puppetlabs-apt", version)
		if version != "10.0.0" {
			metadata["name"] = "puppet-apt"
			metadata["checksums"] = map[string]any{"Modulefile": "original-checksum"}
			delete(metadata, "requirements")
		}
		return metadata
	}
	f := newMetadataV3Fixture(t, func(w http.ResponseWriter, req *http.Request) {
		version := strings.TrimPrefix(req.URL.Path, "/v3/releases/puppetlabs-apt-")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"slug": "puppetlabs-apt-" + version, "version": version,
			"metadata": metadataFor(version), "file_uri": "/v3/files/puppetlabs-apt-" + version + ".tar.gz",
		}); err != nil {
			t.Error(err)
		}
	}, versions...)
	// Simulate old cache entries that already have a file URI and the original namespace.
	for _, version := range versions[:2] {
		release, err := f.st.GetRelease(t.Context(), "puppetlabs", "apt", version)
		if err != nil {
			t.Fatal(err)
		}
		release.Metadata = metadataFor(version)
		if _, err := f.st.CreateRelease(t.Context(), release); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		page := catalogPage(t, metadataV3Request(f.handler, "/v3/releases?module=puppetlabs-apt&limit=20", true))
		if len(page.Results) != 3 || page.Pagination.Total != 3 {
			t.Fatalf("apt releases were lost or excluded: %#v", page)
		}
		for index, version := range versions {
			if page.Results[len(versions)-1-index].Version != version {
				t.Fatalf("apt release order changed: %#v", page.Results)
			}
			rec := metadataV3Request(f.handler, "/v3/releases/puppetlabs-apt-"+version, true)
			if rec.Code != http.StatusOK {
				t.Fatalf("single apt %s: %d %s", version, rec.Code, rec.Body.String())
			}
			var single struct {
				Metadata map[string]any `json:"metadata"`
				FileURI  string         `json:"file_uri"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &single); err != nil {
				t.Fatal(err)
			}
			expected := metadataFor(version)
			expected["name"] = "puppetlabs-apt"
			if !reflect.DeepEqual(single.Metadata, expected) || !reflect.DeepEqual(page.Results[len(versions)-1-index].Metadata, expected) || single.FileURI != "/v3/files/puppetlabs-apt-"+version+".tar.gz" {
				t.Fatalf("apt %s metadata or file URI changed: %#v", version, single)
			}
			stored, err := f.st.GetRelease(t.Context(), "puppetlabs", "apt", version)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(stored.Metadata, expected) || stored.StoragePath != "cached-object" || stored.MD5 != "cached-md5" || stored.SHA256 != "cached-sha" || stored.SizeBytes != 7 {
				t.Fatalf("apt %s repair changed the stored artifact: %#v", version, stored)
			}
		}
	}
	if f.calls.Load() != 3 || f.artifacts.Load() != 0 {
		t.Fatalf("apt repair did not reuse metadata cache: metadata=%d artifacts=%d", f.calls.Load(), f.artifacts.Load())
	}
}

func TestUpstreamV3MetadataFailuresAreNotSuccessfulProxyFallbacks(t *testing.T) {
	for _, failure := range []string{"unavailable", "missing", "wrong module", "wrong version"} {
		t.Run(failure, func(t *testing.T) {
			fixture := newMetadataV3Fixture(t, func(w http.ResponseWriter, _ *http.Request) {
				if failure == "unavailable" {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
					return
				}
				metadata := installerMetadata("puppetlabs-apt", "11.4.0")
				switch failure {
				case "missing":
					metadata = nil
				case "wrong module":
					metadata["name"] = "puppetlabs-stdlib"
				case "wrong version":
					metadata["version"] = "1.0.0"
				}
				if err := json.NewEncoder(w).Encode(map[string]any{"version": "11.4.0", "metadata": metadata}); err != nil {
					t.Error(err)
				}
			})
			for _, path := range []string{"/v3/releases?module=puppetlabs-apt&limit=1", "/v3/releases/puppetlabs-apt-11.4.0"} {
				rec := metadataV3Request(fixture.handler, path, true)
				if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), `"code":"upstream_unavailable"`) {
					t.Fatalf("malformed success: %d %s", rec.Code, rec.Body.String())
				}
			}
			if fixture.calls.Load() != 2 || fixture.artifacts.Load() != 0 {
				t.Fatalf("unexpected fallback traffic: %d %d", fixture.calls.Load(), fixture.artifacts.Load())
			}
			stored, err := fixture.st.GetRelease(t.Context(), "puppetlabs", "apt", "11.4.0")
			if err != nil || len(stored.Metadata) != 0 {
				t.Fatalf("invalid metadata persisted: %#v %v", stored, err)
			}
		})
	}
}

func TestColdV3ReleasePersistsMetadataBeforeServing(t *testing.T) {
	fixture := newMetadataV3Fixture(t, nil)
	for range 2 {
		rec := metadataV3Request(fixture.handler, "/v3/releases/puppetlabs-apt-11.5.0", true)
		if rec.Code != http.StatusOK {
			t.Fatalf("cold release: %d %s", rec.Code, rec.Body.String())
		}
		var payload struct {
			Metadata map[string]any
			FileURI  string `json:"file_uri"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(payload.Metadata, installerMetadata("puppetlabs-apt", "11.5.0")) || payload.FileURI != "/v3/files/puppetlabs-apt-11.5.0.tar.gz" {
			t.Fatalf("incomplete cold response: %#v", payload)
		}
	}
	if fixture.calls.Load() != 1 || fixture.artifacts.Load() != 0 {
		t.Fatalf("cold cache reuse failed: %d %d", fixture.calls.Load(), fixture.artifacts.Load())
	}
	stored, err := fixture.st.GetRelease(t.Context(), "puppetlabs", "apt", "11.5.0")
	if err != nil || !reflect.DeepEqual(stored.Metadata, installerMetadata("puppetlabs-apt", "11.5.0")) || stored.MD5 == "" || stored.SHA256 == "" {
		t.Fatalf("cold release not persisted: %#v %v", stored, err)
	}
}

func TestConcurrentV3MetadataRepairFetchesOnce(t *testing.T) {
	fixture := newMetadataV3Fixture(t, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan string, 16)
	for index := range cap(done) {
		go func() {
			path := "/v3/releases?module=puppetlabs-apt&version=11.4.0&limit=1"
			if index%2 == 0 {
				path = "/v3/releases/puppetlabs-apt-11.4.0"
			}
			req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
			req.Header.Set("Authorization", "Bearer admin-token")
			rec := httptest.NewRecorder()
			fixture.handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				done <- fmt.Sprintf("%d %s", rec.Code, rec.Body.String())
				return
			}
			done <- ""
		}()
	}
	for range cap(done) {
		select {
		case failure := <-done:
			if failure != "" {
				t.Error(failure)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if fixture.calls.Load() != 1 || fixture.artifacts.Load() != 0 {
		t.Fatalf("concurrent hydration traffic: metadata=%d artifacts=%d", fixture.calls.Load(), fixture.artifacts.Load())
	}
}
