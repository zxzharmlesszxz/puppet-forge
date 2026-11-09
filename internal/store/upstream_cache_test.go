package store

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
)

func TestStoreParityClearUpstreamCachePreservesDataAndAuditAtomicity(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		for _, rollback := range []bool{false, true} {
			name := tc.name + "/commit"
			if rollback {
				name = tc.name + "/audit-rollback"
			}
			t.Run(name, func(t *testing.T) {
				st := tc.open(t)
				t.Cleanup(st.Close)
				ctx := t.Context()
				if err := saveTestCatalog(t, ctx, st, "cacheteam", "demo", []string{"1.0.0", "2.0.0"}, CatalogLease{}); err != nil {
					t.Fatal(err)
				}
				var original domain.Release
				for _, version := range []string{"1.0.0", "2.0.0"} {
					release, err := st.GetRelease(ctx, "cacheteam", "demo", version)
					if err != nil {
						t.Fatal(err)
					}
					release.StoragePath = "upstream-cache/v3/files/shared.tar.gz"
					release.UpstreamFileURI = "/v3/files/shared.tar.gz"
					release.MD5, release.SHA256, release.SizeBytes = "md5", "sha", 7
					release.Metadata = map[string]any{"name": "cacheteam-demo", "version": version}
					original, err = st.CreateRelease(ctx, release)
					if err != nil {
						t.Fatal(err)
					}
				}
				status := domain.ResourceStatus{State: domain.StateBlocked, Reason: "keep blocked", ChangedAt: time.Now().UTC()}
				if err := st.(LifecycleStore).SetResourceStatus(ctx, "cacheteam", "demo", "", status); err != nil {
					t.Fatal(err)
				}
				if err := st.MarkReleaseUsed(ctx, "cacheteam", "demo", "2.0.0"); err != nil {
					t.Fatal(err)
				}
				event := AuditEvent{ID: "cache-audit", Actor: "admin", Action: "clear_upstream_cache", Result: "success", OccurredAt: time.Now().UTC()}
				if rollback {
					if err := st.RecordAuditEvent(ctx, event); err != nil {
						t.Fatal(err)
					}
				}
				mutationCtx := ContextWithMutationAudit(ctx, event)
				unlock, err := st.LockModule(ctx, "cacheteam", "demo")
				if err != nil {
					t.Fatal(err)
				}
				err = st.(UpstreamCacheStore).ClearUpstreamArtifactCache(mutationCtx, "cacheteam", "demo")
				if unlockErr := unlock(); unlockErr != nil {
					t.Fatal(unlockErr)
				}
				if (err != nil) != rollback || MutationAuditRecorded(mutationCtx) == rollback {
					t.Fatalf("clear cache: error=%v audit recorded=%v", err, MutationAuditRecorded(mutationCtx))
				}
				stored, err := st.GetRelease(ctx, "cacheteam", "demo", "2.0.0")
				if err != nil {
					t.Fatal(err)
				}
				want := original
				if !rollback {
					want.StoragePath = ""
				}
				if stored.ID != want.ID || stored.StoragePath != want.StoragePath || stored.MD5 != want.MD5 || stored.SHA256 != want.SHA256 || stored.SizeBytes != want.SizeBytes || stored.UpstreamFileURI != want.UpstreamFileURI || !reflect.DeepEqual(stored.Metadata, want.Metadata) {
					t.Fatalf("clear changed release data: %#v", stored)
				}
				indexed, err := st.(UpstreamCatalogStore).HasUpstreamCatalog(ctx, "cacheteam", "demo")
				if err != nil || !indexed {
					t.Fatalf("catalog completion lost: %v %v", indexed, err)
				}
				current, err := st.(LifecycleStore).GetResourceStatus(ctx, "cacheteam", "demo", "")
				if err != nil || current.State != domain.StateBlocked || current.Reason != status.Reason {
					t.Fatalf("block lost: %#v %v", current, err)
				}
				active, err := st.ListActiveReleases(ctx, time.Now().Add(-time.Hour))
				if err != nil || len(active) != 1 {
					t.Fatalf("usage changed: %#v %v", active, err)
				}
				queue, err := st.(ArtifactDeletionStore).ListArtifactDeletions(ctx, 10)
				wantPending := 1
				if rollback {
					wantPending = 0
				}
				if err != nil || len(queue) != wantPending {
					t.Fatalf("queue=%#v error=%v", queue, err)
				}
				if len(queue) == 1 && queue[0].StoragePath != original.StoragePath {
					t.Fatalf("wrong queued object: %#v", queue)
				}
			})
		}
	}
}

func TestStoreParityClearUpstreamCacheIgnoresNonCachePaths(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			st := tc.open(t)
			t.Cleanup(st.Close)
			module, err := st.UpsertModule(t.Context(), "cacheteam", "safepaths")
			if err != nil {
				t.Fatal(err)
			}
			paths := []string{"modules/local/archive", "upstream-cache/../../modules/local/archive", "UPSTREAM-CACHE/archive"}
			for index, objectPath := range paths {
				version := []string{"1.0.0", "2.0.0", "3.0.0"}[index]
				release := NewRelease(module.ID, module.Owner, module.Name, version, "", "", "demo.tar.gz", "application/gzip", "md5", "sha", objectPath, 7, nil)
				release.Source = "upstream"
				if _, err := st.CreateRelease(t.Context(), release); err != nil {
					t.Fatal(err)
				}
			}
			if err := st.(UpstreamCacheStore).ClearUpstreamArtifactCache(t.Context(), module.Owner, module.Name); err != nil {
				t.Fatal(err)
			}
			queue, err := st.(ArtifactDeletionStore).ListArtifactDeletions(t.Context(), 10)
			if err != nil || len(queue) != 0 {
				t.Fatalf("non-cache paths queued: %#v %v", queue, err)
			}
			for index, version := range []string{"1.0.0", "2.0.0", "3.0.0"} {
				release, err := st.GetRelease(t.Context(), module.Owner, module.Name, version)
				if err != nil || release.StoragePath != paths[index] {
					t.Fatalf("non-cache reference changed: %#v %v", release, err)
				}
			}
		})
	}
}

func TestStoreParityClearCacheRejectsLocalOrigin(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			st := tc.open(t)
			t.Cleanup(st.Close)
			module, err := st.UpsertModule(t.Context(), "localteam", "demo")
			if err != nil {
				t.Fatal(err)
			}
			release := NewRelease(module.ID, module.Owner, module.Name, "1.0.0", "", "", "demo.tar.gz", "application/gzip", "md5", "sha", "modules/local/archive", 7, nil)
			if _, err := st.CreateRelease(t.Context(), release); err != nil {
				t.Fatal(err)
			}
			if err := st.(UpstreamCacheStore).ClearUpstreamArtifactCache(t.Context(), module.Owner, module.Name); !errors.Is(err, ErrConflict) {
				t.Fatalf("local cache clear=%v", err)
			}
			stored, err := st.GetRelease(t.Context(), module.Owner, module.Name, release.Version)
			if err != nil || stored.StoragePath != release.StoragePath {
				t.Fatalf("local archive changed: %#v %v", stored, err)
			}
		})
	}
}
