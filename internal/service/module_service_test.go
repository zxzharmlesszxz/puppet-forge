package service

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
	"github.com/zxzharmlesszxz/puppet-forge/internal/proxy"
	artifactstorage "github.com/zxzharmlesszxz/puppet-forge/internal/storage"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
	"github.com/zxzharmlesszxz/puppet-forge/internal/testutil"
)

type testModuleStore struct {
	module  domain.Module
	release domain.Release
}

func (s *testModuleStore) Ping(_ context.Context) error {
	return nil
}

func (s *testModuleStore) AcquireLease(_ context.Context, _, _ string, _ time.Duration) (bool, error) {
	return true, nil
}

func (s *testModuleStore) ReleaseLease(_ context.Context, _, _ string) error {
	return nil
}

func (s *testModuleStore) UpsertModule(_ context.Context, owner, name string) (domain.Module, error) {
	s.module = domain.Module{
		ID:        "module-1",
		Owner:     owner,
		Name:      name,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	return s.module, nil
}

func (s *testModuleStore) CreateRelease(_ context.Context, release domain.Release) (domain.Release, error) {
	s.release = release
	s.release.CreatedAt = time.Now()
	return s.release, nil
}

func (s *testModuleStore) ListModules(_ context.Context, _ int) ([]domain.Module, error) {
	return []domain.Module{s.module}, nil
}

func (s *testModuleStore) ListModulesPage(_ context.Context, _, _ int) ([]domain.Module, int, error) {
	return []domain.Module{s.module}, 1, nil
}

func (s *testModuleStore) GetModule(_ context.Context, owner, name string) (domain.Module, error) {
	if s.module.Owner != owner || s.module.Name != name {
		return domain.Module{}, errors.New("not found")
	}
	return s.module, nil
}

func (s *testModuleStore) GetRelease(_ context.Context, owner, name, version string) (domain.Release, error) {
	if s.release.Owner != owner || s.release.Name != name || s.release.Version != version {
		return domain.Release{}, errors.New("not found")
	}
	return s.release, nil
}

func (s *testModuleStore) ListUpstreamModules(_ context.Context, _ int) ([]domain.Module, error) {
	return nil, nil
}

func (s *testModuleStore) DeleteModule(_ context.Context, _, _ string) error {
	return nil
}

func (s *testModuleStore) DeleteRelease(_ context.Context, _, _, _ string) error {
	return nil
}

func (s *testModuleStore) ListReleases(_ context.Context, _, _ string) ([]domain.ModuleVersion, error) {
	return nil, nil
}

func (s *testModuleStore) ListAllReleases(_ context.Context) ([]store.ReleaseSummary, error) {
	return nil, nil
}

type testArtifactStorage struct {
	objectPath  string
	contentType string
	body        []byte
}

func (s *testArtifactStorage) Upload(_ context.Context, objectPath string, contentType string, body []byte) error {
	s.objectPath = objectPath
	s.contentType = contentType
	s.body = append([]byte(nil), body...)
	return nil
}

func (s *testArtifactStorage) Exists(context.Context, string) (bool, error) {
	return false, nil
}

func (s *testArtifactStorage) Download(context.Context, string) (artifactstorage.Object, error) {
	return artifactstorage.Object{
		Body:        append([]byte(nil), s.body...),
		ContentType: s.contentType,
	}, nil
}

func (s *testArtifactStorage) Stat(context.Context, string) (artifactstorage.ObjectAttrs, error) {
	return artifactstorage.ObjectAttrs{}, artifactstorage.ErrObjectNotFound
}

func (s *testArtifactStorage) PublicURL(objectPath string) string {
	return "https://example.invalid/" + objectPath
}

func TestPublish(t *testing.T) {
	t.Parallel()

	archive, err := testutil.BuildTarGz(map[string]string{
		"metadata.json": `{"name":"acme-apache","version":"1.2.3","summary":"Apache module"}`,
	})
	if err != nil {
		t.Fatalf("testutil.BuildTarGz() error = %v", err)
	}

	moduleStore := &testModuleStore{}
	artifacts := &testArtifactStorage{}
	service := NewModuleService(moduleStore, artifacts, "modules", nil)

	release, err := service.Publish(context.Background(), domain.PublishModuleInput{
		Owner:       "acme",
		Name:        "apache",
		Version:     "1.2.3",
		Description: "Apache module",
		FileName:    "acme-apache-1.2.3.tar.gz",
		ContentType: "application/gzip",
		FileBytes:   archive,
		Metadata: map[string]any{
			"dependencies": []string{"stdlib"},
		},
	})
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	if artifacts.objectPath != "modules/acme/apache/1.2.3/acme-apache-1.2.3.tar.gz" {
		t.Fatalf("unexpected object path: %s", artifacts.objectPath)
	}

	if release.DownloadURL != "https://example.invalid/modules/acme/apache/1.2.3/acme-apache-1.2.3.tar.gz" {
		t.Fatalf("unexpected download url: %s", release.DownloadURL)
	}

	if release.SHA256 == "" {
		t.Fatal("expected SHA256 to be populated")
	}
}

func TestIndexUpstreamModuleSkipsDeletedRelease(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	defer st.Close()

	service := NewModuleService(st, &testArtifactStorage{}, "modules", nil)
	upstreamModule := proxy.UpstreamModule{
		Slug:  "puppetlabs-stdlib",
		Owner: "puppetlabs",
		Name:  "stdlib",
		CurrentRelease: proxy.UpstreamReleaseRef{
			Slug:    "puppetlabs-stdlib-1.0.0",
			Version: "1.0.0",
		},
		Releases: []proxy.UpstreamReleaseRef{
			{Slug: "puppetlabs-stdlib-1.0.0", Version: "1.0.0"},
			{Slug: "puppetlabs-stdlib-2.0.0", Version: "2.0.0"},
		},
	}
	indexDeleteAndReindexUpstreamRelease(t, ctx, st, service, upstreamModule, "puppetlabs", "stdlib", "1.0.0")
	if _, err := st.GetRelease(ctx, "puppetlabs", "stdlib", "2.0.0"); err != nil {
		t.Fatalf("GetRelease(existing) error = %v", err)
	}
}

func TestGetReleaseRestoresDeletedUpstreamReleaseOnDemand(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	defer st.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/releases/puppetlabs-stdlib-1.0.0" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"slug": "puppetlabs-stdlib-1.0.0",
			"version": "1.0.0",
			"description": "stdlib 1.0.0",
			"readme": "# stdlib",
			"file_uri": "https://forge.example/v3/files/puppetlabs-stdlib-1.0.0.tar.gz",
			"file_name": "puppetlabs-stdlib-1.0.0.tar.gz",
			"file_sha256": "abc123"
		}`))
	}))
	defer upstream.Close()

	forgeProxy, err := proxy.NewForgeProxy(upstream.URL, time.Minute, 1024, &testArtifactStorage{}, "upstream-cache")
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}
	service := NewModuleService(st, &testArtifactStorage{}, "modules", forgeProxy)
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

	indexDeleteAndReindexUpstreamRelease(t, ctx, st, service, upstreamModule, "puppetlabs", "stdlib", "1.0.0")

	release, err := service.GetRelease(ctx, "puppetlabs", "stdlib", "1.0.0")
	if err != nil {
		t.Fatalf("GetRelease(on demand) error = %v", err)
	}
	if release.Source != "upstream" || release.Version != "1.0.0" || release.UpstreamFileURI == "" {
		t.Fatalf("unexpected restored release: %#v", release)
	}
	if release.DownloadURL != release.UpstreamFileURI {
		t.Fatalf("unexpected download URL: %q want %q", release.DownloadURL, release.UpstreamFileURI)
	}
	if _, err := st.GetRelease(ctx, "puppetlabs", "stdlib", "1.0.0"); err != nil {
		t.Fatalf("GetRelease(restored local) error = %v", err)
	}
}

func TestIndexUpstreamModuleRestoresReleaseAfterModuleDelete(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	defer st.Close()

	service := NewModuleService(st, &testArtifactStorage{}, "modules", nil)
	upstreamModule := proxy.UpstreamModule{
		Slug:  "puppetlabs-stdlib",
		Owner: "puppetlabs",
		Name:  "stdlib",
		CurrentRelease: proxy.UpstreamReleaseRef{
			Slug:    "puppetlabs-stdlib-1.0.0",
			Version: "1.0.0",
		},
		Releases: []proxy.UpstreamReleaseRef{
			{Slug: "puppetlabs-stdlib-1.0.0", Version: "1.0.0"},
		},
	}

	indexDeleteAndReindexUpstreamRelease(t, ctx, st, service, upstreamModule, "puppetlabs", "stdlib", "1.0.0")

	if err := st.DeleteModule(ctx, "puppetlabs", "stdlib"); err != nil {
		t.Fatalf("DeleteModule() error = %v", err)
	}
	if err := service.IndexUpstreamModule(ctx, upstreamModule); err != nil {
		t.Fatalf("IndexUpstreamModule() after module delete error = %v", err)
	}
	if _, err := st.GetRelease(ctx, "puppetlabs", "stdlib", "1.0.0"); err != nil {
		t.Fatalf("GetRelease(restored) error = %v", err)
	}
}

func indexDeleteAndReindexUpstreamRelease(
	t *testing.T,
	ctx context.Context,
	st *store.SQLiteStore,
	service *ModuleService,
	upstreamModule proxy.UpstreamModule,
	owner string,
	name string,
	version string,
) {
	t.Helper()

	if err := service.IndexUpstreamModule(ctx, upstreamModule); err != nil {
		t.Fatalf("IndexUpstreamModule() error = %v", err)
	}
	if err := st.DeleteRelease(ctx, owner, name, version); err != nil {
		t.Fatalf("DeleteRelease() error = %v", err)
	}
	if err := service.IndexUpstreamModule(ctx, upstreamModule); err != nil {
		t.Fatalf("IndexUpstreamModule() after delete error = %v", err)
	}
	if _, err := st.GetRelease(ctx, owner, name, version); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRelease(deleted) error = %v, want ErrNotFound", err)
	}
}

func TestMarkUpstreamModuleCurrentReleaseUsed(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	defer st.Close()

	service := NewModuleService(st, &testArtifactStorage{}, "modules", nil)
	upstreamModule := proxy.UpstreamModule{
		Slug:  "stm-debconf",
		Owner: "stm",
		Name:  "debconf",
		CurrentRelease: proxy.UpstreamReleaseRef{
			Slug: "stm-debconf-9.1.0",
		},
		Releases: []proxy.UpstreamReleaseRef{
			{Slug: "stm-debconf-9.1.0"},
		},
	}

	if err := service.IndexUpstreamModule(ctx, upstreamModule); err != nil {
		t.Fatalf("IndexUpstreamModule() error = %v", err)
	}
	if err := service.MarkUpstreamModuleCurrentReleaseUsed(ctx, upstreamModule); err != nil {
		t.Fatalf("MarkUpstreamModuleCurrentReleaseUsed() error = %v", err)
	}

	active, err := st.IsReleaseActive(ctx, "stm", "debconf", "9.1.0", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("IsReleaseActive() error = %v", err)
	}
	if !active {
		t.Fatal("expected upstream module current release to be active")
	}
}

func TestPublishRejectsInvalidOwner(t *testing.T) {
	t.Parallel()

	archive, err := testutil.BuildTarGz(map[string]string{
		"metadata.json": `{"name":"acme-apache","version":"1.2.3"}`,
	})
	if err != nil {
		t.Fatalf("testutil.BuildTarGz() error = %v", err)
	}

	service := NewModuleService(&testModuleStore{}, &testArtifactStorage{}, "modules", nil)

	_, err = service.Publish(context.Background(), domain.PublishModuleInput{
		Owner:     "ACME",
		Name:      "apache",
		Version:   "1.2.3",
		FileName:  "module.tar.gz",
		FileBytes: archive,
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestPublishExtractsReadme(t *testing.T) {
	t.Parallel()

	archive, err := testutil.BuildTarGz(map[string]string{
		"metadata.json":     `{"name":"acme-apache","version":"1.2.3","summary":"Apache module"}`,
		"README.md":         "# Apache\nmanaged module",
		"manifests/init.pp": "class apache {}",
	})
	if err != nil {
		t.Fatalf("testutil.BuildTarGz() error = %v", err)
	}

	moduleStore := &testModuleStore{}
	service := NewModuleService(moduleStore, &testArtifactStorage{}, "modules", nil)

	release, err := service.Publish(context.Background(), domain.PublishModuleInput{
		Owner:       "acme",
		Name:        "apache",
		Version:     "1.2.3",
		FileName:    "acme-apache-1.2.3.tar.gz",
		ContentType: "application/gzip",
		FileBytes:   archive,
	})
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	if release.Readme != "# Apache\nmanaged module" {
		t.Fatalf("unexpected readme: %q", release.Readme)
	}
}

func TestPublishUsesMetadataJSONAsSourceOfTruth(t *testing.T) {
	t.Parallel()

	archive, err := testutil.BuildTarGz(map[string]string{
		"metadata.json": `{"name":"teamname-apt","version":"2.3.4","summary":"APT module","dependencies":["stdlib"]}`,
		"README.md":     "# Apt\nmanaged module",
	})
	if err != nil {
		t.Fatalf("testutil.BuildTarGz() error = %v", err)
	}

	moduleStore := &testModuleStore{}
	artifacts := &testArtifactStorage{}
	service := NewModuleService(moduleStore, artifacts, "modules", nil)

	release, err := service.Publish(context.Background(), domain.PublishModuleInput{
		FileName:    "teamname-apt-2.3.4.tar.gz",
		ContentType: "application/gzip",
		FileBytes:   archive,
	})
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	if release.Owner != "teamname" || release.Name != "apt" || release.Version != "2.3.4" {
		t.Fatalf("unexpected release identity: owner=%s name=%s version=%s", release.Owner, release.Name, release.Version)
	}
	if release.Description != "APT module" {
		t.Fatalf("unexpected description: %q", release.Description)
	}
	if artifacts.objectPath != "modules/teamname/apt/2.3.4/teamname-apt-2.3.4.tar.gz" {
		t.Fatalf("unexpected object path: %s", artifacts.objectPath)
	}
}

func TestReadReleaseFileExtractsFileFromArchive(t *testing.T) {
	t.Parallel()

	archive, err := testutil.BuildTarGz(map[string]string{
		"teamname-apt-2.3.4/metadata.json":    `{"name":"teamname-apt","version":"2.3.4"}`,
		"teamname-apt-2.3.4/data/common.yaml": "value: 1\n",
	})
	if err != nil {
		t.Fatalf("testutil.BuildTarGz() error = %v", err)
	}

	moduleStore := &testModuleStore{
		release: domain.Release{
			Owner:       "teamname",
			Name:        "apt",
			Version:     "2.3.4",
			StoragePath: "modules/teamname/apt/2.3.4/teamname-apt-2.3.4.tar.gz",
		},
	}
	artifacts := &testArtifactStorage{
		body:        archive,
		contentType: "application/gzip",
	}
	service := NewModuleService(moduleStore, artifacts, "modules", nil)

	object, err := service.ReadReleaseFile(context.Background(), "teamname", "apt", "2.3.4", "data/common.yaml")
	if err != nil {
		t.Fatalf("ReadReleaseFile() error = %v", err)
	}
	if string(object.Body) != "value: 1\n" {
		t.Fatalf("unexpected file body: %q", string(object.Body))
	}
	if object.ContentType == "" {
		t.Fatal("expected content type to be detected")
	}
}

func TestReadReleaseFileRejectsPathTraversal(t *testing.T) {
	t.Parallel()

	archive, err := testutil.BuildTarGz(map[string]string{
		"teamname-apt-2.3.4/metadata.json": `{"name":"teamname-apt","version":"2.3.4"}`,
	})
	if err != nil {
		t.Fatalf("testutil.BuildTarGz() error = %v", err)
	}

	moduleStore := &testModuleStore{
		release: domain.Release{
			Owner:       "teamname",
			Name:        "apt",
			Version:     "2.3.4",
			StoragePath: "modules/teamname/apt/2.3.4/teamname-apt-2.3.4.tar.gz",
		},
	}
	artifacts := &testArtifactStorage{
		body:        archive,
		contentType: "application/gzip",
	}
	service := NewModuleService(moduleStore, artifacts, "modules", nil)

	_, err = service.ReadReleaseFile(context.Background(), "teamname", "apt", "2.3.4", "../secrets.yaml")
	if err == nil {
		t.Fatal("expected invalid file path error")
	}
	if err.Error() != "invalid file path" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestInspectModuleArchiveRejectsOversizedMetadata(t *testing.T) {
	t.Parallel()

	archive, err := testutil.BuildTarGz(map[string]string{
		"metadata.json": strings.Repeat("x", maxArchiveMetadataSize+1),
	})
	if err != nil {
		t.Fatalf("testutil.BuildTarGz() error = %v", err)
	}

	_, err = inspectModuleArchive(archive)
	if err == nil {
		t.Fatal("expected oversized metadata error")
	}
	if !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestInspectModuleArchiveRejectsTooManyEntries(t *testing.T) {
	t.Parallel()

	archive := buildTarGzWithEntries(t, maxArchiveEntries+1)
	_, err := inspectModuleArchive(archive)
	if err == nil {
		t.Fatal("expected entry count limit error")
	}
	if !strings.Contains(err.Error(), "more than") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReadReleaseFileRejectsOversizedExtractedFile(t *testing.T) {
	t.Parallel()

	archive, err := testutil.BuildTarGz(map[string]string{
		"teamname-apt-2.3.4/data/large.txt": strings.Repeat("x", maxArchiveFileSize+1),
	})
	if err != nil {
		t.Fatalf("testutil.BuildTarGz() error = %v", err)
	}

	moduleStore := &testModuleStore{
		release: domain.Release{
			Owner:       "teamname",
			Name:        "apt",
			Version:     "2.3.4",
			StoragePath: "modules/teamname/apt/2.3.4/teamname-apt-2.3.4.tar.gz",
		},
	}
	artifacts := &testArtifactStorage{
		body:        archive,
		contentType: "application/gzip",
	}
	service := NewModuleService(moduleStore, artifacts, "modules", nil)

	_, err = service.ReadReleaseFile(context.Background(), "teamname", "apt", "2.3.4", "data/large.txt")
	if err == nil {
		t.Fatal("expected oversized archive file error")
	}
	if !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCachedUpstreamArtifactPathHandlesAbsoluteURL(t *testing.T) {
	t.Parallel()

	path := cachedUpstreamArtifactPath("https://forgeapi.puppetlabs.com/v3/files/puppetlabs-apache-1.0.0.tar.gz")
	if path != "upstream-cache/v3/files/puppetlabs-apache-1.0.0.tar.gz" {
		t.Fatalf("unexpected cache path: %s", path)
	}
}

func buildTarGzWithEntries(t *testing.T, entries int) []byte {
	t.Helper()

	var buf bytes.Buffer
	gzipWriter := gzip.NewWriter(&buf)
	tarWriter := tar.NewWriter(gzipWriter)
	for i := 0; i < entries; i++ {
		body := []byte("x")
		header := &tar.Header{
			Name: fmt.Sprintf("module/file-%05d.txt", i),
			Mode: 0o644,
			Size: int64(len(body)),
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatalf("WriteHeader() error = %v", err)
		}
		if _, err := tarWriter.Write(body); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("tar Close() error = %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("gzip Close() error = %v", err)
	}

	return buf.Bytes()
}

func TestNormalizeArchivePathStripsTopLevelModuleDir(t *testing.T) {
	t.Parallel()

	normalized := normalizeArchivePath("/teamname-apt-2.3.4/data/common.yaml")
	if normalized != "data/common.yaml" {
		t.Fatalf("unexpected normalized path: %s", normalized)
	}
}
