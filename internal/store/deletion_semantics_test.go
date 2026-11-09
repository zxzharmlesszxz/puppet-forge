package store

import (
	"errors"
	"testing"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
)

func TestStoreParityLocalDeletionAllowsRepublication(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		for _, whole := range []bool{false, true} {
			name := tc.name + "/release"
			if whole {
				name = tc.name + "/module"
			}
			t.Run(name, func(t *testing.T) {
				st := tc.open(t)
				t.Cleanup(st.Close)
				module, err := st.UpsertModule(t.Context(), "localteam", "demo")
				if err != nil {
					t.Fatal(err)
				}
				release := NewRelease(module.ID, module.Owner, module.Name, "1.0.0", "", "", "demo.tar.gz", "application/gzip", "md5", "sha", "local/archive", 7, nil)
				release.InitialState = domain.StatePending
				if _, err := st.CreateRelease(t.Context(), release); err != nil {
					t.Fatal(err)
				}
				if whole {
					err = st.DeleteModule(t.Context(), module.Owner, module.Name)
				} else {
					err = st.DeleteRelease(t.Context(), module.Owner, module.Name, release.Version)
				}
				if err != nil {
					t.Fatal(err)
				}
				status, err := st.(LifecycleStore).GetResourceStatus(t.Context(), module.Owner, module.Name, release.Version)
				if err != nil || status.State != domain.StateAvailable {
					t.Fatalf("deletion left a ban: %#v %v", status, err)
				}
				local, err := st.(LocalModuleOriginStore).HasLocalModuleOrigin(t.Context(), module.Owner, module.Name)
				if err != nil || !local {
					t.Fatalf("local origin lost: %v %v", local, err)
				}
				if err := saveTestCatalog(t, t.Context(), st, module.Owner, module.Name, []string{release.Version}, CatalogLease{}); err != nil {
					t.Fatal(err)
				}
				if _, err := st.GetModule(t.Context(), module.Owner, module.Name); !errors.Is(err, ErrNotFound) {
					t.Fatalf("upstream replaced a deleted local module: %v", err)
				}
				module, err = st.UpsertModule(t.Context(), module.Owner, module.Name)
				if err != nil {
					t.Fatal(err)
				}
				release.ModuleID = module.ID
				if _, err := st.CreateReleaseIfAbsent(t.Context(), release); err != nil {
					t.Fatal(err)
				}
				status, err = st.(LifecycleStore).GetResourceStatus(t.Context(), module.Owner, module.Name, release.Version)
				if err != nil || status.State != domain.StatePending {
					t.Fatalf("republished release bypassed approval: %#v %v", status, err)
				}
				pending, err := st.(ArtifactDeletionStore).CountArtifactDeletions(t.Context())
				if err != nil || pending != 0 {
					t.Fatalf("republished artifact still queued: %d %v", pending, err)
				}
			})
		}
	}
}

func TestStoreParityBlockedReleaseSurvivesDeletionAndRepublication(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		for _, whole := range []bool{false, true} {
			name := tc.name + "/release"
			if whole {
				name = tc.name + "/module"
			}
			t.Run(name, func(t *testing.T) {
				st := tc.open(t)
				t.Cleanup(st.Close)
				module, err := st.UpsertModule(t.Context(), "localteam", "blocked")
				if err != nil {
					t.Fatal(err)
				}
				release := NewRelease(module.ID, module.Owner, module.Name, "1.0.0", "", "", "demo.tar.gz", "application/gzip", "md5", "sha", "local/blocked", 7, nil)
				release.InitialState = domain.StateAvailable
				if _, err := st.CreateRelease(t.Context(), release); err != nil {
					t.Fatal(err)
				}
				states := st.(LifecycleStore)
				status := domain.ResourceStatus{State: domain.StateBlocked, Previous: domain.StateAvailable, Reason: "broken", ChangedAt: time.Now().UTC()}
				if err := states.SetResourceStatus(t.Context(), module.Owner, module.Name, release.Version, status); err != nil {
					t.Fatal(err)
				}
				if whole {
					err = st.DeleteModule(t.Context(), module.Owner, module.Name)
				} else {
					err = st.DeleteRelease(t.Context(), module.Owner, module.Name, release.Version)
				}
				if err != nil {
					t.Fatal(err)
				}
				status, err = states.GetResourceStatus(t.Context(), module.Owner, module.Name, release.Version)
				if err != nil || status.State != domain.StateBlocked || status.Reason != "broken" {
					t.Fatalf("block lost: %#v %v", status, err)
				}
				module, err = st.UpsertModule(t.Context(), module.Owner, module.Name)
				if err != nil {
					t.Fatal(err)
				}
				release.ModuleID = module.ID
				if _, err := st.CreateReleaseIfAbsent(t.Context(), release); !errors.Is(err, ErrConflict) {
					t.Fatalf("publication bypassed block: %v", err)
				}
			})
		}
	}
}

func TestStoreParityDeletionMigrationPreservesBlocks(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			st := tc.open(t)
			t.Cleanup(st.Close)
			if err := saveTestCatalog(t, t.Context(), st, "migrationteam", "demo", []string{"1.0.0", "2.0.0"}, CatalogLease{}); err != nil {
				t.Fatal(err)
			}
			states := st.(LifecycleStore)
			for _, version := range []string{"", "1.0.0", "2.0.0"} {
				status := domain.ResourceStatus{State: domain.StateDeleted, ChangedAt: time.Now().UTC()}
				if version == "2.0.0" {
					status.State, status.Previous, status.Reason = domain.StateBlocked, domain.StateDeleted, "explicit block"
				}
				if err := states.SetResourceStatus(t.Context(), "migrationteam", "demo", version, status); err != nil {
					t.Fatal(err)
				}
			}
			execute, migrate := migrationTestOperations(t, st)
			for _, q := range []string{
				`delete from modules where owner='migrationteam' and name='demo'`,
				`insert into deleted_releases(owner,name,version,source) values('migrationteam','demo','1.0.0','upstream')`,
				`update schema_metadata set version=10 where id=1`,
			} {
				if err := execute(q); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if err := migrate(); err != nil {
					t.Fatal(err)
				}
			}
			for _, version := range []string{"", "1.0.0"} {
				status, err := states.GetResourceStatus(t.Context(), "migrationteam", "demo", version)
				if err != nil || status.State != domain.StateAvailable {
					t.Fatalf("legacy deletion ban survived: %#v %v", status, err)
				}
			}
			status, err := states.GetResourceStatus(t.Context(), "migrationteam", "demo", "2.0.0")
			if err != nil || status.State != domain.StateBlocked || status.Previous != domain.StateAvailable || status.Reason != "explicit block" {
				t.Fatalf("migration changed block: %#v %v", status, err)
			}
			indexed, err := st.(UpstreamCatalogStore).HasUpstreamCatalog(t.Context(), "migrationteam", "demo")
			if err != nil || indexed {
				t.Fatalf("stale catalog completion survived: %v %v", indexed, err)
			}
			deleted, err := st.IsReleaseDeleted(t.Context(), "migrationteam", "demo", "1.0.0", "upstream")
			if err != nil || deleted {
				t.Fatalf("legacy tombstone survived: %v %v", deleted, err)
			}
			if err := saveTestCatalog(t, t.Context(), st, "migrationteam", "demo", []string{"1.0.0", "2.0.0"}, CatalogLease{}); err != nil {
				t.Fatal(err)
			}
			if _, err := st.GetRelease(t.Context(), "migrationteam", "demo", "1.0.0"); err != nil {
				t.Fatal(err)
			}
			if _, err := st.GetRelease(t.Context(), "migrationteam", "demo", "2.0.0"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("blocked version restored: %v", err)
			}
		})
	}
}
