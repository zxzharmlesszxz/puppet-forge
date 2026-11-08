package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
)

func saveTestCatalog(t *testing.T, ctx context.Context, st parityStore, owner, name string, versions []string, lease CatalogLease) error {
	t.Helper()
	unlock, err := st.LockModule(ctx, owner, name)
	if err != nil {
		return err
	}
	defer func() {
		if err := unlock(); err != nil {
			t.Error(err)
		}
	}()
	return st.(UpstreamCatalogStore).SaveUpstreamCatalog(ctx, owner, name, versions, lease)
}

func TestStoreParityUpstreamCatalogCommitRollbackAndRetry(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			st := tc.open(t)
			t.Cleanup(st.Close)
			event := AuditEvent{ID: "catalog-commit", OccurredAt: time.Now().UTC(), Actor: "test", Action: "index_upstream", Result: "success"}
			if err := st.RecordAuditEvent(t.Context(), event); err != nil {
				t.Fatal(err)
			}
			ctx := ContextWithMutationAudit(t.Context(), event)
			if err := saveTestCatalog(t, ctx, st, "catalogteam", "demo", []string{"1.0.0", "2.0.0"}, CatalogLease{}); err == nil {
				t.Fatal("commit failure did not roll back catalog")
			}
			indexed, err := st.(UpstreamCatalogStore).HasUpstreamCatalog(t.Context(), "catalogteam", "demo")
			if err != nil || indexed || MutationAuditRecorded(ctx) {
				t.Fatalf("failed catalog marked complete: %v %v", indexed, err)
			}
			if _, err := st.GetModule(t.Context(), "catalogteam", "demo"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("partial module survived: %v", err)
			}
			if err := saveTestCatalog(t, t.Context(), st, "catalogteam", "demo", []string{"1.0.0", "2.0.0", "2.0.0"}, CatalogLease{}); err != nil {
				t.Fatal(err)
			}
			indexed, err = st.(UpstreamCatalogStore).HasUpstreamCatalog(t.Context(), "catalogteam", "demo")
			if err != nil || !indexed {
				t.Fatalf("successful catalog not marked: %v %v", indexed, err)
			}
			versions, err := st.ListReleases(t.Context(), "catalogteam", "demo")
			if err != nil || len(versions) != 2 {
				t.Fatalf("duplicate/partial catalog: %#v %v", versions, err)
			}
			module, err := st.GetModule(t.Context(), "catalogteam", "demo")
			if err != nil || module.LatestVersion != "2.0.0" {
				t.Fatalf("catalog latest=%#v %v", module, err)
			}
		})
	}
}

func TestStoreParityUpstreamCatalogLeaseFence(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		for _, stolen := range []bool{false, true} {
			t.Run(tc.name+"/stolen="+map[bool]string{false: "false", true: "true"}[stolen], func(t *testing.T) {
				st := tc.open(t)
				t.Cleanup(st.Close)
				lease := CatalogLease{Name: "catalog-fence", Holder: "old-leader"}
				if acquired, err := st.AcquireLease(t.Context(), lease.Name, lease.Holder, -time.Second); err != nil || !acquired {
					t.Fatalf("expired fixture lease=%v %v", acquired, err)
				}
				if stolen {
					if acquired, err := st.AcquireLease(t.Context(), lease.Name, "new-leader", time.Minute); err != nil || !acquired {
						t.Fatalf("replacement lease=%v %v", acquired, err)
					}
				}
				if err := saveTestCatalog(t, t.Context(), st, "catalogteam", "demo", []string{"1.0.0"}, lease); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("expired leader committed: %v", err)
				}
				indexed, err := st.(UpstreamCatalogStore).HasUpstreamCatalog(t.Context(), "catalogteam", "demo")
				if err != nil || indexed {
					t.Fatalf("lost leader marked complete: %v %v", indexed, err)
				}
				if _, err := st.GetModule(t.Context(), "catalogteam", "demo"); !errors.Is(err, ErrNotFound) {
					t.Fatalf("lost leader wrote module: %v", err)
				}
			})
		}
	}
}

func TestStoreParityUpstreamCatalogRejectsUnavailableModule(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		for _, state := range []domain.ResourceState{domain.StateBlocked, domain.StateDeleted, domain.StatePending} {
			t.Run(tc.name+"/"+string(state), func(t *testing.T) {
				st := tc.open(t)
				t.Cleanup(st.Close)
				if _, err := st.UpsertModule(t.Context(), "catalogteam", "demo"); err != nil {
					t.Fatal(err)
				}
				if err := st.(LifecycleStore).SetResourceStatus(t.Context(), "catalogteam", "demo", "", domain.ResourceStatus{State: state, ChangedAt: time.Now().UTC()}); err != nil {
					t.Fatal(err)
				}
				if err := saveTestCatalog(t, t.Context(), st, "catalogteam", "demo", []string{"1.0.0"}, CatalogLease{}); err != nil {
					t.Fatal(err)
				}
				indexed, err := st.(UpstreamCatalogStore).HasUpstreamCatalog(t.Context(), "catalogteam", "demo")
				if err != nil || indexed {
					t.Fatalf("unavailable catalog marked complete: %v %v", indexed, err)
				}
				if _, err := st.GetRelease(t.Context(), "catalogteam", "demo", "1.0.0"); !errors.Is(err, ErrNotFound) {
					t.Fatalf("unavailable module received release: %v", err)
				}
			})
		}
	}
}
