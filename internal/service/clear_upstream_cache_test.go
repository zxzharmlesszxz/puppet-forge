package service

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

func cachedReleaseFixture(t *testing.T) (*ModuleService, *store.SQLiteStore, *testArtifactStorage, domain.Release) {
	t.Helper()
	svc, artifacts := newSQLiteModuleService(t)
	st := svc.modules.(*store.SQLiteStore)
	module, err := st.UpsertModule(t.Context(), "cacheteam", "demo")
	if err != nil {
		t.Fatal(err)
	}
	release := store.NewRelease(module.ID, module.Owner, module.Name, "1.0.0", "", "", "cacheteam-demo-1.0.0.tar.gz", "application/gzip", "", "", "upstream-cache/v3/files/cacheteam-demo-1.0.0.tar.gz", 0, map[string]any{"name": "cacheteam-demo", "version": "1.0.0"})
	release.Source, release.UpstreamFileURI = "upstream", "/v3/files/cacheteam-demo-1.0.0.tar.gz"
	release, err = st.CreateRelease(t.Context(), release)
	if err != nil {
		t.Fatal(err)
	}
	if err := artifacts.Upload(t.Context(), release.StoragePath, "application/gzip", []byte("upstream archive")); err != nil {
		t.Fatal(err)
	}
	return svc, st, artifacts, release
}

func TestClearUpstreamCacheRequiresGlobalAdmin(t *testing.T) {
	t.Parallel()
	svc, st, artifacts, release := cachedReleaseFixture(t)
	for _, principal := range []auth.Principal{{}, {CanRead: true}, {CanPublish: true}, {CanManageTeam: true, ManagedTeams: map[string]struct{}{"cacheteam": {}}}} {
		if err := svc.ClearUpstreamCache(t.Context(), principal, release.Owner, release.Name); !errors.Is(err, ErrCacheClearForbidden) {
			t.Fatalf("unauthorized cache clear=%v", err)
		}
	}
	if pending, err := st.CountArtifactDeletions(t.Context()); err != nil || pending != 0 || artifacts.objectCount() != 1 {
		t.Fatalf("denied action changed cache: pending=%d error=%v", pending, err)
	}
	release.Source = "local"
	if _, err := st.CreateRelease(t.Context(), release); err != nil {
		t.Fatal(err)
	}
	if err := svc.ClearUpstreamCache(t.Context(), auth.Principal{CanAdmin: true}, release.Owner, release.Name); !errors.Is(err, ErrCacheClearForbidden) {
		t.Fatalf("global admin cleared local-origin data: %v", err)
	}
}

func TestClearUpstreamCacheRetriesStorageFailure(t *testing.T) {
	t.Parallel()
	svc, st, artifacts, release := cachedReleaseFixture(t)
	if err := svc.ClearUpstreamCache(t.Context(), auth.Principal{CanAdmin: true}, release.Owner, release.Name); err != nil {
		t.Fatal(err)
	}
	artifacts.deleteErr = errors.New("storage unavailable")
	if _, err := svc.ProcessArtifactDeletions(t.Context(), 10); !errors.Is(err, artifacts.deleteErr) {
		t.Fatalf("expected storage deletion failure: %v", err)
	}
	pending, err := st.CountArtifactDeletions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if pending != 1 || artifacts.objectCount() != 1 {
		t.Fatalf("failure was not retained: pending=%d objects=%d", pending, artifacts.objectCount())
	}
	artifacts.deleteErr = nil
	if err := st.DeferArtifactDeletion(t.Context(), release.StoragePath, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	result, err := svc.ProcessArtifactDeletions(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 1 || result.Pending != 0 || artifacts.objectCount() != 0 {
		t.Fatalf("cleanup did not recover: %#v %v", result, err)
	}
	if _, err := st.GetRelease(t.Context(), release.Owner, release.Name, release.Version); err != nil {
		t.Fatalf("cleanup deleted release: %v", err)
	}
}

func TestClearUpstreamCacheRechecksOriginAfterLock(t *testing.T) {
	t.Parallel()
	svc, st, artifacts, release := cachedReleaseFixture(t)
	paused := &pausedModuleLockStore{SQLiteStore: st, entered: make(chan struct{}), resume: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-paused.resume:
		default:
			close(paused.resume)
		}
	})
	svc.modules = paused
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- svc.ClearUpstreamCache(ctx, auth.Principal{CanAdmin: true}, release.Owner, release.Name)
	}()
	select {
	case <-paused.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	release.Source = "local"
	if _, err := st.CreateRelease(ctx, release); err != nil {
		t.Fatal(err)
	}
	close(paused.resume)
	select {
	case err := <-done:
		if !errors.Is(err, ErrCacheClearForbidden) {
			t.Fatalf("concurrent local publication lost protection: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if pending, err := st.CountArtifactDeletions(ctx); err != nil || pending != 0 || artifacts.objectCount() != 1 {
		t.Fatalf("local publication cache changed: pending=%d error=%v", pending, err)
	}
}

func TestClearUpstreamCachePreservesSharedObjectsAndRefillsOnDemand(t *testing.T) {
	t.Parallel()
	_, st, artifacts, release := cachedReleaseFixture(t)
	var downloads atomic.Int32
	svc := newUpstreamModuleService(t, st, artifacts, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != release.UpstreamFileURI {
			http.NotFound(w, req)
			return
		}
		downloads.Add(1)
		if _, err := w.Write([]byte("upstream archive")); err != nil {
			t.Error(err)
		}
	}))
	other, err := st.UpsertModule(t.Context(), "cacheteam", "other")
	if err != nil {
		t.Fatal(err)
	}
	shared := release
	shared.ID, shared.ModuleID, shared.Name, shared.Source = "shared-cache", other.ID, other.Name, "local"
	if _, err := st.CreateRelease(t.Context(), shared); err != nil {
		t.Fatal(err)
	}
	admin := auth.Principal{CanAdmin: true}
	clearAndProcess := func() ArtifactDeletionResult {
		t.Helper()
		if err := svc.ClearUpstreamCache(t.Context(), admin, release.Owner, release.Name); err != nil {
			t.Fatal(err)
		}
		result, err := svc.ProcessArtifactDeletions(t.Context(), 10)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if result := clearAndProcess(); result.Canceled != 1 || artifacts.objectCount() != 1 {
		t.Fatalf("shared archive removed: %#v", result)
	}
	// Register the copy again as a concurrent download would, then release the other reference.
	if _, err := st.CreateRelease(t.Context(), release); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteModule(t.Context(), other.Owner, other.Name); err != nil {
		t.Fatal(err)
	}
	if result := clearAndProcess(); result.Deleted != 1 || artifacts.objectCount() != 0 {
		t.Fatalf("unused cache retained: %#v", result)
	}
	stored, err := svc.GetRelease(t.Context(), release.Owner, release.Name, release.Version)
	if err != nil {
		t.Fatal(err)
	}
	refilled, err := svc.EnsureReleaseChecksums(t.Context(), stored)
	if err != nil {
		t.Fatal(err)
	}
	if refilled.StoragePath != release.StoragePath || downloads.Load() != 1 || artifacts.objectCount() != 1 {
		t.Fatalf("refill failed: %#v downloads=%d error=%v", refilled, downloads.Load(), err)
	}
	if err := st.SetResourceStatus(t.Context(), release.Owner, release.Name, "", domain.ResourceStatus{State: domain.StateBlocked, Reason: "broken", ChangedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	clearAndProcess()
	if _, err := svc.GetRelease(context.Background(), release.Owner, release.Name, release.Version); !errors.Is(err, ErrUnavailable) || downloads.Load() != 1 {
		t.Fatalf("cleanup bypassed block: %v", err)
	}
}
