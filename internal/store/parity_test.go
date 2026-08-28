package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
)

type parityStore interface {
	Store
	DeletedReleaseStore
	ReleaseUsageStore
	ModuleReleaseBatchStore
}

func TestStoreParityNormalizesNilReleaseMetadata(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.open(t)
			t.Cleanup(st.Close)

			owner := "parity-metadata-" + tc.name
			module, err := st.UpsertModule(ctx, owner, "empty")
			if err != nil {
				t.Fatalf("UpsertModule() error = %v", err)
			}
			if _, err := st.CreateRelease(ctx, NewRelease(
				module.ID, owner, "empty", "1.0.0", "", "", owner+"-empty-1.0.0.tar.gz",
				"application/gzip", "", "", "", 0, nil,
			)); err != nil {
				t.Fatalf("CreateRelease() error = %v", err)
			}

			release, err := st.GetRelease(ctx, owner, "empty", "1.0.0")
			if err != nil {
				t.Fatalf("GetRelease() error = %v", err)
			}
			if release.Metadata == nil || len(release.Metadata) != 0 {
				t.Fatalf("Metadata = %#v, want non-nil empty map", release.Metadata)
			}
		})
	}
}

func TestStoreParityUpstreamRefreshSchedulingVisitsEveryModule(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.open(t)
			t.Cleanup(st.Close)

			owner := "parity-refresh-" + tc.name
			const moduleCount = 5
			for i := range moduleCount {
				name := fmt.Sprintf("module_%d", i)
				module, err := st.UpsertModule(ctx, owner, name)
				if err != nil {
					t.Fatalf("UpsertModule(%s) error = %v", name, err)
				}
				if _, err := st.CreateRelease(ctx, domain.Release{
					ID:          module.ID + ":1.0.0",
					ModuleID:    module.ID,
					Owner:       owner,
					Name:        name,
					Source:      "upstream",
					Version:     "1.0.0",
					FileName:    owner + "-" + name + "-1.0.0.tar.gz",
					ContentType: "application/gzip",
					Metadata:    map[string]any{},
				}); err != nil {
					t.Fatalf("CreateRelease(%s) error = %v", name, err)
				}
			}

			visited := make(map[string]struct{}, moduleCount)
			for cycle := range 3 {
				modules, err := st.ListUpstreamModules(ctx, 2)
				if err != nil {
					t.Fatalf("ListUpstreamModules(cycle %d) error = %v", cycle, err)
				}
				if len(modules) != 2 {
					t.Fatalf("ListUpstreamModules(cycle %d) returned %d modules, want 2", cycle, len(modules))
				}
				attemptedAt := time.Date(2026, 8, 12, 12, cycle, 0, 0, time.UTC)
				for _, module := range modules {
					visited[module.Name] = struct{}{}
					if err := st.MarkUpstreamModuleRefreshAttempt(ctx, module.Owner, module.Name, attemptedAt); err != nil {
						t.Fatalf("MarkUpstreamModuleRefreshAttempt(%s) error = %v", module.Name, err)
					}
				}
			}
			if len(visited) != moduleCount {
				t.Fatalf("visited %d upstream modules, want %d: %#v", len(visited), moduleCount, visited)
			}
		})
	}
}

func TestStoreParityCountsUpstreamModulesByOwner(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.open(t)
			t.Cleanup(st.Close)

			upstreamOwner := "parity_upstream_" + tc.name
			for index, name := range []string{"stdlib", "concat"} {
				module, err := st.UpsertModule(ctx, upstreamOwner, name)
				if err != nil {
					t.Fatalf("UpsertModule(%s) error = %v", name, err)
				}
				for versionIndex := range index + 1 {
					version := fmt.Sprintf("1.0.%d", versionIndex)
					if _, err := st.CreateRelease(ctx, domain.Release{
						ID:          module.ID + ":" + version,
						ModuleID:    module.ID,
						Owner:       upstreamOwner,
						Name:        name,
						Source:      "upstream",
						Version:     version,
						FileName:    upstreamOwner + "-" + name + "-" + version + ".tar.gz",
						ContentType: "application/gzip",
						Metadata:    map[string]any{},
					}); err != nil {
						t.Fatalf("CreateRelease(%s, %s) error = %v", name, version, err)
					}
				}
			}

			localOwner := "parity_local_" + tc.name
			localModule, err := st.UpsertModule(ctx, localOwner, "private")
			if err != nil {
				t.Fatalf("UpsertModule(local) error = %v", err)
			}
			if _, err := st.CreateRelease(ctx, NewRelease(
				localModule.ID, localOwner, "private", "1.0.0", "", "", localOwner+"-private-1.0.0.tar.gz",
				"application/gzip", "", "", "", 0, map[string]any{},
			)); err != nil {
				t.Fatalf("CreateRelease(local) error = %v", err)
			}

			counts, err := st.CountUpstreamModulesByOwner(ctx)
			if err != nil {
				t.Fatalf("CountUpstreamModulesByOwner() error = %v", err)
			}
			if counts[upstreamOwner] != 2 || counts[localOwner] != 0 {
				t.Fatalf("upstream owner counts = %#v, want %q:2 without local owner", counts, upstreamOwner)
			}
		})
	}
}

func TestStoreParityCountsReleasesForSelectedModules(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.open(t)
			t.Cleanup(st.Close)

			owner := "parity_release_counts_" + tc.name
			modules := make([]domain.Module, 2)
			for index, name := range []string{"first", "second"} {
				module, err := st.UpsertModule(ctx, owner, name)
				if err != nil {
					t.Fatalf("UpsertModule(%s) error = %v", name, err)
				}
				modules[index] = module
				for releaseIndex := range index + 1 {
					version := fmt.Sprintf("1.0.%d", releaseIndex)
					if _, err := st.CreateRelease(ctx, domain.Release{
						ID: module.ID + ":" + version, ModuleID: module.ID, Owner: owner, Name: name,
						Source: "local", Version: version, FileName: owner + "-" + name + "-" + version + ".tar.gz",
						ContentType: "application/gzip", Metadata: map[string]any{},
					}); err != nil {
						t.Fatalf("CreateRelease(%s, %s) error = %v", name, version, err)
					}
				}
			}

			counts, err := st.CountReleasesForModules(ctx, modules)
			if err != nil {
				t.Fatalf("CountReleasesForModules() error = %v", err)
			}
			if len(counts) != 2 || counts[0].Count != 1 || counts[1].Count != 2 {
				t.Fatalf("release counts = %#v, want 1 and 2", counts)
			}
		})
	}
}

func TestStoreParityLifecycle(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.open(t)
			t.Cleanup(st.Close)

			owner := "parity-" + tc.name
			name := "versions"
			module, err := st.UpsertModule(ctx, owner, name)
			if err != nil {
				t.Fatalf("UpsertModule() error = %v", err)
			}
			for _, version := range []string{"1.0.0", "2.0.0", "1.5.0"} {
				_, err = st.CreateRelease(ctx, NewRelease(
					module.ID,
					owner,
					name,
					version,
					"Parity test module",
					"",
					owner+"-"+name+"-"+version+".tar.gz",
					"application/gzip",
					"",
					"deadbeef",
					"modules/"+owner+"/"+name+"/"+version+"/"+owner+"-"+name+"-"+version+".tar.gz",
					123,
					map[string]any{"version": version},
				))
				if err != nil {
					t.Fatalf("CreateRelease(%s) error = %v", version, err)
				}
			}

			got, err := st.GetModule(ctx, owner, name)
			if err != nil {
				t.Fatalf("GetModule() error = %v", err)
			}
			if got.LatestVersion != "2.0.0" {
				t.Fatalf("LatestVersion after create = %q, want 2.0.0", got.LatestVersion)
			}

			versions, err := st.ListReleases(ctx, owner, name)
			if err != nil {
				t.Fatalf("ListReleases() error = %v", err)
			}
			if gotVersions := moduleVersions(versions); !reflect.DeepEqual(gotVersions, []string{"2.0.0", "1.5.0", "1.0.0"}) {
				t.Fatalf("ListReleases() versions = %#v", gotVersions)
			}

			if err := st.DeleteRelease(ctx, owner, name, "2.0.0"); err != nil {
				t.Fatalf("DeleteRelease(latest) error = %v", err)
			}
			got, err = st.GetModule(ctx, owner, name)
			if err != nil {
				t.Fatalf("GetModule() after deleting latest error = %v", err)
			}
			if got.LatestVersion != "1.5.0" {
				t.Fatalf("LatestVersion after deleting latest = %q, want 1.5.0", got.LatestVersion)
			}
		})
	}
}

func TestStoreParityUpdateReleaseChecksums(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.open(t)
			t.Cleanup(st.Close)

			owner := "checksum-" + tc.name
			module, err := st.UpsertModule(ctx, owner, "archive")
			if err != nil {
				t.Fatalf("UpsertModule() error = %v", err)
			}
			if _, err := st.CreateRelease(ctx, NewRelease(
				module.ID,
				owner,
				"archive",
				"1.0.0",
				"Checksum parity test",
				"",
				owner+"-archive-1.0.0.tar.gz",
				"application/gzip",
				"",
				"",
				"",
				0,
				nil,
			)); err != nil {
				t.Fatalf("CreateRelease() error = %v", err)
			}

			checksumStore, ok := st.(ReleaseChecksumStore)
			if !ok {
				t.Fatal("store does not implement ReleaseChecksumStore")
			}
			const storagePath = "upstream-cache/v3/files/checksum-archive-1.0.0.tar.gz"
			if err := checksumStore.UpdateReleaseChecksums(
				ctx,
				owner,
				"archive",
				"1.0.0",
				"md5-value",
				"sha256-value",
				storagePath,
				456,
			); err != nil {
				t.Fatalf("UpdateReleaseChecksums() error = %v", err)
			}

			release, err := st.GetRelease(ctx, owner, "archive", "1.0.0")
			if err != nil {
				t.Fatalf("GetRelease() error = %v", err)
			}
			if release.MD5 != "md5-value" || release.SHA256 != "sha256-value" ||
				release.StoragePath != storagePath || release.SizeBytes != 456 {
				t.Fatalf("updated release = %#v", release)
			}
		})
	}
}

func TestStoreParityAccessConfigLockSerializesWriters(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.open(t)
			t.Cleanup(st.Close)

			firstUnlock, err := st.LockAccessConfig(ctx)
			if err != nil {
				t.Fatalf("LockAccessConfig(first) error = %v", err)
			}

			acquired := make(chan AccessConfigUnlock, 1)
			errs := make(chan error, 1)
			go func() {
				unlock, lockErr := st.LockAccessConfig(ctx)
				if lockErr != nil {
					errs <- lockErr
					return
				}
				acquired <- unlock
			}()

			select {
			case unlock := <-acquired:
				_ = unlock()
				t.Fatal("second access config writer acquired lock before first release")
			case err := <-errs:
				t.Fatalf("LockAccessConfig(second) error = %v", err)
			case <-time.After(50 * time.Millisecond):
			}

			if err := firstUnlock(); err != nil {
				t.Fatalf("LockAccessConfig(first unlock) error = %v", err)
			}
			if err := firstUnlock(); err != nil {
				t.Fatalf("LockAccessConfig(idempotent unlock) error = %v", err)
			}

			select {
			case unlock := <-acquired:
				if err := unlock(); err != nil {
					t.Fatalf("LockAccessConfig(second unlock) error = %v", err)
				}
			case err := <-errs:
				t.Fatalf("LockAccessConfig(second) error = %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("second access config writer did not acquire released lock")
			}
		})
	}
}

func TestStoreParityUpsertModulePreservesUpdatedAt(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.open(t)
			t.Cleanup(st.Close)

			owner := "parity-upsert-timestamp-" + tc.name
			if _, err := st.UpsertModule(ctx, owner, "module"); err != nil {
				t.Fatalf("UpsertModule(create) error = %v", err)
			}
			t.Cleanup(func() { _ = st.DeleteModule(context.Background(), owner, "module") })
			setParityModuleUpdatedAt(t, st, []string{owner}, "2020-01-02 03:04:05+00:00")

			module, err := st.UpsertModule(ctx, owner, "module")
			if err != nil {
				t.Fatalf("UpsertModule(existing) error = %v", err)
			}
			if module.UpdatedAt.UTC() != time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC) {
				t.Fatalf("UpsertModule(existing) updated_at = %s", module.UpdatedAt)
			}
		})
	}
}

func TestStoreParityCreateReleaseIfAbsentIsImmutable(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.open(t)
			t.Cleanup(st.Close)

			owner := "parity-immutable-" + tc.name
			name := "module"
			module, err := st.UpsertModule(ctx, owner, name)
			if err != nil {
				t.Fatalf("UpsertModule() error = %v", err)
			}
			t.Cleanup(func() {
				if err := st.DeleteModule(context.Background(), owner, name); err != nil && !errors.Is(err, ErrNotFound) {
					t.Errorf("cleanup module error = %v", err)
				}
			})

			original := NewRelease(
				module.ID,
				owner,
				name,
				"1.0.0",
				"original",
				"",
				owner+"-"+name+"-1.0.0.tar.gz",
				"application/gzip",
				"",
				"original-sha256",
				"modules/"+owner+"/"+name+"/1.0.0/original-sha256.tar.gz",
				123,
				map[string]any{"summary": "original"},
			)
			if _, err := st.CreateReleaseIfAbsent(ctx, original); err != nil {
				t.Fatalf("CreateReleaseIfAbsent() error = %v", err)
			}

			changed := original
			changed.ID = original.ID + "-changed"
			changed.Description = "changed"
			changed.SHA256 = "changed-sha256"
			changed.StoragePath = "modules/changed.tar.gz"
			if _, err := st.CreateReleaseIfAbsent(ctx, changed); !errors.Is(err, ErrConflict) {
				t.Fatalf("CreateReleaseIfAbsent(conflict) error = %v, want ErrConflict", err)
			}

			got, err := st.GetRelease(ctx, owner, name, "1.0.0")
			if err != nil {
				t.Fatalf("GetRelease() error = %v", err)
			}
			if got.Description != original.Description || got.SHA256 != original.SHA256 || got.StoragePath != original.StoragePath {
				t.Fatalf("conflicting create mutated release: %#v", got)
			}
		})
	}
}

func TestStoreParityRejectsInvalidModuleAndReleaseData(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.open(t)
			t.Cleanup(st.Close)
			if _, err := st.UpsertModule(ctx, "", "module"); err == nil {
				t.Fatal("empty owner was accepted")
			}
			owner := "constraints-" + tc.name + "-" + uuid.NewString()
			module, err := st.UpsertModule(ctx, owner, "module")
			if err != nil {
				t.Fatalf("UpsertModule() error = %v", err)
			}
			t.Cleanup(func() { _ = st.DeleteModule(context.Background(), owner, "module") })
			invalid := NewRelease(
				module.ID, owner, "module", "1.0.0", "", "", "module.tar.gz",
				"application/gzip", "", "sha", "path", -1, map[string]any{},
			)
			if _, err := st.CreateRelease(ctx, invalid); err == nil {
				t.Fatal("negative release size was accepted")
			}
			invalid.SizeBytes = 1
			invalid.Source = "external"
			if _, err := st.CreateRelease(ctx, invalid); err == nil {
				t.Fatal("invalid release source was accepted")
			}
		})
	}
}

func TestStoreParityModuleLockHonorsContext(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			st := tc.open(t)
			t.Cleanup(st.Close)

			owner := "parity-lock-" + tc.name
			unlock, err := st.LockModule(context.Background(), owner, "module")
			if err != nil {
				t.Fatalf("LockModule() error = %v", err)
			}

			waitCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			if _, err := st.LockModule(waitCtx, owner, "module"); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("contended LockModule() error = %v, want context deadline", err)
			}
			if err := unlock(); err != nil {
				t.Fatalf("unlock() error = %v", err)
			}

			unlock, err = st.LockModule(context.Background(), owner, "module")
			if err != nil {
				t.Fatalf("LockModule() after release error = %v", err)
			}
			if err := unlock(); err != nil {
				t.Fatalf("second unlock() error = %v", err)
			}
		})
	}
}

func TestStoreParityDeleteModuleIfEmpty(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.open(t)
			t.Cleanup(st.Close)

			owner := "parity-empty-" + tc.name
			if _, err := st.UpsertModule(ctx, owner, "empty"); err != nil {
				t.Fatalf("UpsertModule(empty) error = %v", err)
			}
			if err := st.DeleteModuleIfEmpty(ctx, owner, "empty"); err != nil {
				t.Fatalf("DeleteModuleIfEmpty(empty) error = %v", err)
			}
			if _, err := st.GetModule(ctx, owner, "empty"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("GetModule(empty) error = %v, want ErrNotFound", err)
			}

			module, err := st.UpsertModule(ctx, owner, "nonempty")
			if err != nil {
				t.Fatalf("UpsertModule(nonempty) error = %v", err)
			}
			if _, err := st.CreateReleaseIfAbsent(ctx, NewRelease(
				module.ID, owner, "nonempty", "1.0.0", "", "", "module.tar.gz",
				"application/gzip", "", "sha256", "modules/artifact.tar.gz", 1, map[string]any{},
			)); err != nil {
				t.Fatalf("CreateReleaseIfAbsent(nonempty) error = %v", err)
			}
			t.Cleanup(func() {
				if err := st.DeleteModule(context.Background(), owner, "nonempty"); err != nil && !errors.Is(err, ErrNotFound) {
					t.Errorf("cleanup nonempty module error = %v", err)
				}
			})

			if err := st.DeleteModuleIfEmpty(ctx, owner, "nonempty"); err != nil {
				t.Fatalf("DeleteModuleIfEmpty(nonempty) error = %v", err)
			}
			if _, err := st.GetModule(ctx, owner, "nonempty"); err != nil {
				t.Fatalf("DeleteModuleIfEmpty(nonempty) removed module: %v", err)
			}
		})
	}
}

func TestStoreParityTombstonesAndUsage(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.open(t)
			t.Cleanup(st.Close)

			owner := "parity-" + tc.name
			name := "stdlib"
			module, err := st.UpsertModule(ctx, owner, name)
			if err != nil {
				t.Fatalf("UpsertModule() error = %v", err)
			}
			for _, version := range []string{"1.0.0", "2.0.0"} {
				_, err = st.CreateRelease(ctx, domain.Release{
					ID:          "release-" + tc.name + "-" + version,
					ModuleID:    module.ID,
					Owner:       owner,
					Name:        name,
					Source:      "upstream",
					Version:     version,
					FileName:    name + "-" + version + ".tar.gz",
					ContentType: "application/gzip",
					Metadata:    map[string]any{},
				})
				if err != nil {
					t.Fatalf("CreateRelease(%s) error = %v", version, err)
				}
			}

			if err := st.MarkReleaseUsed(ctx, owner, name, "1.0.0"); err != nil {
				t.Fatalf("MarkReleaseUsed() error = %v", err)
			}
			active, err := st.IsReleaseActive(ctx, owner, name, "1.0.0", time.Now().Add(-time.Hour))
			if err != nil {
				t.Fatalf("IsReleaseActive() error = %v", err)
			}
			if !active {
				t.Fatal("release should be active after mark")
			}
			activeReleases, err := st.ListActiveReleases(ctx, time.Now().Add(-time.Hour))
			if err != nil {
				t.Fatalf("ListActiveReleases() error = %v", err)
			}
			if !hasReleaseSummary(activeReleases, owner, name, "1.0.0") {
				t.Fatalf("ListActiveReleases() missing active release: %#v", activeReleases)
			}
			scopedActive, err := st.ListActiveReleasesForModules(ctx, time.Now().Add(-time.Hour), []domain.Module{module})
			if err != nil {
				t.Fatalf("ListActiveReleasesForModules() error = %v", err)
			}
			if len(scopedActive) != 1 || !hasReleaseSummary(scopedActive, owner, name, "1.0.0") {
				t.Fatalf("ListActiveReleasesForModules() = %#v", scopedActive)
			}

			if err := st.DeleteRelease(ctx, owner, name, "1.0.0"); err != nil {
				t.Fatalf("DeleteRelease() error = %v", err)
			}
			deleted, err := st.IsReleaseDeleted(ctx, owner, name, "1.0.0", "upstream")
			if err != nil {
				t.Fatalf("IsReleaseDeleted() error = %v", err)
			}
			if !deleted {
				t.Fatal("expected upstream release tombstone")
			}

			if err := st.DeleteModule(ctx, owner, name); err != nil {
				t.Fatalf("DeleteModule() error = %v", err)
			}
			deleted, err = st.IsReleaseDeleted(ctx, owner, name, "1.0.0", "upstream")
			if err != nil {
				t.Fatalf("IsReleaseDeleted() after module delete error = %v", err)
			}
			if deleted {
				t.Fatal("expected module delete to clear release tombstone")
			}
			active, err = st.IsReleaseActive(ctx, owner, name, "1.0.0", time.Now().Add(-time.Hour))
			if err != nil {
				t.Fatalf("IsReleaseActive() after module delete error = %v", err)
			}
			if active {
				t.Fatal("expected module delete to clear release usage")
			}
		})
	}
}

func TestStoreParityPurgesDeletedReleaseTombstonesByAge(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.open(t)
			t.Cleanup(st.Close)

			owner := "parity-tombstone-retention-" + tc.name
			name := "stdlib"
			module, err := st.UpsertModule(ctx, owner, name)
			if err != nil {
				t.Fatalf("UpsertModule() error = %v", err)
			}
			if _, err := st.CreateRelease(ctx, domain.Release{
				ID: "release-tombstone-retention-" + tc.name, ModuleID: module.ID,
				Owner: owner, Name: name, Source: "upstream", Version: "1.0.0",
				FileName: name + "-1.0.0.tar.gz", ContentType: "application/gzip", Metadata: map[string]any{},
			}); err != nil {
				t.Fatalf("CreateRelease() error = %v", err)
			}
			if err := st.DeleteRelease(ctx, owner, name, "1.0.0"); err != nil {
				t.Fatalf("DeleteRelease() error = %v", err)
			}

			deleted, err := st.PurgeDeletedReleases(ctx, time.Now().UTC().Add(-time.Hour))
			if err != nil || deleted != 0 {
				t.Fatalf("PurgeDeletedReleases(past) = %d, %v, want 0, nil", deleted, err)
			}
			deleted, err = st.PurgeDeletedReleases(ctx, time.Now().UTC().Add(time.Hour))
			if err != nil || deleted != 1 {
				t.Fatalf("PurgeDeletedReleases(future) = %d, %v, want 1, nil", deleted, err)
			}
			isDeleted, err := st.IsReleaseDeleted(ctx, owner, name, "1.0.0", "upstream")
			if err != nil || isDeleted {
				t.Fatalf("IsReleaseDeleted() = %v, %v, want false, nil", isDeleted, err)
			}
		})
	}
}

func TestStoreParityArtifactDeletionOutbox(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.open(t)
			t.Cleanup(st.Close)
			deletionStore, ok := st.(ArtifactDeletionStore)
			if !ok {
				t.Fatal("store does not implement ArtifactDeletionStore")
			}

			owner := "artifact-outbox-" + tc.name
			module, err := st.UpsertModule(ctx, owner, "archive")
			if err != nil {
				t.Fatalf("UpsertModule() error = %v", err)
			}
			const storagePath = "modules/artifact-outbox/archive/1.0.0/digest.tar.gz"
			if _, err := st.CreateRelease(ctx, NewRelease(
				module.ID, owner, "archive", "1.0.0", "", "", "archive.tar.gz", "application/gzip",
				"md5", "sha256", storagePath, 7, nil,
			)); err != nil {
				t.Fatalf("CreateRelease() error = %v", err)
			}
			if err := st.DeleteRelease(ctx, owner, "archive", "1.0.0"); err != nil {
				t.Fatalf("DeleteRelease() error = %v", err)
			}

			deletions, err := deletionStore.ListArtifactDeletions(ctx, 10)
			if err != nil {
				t.Fatalf("ListArtifactDeletions() error = %v", err)
			}
			if len(deletions) != 1 || deletions[0].StoragePath != storagePath || deletions[0].Owner != owner || deletions[0].Name != "archive" {
				t.Fatalf("artifact deletions = %#v", deletions)
			}
			referenced, err := deletionStore.IsArtifactReferenced(ctx, storagePath)
			if err != nil {
				t.Fatalf("IsArtifactReferenced() error = %v", err)
			}
			if referenced {
				t.Fatal("deleted release still references queued artifact")
			}
			if err := deletionStore.DeferArtifactDeletion(ctx, storagePath, time.Now().Add(time.Hour)); err != nil {
				t.Fatalf("DeferArtifactDeletion() error = %v", err)
			}
			deletions, err = deletionStore.ListArtifactDeletions(ctx, 10)
			if err != nil {
				t.Fatalf("ListArtifactDeletions(after defer) error = %v", err)
			}
			if len(deletions) != 0 {
				t.Fatalf("deferred artifact deletion is already due: %#v", deletions)
			}
			count, err := deletionStore.CountArtifactDeletions(ctx)
			if err != nil {
				t.Fatalf("CountArtifactDeletions() error = %v", err)
			}
			if count != 1 {
				t.Fatalf("artifact deletion count = %d, want 1", count)
			}
			if err := deletionStore.CompleteArtifactDeletion(ctx, storagePath); err != nil {
				t.Fatalf("CompleteArtifactDeletion() error = %v", err)
			}
			deletions, err = deletionStore.ListArtifactDeletions(ctx, 10)
			if err != nil {
				t.Fatalf("ListArtifactDeletions(after complete) error = %v", err)
			}
			if len(deletions) != 0 {
				t.Fatalf("completed artifact deletions = %#v", deletions)
			}
		})
	}
}

func TestStoreParityReleaseReplacementQueuesOnlySupersededArtifact(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.open(t)
			t.Cleanup(st.Close)
			deletionStore := st.(ArtifactDeletionStore)

			owner := "replace-artifact-" + tc.name
			module, err := st.UpsertModule(ctx, owner, "archive")
			if err != nil {
				t.Fatalf("UpsertModule() error = %v", err)
			}
			first := NewRelease(
				module.ID, owner, "archive", "1.0.0", "", "", "archive.tar.gz", "application/gzip",
				"old-md5", "old-sha256", "modules/replace/archive/1.0.0/old.tar.gz", 7, nil,
			)
			first, err = st.CreateRelease(ctx, first)
			if err != nil {
				t.Fatalf("CreateRelease(first) error = %v", err)
			}
			replacement := NewRelease(
				module.ID, owner, "archive", "1.0.0", "", "", "archive.tar.gz", "application/gzip",
				"new-md5", "new-sha256", "modules/replace/archive/1.0.0/new.tar.gz", 8, nil,
			)
			replacement, err = st.CreateRelease(ctx, replacement)
			if err != nil {
				t.Fatalf("CreateRelease(replacement) error = %v", err)
			}
			if replacement.ID != first.ID || replacement.SHA256 != "new-sha256" {
				t.Fatalf("replacement = %#v, want original ID and new checksum", replacement)
			}
			deletions, err := deletionStore.ListArtifactDeletions(ctx, 10)
			if err != nil {
				t.Fatalf("ListArtifactDeletions() error = %v", err)
			}
			if len(deletions) != 1 || deletions[0].StoragePath != first.StoragePath {
				t.Fatalf("artifact deletions = %#v, want superseded artifact %q", deletions, first.StoragePath)
			}
		})
	}
}

func TestPostgresStoreConcurrentSchemaSetup(t *testing.T) {
	dsn := os.Getenv("PUPPET_FORGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PUPPET_FORGE_TEST_POSTGRES_DSN is not set")
	}

	const workers = 8
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			st, err := NewPostgresStore(context.Background(), dsn)
			if err != nil {
				errs <- err
				return
			}
			st.Close()
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("NewPostgresStore() concurrent schema setup error = %v", err)
	}
}

func TestStoreParityAccessConfigRoundTrip(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.open(t)
			t.Cleanup(st.Close)

			prefix := "parityaccess" + tc.name
			want := []auth.TeamConfig{
				{
					Team:                prefix + "teamname",
					ReadTokens:          []string{"read-token"},
					PublishTokens:       []string{"publish-token"},
					PublishOwners:       []string{"teamname", "shared"},
					OIDCGroups:          []string{"teamname-devops"},
					OIDCTeamAdminEmails: []string{"owner@example.com"},
					OIDCTeamAdminGroups: []string{"teamname-admins"},
				},
				{
					Team:         prefix + "secondary",
					OIDCEmails:   []string{"publisher@example.com"},
					OIDCSubjects: []string{"publisher-subject"},
					OIDCDomains:  []string{"example.com"},
				},
			}

			existing, err := st.LoadTeamConfigs(ctx)
			if err != nil {
				t.Fatalf("LoadTeamConfigs(before) error = %v", err)
			}
			withoutParity := withoutParityTeams(existing, prefix)
			t.Cleanup(func() {
				if err := st.ReplaceTeamConfigs(context.Background(), withoutParity); err != nil {
					t.Errorf("cleanup parity access configs error = %v", err)
				}
			})

			configs := append(append([]auth.TeamConfig{}, withoutParity...), want...)
			if err := st.ReplaceTeamConfigs(ctx, configs); err != nil {
				t.Fatalf("ReplaceTeamConfigs() error = %v", err)
			}
			got, err := st.LoadTeamConfigs(ctx)
			if err != nil {
				t.Fatalf("LoadTeamConfigs() error = %v", err)
			}
			got = filterParityTeams(got, prefix)
			var tokenConfig *auth.TeamConfig
			for i := range got {
				if got[i].Team == prefix+"teamname" {
					tokenConfig = &got[i]
					break
				}
			}
			if len(got) != 2 || tokenConfig == nil || len(tokenConfig.ReadTokenRecords) != 1 || len(tokenConfig.PublishTokenRecords) != 1 {
				t.Fatalf("loaded token metadata = %#v", got)
			}
			hasher := testAccessTokenHasher(t)
			if tokenConfig.ReadTokenRecords[0].Digest != hasher.Digest("read-token") || tokenConfig.PublishTokenRecords[0].Digest != hasher.Digest("publish-token") {
				t.Fatalf("loaded token hashes do not match submitted credentials: %#v", tokenConfig)
			}
			for i := range got {
				got[i].ReadTokenRecords = nil
				got[i].PublishTokenRecords = nil
			}
			want[0].ReadTokens = nil
			want[0].PublishTokens = nil
			normalizeTeamConfigs(got)
			normalizeTeamConfigs(want)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("LoadTeamConfigs() = %#v, want %#v", got, want)
			}
		})
	}
}

func TestStoreParityMarksAccessTokenUsed(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.open(t)
			t.Cleanup(st.Close)

			team := "paritytokenusage" + tc.name
			existing, err := st.LoadTeamConfigs(ctx)
			if err != nil {
				t.Fatalf("LoadTeamConfigs(before) error = %v", err)
			}
			withoutTeam := withoutParityTeams(existing, team)
			t.Cleanup(func() {
				if err := st.ReplaceTeamConfigs(context.Background(), withoutTeam); err != nil {
					t.Errorf("cleanup token usage config error = %v", err)
				}
			})

			configs := append(append([]auth.TeamConfig{}, withoutTeam...), auth.TeamConfig{
				Team:       team,
				ReadTokens: []string{"token-usage-secret-" + tc.name},
			})
			if err := st.ReplaceTeamConfigs(ctx, configs); err != nil {
				t.Fatalf("ReplaceTeamConfigs() error = %v", err)
			}
			loaded, err := st.LoadTeamConfigs(ctx)
			if err != nil {
				t.Fatalf("LoadTeamConfigs(token) error = %v", err)
			}
			cfg := findParityTeamConfig(loaded, team)
			if cfg == nil || len(cfg.ReadTokenRecords) != 1 {
				t.Fatalf("managed token record missing: %#v", loaded)
			}
			active, err := st.IsAccessTokenActive(ctx, cfg.ReadTokenRecords[0].ID, time.Now())
			if err != nil {
				t.Fatalf("IsAccessTokenActive(active) error = %v", err)
			}
			if !active {
				t.Fatal("active access token reported inactive")
			}

			usedAt := time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC)
			if err := st.MarkAccessTokenUsed(ctx, cfg.ReadTokenRecords[0].ID, usedAt); err != nil {
				t.Fatalf("MarkAccessTokenUsed() error = %v", err)
			}
			if err := st.MarkAccessTokenUsed(ctx, cfg.ReadTokenRecords[0].ID, usedAt.Add(-time.Hour)); err != nil {
				t.Fatalf("MarkAccessTokenUsed(older) error = %v", err)
			}
			loaded, err = st.LoadTeamConfigs(ctx)
			if err != nil {
				t.Fatalf("LoadTeamConfigs(after use) error = %v", err)
			}
			cfg = findParityTeamConfig(loaded, team)
			if cfg == nil || len(cfg.ReadTokenRecords) != 1 || cfg.ReadTokenRecords[0].LastUsedAt == nil || !cfg.ReadTokenRecords[0].LastUsedAt.Equal(usedAt) {
				t.Fatalf("last_used_at = %#v, want %s", cfg, usedAt)
			}

			revokedAt := time.Now().UTC()
			cfg.ReadTokenRecords[0].RevokedAt = &revokedAt
			if err := st.ReplaceTeamConfigs(ctx, loaded); err != nil {
				t.Fatalf("ReplaceTeamConfigs(revoked) error = %v", err)
			}
			active, err = st.IsAccessTokenActive(ctx, cfg.ReadTokenRecords[0].ID, revokedAt)
			if err != nil {
				t.Fatalf("IsAccessTokenActive(revoked) error = %v", err)
			}
			if active {
				t.Fatal("revoked access token reported active")
			}
		})
	}
}

func TestStoreParityPurgesOnlyOldInactiveAccessTokens(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.open(t)
			t.Cleanup(st.Close)

			team := "paritytokenretention" + tc.name
			existing, err := st.LoadTeamConfigs(ctx)
			if err != nil {
				t.Fatalf("LoadTeamConfigs(before) error = %v", err)
			}
			withoutTeam := withoutParityTeams(existing, team)
			t.Cleanup(func() {
				if err := st.ReplaceTeamConfigs(context.Background(), withoutTeam); err != nil {
					t.Errorf("cleanup token retention config error = %v", err)
				}
			})

			cutoff := time.Date(2026, time.May, 1, 0, 0, 0, 0, time.UTC)
			old := cutoff.Add(-time.Hour)
			recent := cutoff.Add(time.Hour)
			future := cutoff.Add(365 * 24 * time.Hour)
			record := func(id string, expiresAt, revokedAt *time.Time) auth.AccessTokenRecord {
				return auth.AccessTokenRecord{
					ID:        team + "-" + id,
					Prefix:    "pf_read_" + id,
					Digest:    testAccessTokenHasher(t).Digest(team + "-secret-" + id),
					CreatedAt: cutoff.Add(-365 * 24 * time.Hour),
					ExpiresAt: expiresAt,
					RevokedAt: revokedAt,
				}
			}
			configs := append(append([]auth.TeamConfig{}, withoutTeam...), auth.TeamConfig{
				Team: team,
				ReadTokenRecords: []auth.AccessTokenRecord{
					record("active", nil, nil),
					record("future", &future, nil),
					record("recent-expired", &recent, nil),
					record("old-expired", &old, nil),
					record("recent-revoked", &future, &recent),
					record("old-revoked", &future, &old),
				},
			})
			if err := st.ReplaceTeamConfigs(ctx, configs); err != nil {
				t.Fatalf("ReplaceTeamConfigs() error = %v", err)
			}

			deleted, err := st.PurgeAccessTokenHistory(ctx, cutoff)
			if err != nil {
				t.Fatalf("PurgeAccessTokenHistory() error = %v", err)
			}
			if deleted != 2 {
				t.Fatalf("PurgeAccessTokenHistory() deleted = %d, want 2", deleted)
			}
			loaded, err := st.LoadTeamConfigs(ctx)
			if err != nil {
				t.Fatalf("LoadTeamConfigs(after purge) error = %v", err)
			}
			cfg := findParityTeamConfig(loaded, team)
			if cfg == nil || len(cfg.ReadTokenRecords) != 4 {
				t.Fatalf("retained access token records = %#v, want 4", cfg)
			}
			for _, token := range cfg.ReadTokenRecords {
				if strings.Contains(token.ID, "old-expired") || strings.Contains(token.ID, "old-revoked") {
					t.Fatalf("old inactive token was retained: %#v", token)
				}
			}
		})
	}
}

func TestStoreParityLeaseExcludesConcurrentHolderAndRecoversAfterExpiry(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.open(t)
			t.Cleanup(st.Close)
			leaseName := "parity-expiring-lease-" + tc.name

			acquired, err := st.AcquireLease(ctx, leaseName, "replica-a", time.Second)
			if err != nil || !acquired {
				t.Fatalf("AcquireLease(replica-a) = %v, %v", acquired, err)
			}
			acquired, err = st.AcquireLease(ctx, leaseName, "replica-b", time.Second)
			if err != nil {
				t.Fatalf("AcquireLease(replica-b active) error = %v", err)
			}
			if acquired {
				t.Fatal("second replica acquired an active singleton lease")
			}

			time.Sleep(1100 * time.Millisecond)
			acquired, err = st.AcquireLease(ctx, leaseName, "replica-b", time.Second)
			if err != nil || !acquired {
				t.Fatalf("AcquireLease(replica-b expired) = %v, %v", acquired, err)
			}
		})
	}
}

func TestStoreParityManageSessionLifecycle(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.open(t)
			t.Cleanup(st.Close)
			now := time.Now().UTC().Truncate(time.Microsecond)
			suffix := tc.name + "-" + uuid.NewString()
			session := ManageSession{
				SessionHash:    "parity-session-" + suffix,
				CredentialHash: "credential-digest-" + suffix,
				CredentialID:   "token-id-" + suffix,
				AuthMethod:     "token",
				CSRFSecret:     "csrf-secret-" + suffix,
				CreatedAt:      now,
				ExpiresAt:      now.Add(time.Hour),
				LastSeenAt:     now,
			}
			if err := st.CreateManageSession(ctx, session); err != nil {
				t.Fatalf("CreateManageSession() error = %v", err)
			}
			got, err := st.GetManageSession(ctx, session.SessionHash, now.Add(2*time.Minute))
			if err != nil {
				t.Fatalf("GetManageSession() error = %v", err)
			}
			if got.CredentialHash != session.CredentialHash || got.CredentialID != session.CredentialID || got.CSRFSecret != session.CSRFSecret {
				t.Fatalf("GetManageSession() = %#v", got)
			}
			if !got.CreatedAt.Equal(session.CreatedAt) || !got.ExpiresAt.Equal(session.ExpiresAt) || !got.LastSeenAt.Equal(session.LastSeenAt) {
				t.Fatalf("GetManageSession() timestamps = created:%s expires:%s seen:%s", got.CreatedAt, got.ExpiresAt, got.LastSeenAt)
			}
			if err := st.RevokeManageSession(ctx, session.SessionHash, now.Add(3*time.Minute)); err != nil {
				t.Fatalf("RevokeManageSession() error = %v", err)
			}
			if _, err := st.GetManageSession(ctx, session.SessionHash, now.Add(4*time.Minute)); !errors.Is(err, ErrNotFound) {
				t.Fatalf("GetManageSession(revoked) error = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestStoreParitySharedRateLimitLifecycle(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.open(t)
			t.Cleanup(st.Close)
			now := time.Now().UTC().Truncate(time.Microsecond)
			key := "parity-rate-limit-" + tc.name + "-" + uuid.NewString()

			for attempt := range 3 {
				allowed, err := st.ConsumeRateLimit(ctx, key, 2, time.Minute, now)
				if err != nil {
					t.Fatalf("ConsumeRateLimit(attempt %d) error = %v", attempt, err)
				}
				if allowed != (attempt < 2) {
					t.Fatalf("ConsumeRateLimit(attempt %d) = %v", attempt, allowed)
				}
			}

			allowed, err := st.ConsumeRateLimit(ctx, key, 2, time.Minute, now.Add(time.Minute))
			if err != nil || !allowed {
				t.Fatalf("ConsumeRateLimit(after reset) = %v, %v", allowed, err)
			}
			deleted, err := st.PurgeRateLimits(ctx, now.Add(3*time.Minute))
			if err != nil {
				t.Fatalf("PurgeRateLimits() error = %v", err)
			}
			if deleted < 1 {
				t.Fatalf("PurgeRateLimits() = %d, want at least 1", deleted)
			}
		})
	}
}

func TestStoreParityPurgesExpiredSessionState(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.open(t)
			t.Cleanup(st.Close)
			now := time.Now().UTC().Truncate(time.Microsecond)
			suffix := tc.name + "-" + uuid.NewString()

			if err := st.CreateManageSession(ctx, ManageSession{
				SessionHash: "expired-manage-" + suffix, CredentialHash: "credential-" + suffix,
				AuthMethod: "token", CSRFSecret: "csrf-" + suffix,
				CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour), LastSeenAt: now.Add(-2 * time.Hour),
			}); err != nil {
				t.Fatalf("CreateManageSession(expired) error = %v", err)
			}
			if err := st.CreateOIDCState(ctx, OIDCState{
				StateHash: "expired-state-" + suffix, Nonce: "nonce-" + suffix, PKCEVerifier: "pkce-" + suffix,
				CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour),
			}); err != nil {
				t.Fatalf("CreateOIDCState(expired) error = %v", err)
			}
			if err := st.CreateOIDCSession(ctx, OIDCSession{
				SessionHash: "expired-oidc-" + suffix, Subject: "subject-" + suffix, CSRFSecret: "csrf-" + suffix,
				CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour), LastSeenAt: now.Add(-2 * time.Hour),
			}); err != nil {
				t.Fatalf("CreateOIDCSession(expired) error = %v", err)
			}

			result, err := st.PurgeSessionState(ctx, now, now.Add(-24*time.Hour))
			if err != nil {
				t.Fatalf("PurgeSessionState() error = %v", err)
			}
			if result.ManageSessions != 1 || result.OIDCStates != 1 || result.OIDCSessions != 1 {
				t.Fatalf("PurgeSessionState() = %#v", result)
			}
		})
	}
}

func TestStoreParityOIDCStateIsSingleUseAndExpires(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.open(t)
			t.Cleanup(st.Close)
			now := time.Now().UTC().Truncate(time.Microsecond)
			suffix := tc.name + "-" + uuid.NewString()
			state := OIDCState{
				StateHash:    "oidc-state-" + suffix,
				Nonce:        "nonce-" + suffix,
				PKCEVerifier: "pkce-verifier-" + suffix,
				NextPath:     "/manage",
				CreatedAt:    now,
				ExpiresAt:    now.Add(5 * time.Minute),
			}
			if err := st.CreateOIDCState(ctx, state); err != nil {
				t.Fatalf("CreateOIDCState() error = %v", err)
			}
			got, err := st.ConsumeOIDCState(ctx, state.StateHash, now.Add(time.Minute))
			if err != nil {
				t.Fatalf("ConsumeOIDCState() error = %v", err)
			}
			if got.Nonce != state.Nonce || got.PKCEVerifier != state.PKCEVerifier || got.NextPath != state.NextPath {
				t.Fatalf("ConsumeOIDCState() = %#v", got)
			}
			if !got.CreatedAt.Equal(state.CreatedAt) || !got.ExpiresAt.Equal(state.ExpiresAt) {
				t.Fatalf("ConsumeOIDCState() timestamps = created:%s expires:%s", got.CreatedAt, got.ExpiresAt)
			}
			if _, err := st.ConsumeOIDCState(ctx, state.StateHash, now.Add(2*time.Minute)); !errors.Is(err, ErrNotFound) {
				t.Fatalf("ConsumeOIDCState(replay) error = %v, want ErrNotFound", err)
			}

			expired := state
			expired.StateHash += "-expired"
			if err := st.CreateOIDCState(ctx, expired); err != nil {
				t.Fatalf("CreateOIDCState(expired) error = %v", err)
			}
			if _, err := st.ConsumeOIDCState(ctx, expired.StateHash, now.Add(6*time.Minute)); !errors.Is(err, ErrNotFound) {
				t.Fatalf("ConsumeOIDCState(expired) error = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestStoreParityOIDCSessionLifecycle(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.open(t)
			t.Cleanup(st.Close)
			now := time.Now().UTC().Truncate(time.Microsecond)
			suffix := tc.name + "-" + uuid.NewString()
			session := OIDCSession{
				SessionHash: "oidc-session-" + suffix, Subject: "subject-" + suffix,
				Email: "user@example.com", Name: "Test User", Groups: []string{"teamname-admins"}, CSRFSecret: "csrf-" + suffix,
				CreatedAt: now, ExpiresAt: now.Add(8 * time.Hour), LastSeenAt: now,
			}
			if err := st.CreateOIDCSession(ctx, session); err != nil {
				t.Fatalf("CreateOIDCSession() error = %v", err)
			}
			got, err := st.GetOIDCSession(ctx, session.SessionHash, now.Add(time.Minute))
			if err != nil {
				t.Fatalf("GetOIDCSession() error = %v", err)
			}
			if got.Subject != session.Subject || got.Email != session.Email || len(got.Groups) != 1 || got.Groups[0] != session.Groups[0] || got.CSRFSecret != session.CSRFSecret {
				t.Fatalf("GetOIDCSession() = %#v", got)
			}
			if !got.CreatedAt.Equal(session.CreatedAt) || !got.ExpiresAt.Equal(session.ExpiresAt) || !got.LastSeenAt.Equal(session.LastSeenAt) {
				t.Fatalf("GetOIDCSession() timestamps = created:%s expires:%s seen:%s", got.CreatedAt, got.ExpiresAt, got.LastSeenAt)
			}
			if err := st.RevokeOIDCSession(ctx, session.SessionHash, now.Add(2*time.Minute)); err != nil {
				t.Fatalf("RevokeOIDCSession() error = %v", err)
			}
			if _, err := st.GetOIDCSession(ctx, session.SessionHash, now.Add(3*time.Minute)); !errors.Is(err, ErrNotFound) {
				t.Fatalf("GetOIDCSession(revoked) error = %v, want ErrNotFound", err)
			}
		})
	}
}

func findParityTeamConfig(configs []auth.TeamConfig, team string) *auth.TeamConfig {
	for i := range configs {
		if configs[i].Team == team {
			return &configs[i]
		}
	}
	return nil
}

func TestStoreParityRejectsReusedAccessTokensWithoutReplacingConfig(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.open(t)
			t.Cleanup(st.Close)

			prefix := "parityduplicate" + tc.name
			existing, err := st.LoadTeamConfigs(ctx)
			if err != nil {
				t.Fatalf("LoadTeamConfigs(before) error = %v", err)
			}
			withoutParity := withoutParityTeams(existing, prefix)
			t.Cleanup(func() {
				if err := st.ReplaceTeamConfigs(context.Background(), withoutParity); err != nil {
					t.Errorf("cleanup parity access configs error = %v", err)
				}
			})

			valid := append(append([]auth.TeamConfig{}, withoutParity...), auth.TeamConfig{
				Team:       prefix + "original",
				ReadTokens: []string{prefix + "read"},
			})
			if err := st.ReplaceTeamConfigs(ctx, valid); err != nil {
				t.Fatalf("ReplaceTeamConfigs(valid) error = %v", err)
			}

			reusedToken := prefix + "reused"
			invalid := append(append([]auth.TeamConfig{}, withoutParity...),
				auth.TeamConfig{Team: prefix + "reader", ReadTokens: []string{reusedToken}},
				auth.TeamConfig{Team: prefix + "publisher", PublishTokens: []string{reusedToken}},
			)
			err = st.ReplaceTeamConfigs(ctx, invalid)
			if err == nil || !strings.Contains(err.Error(), "tokens must be globally unique") {
				t.Fatalf("ReplaceTeamConfigs(invalid) error = %v, want reused-token validation", err)
			}
			if strings.Contains(err.Error(), reusedToken) {
				t.Fatalf("ReplaceTeamConfigs(invalid) leaked token in error: %v", err)
			}

			got, err := st.LoadTeamConfigs(ctx)
			if err != nil {
				t.Fatalf("LoadTeamConfigs(after) error = %v", err)
			}
			got = filterParityTeams(got, prefix)
			if len(got) != 1 || got[0].Team != prefix+"original" {
				t.Fatalf("invalid replacement changed access config: %#v", got)
			}
		})
	}
}

func TestStoreParityFilteredModulePagination(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.open(t)
			t.Cleanup(st.Close)

			prefix := "parity-filter-" + tc.name + "-"
			owners := []string{prefix + "alpha", prefix + "beta"}
			for _, moduleIdentity := range []struct {
				owner string
				name  string
			}{
				{owner: owners[0], name: "service-one"},
				{owner: owners[0], name: "service-two"},
				{owner: owners[1], name: "service-three"},
				{owner: owners[1], name: "literal%percent"},
				{owner: owners[1], name: "literal_under_score"},
			} {
				module, err := st.UpsertModule(ctx, moduleIdentity.owner, moduleIdentity.name)
				if err != nil {
					t.Fatalf("UpsertModule(%s/%s) error = %v", moduleIdentity.owner, moduleIdentity.name, err)
				}
				if _, err := st.CreateRelease(ctx, domain.Release{
					ID:          module.ID + ":1.0.0",
					ModuleID:    module.ID,
					Owner:       moduleIdentity.owner,
					Name:        moduleIdentity.name,
					Source:      "local",
					Version:     "1.0.0",
					FileName:    moduleIdentity.owner + "-" + moduleIdentity.name + "-1.0.0.tar.gz",
					ContentType: "application/gzip",
					Metadata:    map[string]any{},
				}); err != nil {
					t.Fatalf("CreateRelease(%s/%s) error = %v", moduleIdentity.owner, moduleIdentity.name, err)
				}
				t.Cleanup(func() {
					if err := st.DeleteModule(context.Background(), moduleIdentity.owner, moduleIdentity.name); err != nil && !errors.Is(err, ErrNotFound) {
						t.Errorf("cleanup module %s/%s error = %v", moduleIdentity.owner, moduleIdentity.name, err)
					}
				})
			}
			setParityModuleUpdatedAt(t, st, owners, "2026-01-01 00:00:00+00:00")

			first, total, err := st.ListModulesPageFiltered(ctx, []string{owners[0]}, "service", 1, 0)
			if err != nil {
				t.Fatalf("ListModulesPageFiltered(first) error = %v", err)
			}
			if total != 2 || len(first) != 1 || first[0].Owner != owners[0] {
				t.Fatalf("first filtered page = %#v, total = %d", first, total)
			}

			second, total, err := st.ListModulesPageFiltered(ctx, []string{owners[0]}, "service", 1, 1)
			if err != nil {
				t.Fatalf("ListModulesPageFiltered(second) error = %v", err)
			}
			if total != 2 || len(second) != 1 || second[0].Owner != owners[0] || second[0].Name == first[0].Name {
				t.Fatalf("second filtered page = %#v, total = %d", second, total)
			}

			matched, total, err := st.ListModulesPageFiltered(ctx, owners, owners[1]+"/service-three", 10, 0)
			if err != nil {
				t.Fatalf("ListModulesPageFiltered(exact search) error = %v", err)
			}
			if total != 1 || len(matched) != 1 || matched[0].Owner != owners[1] {
				t.Fatalf("exact filtered page = %#v, total = %d", matched, total)
			}

			literalPercent, total, err := st.ListModulesPageFiltered(ctx, owners, "%", 10, 0)
			if err != nil {
				t.Fatalf("ListModulesPageFiltered(literal percent) error = %v", err)
			}
			if total != 1 || len(literalPercent) != 1 || literalPercent[0].Name != "literal%percent" {
				t.Fatalf("literal percent page = %#v, total = %d", literalPercent, total)
			}

			literalUnderscore, total, err := st.ListModulesPageFiltered(ctx, owners, "_", 10, 0)
			if err != nil {
				t.Fatalf("ListModulesPageFiltered(literal underscore) error = %v", err)
			}
			if total != 1 || len(literalUnderscore) != 1 || literalUnderscore[0].Name != "literal_under_score" {
				t.Fatalf("literal underscore page = %#v, total = %d", literalUnderscore, total)
			}

			stable, total, err := st.ListModulesPageFiltered(ctx, []string{owners[0]}, "service", 10, 0)
			if err != nil {
				t.Fatalf("ListModulesPageFiltered(stable order) error = %v", err)
			}
			if total != 2 || len(stable) != 2 || stable[0].Name != "service-one" || stable[1].Name != "service-two" {
				t.Fatalf("stable page = %#v, total = %d", stable, total)
			}

			prioritized, total, err := st.ListModulesPagePrioritized(ctx, owners, []string{owners[1]}, "service", 10, 0)
			if err != nil {
				t.Fatalf("ListModulesPagePrioritized() error = %v", err)
			}
			if total != 3 || len(prioritized) != 3 || prioritized[0].Owner != owners[1] || prioritized[0].Name != "service-three" {
				t.Fatalf("prioritized page = %#v, total = %d", prioritized, total)
			}

			selectedCounts, err := st.CountModulesByOwner(ctx, []string{owners[0]})
			if err != nil {
				t.Fatalf("CountModulesByOwner(selected) error = %v", err)
			}
			if !reflect.DeepEqual(selectedCounts, map[string]int{owners[0]: 2}) {
				t.Fatalf("selected owner counts = %#v", selectedCounts)
			}

			emptyCounts, err := st.CountModulesByOwner(ctx, []string{})
			if err != nil {
				t.Fatalf("CountModulesByOwner(empty) error = %v", err)
			}
			if len(emptyCounts) != 0 {
				t.Fatalf("empty owner counts = %#v, want empty", emptyCounts)
			}

			allCounts, err := st.CountModulesByOwner(ctx, nil)
			if err != nil {
				t.Fatalf("CountModulesByOwner(all) error = %v", err)
			}
			if allCounts[owners[0]] != 2 || allCounts[owners[1]] != 3 {
				t.Fatalf("all owner counts = %#v", allCounts)
			}

			for _, pagination := range []struct {
				limit  int
				offset int
			}{
				{limit: 0},
				{limit: MaxModulePageSize + 1},
				{limit: 1, offset: -1},
				{limit: 1, offset: MaxModulePageOffset + 1},
			} {
				if _, _, err := st.ListModulesPageFiltered(ctx, nil, "", pagination.limit, pagination.offset); err == nil {
					t.Fatalf("ListModulesPageFiltered(limit=%d, offset=%d) error = nil", pagination.limit, pagination.offset)
				}
			}
			deep, _, err := st.ListModulesPageFiltered(ctx, nil, "", 1, MaxModulePageOffset)
			if err != nil {
				t.Fatalf("ListModulesPageFiltered(maximum offset) error = %v", err)
			}
			if len(deep) != 0 {
				t.Fatalf("maximum-offset page = %#v, want empty", deep)
			}
		})
	}
}

func setParityModuleUpdatedAt(t *testing.T, st parityStore, owners []string, updatedAt string) {
	t.Helper()
	ctx := context.Background()
	switch typed := st.(type) {
	case *SQLiteStore:
		for _, owner := range owners {
			if _, err := typed.db.ExecContext(ctx, `update modules set updated_at = ? where owner = ?`, updatedAt, owner); err != nil {
				t.Fatalf("set SQLite module timestamp error = %v", err)
			}
		}
	case *PostgresStore:
		if _, err := typed.pool.Exec(ctx, `update modules set updated_at = $1 where owner = any($2::text[])`, updatedAt, owners); err != nil {
			t.Fatalf("set PostgreSQL module timestamps error = %v", err)
		}
	default:
		t.Fatalf("unsupported parity store %T", st)
	}
}

type parityStoreCase struct {
	name string
	open func(t *testing.T) parityStore
}

func parityStoreCases(t *testing.T) []parityStoreCase {
	t.Helper()

	cases := []parityStoreCase{
		{
			name: "sqlite",
			open: func(t *testing.T) parityStore {
				t.Helper()
				st, err := NewSQLiteStore("sqlite://:memory:", testAccessTokenHasher(t))
				if err != nil {
					t.Fatalf("NewSQLiteStore() error = %v", err)
				}
				return st
			},
		},
	}

	if dsn := os.Getenv("PUPPET_FORGE_TEST_POSTGRES_DSN"); dsn != "" {
		cases = append(cases, parityStoreCase{
			name: "postgres",
			open: func(t *testing.T) parityStore {
				t.Helper()
				isolatedDSN := isolatedPostgresTestDSN(t, dsn)
				st, err := NewPostgresStore(context.Background(), isolatedDSN, testAccessTokenHasher(t))
				if err != nil {
					t.Fatalf("NewPostgresStore() error = %v", err)
				}
				return st
			},
		})
	}
	return cases
}

func isolatedPostgresTestDSN(t *testing.T, dsn string) string {
	t.Helper()
	ctx := context.Background()
	adminPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open PostgreSQL test administration connection error = %v", err)
	}
	schema := "parity_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := adminPool.Exec(ctx, "create schema "+schema); err != nil {
		adminPool.Close()
		t.Fatalf("create PostgreSQL test schema error = %v", err)
	}
	t.Cleanup(func() {
		if _, err := adminPool.Exec(context.Background(), "drop schema "+schema+" cascade"); err != nil {
			t.Errorf("drop PostgreSQL test schema error = %v", err)
		}
		adminPool.Close()
	})
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	return fmt.Sprintf("%s%ssearch_path=%s", dsn, separator, schema)
}

func moduleVersions(versions []domain.ModuleVersion) []string {
	got := make([]string, 0, len(versions))
	for _, version := range versions {
		got = append(got, version.Version)
	}
	return got
}

func hasReleaseSummary(releases []ReleaseSummary, owner, name, version string) bool {
	for _, release := range releases {
		if release.Owner == owner && release.Name == name && release.Version == version {
			return true
		}
	}
	return false
}

func filterParityTeams(configs []auth.TeamConfig, prefix string) []auth.TeamConfig {
	next := make([]auth.TeamConfig, 0, len(configs))
	for _, cfg := range configs {
		if strings.HasPrefix(cfg.Team, prefix) {
			next = append(next, cfg)
		}
	}
	return next
}

func withoutParityTeams(configs []auth.TeamConfig, prefix string) []auth.TeamConfig {
	next := make([]auth.TeamConfig, 0, len(configs))
	for _, cfg := range configs {
		if !strings.HasPrefix(cfg.Team, prefix) {
			next = append(next, cfg)
		}
	}
	return next
}

func normalizeTeamConfigs(configs []auth.TeamConfig) {
	sort.Slice(configs, func(i, j int) bool {
		return configs[i].Team < configs[j].Team
	})
	for i := range configs {
		sort.Strings(configs[i].ReadTokens)
		sort.Strings(configs[i].PublishTokens)
		sort.Strings(configs[i].PublishOwners)
		sort.Strings(configs[i].OIDCGroups)
		sort.Strings(configs[i].OIDCTeamAdminEmails)
		sort.Strings(configs[i].OIDCTeamAdminGroups)
		sort.Strings(configs[i].OIDCAdminEmails)
		sort.Strings(configs[i].OIDCAdminSubjects)
		sort.Strings(configs[i].OIDCAdminGroups)
	}
}
