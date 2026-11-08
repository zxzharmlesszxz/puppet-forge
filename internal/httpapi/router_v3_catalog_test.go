package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
	"github.com/zxzharmlesszxz/puppet-forge/internal/proxy"
	"github.com/zxzharmlesszxz/puppet-forge/internal/service"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
	"github.com/zxzharmlesszxz/puppet-forge/internal/testutil"
)

type catalogV3Fixture struct {
	st                           store.Store
	handlers                     [2]http.Handler
	mode                         atomic.Value
	catalogs, metadata, archives atomic.Int32
	entered, resume              chan struct{}
	enteredOnce, resumeOnce      sync.Once
}

func catalogBackends() []string {
	backends := []string{"sqlite"}
	if os.Getenv("PUPPET_FORGE_TEST_POSTGRES_DSN") != "" {
		backends = append(backends, "postgres")
	}
	return backends
}

func newCatalogV3Fixture(t *testing.T, backend string, paused bool) *catalogV3Fixture {
	t.Helper()
	fixture := &catalogV3Fixture{entered: make(chan struct{}), resume: make(chan struct{})}
	fixture.mode.Store("")
	var stores [2]store.Store
	if backend == "postgres" {
		dsn := testutil.IsolatedPostgresDSN(t, os.Getenv("PUPPET_FORGE_TEST_POSTGRES_DSN"))
		for index := range stores {
			st, err := store.NewPostgresStore(t.Context(), dsn, httpAPITestTokenHasher())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(st.Close)
			stores[index] = st
		}
	} else {
		st := newHTTPAPITestStore(t)
		stores = [2]store.Store{st, st}
	}
	fixture.st = stores[0]
	if err := fixture.st.ReplaceTeamConfigs(t.Context(), []auth.TeamConfig{{Team: "consumer", ReadTokens: []string{"catalog-read-token"}}}); err != nil {
		t.Fatal(err)
	}
	configs, err := fixture.st.LoadTeamConfigs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	authorizer, err := auth.NewAuthorizerWithTokenHasher(configs, httpAPITestTokenHasher())
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" {
			t.Error("client credentials reached upstream")
		}
		switch {
		case req.URL.Path == "/v3/modules/puppetlabs-stdlib":
			fixture.catalogs.Add(1)
			fixture.enteredOnce.Do(func() { close(fixture.entered) })
			if paused {
				select {
				case <-fixture.resume:
				case <-req.Context().Done():
					return
				}
			}
			mode := fixture.mode.Load().(string)
			switch mode {
			case "404":
				http.NotFound(w, req)
				return
			case "500":
				http.Error(w, "unavailable", http.StatusInternalServerError)
				return
			case "invalid json":
				_, _ = w.Write([]byte("{"))
				return
			case "timeout":
				<-req.Context().Done()
				return
			}
			refs := []map[string]string{{"version": "10.0.0"}, {"version": "10.1.0"}, {"version": "11.0.0"}}
			if mode == "one" {
				refs = refs[1:2]
			}
			if mode == "empty" {
				refs = []map[string]string{}
			}
			if mode == "bad version" {
				refs = append(refs, map[string]string{"version": "garbage"})
			}
			payload := map[string]any{"slug": "puppetlabs-stdlib", "owner": "puppetlabs", "name": "stdlib", "releases": refs}
			if mode == "wrong identity" {
				payload["owner"] = "anotherowner"
			}
			if mode == "missing catalog" {
				delete(payload, "releases")
			}
			if err := json.NewEncoder(w).Encode(payload); err != nil {
				t.Error(err)
			}
		case strings.HasPrefix(req.URL.Path, "/v3/releases/puppetlabs-stdlib-"):
			fixture.metadata.Add(1)
			version := strings.TrimPrefix(req.URL.Path, "/v3/releases/puppetlabs-stdlib-")
			metadataName := "puppetlabs-stdlib"
			if fixture.mode.Load().(string) == "legacy namespace" {
				metadataName = "previousowner-stdlib"
			}
			if err := json.NewEncoder(w).Encode(map[string]any{"slug": "puppetlabs-stdlib-" + version, "version": version, "metadata": installerMetadata(metadataName, version), "file_uri": "/v3/files/puppetlabs-stdlib-" + version + ".tar.gz", "file_md5": "md5", "file_sha256": "sha", "file_size": 7}); err != nil {
				t.Error(err)
			}
		default:
			fixture.archives.Add(1)
			http.Error(w, "catalog must not download archives", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(func() { fixture.resumeCatalog(); upstream.Close() })
	for index, st := range stores {
		forge, err := proxy.NewForgeProxy(upstream.URL, time.Minute, 1<<20, testArtifactStorage{}, "upstream-cache", proxy.WithHTTPClient(upstream.Client()), proxy.WithPrivateNetworks())
		if err != nil {
			t.Fatal(err)
		}
		fixture.handlers[index] = newTestRouter(service.NewModuleService(st, testArtifactStorage{}, "modules", forge), forge.Handler(), "https://forge.example", authorizer, nil, "", false, defaultActiveReleaseTTL)
	}
	return fixture
}

func (f *catalogV3Fixture) request(ctx context.Context, index int, path string, authorized bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	if authorized {
		req.Header.Set("Authorization", "Bearer catalog-read-token")
	}
	rec := httptest.NewRecorder()
	f.handlers[index].ServeHTTP(rec, req)
	return rec
}

func (f *catalogV3Fixture) resumeCatalog() { f.resumeOnce.Do(func() { close(f.resume) }) }

func catalogPage(t *testing.T, rec *httptest.ResponseRecorder) v3ReleaseCollectionPayload {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog status=%d body=%s", rec.Code, rec.Body.String())
	}
	var page v3ReleaseCollectionPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	return page
}

func TestV3ColdCatalogPreservesPaginationAndCache(t *testing.T) {
	for _, backend := range catalogBackends() {
		t.Run(backend, func(t *testing.T) {
			f := newCatalogV3Fixture(t, backend, false)
			path := "/v3/releases?module=puppetlabs-stdlib&sort_by=version&limit=1&offset=1"
			first := catalogPage(t, f.request(t.Context(), 0, path, true))
			if first.Pagination.Total != 3 || first.Pagination.Offset != 1 || len(first.Results) != 1 || first.Results[0].Version != "10.1.0" || first.Results[0].Metadata["name"] != "puppetlabs-stdlib" || first.Results[0].Metadata["requirements"] == nil || first.Results[0].FileURI != "/v3/files/puppetlabs-stdlib-10.1.0.tar.gz" {
				t.Fatalf("cold page=%#v", first)
			}
			if first.Pagination.Next == nil || !strings.HasPrefix(*first.Pagination.Next, "/v3/releases?") {
				t.Fatalf("pagination=%#v", first.Pagination)
			}
			catalogPage(t, f.request(t.Context(), 1, path, true))
			last := catalogPage(t, f.request(t.Context(), 1, *first.Pagination.Next, true))
			if len(last.Results) != 1 || last.Results[0].Version != "10.0.0" {
				t.Fatalf("last page=%#v", last)
			}
			for _, query := range []string{"&version=99.0.0", "&offset=100"} {
				page := catalogPage(t, f.request(t.Context(), 0, "/v3/releases?module=puppetlabs-stdlib&limit=1"+query, true))
				if len(page.Results) != 0 {
					t.Fatalf("empty filter=%#v", page)
				}
			}
			if f.catalogs.Load() != 1 || f.metadata.Load() != 2 || f.archives.Load() != 0 {
				t.Fatalf("unexpected traffic: catalog=%d metadata=%d archives=%d", f.catalogs.Load(), f.metadata.Load(), f.archives.Load())
			}
		})
	}
}

func TestV3CatalogNormalizesLegacyNamespacesAndPersistsMetadata(t *testing.T) {
	for _, backend := range catalogBackends() {
		t.Run(backend, func(t *testing.T) {
			f := newCatalogV3Fixture(t, backend, false)
			f.mode.Store("legacy namespace")
			path := "/v3/releases?module=puppetlabs-stdlib&limit=20"
			for index := range 2 {
				page := catalogPage(t, f.request(t.Context(), index, path, true))
				if len(page.Results) != 3 || page.Pagination.Total != 3 {
					t.Fatalf("legacy catalog lost releases: %#v", page)
				}
				for _, release := range page.Results {
					if release.Metadata["name"] != "puppetlabs-stdlib" || release.Metadata["version"] != release.Version || release.Metadata["dependencies"] == nil || release.Metadata["custom"] == nil {
						t.Fatalf("invalid normalized metadata: %#v", release.Metadata)
					}
					stored, err := f.st.GetRelease(t.Context(), "puppetlabs", "stdlib", release.Version)
					if err != nil || stored.Metadata["name"] != "puppetlabs-stdlib" {
						t.Fatalf("normalized metadata not persisted: %#v %v", stored.Metadata, err)
					}
				}
			}
			if f.catalogs.Load() != 1 || f.metadata.Load() != 3 || f.archives.Load() != 0 {
				t.Fatalf("legacy repair refetched metadata or downloaded archives: catalogs=%d metadata=%d archives=%d", f.catalogs.Load(), f.metadata.Load(), f.archives.Load())
			}
		})
	}
}

func TestV3ColdCatalogIndexesBeforeApplyingEmptyFilters(t *testing.T) {
	for _, backend := range catalogBackends() {
		for _, query := range []string{"&offset=100", "&version=99.0.0"} {
			t.Run(backend+query, func(t *testing.T) {
				f := newCatalogV3Fixture(t, backend, false)
				page := catalogPage(t, f.request(t.Context(), 0, "/v3/releases?module=puppetlabs-stdlib&limit=1"+query, true))
				if len(page.Results) != 0 || f.catalogs.Load() != 1 || f.metadata.Load() != 0 || f.archives.Load() != 0 {
					t.Fatalf("filters triggered wrong indexing: %#v", page)
				}
				catalogPage(t, f.request(t.Context(), 1, "/v3/releases?module=puppetlabs-stdlib&limit=1"+query, true))
				if f.catalogs.Load() != 1 {
					t.Fatal("empty result refetched known catalog")
				}
			})
		}
	}
}

func TestV3ColdCatalogAuthorizationAndLocalModules(t *testing.T) {
	for _, backend := range catalogBackends() {
		t.Run(backend, func(t *testing.T) {
			f := newCatalogV3Fixture(t, backend, false)
			if rec := f.request(t.Context(), 0, "/v3/releases?module=puppetlabs-stdlib", false); rec.Code != http.StatusUnauthorized {
				t.Fatalf("anonymous=%d", rec.Code)
			}
			catalogPage(t, f.request(t.Context(), 0, "/v3/releases", true))
			catalogPage(t, f.request(t.Context(), 0, "/v3/releases?module=puppetlabs-*", true))
			module, err := f.st.UpsertModule(t.Context(), "puppetlabs", "stdlib")
			if err != nil {
				t.Fatal(err)
			}
			release := store.NewRelease(module.ID, module.Owner, module.Name, "1.0.0", "", "", "local.tar.gz", "application/gzip", "md5", "sha", "local-object", 7, installerMetadata("puppetlabs-stdlib", "1.0.0"))
			if _, err := f.st.CreateRelease(t.Context(), release); err != nil {
				t.Fatal(err)
			}
			page := catalogPage(t, f.request(t.Context(), 0, "/v3/releases?module=puppetlabs-stdlib", true))
			if len(page.Results) != 1 || page.Results[0].Version != "1.0.0" || f.catalogs.Load() != 0 || f.metadata.Load() != 0 || f.archives.Load() != 0 {
				t.Fatalf("local module behavior changed: %#v", page)
			}
		})
	}
}

func TestV3ColdCatalogFailuresAndRecovery(t *testing.T) {
	for _, backend := range catalogBackends() {
		for _, mode := range []string{"404", "500", "invalid json", "bad version", "wrong identity", "missing catalog", "timeout"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				f := newCatalogV3Fixture(t, backend, false)
				f.mode.Store(mode)
				timeout := 10 * time.Second
				if mode == "timeout" {
					timeout = 150 * time.Millisecond
				}
				ctx, cancel := context.WithTimeout(t.Context(), timeout)
				defer cancel()
				rec := f.request(ctx, 0, "/v3/releases?module=puppetlabs-stdlib&limit=1", true)
				expected := http.StatusBadGateway
				if mode == "404" {
					expected = http.StatusOK
				}
				if mode == "timeout" {
					expected = http.StatusServiceUnavailable
				}
				if rec.Code != expected {
					t.Fatalf("%s status=%d body=%s", mode, rec.Code, rec.Body.String())
				}
				indexed, err := f.st.(store.UpstreamCatalogStore).HasUpstreamCatalog(t.Context(), "puppetlabs", "stdlib")
				if err != nil || indexed {
					t.Fatalf("failed catalog marked complete: %v %v", indexed, err)
				}
				if _, err := f.st.GetModule(t.Context(), "puppetlabs", "stdlib"); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("failure left partial module: %v", err)
				}
				f.mode.Store("")
				page := catalogPage(t, f.request(t.Context(), 1, "/v3/releases?module=puppetlabs-stdlib&version=10.1.0", true))
				if len(page.Results) != 1 || f.catalogs.Load() != 2 || f.archives.Load() != 0 {
					t.Fatalf("recovery failed: %#v", page)
				}
			})
		}
	}
}

func TestV3ConcurrentColdCatalogAcrossReplicas(t *testing.T) {
	for _, backend := range catalogBackends() {
		t.Run(backend, func(t *testing.T) {
			f := newCatalogV3Fixture(t, backend, true)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			results := make(chan *httptest.ResponseRecorder, 12)
			for index := range cap(results) {
				go func() {
					results <- f.request(ctx, index%2, "/v3/releases?module=puppetlabs-stdlib&version=10.1.0", true)
				}()
			}
			select {
			case <-f.entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			f.resumeCatalog()
			for range cap(results) {
				select {
				case rec := <-results:
					page := catalogPage(t, rec)
					if len(page.Results) != 1 {
						t.Fatalf("concurrent page=%#v", page)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			if f.catalogs.Load() != 1 || f.metadata.Load() != 1 || f.archives.Load() != 0 {
				t.Fatalf("duplicate work: catalog=%d metadata=%d archives=%d", f.catalogs.Load(), f.metadata.Load(), f.archives.Load())
			}
			versions, err := f.st.ListReleases(t.Context(), "puppetlabs", "stdlib")
			if err != nil || len(versions) != 3 {
				t.Fatalf("duplicate/partial releases: %#v %v", versions, err)
			}
		})
	}
}

func TestV3CatalogFetchCannotOverwriteConcurrentLocalPublication(t *testing.T) {
	for _, backend := range catalogBackends() {
		t.Run(backend, func(t *testing.T) {
			f := newCatalogV3Fixture(t, backend, true)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- f.request(ctx, 0, "/v3/releases?module=puppetlabs-stdlib", true) }()
			select {
			case <-f.entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			module, err := f.st.UpsertModule(ctx, "puppetlabs", "stdlib")
			if err != nil {
				t.Fatal(err)
			}
			release := store.NewRelease(module.ID, module.Owner, module.Name, "10.1.0", "", "", "local.tar.gz", "application/gzip", "local-md5", "local-sha", "local/object", 7, installerMetadata("puppetlabs-stdlib", "10.1.0"))
			if _, err := f.st.CreateRelease(ctx, release); err != nil {
				t.Fatal(err)
			}
			f.resumeCatalog()
			select {
			case rec := <-done:
				page := catalogPage(t, rec)
				if len(page.Results) != 1 || page.Results[0].Version != "10.1.0" {
					t.Fatalf("upstream mixed with local publication: %#v", page)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if f.metadata.Load() != 0 || f.archives.Load() != 0 {
				t.Fatal("local publication triggered upstream hydration")
			}
			stored, err := f.st.GetRelease(ctx, module.Owner, module.Name, release.Version)
			if err != nil || stored.Source != "local" || stored.StoragePath != "local/object" {
				t.Fatalf("local publication changed: %#v %v", stored, err)
			}
		})
	}
}

func TestV3EmptyUpstreamCatalogIsComplete(t *testing.T) {
	for _, backend := range catalogBackends() {
		t.Run(backend, func(t *testing.T) {
			f := newCatalogV3Fixture(t, backend, false)
			f.mode.Store("empty")
			for index := range 2 {
				page := catalogPage(t, f.request(t.Context(), index, "/v3/releases?module=puppetlabs-stdlib", true))
				if len(page.Results) != 0 {
					t.Fatal("empty catalog returned releases")
				}
			}
			if f.catalogs.Load() != 1 {
				t.Fatal("completed empty catalog was refetched")
			}
			modules, err := f.st.ListUpstreamModules(t.Context(), 10)
			if err != nil || len(modules) != 1 {
				t.Fatalf("empty catalog lost background refresh: %#v %v", modules, err)
			}
		})
	}
}

func TestV3CatalogRespectsExistingModeration(t *testing.T) {
	for _, backend := range catalogBackends() {
		for _, state := range []domain.ResourceState{domain.StateBlocked} {
			t.Run(backend+"/"+string(state), func(t *testing.T) {
				f := newCatalogV3Fixture(t, backend, false)
				if _, err := f.st.UpsertModule(t.Context(), "puppetlabs", "stdlib"); err != nil {
					t.Fatal(err)
				}
				if err := f.st.(store.LifecycleStore).SetResourceStatus(t.Context(), "puppetlabs", "stdlib", "", domain.ResourceStatus{State: state, ChangedAt: time.Now().UTC()}); err != nil {
					t.Fatal(err)
				}
				page := catalogPage(t, f.request(t.Context(), 0, "/v3/releases?module=puppetlabs-stdlib", true))
				if len(page.Results) != 0 || f.catalogs.Load() != 0 || f.metadata.Load() != 0 {
					t.Fatalf("unavailable module triggered fetch: %#v", page)
				}
			})
		}
	}
}
