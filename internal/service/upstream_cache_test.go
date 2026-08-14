package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

type selectiveDeleteFailureStorage struct {
	*testArtifactStorage
	failPath string
}

type upstreamCachePruneOutcome struct {
	result UpstreamCachePruneResult
	err    error
}

func captureUpstreamCachePruneOutcome(result UpstreamCachePruneResult, err error) upstreamCachePruneOutcome {
	return upstreamCachePruneOutcome{result: result, err: err}
}

func (s *selectiveDeleteFailureStorage) Delete(ctx context.Context, objectPath string) error {
	if objectPath == s.failPath {
		return errors.New("delete failed")
	}
	return s.testArtifactStorage.Delete(ctx, objectPath)
}

func TestPruneUpstreamArtifactCacheDeletesOnlyOldUnreferencedObjects(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	t.Cleanup(st.Close)
	module, err := st.UpsertModule(ctx, "puppetlabs", "stdlib")
	if err != nil {
		t.Fatalf("UpsertModule() error = %v", err)
	}
	const referenced = "upstream-cache/v3/files/puppetlabs-stdlib-1.0.0.tar.gz"
	if _, err := st.CreateRelease(ctx, domain.Release{
		ID: "release", ModuleID: module.ID, Owner: module.Owner, Name: module.Name,
		Source: "upstream", Version: "1.0.0", StoragePath: referenced,
	}); err != nil {
		t.Fatalf("CreateRelease() error = %v", err)
	}
	artifacts := &testArtifactStorage{}
	for _, objectPath := range []string{
		referenced,
		"upstream-cache/v3/files/old-orphan.tar.gz",
		"upstream-cache/v3/files/recent-orphan.tar.gz",
		"modules/local-orphan.tar.gz",
	} {
		if err := artifacts.Upload(ctx, objectPath, "application/gzip", []byte(objectPath)); err != nil {
			t.Fatalf("Upload(%s) error = %v", objectPath, err)
		}
	}
	now := time.Now().UTC()
	artifacts.mu.Lock()
	artifacts.objectTimes[referenced] = now.Add(-48 * time.Hour)
	artifacts.objectTimes["upstream-cache/v3/files/old-orphan.tar.gz"] = now.Add(-48 * time.Hour)
	artifacts.objectTimes["upstream-cache/v3/files/recent-orphan.tar.gz"] = now.Add(-time.Hour)
	artifacts.objectTimes["modules/local-orphan.tar.gz"] = now.Add(-48 * time.Hour)
	artifacts.mu.Unlock()

	result, err := NewModuleService(st, artifacts, "modules", nil).PruneUpstreamArtifactCache(ctx, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("PruneUpstreamArtifactCache() error = %v", err)
	}
	if result.Scanned != 3 || result.Deleted != 1 {
		t.Fatalf("prune result = %#v", result)
	}
	if exists, _ := artifacts.Exists(ctx, "upstream-cache/v3/files/old-orphan.tar.gz"); exists {
		t.Fatal("old unreferenced upstream object was retained")
	}
	for _, retained := range []string{referenced, "upstream-cache/v3/files/recent-orphan.tar.gz", "modules/local-orphan.tar.gz"} {
		if exists, _ := artifacts.Exists(ctx, retained); !exists {
			t.Fatalf("retained object %q was deleted", retained)
		}
	}
}

func TestPruneUpstreamArtifactCacheContinuesAfterDeleteFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	t.Cleanup(st.Close)
	baseStorage := &testArtifactStorage{}
	const failedPath = "upstream-cache/v3/files/a-failed.tar.gz"
	const deletedPath = "upstream-cache/v3/files/b-deleted.tar.gz"
	for _, objectPath := range []string{failedPath, deletedPath} {
		if err := baseStorage.Upload(ctx, objectPath, "application/gzip", []byte(objectPath)); err != nil {
			t.Fatalf("Upload(%s) error = %v", objectPath, err)
		}
	}
	baseStorage.mu.Lock()
	baseStorage.objectTimes[failedPath] = time.Now().UTC().Add(-48 * time.Hour)
	baseStorage.objectTimes[deletedPath] = time.Now().UTC().Add(-48 * time.Hour)
	baseStorage.mu.Unlock()
	artifacts := &selectiveDeleteFailureStorage{testArtifactStorage: baseStorage, failPath: failedPath}

	outcome := captureUpstreamCachePruneOutcome(
		NewModuleService(st, artifacts, "modules", nil).PruneUpstreamArtifactCache(ctx, time.Now().UTC().Add(-24*time.Hour)),
	)
	if outcome.err == nil {
		t.Fatal("PruneUpstreamArtifactCache() error = nil, want aggregated delete error")
	}
	if outcome.result.Scanned != 2 || outcome.result.Deleted != 1 || outcome.result.Failed != 1 {
		t.Fatalf("prune result = %#v", outcome.result)
	}
	if exists, _ := artifacts.Exists(ctx, deletedPath); exists {
		t.Fatal("object after failed deletion was not processed")
	}
	if exists, _ := artifacts.Exists(ctx, failedPath); !exists {
		t.Fatal("failed object unexpectedly disappeared")
	}
}
