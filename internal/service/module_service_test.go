package service

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5" // #nosec G501 -- Puppet Forge compatibility checksum asserted in tests.
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
	"github.com/zxzharmlesszxz/puppet-forge/internal/proxy"
	artifactstorage "github.com/zxzharmlesszxz/puppet-forge/internal/storage"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
	"github.com/zxzharmlesszxz/puppet-forge/internal/testutil"
)

type testModuleStore struct {
	moduleLock sync.Mutex
	lockCalls  atomic.Int32
	module     domain.Module
	release    domain.Release
}

type countingReleaseUsageStore struct {
	*store.SQLiteStore
	markCalls atomic.Int32
	failNext  atomic.Bool
}

func (s *countingReleaseUsageStore) MarkReleaseUsed(ctx context.Context, owner, name, version string) error {
	s.markCalls.Add(1)
	if s.failNext.CompareAndSwap(true, false) {
		return errors.New("record usage")
	}
	return s.SQLiteStore.MarkReleaseUsed(ctx, owner, name, version)
}

type cancelAfterListingStore struct {
	*testModuleStore
	cancel context.CancelFunc
}

func (s *cancelAfterListingStore) ListUpstreamModules(_ context.Context, _ int) ([]domain.Module, error) {
	s.cancel()
	return nil, nil
}

type refreshMarkerContextStore struct {
	*store.SQLiteStore
	markerContextErr chan error
}

func (s *refreshMarkerContextStore) MarkUpstreamModuleRefreshAttempt(ctx context.Context, owner, name string, attemptedAt time.Time) error {
	s.markerContextErr <- ctx.Err()
	return s.SQLiteStore.MarkUpstreamModuleRefreshAttempt(ctx, owner, name, attemptedAt)
}

func (s *testModuleStore) Ping(_ context.Context) error {
	return nil
}

func (s *testModuleStore) LockModule(_ context.Context, _, _ string) (store.ModuleUnlock, error) {
	s.moduleLock.Lock()
	s.lockCalls.Add(1)
	return func() error {
		s.moduleLock.Unlock()
		return nil
	}, nil
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

func (s *testModuleStore) CreateReleaseIfAbsent(ctx context.Context, release domain.Release) (domain.Release, error) {
	if _, err := s.GetRelease(ctx, release.Owner, release.Name, release.Version); err == nil {
		return domain.Release{}, store.ErrConflict
	}
	return s.CreateRelease(ctx, release)
}

func (s *testModuleStore) ListModules(_ context.Context, _ int) ([]domain.Module, error) {
	return []domain.Module{s.module}, nil
}

func (s *testModuleStore) ListModulesPage(_ context.Context, _, _ int) ([]domain.Module, int, error) {
	return []domain.Module{s.module}, 1, nil
}

func (s *testModuleStore) ListModulesPageFiltered(_ context.Context, _ []string, _ string, _, _ int) ([]domain.Module, int, error) {
	return []domain.Module{s.module}, 1, nil
}

func (s *testModuleStore) ListModulesPagePrioritized(_ context.Context, _, _ []string, _ string, _, _ int) ([]domain.Module, int, error) {
	return []domain.Module{s.module}, 1, nil
}

func (s *testModuleStore) CountModulesByOwner(_ context.Context, _ []string) (map[string]int, error) {
	return map[string]int{s.module.Owner: 1}, nil
}

func (s *testModuleStore) CountUpstreamModulesByOwner(_ context.Context) (map[string]int, error) {
	if s.release.Source != "upstream" {
		return map[string]int{}, nil
	}
	return map[string]int{s.module.Owner: 1}, nil
}

func (s *testModuleStore) GetModule(_ context.Context, owner, name string) (domain.Module, error) {
	if s.module.Owner != owner || s.module.Name != name {
		return domain.Module{}, store.ErrNotFound
	}
	return s.module, nil
}

func (s *testModuleStore) GetRelease(_ context.Context, owner, name, version string) (domain.Release, error) {
	if s.release.Owner != owner || s.release.Name != name || s.release.Version != version {
		return domain.Release{}, store.ErrNotFound
	}
	return s.release, nil
}

func (s *testModuleStore) ListUpstreamModules(_ context.Context, _ int) ([]domain.Module, error) {
	return nil, nil
}

func (s *testModuleStore) MarkUpstreamModuleRefreshAttempt(context.Context, string, string, time.Time) error {
	return nil
}

func (s *testModuleStore) DeleteModule(_ context.Context, _, _ string) error {
	return nil
}

func (s *testModuleStore) DeleteModuleIfEmpty(_ context.Context, _, _ string) error {
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
	mu          sync.Mutex
	objectPath  string
	contentType string
	body        []byte
	objects     map[string]testStoredObject
	objectTimes map[string]time.Time
	uploadErr   error
	deleteErr   error
	uploads     int
	deletes     int
	opens       int
}

type testStoredObject struct {
	Body        []byte
	ContentType string
}

type readinessFailureStorage struct {
	*testArtifactStorage
	err error
}

func (s *readinessFailureStorage) Stat(context.Context, string) (artifactstorage.ObjectAttrs, error) {
	return artifactstorage.ObjectAttrs{}, s.err
}

func (s *readinessFailureStorage) UploadReaderIfAbsent(context.Context, string, string, io.Reader) (bool, error) {
	return false, s.err
}

func newSQLiteModuleService(t *testing.T) (*ModuleService, *testArtifactStorage) {
	t.Helper()
	st, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	t.Cleanup(st.Close)
	artifacts := &testArtifactStorage{}
	return NewModuleService(st, artifacts, "modules", nil), artifacts
}

func newUpstreamModuleService(
	t *testing.T,
	modules store.ModuleStore,
	artifacts artifactstorage.ArtifactStorage,
	handler http.Handler,
) *ModuleService {
	t.Helper()

	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	forgeProxy, err := proxy.NewForgeProxy(
		upstream.URL,
		time.Minute,
		1024,
		artifacts,
		"upstream-cache",
		proxy.WithHTTPClient(upstream.Client()),
		proxy.WithPrivateNetworks(),
	)
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}
	return NewModuleService(modules, artifacts, "modules", forgeProxy)
}

func newReleaseUsageTestService(t *testing.T) (*countingReleaseUsageStore, *ModuleService) {
	t.Helper()

	ctx := t.Context()
	st, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	t.Cleanup(st.Close)

	module, err := st.UpsertModule(ctx, "teamname", "apache")
	if err != nil {
		t.Fatalf("UpsertModule() error = %v", err)
	}
	if _, err := st.CreateRelease(ctx, domain.Release{
		ID: "release-1", ModuleID: module.ID, Owner: module.Owner, Name: module.Name, Version: "1.2.3",
		Source: "upstream", FileName: "teamname-apache-1.2.3.tar.gz", ContentType: "application/gzip",
	}); err != nil {
		t.Fatalf("CreateRelease() error = %v", err)
	}

	countingStore := &countingReleaseUsageStore{SQLiteStore: st}
	return countingStore, NewModuleService(countingStore, &testArtifactStorage{}, "modules", nil)
}

func (s *testArtifactStorage) Upload(_ context.Context, objectPath string, contentType string, body []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.uploadErr != nil {
		return s.uploadErr
	}
	s.uploads++
	s.storeObjectLocked(objectPath, contentType, body)
	return nil
}

func (s *testArtifactStorage) storeObjectLocked(objectPath string, contentType string, body []byte) {
	if s.objects == nil {
		s.objects = make(map[string]testStoredObject)
	}
	if s.objectTimes == nil {
		s.objectTimes = make(map[string]time.Time)
	}
	s.objectPath = objectPath
	s.contentType = contentType
	s.body = append([]byte(nil), body...)
	s.objects[objectPath] = testStoredObject{Body: append([]byte(nil), body...), ContentType: contentType}
	s.objectTimes[objectPath] = time.Now().UTC()
}

func (s *testArtifactStorage) UploadIfAbsent(_ context.Context, objectPath string, contentType string, body []byte) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.uploadErr != nil {
		return false, s.uploadErr
	}
	if _, exists := s.objects[objectPath]; exists || s.objectPath == objectPath {
		return false, nil
	}
	s.uploads++
	s.storeObjectLocked(objectPath, contentType, body)
	return true, nil
}

func (s *testArtifactStorage) UploadReaderIfAbsent(ctx context.Context, objectPath string, contentType string, body io.Reader) (bool, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return false, err
	}
	return s.UploadIfAbsent(ctx, objectPath, contentType, data)
}

func (s *testArtifactStorage) Delete(_ context.Context, objectPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.deletes++
	delete(s.objects, objectPath)
	delete(s.objectTimes, objectPath)
	if s.objectPath == objectPath {
		s.objectPath = ""
		s.contentType = ""
		s.body = nil
	}
	return nil
}

func (s *testArtifactStorage) Exists(_ context.Context, objectPath string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, exists := s.objects[objectPath]
	return exists, nil
}

func (s *testArtifactStorage) Open(_ context.Context, objectPath string) (artifactstorage.ObjectReader, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opens++
	if object, exists := s.objects[objectPath]; exists {
		body := append([]byte(nil), object.Body...)
		return artifactstorage.ObjectReader{Body: io.NopCloser(bytes.NewReader(body)), ContentType: object.ContentType, Size: int64(len(body))}, nil
	}
	if s.objects == nil && len(s.body) > 0 {
		body := append([]byte(nil), s.body...)
		return artifactstorage.ObjectReader{Body: io.NopCloser(bytes.NewReader(body)), ContentType: s.contentType, Size: int64(len(body))}, nil
	}
	return artifactstorage.ObjectReader{}, artifactstorage.ErrObjectNotFound
}

func (s *testArtifactStorage) Stat(_ context.Context, objectPath string) (artifactstorage.ObjectAttrs, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	object, exists := s.objects[objectPath]
	if !exists {
		return artifactstorage.ObjectAttrs{}, artifactstorage.ErrObjectNotFound
	}
	return artifactstorage.ObjectAttrs{Size: int64(len(object.Body))}, nil
}

func (s *testArtifactStorage) PublicURL(objectPath string) string {
	return "https://example.invalid/" + objectPath
}

func (s *testArtifactStorage) ListObjects(_ context.Context, prefix string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var paths []string
	for objectPath := range s.objects {
		if strings.HasPrefix(objectPath, prefix) {
			paths = append(paths, objectPath)
		}
	}
	slices.Sort(paths)
	return paths, nil
}

func (s *testArtifactStorage) ListObjectMetadata(_ context.Context, prefix string) ([]artifactstorage.ListedObject, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var objects []artifactstorage.ListedObject
	for objectPath, object := range s.objects {
		if strings.HasPrefix(objectPath, prefix) {
			objects = append(objects, artifactstorage.ListedObject{
				Path:      objectPath,
				Size:      int64(len(object.Body)),
				UpdatedAt: s.objectTimes[objectPath],
			})
		}
	}
	slices.SortFunc(objects, func(a, b artifactstorage.ListedObject) int { return strings.Compare(a.Path, b.Path) })
	return objects, nil
}

func (s *testArtifactStorage) IterateObjectMetadata(ctx context.Context, prefix string, visit func(artifactstorage.ListedObject) error) error {
	objects, err := s.ListObjectMetadata(ctx, prefix)
	if err != nil {
		return err
	}
	for _, object := range objects {
		if err := visit(object); err != nil {
			return err
		}
	}
	return nil
}

func TestReadyChecksMetadataAndArtifactStorage(t *testing.T) {
	t.Parallel()

	st, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	t.Cleanup(st.Close)
	if err := NewModuleService(st, &testArtifactStorage{}, "modules", nil).Ready(context.Background()); err != nil {
		t.Fatalf("Ready() error = %v", err)
	}

	storageErr := errors.New("artifact backend unavailable")
	artifacts := &readinessFailureStorage{testArtifactStorage: &testArtifactStorage{}, err: storageErr}
	err = NewModuleService(st, artifacts, "modules", nil).Ready(context.Background())
	if !errors.Is(err, storageErr) {
		t.Fatalf("Ready() error = %v, want artifact backend error", err)
	}
}

func TestReconcileArtifactsReportsAndRepairsOnlyOrphans(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	defer st.Close()
	artifacts := &testArtifactStorage{}
	moduleService := NewModuleService(st, artifacts, "modules", nil)

	first, err := moduleService.Publish(ctx, domain.PublishModuleInput{
		Owner: "teamname", FileName: "first.tar.gz", FileBytes: buildPublishArchive(t, "teamname-first", "1.0.0", "first"),
	})
	if err != nil {
		t.Fatalf("Publish(first) error = %v", err)
	}
	second, err := moduleService.Publish(ctx, domain.PublishModuleInput{
		Owner: "teamname", FileName: "second.tar.gz", FileBytes: buildPublishArchive(t, "teamname-second", "2.0.0", "second"),
	})
	if err != nil {
		t.Fatalf("Publish(second) error = %v", err)
	}
	if err := artifacts.Delete(ctx, first.StoragePath); err != nil {
		t.Fatalf("Delete(first artifact) error = %v", err)
	}
	artifacts.mu.Lock()
	artifacts.storeObjectLocked(second.StoragePath, "application/gzip", []byte("corrupt"))
	artifacts.storeObjectLocked("modules/orphan/archive.tar.gz", "application/gzip", []byte("orphan"))
	artifacts.mu.Unlock()

	report, err := moduleService.ReconcileArtifacts(ctx, false)
	if err != nil {
		t.Fatalf("ReconcileArtifacts(report) error = %v", err)
	}
	if !slices.Equal(report.MissingObjects, []string{first.StoragePath}) {
		t.Fatalf("missing objects = %#v", report.MissingObjects)
	}
	if !slices.Equal(report.CorruptObjects, []string{second.StoragePath}) {
		t.Fatalf("corrupt objects = %#v", report.CorruptObjects)
	}
	if !slices.Equal(report.OrphanObjects, []string{"modules/orphan/archive.tar.gz"}) {
		t.Fatalf("orphan objects = %#v", report.OrphanObjects)
	}
	if exists, _ := artifacts.Exists(ctx, "modules/orphan/archive.tar.gz"); !exists {
		t.Fatal("report mode deleted orphan")
	}

	repaired, err := moduleService.ReconcileArtifacts(ctx, true)
	if err != nil {
		t.Fatalf("ReconcileArtifacts(repair) error = %v", err)
	}
	if !slices.Equal(repaired.DeletedOrphans, []string{"modules/orphan/archive.tar.gz"}) {
		t.Fatalf("deleted orphans = %#v", repaired.DeletedOrphans)
	}
	if exists, _ := artifacts.Exists(ctx, "modules/orphan/archive.tar.gz"); exists {
		t.Fatal("repair mode kept orphan")
	}
	if _, err := st.GetRelease(ctx, first.Owner, first.Name, first.Version); err != nil {
		t.Fatalf("repair mode changed missing release metadata: %v", err)
	}
}

type reconciliationCommitStorage struct {
	*testArtifactStorage
	onList func()
}

func (s *reconciliationCommitStorage) ListObjectMetadata(ctx context.Context, prefix string) ([]artifactstorage.ListedObject, error) {
	objects, err := s.testArtifactStorage.ListObjectMetadata(ctx, prefix)
	if err == nil && s.onList != nil {
		s.onList()
		s.onList = nil
	}
	return objects, err
}

func TestReconcileArtifactsRepairRechecksReferencesBeforeDelete(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	t.Cleanup(st.Close)

	const objectPath = "modules/teamname/module/1.0.0/checksum.tar.gz"
	baseStorage := &testArtifactStorage{}
	if err := baseStorage.Upload(ctx, objectPath, "application/gzip", []byte("archive")); err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	baseStorage.mu.Lock()
	baseStorage.objectTimes[objectPath] = time.Now().UTC().Add(-time.Hour)
	baseStorage.mu.Unlock()

	artifacts := &reconciliationCommitStorage{testArtifactStorage: baseStorage}
	artifacts.onList = func() {
		module, createErr := st.UpsertModule(ctx, "teamname", "module")
		if createErr != nil {
			t.Fatalf("UpsertModule() error = %v", createErr)
		}
		_, createErr = st.CreateRelease(ctx, domain.Release{
			ID: "release", ModuleID: module.ID, Owner: module.Owner, Name: module.Name,
			Version: "1.0.0", Source: "local", StoragePath: objectPath,
		})
		if createErr != nil {
			t.Fatalf("CreateRelease() error = %v", createErr)
		}
	}

	report, err := NewModuleService(st, artifacts, "modules", nil).ReconcileArtifacts(ctx, true)
	if err != nil {
		t.Fatalf("ReconcileArtifacts() error = %v", err)
	}
	if len(report.OrphanObjects) != 0 || len(report.DeletedOrphans) != 0 {
		t.Fatalf("report = %#v, want concurrently referenced object excluded", report)
	}
	if exists, _ := artifacts.Exists(ctx, objectPath); !exists {
		t.Fatal("repair deleted an object referenced after the initial SQL snapshot")
	}
}

func TestReconciliationObjectIdentity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		prefix     string
		objectPath string
		owner      string
		module     string
		ok         bool
	}{
		{name: "content addressed", prefix: "modules/", objectPath: "modules/teamname/module/1.0.0/checksum.tar.gz", owner: "teamname", module: "module", ok: true},
		{name: "legacy path", prefix: "modules/", objectPath: "modules/teamname/archive.tar.gz", owner: "teamname", module: "archive.tar.gz", ok: true},
		{name: "outside prefix", prefix: "modules/", objectPath: "other/teamname/module/archive.tar.gz"},
		{name: "missing module", prefix: "modules/", objectPath: "modules/teamname"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owner, module, ok := reconciliationObjectIdentity(tt.prefix, tt.objectPath)
			if owner != tt.owner || module != tt.module || ok != tt.ok {
				t.Fatalf("reconciliationObjectIdentity() = (%q, %q, %t), want (%q, %q, %t)", owner, module, ok, tt.owner, tt.module, tt.ok)
			}
		})
	}
}

func (s *testArtifactStorage) objectCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.objects)
}

func (s *testArtifactStorage) objectBody(objectPath string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.objects[objectPath].Body...)
}

type failingCreateReleaseStore struct {
	store.ModuleStore
	err error
}

func (s failingCreateReleaseStore) CreateReleaseIfAbsent(context.Context, domain.Release) (domain.Release, error) {
	return domain.Release{}, s.err
}

type failGetAfterCreateStore struct {
	*testModuleStore
	failGet atomic.Bool
}

func (s *failGetAfterCreateStore) CreateRelease(ctx context.Context, release domain.Release) (domain.Release, error) {
	saved, err := s.testModuleStore.CreateRelease(ctx, release)
	if err == nil {
		s.failGet.Store(true)
	}
	return saved, err
}

func (s *failGetAfterCreateStore) GetRelease(ctx context.Context, owner, name, version string) (domain.Release, error) {
	if s.failGet.Load() {
		return domain.Release{}, errors.New("transient release read failure")
	}
	return s.testModuleStore.GetRelease(ctx, owner, name, version)
}

func TestPublish(t *testing.T) {
	t.Parallel()

	archive, err := testutil.BuildTarGz(map[string]string{
		"acme-apache-1.2.3/metadata.json": `{"name":"acme-apache","version":"1.2.3","summary":"Apache module"}`,
	})
	if err != nil {
		t.Fatalf("testutil.BuildTarGz() error = %v", err)
	}

	moduleStore := &testModuleStore{}
	artifacts := &testArtifactStorage{}
	service := NewModuleService(moduleStore, artifacts, "modules", nil)

	release, err := service.Publish(context.Background(), domain.PublishModuleInput{
		Owner:       "acme",
		Name:        "ignored-name",
		Version:     "9.9.9",
		Description: "ignored description",
		FileName:    "acme-apache-1.2.3.tar.gz",
		ContentType: "application/gzip",
		FileBytes:   archive,
		Metadata: map[string]any{
			"source": "ignored",
		},
	})
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	wantObjectPath := "modules/acme/apache/1.2.3/" + release.SHA256 + ".tar.gz"
	if artifacts.objectPath != wantObjectPath {
		t.Fatalf("unexpected object path: %s", artifacts.objectPath)
	}

	if release.FileName != "acme-apache-1.2.3.tar.gz" {
		t.Fatalf("unexpected release file name: %s", release.FileName)
	}
	if release.Name != "apache" || release.Version != "1.2.3" || release.Description != "Apache module" {
		t.Fatalf("archive metadata was not authoritative: %#v", release)
	}
	if _, exists := release.Metadata["source"]; exists {
		t.Fatalf("manual metadata override was preserved: %#v", release.Metadata)
	}

	if release.DownloadURL != "https://example.invalid/"+wantObjectPath {
		t.Fatalf("unexpected download url: %s", release.DownloadURL)
	}

	if release.SHA256 == "" {
		t.Fatal("expected SHA256 to be populated")
	}
	if release.MD5 == "" {
		t.Fatal("expected MD5 to be populated")
	}
}

func TestPublishRetryWithSameArchiveIsIdempotent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	moduleService, artifacts := newSQLiteModuleService(t)
	archive := buildPublishArchive(t, "teamname-module", "1.0.0", "same")

	first, err := moduleService.Publish(ctx, domain.PublishModuleInput{Owner: "teamname", FileName: "module.tar.gz", FileBytes: archive})
	if err != nil {
		t.Fatalf("first Publish() error = %v", err)
	}
	second, err := moduleService.Publish(ctx, domain.PublishModuleInput{Owner: "teamname", FileName: "retry.tar.gz", FileBytes: archive})
	if err != nil {
		t.Fatalf("second Publish() error = %v", err)
	}
	if second.ID != first.ID || second.SHA256 != first.SHA256 || second.StoragePath != first.StoragePath {
		t.Fatalf("retry returned a different release: first=%#v second=%#v", first, second)
	}
	if artifacts.uploads != 1 || artifacts.objectCount() != 1 {
		t.Fatalf("idempotent retry changed storage: uploads=%d objects=%d", artifacts.uploads, artifacts.objectCount())
	}
}

func TestPublishRetryRejectsMissingOrCorruptRegisteredArtifact(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name   string
		mutate func(*testArtifactStorage, string)
	}{
		{
			name: "missing",
			mutate: func(artifacts *testArtifactStorage, objectPath string) {
				artifacts.mu.Lock()
				defer artifacts.mu.Unlock()
				delete(artifacts.objects, objectPath)
				artifacts.objectPath = ""
				artifacts.body = nil
			},
		},
		{
			name: "corrupt",
			mutate: func(artifacts *testArtifactStorage, objectPath string) {
				artifacts.mu.Lock()
				defer artifacts.mu.Unlock()
				artifacts.storeObjectLocked(objectPath, "application/gzip", []byte("corrupt"))
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			moduleService, artifacts := newSQLiteModuleService(t)
			archive := buildPublishArchive(t, "teamname-module", "1.0.0", "same")

			release, err := moduleService.Publish(ctx, domain.PublishModuleInput{Owner: "teamname", FileName: "module.tar.gz", FileBytes: archive})
			if err != nil {
				t.Fatalf("first Publish() error = %v", err)
			}
			testCase.mutate(artifacts, release.StoragePath)

			_, err = moduleService.Publish(ctx, domain.PublishModuleInput{Owner: "teamname", FileName: "retry.tar.gz", FileBytes: archive})
			if err == nil || !strings.Contains(err.Error(), "verify registered artifact") {
				t.Fatalf("retry Publish() error = %v, want registered-artifact integrity failure", err)
			}
		})
	}
}

func TestPublishRejectsDifferentBytesWithoutOverwritingArtifact(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	defer st.Close()
	artifacts := &testArtifactStorage{}
	moduleService := NewModuleService(st, artifacts, "modules", nil)
	firstArchive := buildPublishArchive(t, "teamname-module", "1.0.0", "first")
	secondArchive := buildPublishArchive(t, "teamname-module", "1.0.0", "second")

	release, err := moduleService.Publish(ctx, domain.PublishModuleInput{Owner: "teamname", FileName: "first.tar.gz", FileBytes: firstArchive})
	if err != nil {
		t.Fatalf("first Publish() error = %v", err)
	}
	_, err = moduleService.Publish(ctx, domain.PublishModuleInput{Owner: "teamname", FileName: "second.tar.gz", FileBytes: secondArchive})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second Publish() error = %v, want ErrConflict", err)
	}
	if got := artifacts.objectBody(release.StoragePath); !bytes.Equal(got, firstArchive) {
		t.Fatal("duplicate publish overwrote the existing artifact")
	}
	if artifacts.uploads != 1 || artifacts.objectCount() != 1 {
		t.Fatalf("conflicting publish changed storage: uploads=%d objects=%d", artifacts.uploads, artifacts.objectCount())
	}
}

func TestConcurrentPublishSameVersionIsDeterministic(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	defer st.Close()
	artifacts := &testArtifactStorage{}
	moduleService := NewModuleService(st, artifacts, "modules", nil)
	archives := [][]byte{
		buildPublishArchive(t, "teamname-module", "1.0.0", "first"),
		buildPublishArchive(t, "teamname-module", "1.0.0", "second"),
	}

	results := make(chan error, len(archives))
	var wg sync.WaitGroup
	for _, archive := range archives {
		wg.Go(func() {
			_, publishErr := moduleService.Publish(ctx, domain.PublishModuleInput{Owner: "teamname", FileName: "module.tar.gz", FileBytes: archive})
			results <- publishErr
		})
	}
	wg.Wait()
	close(results)

	succeeded := 0
	conflicted := 0
	for publishErr := range results {
		switch {
		case publishErr == nil:
			succeeded++
		case errors.Is(publishErr, store.ErrConflict):
			conflicted++
		default:
			t.Fatalf("unexpected concurrent Publish() error = %v", publishErr)
		}
	}
	if succeeded != 1 || conflicted != 1 || artifacts.objectCount() != 1 {
		t.Fatalf("unexpected concurrent result: success=%d conflict=%d objects=%d", succeeded, conflicted, artifacts.objectCount())
	}
	release, err := st.GetRelease(ctx, "teamname", "module", "1.0.0")
	if err != nil {
		t.Fatalf("GetRelease() error = %v", err)
	}
	storedBody := artifacts.objectBody(release.StoragePath)
	sha := sha256.Sum256(storedBody)
	if release.SHA256 != hex.EncodeToString(sha[:]) {
		t.Fatalf("stored checksum %s does not match object bytes", release.SHA256)
	}
}

func TestPublishRejectsCorruptPreexistingContentAddressedObject(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	defer st.Close()
	archive := buildPublishArchive(t, "teamname-module", "1.0.0", "payload")
	sha := sha256.Sum256(archive)
	objectPath := "modules/teamname/module/1.0.0/" + hex.EncodeToString(sha[:]) + ".tar.gz"
	artifacts := &testArtifactStorage{}
	artifacts.storeObjectLocked(objectPath, "application/gzip", []byte("corrupt object"))
	moduleService := NewModuleService(st, artifacts, "modules", nil)

	_, publishErr := moduleService.Publish(ctx, domain.PublishModuleInput{Owner: "teamname", FileName: "module.tar.gz", FileBytes: archive})
	if publishErr == nil || !strings.Contains(publishErr.Error(), "integrity mismatch") {
		t.Fatalf("Publish() error = %v, want existing-object integrity failure", publishErr)
	}
	if _, getErr := st.GetModule(ctx, "teamname", "module"); !errors.Is(getErr, store.ErrNotFound) {
		t.Fatalf("failed publish left module metadata: %v", getErr)
	}
	if got := artifacts.objectBody(objectPath); string(got) != "corrupt object" {
		t.Fatalf("failed publish mutated preexisting object: %q", got)
	}
}

func TestPublishFailureDoesNotLeaveArtifactOrEmptyModule(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	defer st.Close()
	archive := buildPublishArchive(t, "teamname-module", "1.0.0", "payload")

	t.Run("upload failure", func(t *testing.T) {
		artifacts := &testArtifactStorage{uploadErr: errors.New("upload failed")}
		moduleService := NewModuleService(st, artifacts, "modules", nil)
		_, publishErr := moduleService.Publish(ctx, domain.PublishModuleInput{Owner: "teamname", FileName: "module.tar.gz", FileBytes: archive})
		if publishErr == nil {
			t.Fatal("expected upload failure")
		}
		if _, getErr := st.GetModule(ctx, "teamname", "module"); !errors.Is(getErr, store.ErrNotFound) {
			t.Fatalf("upload failure left a module: %v", getErr)
		}
	})

	t.Run("release insert failure", func(t *testing.T) {
		artifacts := &testArtifactStorage{}
		moduleService := NewModuleService(failingCreateReleaseStore{ModuleStore: st, err: errors.New("insert failed")}, artifacts, "modules", nil)
		_, publishErr := moduleService.Publish(ctx, domain.PublishModuleInput{Owner: "teamname", FileName: "module.tar.gz", FileBytes: archive})
		if publishErr == nil {
			t.Fatal("expected release insert failure")
		}
		if artifacts.objectCount() != 0 {
			t.Fatalf("release insert failure left %d artifact(s)", artifacts.objectCount())
		}
		if _, getErr := st.GetModule(ctx, "teamname", "module"); !errors.Is(getErr, store.ErrNotFound) {
			t.Fatalf("release insert failure left an empty module: %v", getErr)
		}
	})
}

func TestDeleteReleaseQueuesArtifactAndRetriesStorageFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	defer st.Close()
	artifacts := &testArtifactStorage{}
	moduleService := NewModuleService(st, artifacts, "modules", nil)
	release, err := moduleService.Publish(ctx, domain.PublishModuleInput{
		Owner: "teamname", FileName: "module.tar.gz", FileBytes: buildPublishArchive(t, "teamname-module", "1.0.0", "payload"),
	})
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	artifacts.deleteErr = errors.New("delete failed")
	if err := moduleService.DeleteRelease(ctx, "teamname", "module", "1.0.0"); err != nil {
		t.Fatalf("DeleteRelease() error = %v", err)
	}
	if _, err := st.GetRelease(ctx, "teamname", "module", "1.0.0"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("DeleteRelease() metadata error = %v, want ErrNotFound", err)
	}
	if len(artifacts.objectBody(release.StoragePath)) == 0 {
		t.Fatal("queued artifact disappeared before cleanup")
	}
	_, err = moduleService.ProcessArtifactDeletions(ctx, 10)
	if err == nil {
		t.Fatal("expected queued storage delete failure")
	}
	if len(artifacts.objectBody(release.StoragePath)) == 0 {
		t.Fatal("failed cleanup removed the queued artifact")
	}
	pending, err := moduleService.CountArtifactDeletions(ctx)
	if err != nil {
		t.Fatalf("CountArtifactDeletions() after failure error = %v", err)
	}
	if pending != 1 {
		t.Fatalf("pending artifact deletions after failure = %d, want 1", pending)
	}

	artifacts.deleteErr = nil
	if err := st.DeferArtifactDeletion(ctx, release.StoragePath, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("make artifact deletion retryable: %v", err)
	}
	result, err := moduleService.ProcessArtifactDeletions(ctx, 10)
	if err != nil {
		t.Fatalf("ProcessArtifactDeletions() retry error = %v", err)
	}
	if result.Attempted != 1 || result.Deleted != 1 || result.Failed != 0 || result.Pending != 0 {
		t.Fatalf("successful cleanup result = %#v", result)
	}
	if artifacts.objectCount() != 0 {
		t.Fatalf("artifact cleanup left %d artifact(s)", artifacts.objectCount())
	}
	deletions, err := st.ListArtifactDeletions(ctx, 10)
	if err != nil {
		t.Fatalf("ListArtifactDeletions() error = %v", err)
	}
	if len(deletions) != 0 {
		t.Fatalf("completed cleanup left queue entries: %#v", deletions)
	}
}

func TestArtifactDeletionPreservesRepublishedObject(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	moduleService, artifacts := newSQLiteModuleService(t)
	archive := buildPublishArchive(t, "teamname-module", "1.0.0", "payload")
	first, err := moduleService.Publish(ctx, domain.PublishModuleInput{Owner: "teamname", FileName: "module.tar.gz", FileBytes: archive})
	if err != nil {
		t.Fatalf("Publish(first) error = %v", err)
	}
	if err := moduleService.DeleteRelease(ctx, "teamname", "module", "1.0.0"); err != nil {
		t.Fatalf("DeleteRelease() error = %v", err)
	}
	second, err := moduleService.Publish(ctx, domain.PublishModuleInput{Owner: "teamname", FileName: "module.tar.gz", FileBytes: archive})
	if err != nil {
		t.Fatalf("Publish(second) error = %v", err)
	}
	if second.StoragePath != first.StoragePath {
		t.Fatalf("republished storage path = %q, want %q", second.StoragePath, first.StoragePath)
	}

	result, err := moduleService.ProcessArtifactDeletions(ctx, 10)
	if err != nil {
		t.Fatalf("ProcessArtifactDeletions() error = %v", err)
	}
	if result.Attempted != 1 || result.Canceled != 1 || result.Deleted != 0 || result.Pending != 0 {
		t.Fatalf("cleanup result = %#v", result)
	}
	if len(artifacts.objectBody(first.StoragePath)) == 0 {
		t.Fatal("cleanup deleted the republished artifact")
	}
}

func TestDeleteModuleDeletesAllLocalArtifacts(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	defer st.Close()
	artifacts := &testArtifactStorage{}
	moduleService := NewModuleService(st, artifacts, "modules", nil)
	for _, version := range []string{"1.0.0", "2.0.0"} {
		_, err := moduleService.Publish(ctx, domain.PublishModuleInput{
			Owner: "teamname", FileName: "module.tar.gz", FileBytes: buildPublishArchive(t, "teamname-module", version, version),
		})
		if err != nil {
			t.Fatalf("Publish(%s) error = %v", version, err)
		}
	}
	if artifacts.objectCount() != 2 {
		t.Fatalf("expected two artifacts before delete, got %d", artifacts.objectCount())
	}

	if err := moduleService.DeleteModule(ctx, "teamname", "module"); err != nil {
		t.Fatalf("DeleteModule() error = %v", err)
	}
	result, err := moduleService.ProcessArtifactDeletions(ctx, 10)
	if err != nil {
		t.Fatalf("ProcessArtifactDeletions() error = %v", err)
	}
	if result.Attempted != 2 || result.Deleted != 2 {
		t.Fatalf("cleanup result = %#v", result)
	}
	if artifacts.objectCount() != 0 {
		t.Fatalf("DeleteModule() left %d artifact(s)", artifacts.objectCount())
	}
	if _, err := st.GetModule(ctx, "teamname", "module"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetModule() error = %v, want ErrNotFound", err)
	}
}

func TestDeleteUpstreamReleaseDoesNotDeleteSharedCacheArtifact(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	defer st.Close()
	module, err := st.UpsertModule(ctx, "puppetlabs", "stdlib")
	if err != nil {
		t.Fatalf("UpsertModule() error = %v", err)
	}
	_, err = st.CreateRelease(ctx, domain.Release{
		ID: "upstream-release", ModuleID: module.ID, Owner: "puppetlabs", Name: "stdlib", Source: "upstream",
		Version: "9.0.0", FileName: "puppetlabs-stdlib-9.0.0.tar.gz", ContentType: "application/gzip",
		SHA256: "upstream", StoragePath: "upstream-cache/v3/files/puppetlabs-stdlib-9.0.0.tar.gz",
	})
	if err != nil {
		t.Fatalf("CreateRelease() error = %v", err)
	}
	artifacts := &testArtifactStorage{}
	if err := artifacts.Upload(ctx, "upstream-cache/v3/files/puppetlabs-stdlib-9.0.0.tar.gz", "application/gzip", []byte("shared")); err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	moduleService := NewModuleService(st, artifacts, "modules", nil)

	if err := moduleService.DeleteRelease(ctx, "puppetlabs", "stdlib", "9.0.0"); err != nil {
		t.Fatalf("DeleteRelease() error = %v", err)
	}
	if artifacts.objectCount() != 1 || artifacts.deletes != 0 {
		t.Fatalf("upstream metadata delete changed shared cache: objects=%d deletes=%d", artifacts.objectCount(), artifacts.deletes)
	}
}

func buildPublishArchive(t *testing.T, moduleName, version, marker string) []byte {
	t.Helper()
	archive, err := testutil.BuildTarGz(map[string]string{
		moduleName + "-" + version + "/metadata.json": fmt.Sprintf(`{"name":%q,"version":%q}`, moduleName, version),
		moduleName + "-" + version + "/payload.txt":   marker,
	})
	if err != nil {
		t.Fatalf("BuildTarGz() error = %v", err)
	}
	return archive
}

func TestRefreshCachedUpstreamModulesUsesBoundedConcurrency(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	t.Cleanup(st.Close)

	const moduleCount = 12
	const concurrency = 3
	for i := range moduleCount {
		name := fmt.Sprintf("module_%02d", i)
		module, err := st.UpsertModule(ctx, "teamname", name)
		if err != nil {
			t.Fatalf("UpsertModule(%s) error = %v", name, err)
		}
		_, err = st.CreateRelease(ctx, domain.Release{
			ID:          "release-" + name,
			ModuleID:    module.ID,
			Owner:       module.Owner,
			Name:        module.Name,
			Version:     "1.0.0",
			Source:      "upstream",
			FileName:    module.Owner + "-" + module.Name + "-1.0.0.tar.gz",
			ContentType: "application/gzip",
		})
		if err != nil {
			t.Fatalf("CreateRelease(%s) error = %v", name, err)
		}
	}

	var requests atomic.Int32
	var active atomic.Int32
	var maximum atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/v3/modules/teamname-"), "/")
		current := active.Add(1)
		defer active.Add(-1)
		for observed := maximum.Load(); current > observed && !maximum.CompareAndSwap(observed, current); observed = maximum.Load() {
		}
		requests.Add(1)
		time.Sleep(25 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"slug":"teamname-%s","owner":"teamname","name":"%s","current_release":{"slug":"teamname-%s-1.0.0","version":"1.0.0"},"releases":[{"slug":"teamname-%s-1.0.0","version":"1.0.0"}]}`, name, name, name, name)
	}))
	t.Cleanup(upstream.Close)

	forgeProxy, err := proxy.NewForgeProxy(upstream.URL, time.Minute, 1<<20, &testArtifactStorage{}, "upstream-cache", proxy.WithHTTPClient(upstream.Client()), proxy.WithPrivateNetworks())
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}
	moduleService := NewModuleService(st, &testArtifactStorage{}, "modules", forgeProxy)
	if err := moduleService.RefreshCachedUpstreamModules(ctx, moduleCount, concurrency); err != nil {
		t.Fatalf("RefreshCachedUpstreamModules() error = %v", err)
	}
	if got := requests.Load(); got != moduleCount {
		t.Fatalf("upstream requests = %d, want %d", got, moduleCount)
	}
	if got := maximum.Load(); got < 2 || got > concurrency {
		t.Fatalf("maximum concurrent requests = %d, want between 2 and %d", got, concurrency)
	}
}

func TestRefreshCachedUpstreamModulesIgnoresCancellationAfterCompletedListing(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	st := &cancelAfterListingStore{testModuleStore: &testModuleStore{}, cancel: cancel}
	moduleService := NewModuleService(st, &testArtifactStorage{}, "modules", &proxy.ForgeProxy{})

	if err := moduleService.RefreshCachedUpstreamModules(ctx, 10, 2); err != nil {
		t.Fatalf("RefreshCachedUpstreamModules() error = %v, want nil after all listed modules were processed", err)
	}
}

func TestRefreshCachedUpstreamModulesReturnsPerModuleFailures(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	t.Cleanup(st.Close)

	module, err := st.UpsertModule(ctx, "teamname", "broken")
	if err != nil {
		t.Fatalf("UpsertModule() error = %v", err)
	}
	_, err = st.CreateRelease(ctx, domain.Release{
		ID: "release-broken", ModuleID: module.ID, Owner: module.Owner, Name: module.Name,
		Version: "1.0.0", Source: "upstream", FileName: "teamname-broken-1.0.0.tar.gz", ContentType: "application/gzip",
	})
	if err != nil {
		t.Fatalf("CreateRelease() error = %v", err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(upstream.Close)
	forgeProxy, err := proxy.NewForgeProxy(
		upstream.URL,
		time.Minute,
		1<<20,
		&testArtifactStorage{},
		"upstream-cache",
		proxy.WithHTTPClient(upstream.Client()),
		proxy.WithPrivateNetworks(),
	)
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}

	err = NewModuleService(st, &testArtifactStorage{}, "modules", forgeProxy).RefreshCachedUpstreamModules(ctx, 10, 1)
	if err == nil || !strings.Contains(err.Error(), "refresh teamname/broken") {
		t.Fatalf("RefreshCachedUpstreamModules() error = %v, want module-scoped failure", err)
	}
}

func TestRefreshCachedUpstreamModulesPersistsAttemptAfterRefreshCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	baseStore, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	t.Cleanup(baseStore.Close)
	module, err := baseStore.UpsertModule(ctx, "teamname", "blocked")
	if err != nil {
		t.Fatalf("UpsertModule() error = %v", err)
	}
	if _, err := baseStore.CreateRelease(ctx, domain.Release{
		ID: "release-blocked", ModuleID: module.ID, Owner: module.Owner, Name: module.Name,
		Version: "1.0.0", Source: "upstream", FileName: "teamname-blocked-1.0.0.tar.gz", ContentType: "application/gzip",
	}); err != nil {
		t.Fatalf("CreateRelease() error = %v", err)
	}

	requestStarted := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		close(requestStarted)
		<-req.Context().Done()
	}))
	t.Cleanup(upstream.Close)
	forgeProxy, err := proxy.NewForgeProxy(
		upstream.URL,
		time.Minute,
		1<<20,
		&testArtifactStorage{},
		"upstream-cache",
		proxy.WithHTTPClient(upstream.Client()),
		proxy.WithPrivateNetworks(),
	)
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}
	markerStore := &refreshMarkerContextStore{
		SQLiteStore:      baseStore,
		markerContextErr: make(chan error, 1),
	}
	done := make(chan error, 1)
	go func() {
		done <- NewModuleService(markerStore, &testArtifactStorage{}, "modules", forgeProxy).RefreshCachedUpstreamModules(ctx, 1, 1)
	}()
	<-requestStarted
	cancel()

	if markerErr := <-markerStore.markerContextErr; markerErr != nil {
		t.Fatalf("refresh marker context error = %v, want detached context", markerErr)
	}
	if refreshErr := <-done; !errors.Is(refreshErr, context.Canceled) {
		t.Fatalf("RefreshCachedUpstreamModules() error = %v, want context.Canceled", refreshErr)
	}
	selected, err := baseStore.ListUpstreamModules(context.Background(), 1)
	if err != nil {
		t.Fatalf("ListUpstreamModules() error = %v", err)
	}
	if len(selected) != 1 || selected[0].Name != "blocked" {
		t.Fatalf("selected modules = %#v", selected)
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

	service := newUpstreamModuleService(t, st, &testArtifactStorage{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

func TestGetReleaseKeepsHydratedReleaseWhenSubsequentReadWouldFail(t *testing.T) {
	t.Parallel()

	baseStore := &testModuleStore{
		module: domain.Module{ID: "module-1", Owner: "puppetlabs", Name: "stdlib"},
		release: domain.Release{
			Owner:        "puppetlabs",
			Name:         "stdlib",
			Version:      "1.0.0",
			Source:       "upstream",
			UpstreamSlug: "puppetlabs-stdlib-1.0.0",
		},
	}
	st := &failGetAfterCreateStore{testModuleStore: baseStore}
	moduleService := newUpstreamModuleService(t, st, &testArtifactStorage{}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"slug":"puppetlabs-stdlib-1.0.0",
			"version":"1.0.0",
			"file_uri":"https://forge.example/v3/files/puppetlabs-stdlib-1.0.0.tar.gz",
			"file_name":"puppetlabs-stdlib-1.0.0.tar.gz"
		}`)
	}))

	release, err := moduleService.GetRelease(context.Background(), "puppetlabs", "stdlib", "1.0.0")
	if err != nil {
		t.Fatalf("GetRelease() error = %v", err)
	}
	if release.Owner != "puppetlabs" || release.Name != "stdlib" || release.Version != "1.0.0" {
		t.Fatalf("GetRelease() returned zero or mismatched release: %#v", release)
	}
	if release.DownloadURL == "" || release.DownloadURL != release.UpstreamFileURI {
		t.Fatalf("GetRelease() download URL = %q, upstream URI = %q", release.DownloadURL, release.UpstreamFileURI)
	}
}

func TestGetReleaseReportsUpstreamHydrationFailure(t *testing.T) {
	t.Parallel()

	st := &testModuleStore{
		module: domain.Module{ID: "module-1", Owner: "puppetlabs", Name: "stdlib"},
		release: domain.Release{
			Owner:        "puppetlabs",
			Name:         "stdlib",
			Version:      "1.0.0",
			Source:       "upstream",
			UpstreamSlug: "puppetlabs-stdlib-1.0.0",
		},
	}
	moduleService := newUpstreamModuleService(t, st, &testArtifactStorage{}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))

	_, err := moduleService.GetRelease(context.Background(), "puppetlabs", "stdlib", "1.0.0")
	if err == nil || !errors.Is(err, ErrUpstreamHydration) || !strings.Contains(err.Error(), "puppetlabs-stdlib-1.0.0") {
		t.Fatalf("GetRelease() error = %v, want hydration error", err)
	}
}

func TestGetReleaseDistinguishesMissingAndUnavailableUpstreamRestore(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		status      int
		want        error
		wantRestore bool
	}{
		{name: "missing", status: http.StatusNotFound, want: store.ErrNotFound},
		{name: "unavailable", status: http.StatusServiceUnavailable, want: ErrUpstreamHydration, wantRestore: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st, err := store.NewSQLiteStore("sqlite://:memory:")
			if err != nil {
				t.Fatalf("NewSQLiteStore() error = %v", err)
			}
			t.Cleanup(st.Close)

			moduleService := newUpstreamModuleService(t, st, &testArtifactStorage{}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, http.StatusText(tt.status), tt.status)
			}))
			_, err = moduleService.GetRelease(context.Background(), "puppetlabs", "stdlib", "1.0.0")
			if !errors.Is(err, tt.want) {
				t.Fatalf("GetRelease() error = %v, want %v", err, tt.want)
			}
			if errors.Is(err, ErrUpstreamRestore) != tt.wantRestore {
				t.Fatalf("GetRelease() restore classification = %t, want %t", errors.Is(err, ErrUpstreamRestore), tt.wantRestore)
			}
		})
	}
}

func TestEnsureReleaseChecksumsMaterializesColdUpstreamRelease(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	t.Cleanup(st.Close)

	archive := []byte("cold upstream archive")
	md5Sum := md5.Sum(archive) // #nosec G401 -- Puppet Forge compatibility checksum asserted in tests.
	sha256Sum := sha256.Sum256(archive)
	expectedMD5 := hex.EncodeToString(md5Sum[:])
	expectedSHA256 := hex.EncodeToString(sha256Sum[:])
	var releaseRequests atomic.Int32
	var artifactRequests atomic.Int32
	artifacts := &testArtifactStorage{}
	moduleService := newUpstreamModuleService(t, st, artifacts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v3/releases/puppetlabs-stdlib-1.0.0":
			releaseRequests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{
				"slug":"puppetlabs-stdlib-1.0.0",
				"version":"1.0.0",
				"description":"stdlib release",
				"readme":"# stdlib",
				"file_uri":"https://forge.example/v3/files/puppetlabs-stdlib-1.0.0.tar.gz",
					"file_name":"puppetlabs-stdlib-1.0.0.tar.gz",
					"file_size":%d,
					"file_md5":%q,
					"file_sha256":%q
				}`, len(archive), expectedMD5, expectedSHA256)
		case "/v3/files/puppetlabs-stdlib-1.0.0.tar.gz":
			artifactRequests.Add(1)
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	upstreamModule := proxy.UpstreamModule{
		Slug:           "puppetlabs-stdlib",
		Owner:          "puppetlabs",
		Name:           "stdlib",
		CurrentRelease: proxy.UpstreamReleaseRef{Slug: "puppetlabs-stdlib-1.0.0", Version: "1.0.0"},
		Releases:       []proxy.UpstreamReleaseRef{{Slug: "puppetlabs-stdlib-1.0.0", Version: "1.0.0"}},
	}
	if err := moduleService.IndexUpstreamModule(ctx, upstreamModule); err != nil {
		t.Fatalf("IndexUpstreamModule() error = %v", err)
	}

	release, err := moduleService.GetReleaseBySlug(ctx, "puppetlabs-stdlib-1.0.0")
	if err != nil {
		t.Fatalf("GetReleaseBySlug() error = %v", err)
	}
	verified, err := moduleService.EnsureReleaseChecksums(ctx, release)
	if err != nil {
		t.Fatalf("EnsureReleaseChecksums() error = %v", err)
	}
	if verified.MD5 != expectedMD5 || verified.SHA256 != expectedSHA256 || verified.SizeBytes != int64(len(archive)) {
		t.Fatalf("verified release checksums = md5:%q sha256:%q size:%d", verified.MD5, verified.SHA256, verified.SizeBytes)
	}
	if verified.StoragePath != "upstream-cache/v3/files/puppetlabs-stdlib-1.0.0.tar.gz" {
		t.Fatalf("verified release storage path = %q", verified.StoragePath)
	}

	if err := moduleService.IndexUpstreamModule(ctx, upstreamModule); err != nil {
		t.Fatalf("IndexUpstreamModule(reindex) error = %v", err)
	}
	stored, err := st.GetRelease(ctx, "puppetlabs", "stdlib", "1.0.0")
	if err != nil {
		t.Fatalf("GetRelease() error = %v", err)
	}
	if stored.MD5 != expectedMD5 || stored.SHA256 != expectedSHA256 || stored.StoragePath != verified.StoragePath {
		t.Fatalf("reindex replaced verified release: %#v", stored)
	}
	if _, err := moduleService.EnsureReleaseChecksums(ctx, stored); err != nil {
		t.Fatalf("EnsureReleaseChecksums(cached) error = %v", err)
	}
	artifacts.mu.Lock()
	openCount := artifacts.opens
	artifacts.mu.Unlock()
	if openCount != 3 {
		t.Fatalf("artifact opens after cold and warm checksum requests = %d, want 3 cold-path integrity/checksum reads", openCount)
	}
	if releaseRequests.Load() != 1 || artifactRequests.Load() != 1 {
		t.Fatalf("upstream requests = release:%d artifact:%d, want 1 each", releaseRequests.Load(), artifactRequests.Load())
	}
}

func TestEnsureReleaseChecksumsClassifiesUpstreamArtifactFailure(t *testing.T) {
	t.Parallel()

	moduleService := newUpstreamModuleService(t, &testModuleStore{}, &testArtifactStorage{}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	_, err := moduleService.EnsureReleaseChecksums(context.Background(), domain.Release{
		Owner: "puppetlabs", Name: "stdlib", Version: "1.0.0", Source: "upstream",
		UpstreamFileURI: "https://forge.example/v3/files/puppetlabs-stdlib-1.0.0.tar.gz",
	})
	if !errors.Is(err, ErrUpstreamHydration) {
		t.Fatalf("EnsureReleaseChecksums() error = %v, want ErrUpstreamHydration", err)
	}
}

func TestReadReleaseFileMaterializesColdUpstreamArchive(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	t.Cleanup(st.Close)

	archive, err := testutil.BuildTarGz(map[string]string{
		"puppetlabs-stdlib-1.0.0/metadata.json": `{"name":"puppetlabs-stdlib","version":"1.0.0"}`,
		"puppetlabs-stdlib-1.0.0/README.md":     "# stdlib\n",
	})
	if err != nil {
		t.Fatalf("testutil.BuildTarGz() error = %v", err)
	}
	var artifactRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v3/releases/puppetlabs-stdlib-1.0.0":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"slug":"puppetlabs-stdlib-1.0.0",
				"version":"1.0.0",
				"file_uri":"https://forge.example/v3/files/puppetlabs-stdlib-1.0.0.tar.gz"
			}`))
		case "/v3/files/puppetlabs-stdlib-1.0.0.tar.gz":
			artifactRequests.Add(1)
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	artifacts := &testArtifactStorage{}
	forgeProxy, err := proxy.NewForgeProxy(upstream.URL, time.Minute, 1<<20, artifacts, "upstream-cache", proxy.WithHTTPClient(upstream.Client()), proxy.WithPrivateNetworks())
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}
	moduleService := NewModuleService(st, artifacts, "modules", forgeProxy)
	if err := moduleService.IndexUpstreamModule(ctx, proxy.UpstreamModule{
		Slug:           "puppetlabs-stdlib",
		Owner:          "puppetlabs",
		Name:           "stdlib",
		CurrentRelease: proxy.UpstreamReleaseRef{Slug: "puppetlabs-stdlib-1.0.0", Version: "1.0.0"},
		Releases:       []proxy.UpstreamReleaseRef{{Slug: "puppetlabs-stdlib-1.0.0", Version: "1.0.0"}},
	}); err != nil {
		t.Fatalf("IndexUpstreamModule() error = %v", err)
	}

	object, err := moduleService.ReadReleaseFile(ctx, "puppetlabs", "stdlib", "1.0.0", "README.md")
	if err != nil {
		t.Fatalf("ReadReleaseFile() error = %v", err)
	}
	if string(object.Body) != "# stdlib\n" {
		t.Fatalf("README body = %q", object.Body)
	}
	if artifactRequests.Load() != 1 {
		t.Fatalf("artifact requests = %d, want 1", artifactRequests.Load())
	}
	stored, err := st.GetRelease(ctx, "puppetlabs", "stdlib", "1.0.0")
	if err != nil {
		t.Fatalf("GetRelease() error = %v", err)
	}
	if stored.StoragePath == "" || stored.SHA256 == "" || stored.MD5 == "" {
		t.Fatalf("cold artifact metadata was not persisted: %#v", stored)
	}
}

func TestGetReleaseDoesNotRefetchHydratedEmptyMetadata(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	t.Cleanup(st.Close)

	var releaseRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/releases/puppetlabs-stdlib-1.0.0" {
			http.NotFound(w, r)
			return
		}
		releaseRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"slug":"puppetlabs-stdlib-1.0.0",
			"version":"1.0.0",
			"description":"",
			"readme":"",
			"file_uri":"https://forge.example/v3/files/puppetlabs-stdlib-1.0.0.tar.gz"
		}`))
	}))
	t.Cleanup(upstream.Close)
	forgeProxy, err := proxy.NewForgeProxy(upstream.URL, time.Minute, 1<<20, &testArtifactStorage{}, "upstream-cache", proxy.WithHTTPClient(upstream.Client()), proxy.WithPrivateNetworks())
	if err != nil {
		t.Fatalf("NewForgeProxy() error = %v", err)
	}
	moduleService := NewModuleService(st, &testArtifactStorage{}, "modules", forgeProxy)
	if err := moduleService.IndexUpstreamModule(ctx, proxy.UpstreamModule{
		Slug: "puppetlabs-stdlib", Owner: "puppetlabs", Name: "stdlib",
		CurrentRelease: proxy.UpstreamReleaseRef{Slug: "puppetlabs-stdlib-1.0.0", Version: "1.0.0"},
	}); err != nil {
		t.Fatalf("IndexUpstreamModule() error = %v", err)
	}
	for range 2 {
		if _, err := moduleService.GetRelease(ctx, "puppetlabs", "stdlib", "1.0.0"); err != nil {
			t.Fatalf("GetRelease() error = %v", err)
		}
	}
	if releaseRequests.Load() != 1 {
		t.Fatalf("release requests = %d, want 1", releaseRequests.Load())
	}
}

func TestIndexUpstreamModuleUsesModuleLock(t *testing.T) {
	t.Parallel()

	moduleStore := &testModuleStore{}
	moduleService := NewModuleService(moduleStore, &testArtifactStorage{}, "modules", nil)
	if err := moduleService.IndexUpstreamModule(context.Background(), proxy.UpstreamModule{
		Slug: "puppetlabs-stdlib", Owner: "puppetlabs", Name: "stdlib",
		CurrentRelease: proxy.UpstreamReleaseRef{Slug: "puppetlabs-stdlib-1.0.0", Version: "1.0.0"},
	}); err != nil {
		t.Fatalf("IndexUpstreamModule() error = %v", err)
	}
	if moduleStore.lockCalls.Load() != 1 {
		t.Fatalf("LockModule() calls = %d, want 1", moduleStore.lockCalls.Load())
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

func TestMarkReleaseUsedThrottlesRepeatedLocalUsage(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	countingStore, service := newReleaseUsageTestService(t)
	if err := service.MarkReleaseUsed(ctx, "teamname", "apache", "1.2.3"); err != nil {
		t.Fatalf("MarkReleaseUsed() error = %v", err)
	}
	if err := service.MarkReleaseUsed(ctx, "teamname", "apache", "1.2.3"); err != nil {
		t.Fatalf("second MarkReleaseUsed() error = %v", err)
	}
	if got := countingStore.markCalls.Load(); got != 1 {
		t.Fatalf("store MarkReleaseUsed() calls = %d, want 1 within the throttle interval", got)
	}
}

func TestMarkReleaseUsedRetriesAfterStoreError(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	countingStore, service := newReleaseUsageTestService(t)
	countingStore.failNext.Store(true)
	if err := service.MarkReleaseUsed(ctx, "teamname", "apache", "1.2.3"); err == nil {
		t.Fatal("first MarkReleaseUsed() error = nil, want store error")
	}
	if err := service.MarkReleaseUsed(ctx, "teamname", "apache", "1.2.3"); err != nil {
		t.Fatalf("second MarkReleaseUsed() error = %v", err)
	}
	if got := countingStore.markCalls.Load(); got != 2 {
		t.Fatalf("store MarkReleaseUsed() calls = %d, want 2 after failed first attempt", got)
	}
}

func TestIndexUpstreamModuleRejectsAmbiguousIdentity(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	defer st.Close()
	service := NewModuleService(st, &testArtifactStorage{}, "modules", nil)

	tests := []struct {
		name   string
		module proxy.UpstreamModule
		want   string
	}{
		{name: "hyphenated owner", module: proxy.UpstreamModule{Owner: "team-name", Name: "apache"}, want: "invalid upstream module owner"},
		{name: "hyphenated name", module: proxy.UpstreamModule{Owner: "teamname", Name: "apache-module"}, want: "invalid upstream module name"},
		{name: "uppercase name", module: proxy.UpstreamModule{Owner: "TeamName", Name: "Apache"}, want: "invalid upstream module name"},
		{name: "missing owner", module: proxy.UpstreamModule{Name: "apache"}, want: "invalid upstream module owner"},
		{name: "missing name", module: proxy.UpstreamModule{Owner: "teamname"}, want: "invalid upstream module name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := service.IndexUpstreamModule(ctx, tt.module)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("IndexUpstreamModule() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestPublishRejectsInvalidOwner(t *testing.T) {
	t.Parallel()

	archive, err := testutil.BuildTarGz(map[string]string{
		"AC_ME-apache-1.2.3/metadata.json": `{"name":"AC_ME-apache","version":"1.2.3"}`,
	})
	if err != nil {
		t.Fatalf("testutil.BuildTarGz() error = %v", err)
	}

	service := NewModuleService(&testModuleStore{}, &testArtifactStorage{}, "modules", nil)

	_, err = service.Publish(context.Background(), domain.PublishModuleInput{
		Owner:     "AC_ME",
		Name:      "apache",
		Version:   "1.2.3",
		FileName:  "module.tar.gz",
		FileBytes: archive,
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestPublishAcceptsUppercaseModuleOwner(t *testing.T) {
	t.Parallel()

	archive, err := testutil.BuildTarGz(map[string]string{
		"ACME-apache_2-1.2.3/metadata.json": `{"name":"ACME-apache_2","version":"1.2.3"}`,
	})
	if err != nil {
		t.Fatalf("testutil.BuildTarGz() error = %v", err)
	}

	service := NewModuleService(&testModuleStore{}, &testArtifactStorage{}, "modules", nil)
	release, err := service.Publish(context.Background(), domain.PublishModuleInput{
		Owner:     "ACME",
		FileName:  "module.tar.gz",
		FileBytes: archive,
	})
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if release.Owner != "ACME" || release.Name != "apache_2" {
		t.Fatalf("Publish() identity = %s/%s, want ACME/apache_2", release.Owner, release.Name)
	}
}

func TestPublishRejectsAmbiguousCanonicalSlug(t *testing.T) {
	t.Parallel()

	archive, err := testutil.BuildTarGz(map[string]string{
		"acme-foo-bar-1.2.3/metadata.json": `{"name":"acme-foo-bar","version":"1.2.3"}`,
	})
	if err != nil {
		t.Fatalf("testutil.BuildTarGz() error = %v", err)
	}

	service := NewModuleService(&testModuleStore{}, &testArtifactStorage{}, "modules", nil)
	_, err = service.Publish(context.Background(), domain.PublishModuleInput{
		Owner:     "acme",
		FileName:  "module.tar.gz",
		FileBytes: archive,
	})
	if err == nil || !strings.Contains(err.Error(), "invalid name") {
		t.Fatalf("Publish() error = %v, want invalid name", err)
	}
}

func TestPublishExtractsReadme(t *testing.T) {
	t.Parallel()

	archive, err := testutil.BuildTarGz(map[string]string{
		"acme-apache-1.2.3/metadata.json":     `{"name":"acme-apache","version":"1.2.3","summary":"Apache module"}`,
		"acme-apache-1.2.3/README.md":         "# Apache\nmanaged module",
		"acme-apache-1.2.3/manifests/init.pp": "class apache {}",
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
		"teamname-apt-2.3.4/metadata.json": `{"name":"teamname-apt","version":"2.3.4","summary":"APT module","dependencies":["stdlib"]}`,
		"teamname-apt-2.3.4/README.md":     "# Apt\nmanaged module",
	})
	if err != nil {
		t.Fatalf("testutil.BuildTarGz() error = %v", err)
	}

	moduleStore := &testModuleStore{}
	artifacts := &testArtifactStorage{}
	service := NewModuleService(moduleStore, artifacts, "modules", nil)

	release, err := service.Publish(context.Background(), domain.PublishModuleInput{
		Owner:       "teamname",
		FileName:    "uploaded-archive.tar.gz",
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
	if release.FileName != "teamname-apt-2.3.4.tar.gz" {
		t.Fatalf("unexpected normalized file name: %s", release.FileName)
	}
	if artifacts.objectPath != "modules/teamname/apt/2.3.4/"+release.SHA256+".tar.gz" {
		t.Fatalf("unexpected object path: %s", artifacts.objectPath)
	}
}

func TestNormalizePublishInputRejectsInvalidArchiveIdentity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		space   string
		entries map[string]string
		wantErr string
	}{
		{
			name:    "missing metadata",
			space:   "teamname",
			entries: map[string]string{"teamname-module-1.0.0/README.md": "# Module"},
			wantErr: "metadata.json is required",
		},
		{
			name:  "multiple metadata files",
			space: "teamname",
			entries: map[string]string{
				"one/metadata.json": `{"name":"teamname-one","version":"1.0.0"}`,
				"two/metadata.json": `{"name":"teamname-two","version":"1.0.0"}`,
			},
			wantErr: "one root directory",
		},
		{
			name:    "missing namespace",
			space:   "teamname",
			entries: map[string]string{"apache-1.0.0/metadata.json": `{"name":"apache","version":"1.0.0"}`},
			wantErr: "name must include a module namespace",
		},
		{
			name:    "missing version",
			space:   "teamname",
			entries: map[string]string{"teamname-apache/metadata.json": `{"name":"teamname-apache"}`},
			wantErr: "version is required",
		},
		{
			name:    "invalid version",
			space:   "teamname",
			entries: map[string]string{"teamname-apache-1.a.0/metadata.json": `{"name":"teamname-apache","version":"1.a.0"}`},
			wantErr: "version must be a valid MAJOR.MINOR.PATCH semantic version",
		},
		{
			name:    "namespace mismatch",
			space:   "teamname",
			entries: map[string]string{"alpha-apache-1.0.0/metadata.json": `{"name":"alpha-apache","version":"1.0.0"}`},
			wantErr: `module namespace "alpha" does not match selected space "teamname"`,
		},
	}

	service := NewModuleService(&testModuleStore{}, &testArtifactStorage{}, "modules", nil)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			archive, err := testutil.BuildTarGz(tc.entries)
			if err != nil {
				t.Fatalf("testutil.BuildTarGz() error = %v", err)
			}
			_, err = service.NormalizePublishInput(domain.PublishModuleInput{
				Owner:     tc.space,
				FileName:  "module.tar.gz",
				FileBytes: archive,
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("NormalizePublishInput() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestInspectModuleArchiveRejectsUnsafeStructure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		entries []testTarEntry
		wantErr string
	}{
		{
			name:    "parent traversal",
			entries: []testTarEntry{{header: tar.Header{Name: "module/../../escape", Typeflag: tar.TypeReg}}},
			wantErr: "invalid path",
		},
		{
			name:    "normalized traversal",
			entries: []testTarEntry{{header: tar.Header{Name: "module/dir/../escape", Typeflag: tar.TypeReg}}},
			wantErr: "invalid path",
		},
		{
			name:    "absolute path",
			entries: []testTarEntry{{header: tar.Header{Name: "/module/metadata.json", Typeflag: tar.TypeReg}}},
			wantErr: "invalid path",
		},
		{
			name:    "symbolic link",
			entries: []testTarEntry{{header: tar.Header{Name: "module/link", Typeflag: tar.TypeSymlink, Linkname: "../../escape"}}},
			wantErr: "unsupported type",
		},
		{
			name: "multiple roots",
			entries: []testTarEntry{
				{header: tar.Header{Name: "one/file", Typeflag: tar.TypeReg}},
				{header: tar.Header{Name: "two/file", Typeflag: tar.TypeReg}},
			},
			wantErr: "one root directory",
		},
		{
			name: "nested metadata",
			entries: []testTarEntry{{
				header: tar.Header{Name: "teamname-module-1.0.0/config/metadata.json", Typeflag: tar.TypeReg},
				body:   `{"name":"teamname-module","version":"1.0.0"}`,
			}},
			wantErr: "metadata.json must be in the archive root directory",
		},
		{
			name: "identity root mismatch",
			entries: []testTarEntry{{
				header: tar.Header{Name: "wrong-root/metadata.json", Typeflag: tar.TypeReg},
				body:   `{"name":"teamname-module","version":"1.0.0"}`,
			}},
			wantErr: "does not match module identity",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			archive := buildTarGzEntries(t, tc.entries)
			_, err := inspectModuleArchive(bytes.NewReader(archive))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("inspectModuleArchive() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestInspectModuleArchiveAcceptsCanonicalDirectoryEntries(t *testing.T) {
	t.Parallel()

	archive := buildTarGzEntries(t, []testTarEntry{
		{header: tar.Header{Name: "teamname-module-1.0.0/", Typeflag: tar.TypeDir}},
		{header: tar.Header{Name: "teamname-module-1.0.0/manifests/", Typeflag: tar.TypeDir}},
		{header: tar.Header{Name: "teamname-module-1.0.0/metadata.json", Typeflag: tar.TypeReg}, body: `{"name":"teamname-module","version":"1.0.0"}`},
	})
	if _, err := inspectModuleArchive(bytes.NewReader(archive)); err != nil {
		t.Fatalf("inspectModuleArchive() error = %v", err)
	}
}

func TestInspectModuleArchiveRejectsExpandedSizeLimit(t *testing.T) {
	t.Parallel()

	archive := buildTarGzEntries(t, []testTarEntry{
		{header: tar.Header{Name: "teamname-module-1.0.0/metadata.json", Typeflag: tar.TypeReg}, body: `{"name":"teamname-module","version":"1.0.0"}`},
		{header: tar.Header{Name: "teamname-module-1.0.0/data.txt", Typeflag: tar.TypeReg}, body: "payload"},
	})
	_, err := inspectModuleArchiveWithLimit(bytes.NewReader(archive), 32)
	if err == nil || !strings.Contains(err.Error(), "expanded size exceeds") {
		t.Fatalf("inspectModuleArchiveWithLimit() error = %v, want expanded-size error", err)
	}
}

func TestPublishAcceptsSeekableArchiveWithoutFileBytes(t *testing.T) {
	t.Parallel()

	archive, err := testutil.BuildTarGz(map[string]string{
		"teamname-module-1.0.0/metadata.json": `{"name":"teamname-module","version":"1.0.0"}`,
	})
	if err != nil {
		t.Fatalf("BuildTarGz() error = %v", err)
	}
	artifacts := &testArtifactStorage{}
	moduleService := NewModuleService(&testModuleStore{}, artifacts, "modules", nil)
	release, err := moduleService.Publish(context.Background(), domain.PublishModuleInput{
		Owner:    "teamname",
		FileName: "upload.tar.gz",
		File:     bytes.NewReader(archive),
	})
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if release.SizeBytes != int64(len(archive)) {
		t.Fatalf("SizeBytes = %d, want %d", release.SizeBytes, len(archive))
	}
	if !bytes.Equal(artifacts.body, archive) {
		t.Fatal("streamed artifact body differs from uploaded archive")
	}
}

func TestPublishStopsReadingCanceledArchive(t *testing.T) {
	t.Parallel()

	archive, err := testutil.BuildTarGz(map[string]string{
		"teamname-module-1.0.0/metadata.json": `{"name":"teamname-module","version":"1.0.0"}`,
	})
	if err != nil {
		t.Fatalf("BuildTarGz() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	artifacts := &testArtifactStorage{}
	moduleService := NewModuleService(&testModuleStore{}, artifacts, "modules", nil)
	_, err = moduleService.Publish(ctx, domain.PublishModuleInput{
		Owner:    "teamname",
		FileName: "upload.tar.gz",
		File:     bytes.NewReader(archive),
	})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Publish() error = %v, want context.Canceled", err)
	}
	if artifacts.uploads != 0 {
		t.Fatalf("canceled publish uploaded %d artifact(s)", artifacts.uploads)
	}
}

func FuzzInspectModuleArchive(f *testing.F) {
	f.Add([]byte("not an archive"))
	f.Fuzz(func(t *testing.T, archive []byte) {
		_, _ = inspectModuleArchive(bytes.NewReader(archive))
	})
}

func FuzzParseArchiveMetadata(f *testing.F) {
	f.Add([]byte(`{"name":"teamname-module","version":"1.0.0"}`))
	f.Add([]byte(`{"name":42,"version":false}`))
	f.Add([]byte(`{`))
	f.Fuzz(func(t *testing.T, body []byte) {
		metadata, err := parseArchiveMetadata(body)
		if err == nil && metadata.Metadata == nil {
			t.Fatal("successful metadata parse returned a nil metadata map")
		}
	})
}

func TestPublishNormalizesUploadedArchiveFileName(t *testing.T) {
	t.Parallel()

	archive, err := testutil.BuildTarGz(map[string]string{
		"teamname-build-4.0.0/metadata.json": `{"name":"teamname-build","version":"4.0.0"}`,
	})
	if err != nil {
		t.Fatalf("testutil.BuildTarGz() error = %v", err)
	}

	moduleStore := &testModuleStore{}
	artifacts := &testArtifactStorage{}
	service := NewModuleService(moduleStore, artifacts, "modules", nil)

	release, err := service.Publish(context.Background(), domain.PublishModuleInput{
		Owner:     "teamname",
		FileName:  "build.tar.gz",
		FileBytes: archive,
	})
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	if release.FileName != "teamname-build-4.0.0.tar.gz" {
		t.Fatalf("unexpected normalized file name: %s", release.FileName)
	}
	if release.StoragePath != "modules/teamname/build/4.0.0/"+release.SHA256+".tar.gz" {
		t.Fatalf("unexpected storage path: %s", release.StoragePath)
	}
	if artifacts.objectPath != release.StoragePath {
		t.Fatalf("uploaded object path %s does not match release storage path %s", artifacts.objectPath, release.StoragePath)
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
			StoragePath: "modules/teamname/apt/teamname-apt-2.3.4.tar.gz",
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
			StoragePath: "modules/teamname/apt/teamname-apt-2.3.4.tar.gz",
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
		"teamname-module-1.0.0/metadata.json": strings.Repeat("x", maxArchiveMetadataSize+1),
	})
	if err != nil {
		t.Fatalf("testutil.BuildTarGz() error = %v", err)
	}

	_, err = inspectModuleArchive(bytes.NewReader(archive))
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
	_, err := inspectModuleArchive(bytes.NewReader(archive))
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
			StoragePath: "modules/teamname/apt/teamname-apt-2.3.4.tar.gz",
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
	for i := range entries {
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

type testTarEntry struct {
	header tar.Header
	body   string
}

func buildTarGzEntries(t *testing.T, entries []testTarEntry) []byte {
	t.Helper()

	var buf bytes.Buffer
	gzipWriter := gzip.NewWriter(&buf)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, entry := range entries {
		header := entry.header
		header.Mode = 0o644
		if header.Typeflag == 0 || header.Typeflag == tar.TypeReg {
			header.Size = int64(len(entry.body))
		}
		if err := tarWriter.WriteHeader(&header); err != nil {
			t.Fatalf("WriteHeader() error = %v", err)
		}
		if entry.body != "" {
			if _, err := io.WriteString(tarWriter, entry.body); err != nil {
				t.Fatalf("WriteString() error = %v", err)
			}
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
