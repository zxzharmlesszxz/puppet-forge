package service

import (
	"context"
	"fmt"
	"testing"

	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

type releaseListingStore struct {
	*store.SQLiteStore
	lists, batches, statuses int
}

func (s *releaseListingStore) ListReleases(ctx context.Context, owner, name string) ([]domain.ModuleVersion, error) {
	s.lists++
	return s.SQLiteStore.ListReleases(ctx, owner, name)
}

func (s *releaseListingStore) ListReleasesForModules(ctx context.Context, modules []domain.Module) ([]store.ModuleReleaseSummary, error) {
	s.batches++
	return s.SQLiteStore.ListReleasesForModules(ctx, modules)
}

func (s *releaseListingStore) GetResourceStatus(ctx context.Context, owner, name, version string) (domain.ResourceStatus, error) {
	s.statuses++
	return s.SQLiteStore.GetResourceStatus(ctx, owner, name, version)
}

func TestReleaseListingDoesNotReadStatusPerVersion(t *testing.T) {
	for _, count := range []int{1, 100} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			st, err := store.NewSQLiteStore("sqlite://:memory:")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(st.Close)
			module, err := st.UpsertModule(t.Context(), "teamname", "demo")
			if err != nil {
				t.Fatal(err)
			}
			for index := range count {
				release := store.NewRelease(module.ID, module.Owner, module.Name, fmt.Sprintf("1.0.%d", index), "", "", "demo.tar.gz", "application/gzip", "md5", "sha", "object", 1, nil)
				release.InitialState = domain.StateAvailable
				if _, err := st.CreateRelease(t.Context(), release); err != nil {
					t.Fatal(err)
				}
			}
			counted := &releaseListingStore{SQLiteStore: st}
			svc := NewModuleService(counted, &testArtifactStorage{}, "modules", nil)
			ctx := store.ReadableCatalogContext(t.Context())
			versions, err := svc.ListReleases(ctx, module.Owner, module.Name)
			if err != nil || len(versions) != count {
				t.Fatalf("single listing: %d %v", len(versions), err)
			}
			grouped, err := svc.ListReleasesForModules(ctx, []domain.Module{module})
			if err != nil || len(grouped[module.Owner+"\x00"+module.Name]) != count {
				t.Fatalf("batch listing: %#v %v", grouped, err)
			}
			if counted.lists != 1 || counted.batches != 1 || counted.statuses != 0 {
				t.Fatalf("N+1 reads: %#v", counted)
			}
			for _, version := range grouped[module.Owner+"\x00"+module.Name] {
				if version.Status.State != domain.StateAvailable || version.Status.SHA256 != "sha" {
					t.Fatalf("missing joined status: %#v", version)
				}
			}
		})
	}
}
