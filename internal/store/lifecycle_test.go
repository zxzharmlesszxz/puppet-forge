package store

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
)

func TestStoreParityReleaseListsIncludeLifecycle(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			st := tc.open(t)
			t.Cleanup(st.Close)
			states := st.(LifecycleStore)
			var modules []domain.Module
			expected := make(map[string]domain.ResourceStatus)
			for _, name := range []string{"first", "second"} {
				module, err := st.UpsertModule(t.Context(), "reviewteam", name)
				if err != nil {
					t.Fatal(err)
				}
				modules = append(modules, module)
				for index, state := range []domain.ResourceState{domain.StateAvailable, domain.StatePending, domain.StateDeprecated, domain.StateBlocked, domain.StateDeleted} {
					version := fmt.Sprintf("1.0.%d", index)
					release := NewRelease(module.ID, module.Owner, name, version, "", "", "demo.tar.gz", "application/gzip", "md5", "sha", "object", 1, nil)
					if _, err := st.CreateRelease(t.Context(), release); err != nil {
						t.Fatal(err)
					}
					status := domain.ResourceStatus{State: domain.StateAvailable}
					if index != 0 {
						status = domain.ResourceStatus{State: state, Previous: domain.StateAvailable, Reason: "reviewed", Replacement: "2.0.0", SHA256: "sha", ChangedAt: time.Now().UTC().Truncate(time.Microsecond)}
						if err := states.SetResourceStatus(t.Context(), module.Owner, name, version, status); err != nil {
							t.Fatal(err)
						}
					}
					expected[name+"@"+version] = status
				}
			}
			for _, readable := range []bool{false, true} {
				ctx := t.Context()
				want := 5
				if readable {
					ctx, want = ReadableCatalogContext(ctx), 2
				}
				for _, module := range modules {
					versions, err := st.ListReleases(ctx, module.Owner, module.Name)
					if err != nil || len(versions) != want {
						t.Fatalf("list readable=%v: %#v %v", readable, versions, err)
					}
					for _, version := range versions {
						if !reflect.DeepEqual(version.Status, expected[module.Name+"@"+version.Version]) || readable && !version.Status.State.Readable() {
							t.Fatalf("wrong version status: %#v", version)
						}
					}
				}
				releases, err := st.ListReleasesForModules(ctx, modules)
				if err != nil || len(releases) != want*len(modules) {
					t.Fatalf("batch readable=%v: %#v %v", readable, releases, err)
				}
				for _, release := range releases {
					if !reflect.DeepEqual(release.Status, expected[release.Name+"@"+release.Version]) || readable && !release.Status.State.Readable() {
						t.Fatalf("wrong batch status: %#v", release)
					}
				}
			}
		})
	}
}

func TestStoreParityResourceStatusFailureRollsBack(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			st := tc.open(t)
			t.Cleanup(st.Close)
			states, ok := st.(LifecycleStore)
			if !ok {
				t.Fatal("missing lifecycle store")
			}
			status := domain.ResourceStatus{State: domain.StateBlocked, Reason: "missing module", ChangedAt: time.Now().UTC()}
			ctx := ContextWithMutationAudit(t.Context(), AuditEvent{Actor: "reviewer", Action: "moderate_resource", Result: "success", OccurredAt: time.Now()})
			if err := states.SetResourceStatus(ctx, "reviewteam", "missing", "1.0.0", status); err == nil {
				t.Fatal("missing module mutation unexpectedly succeeded")
			}
			stored, err := states.GetResourceStatus(t.Context(), "reviewteam", "missing", "1.0.0")
			if err != nil || stored.State != domain.StateAvailable {
				t.Fatalf("failed mutation persisted status: %#v err=%v", stored, err)
			}
			_, count, err := st.ListAuditEvents(t.Context(), nil, "", 10, 0)
			if err != nil || count != 0 {
				t.Fatalf("failed mutation persisted audit: count=%d err=%v", count, err)
			}
		})
	}
}

func TestStoreParityReleaseLifecycle(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			st := tc.open(t)
			t.Cleanup(st.Close)
			states, ok := st.(LifecycleStore)
			if !ok {
				t.Fatal("missing lifecycle store")
			}
			module, err := st.UpsertModule(t.Context(), "reviewteam", "demo")
			if err != nil {
				t.Fatal(err)
			}
			for _, version := range []string{"1.0.0", "2.0.0"} {
				release := NewRelease(module.ID, "reviewteam", "demo", version, "", "", "module.tar.gz", "application/gzip", "md5", "sha", "modules/"+version, 1, map[string]any{})
				if version == "2.0.0" {
					release.InitialState = domain.StatePending
				}
				if _, err := st.CreateRelease(t.Context(), release); err != nil {
					t.Fatal(err)
				}
			}
			status, err := states.GetResourceStatus(t.Context(), "reviewteam", "demo", "1.0.0")
			if err != nil || status.State != domain.StateAvailable {
				t.Fatalf("legacy status = %#v %v", status, err)
			}
			module, err = st.GetModule(t.Context(), "reviewteam", "demo")
			if err != nil || module.LatestVersion != "1.0.0" {
				t.Fatalf("pending latest = %#v %v", module, err)
			}
			status = domain.ResourceStatus{State: domain.StateAvailable, SHA256: "sha", Reason: "reviewed", ChangedAt: time.Now().UTC()}
			ctx := ContextWithMutationAudit(t.Context(), AuditEvent{Actor: "reviewer", Action: "moderate_resource", Result: "success", OccurredAt: time.Now(), Attributes: map[string]string{"module": "reviewteam/demo"}})
			if err := states.SetResourceStatus(ctx, "reviewteam", "demo", "2.0.0", status); err != nil {
				t.Fatal(err)
			}
			module, err = st.GetModule(t.Context(), "reviewteam", "demo")
			if err != nil || module.LatestVersion != "2.0.0" {
				t.Fatalf("approved latest = %#v %v", module, err)
			}
			_, count, err := st.ListAuditEvents(t.Context(), nil, "", 10, 0)
			if err != nil || count != 1 {
				t.Fatalf("moderation audit = %d %v", count, err)
			}
			status.State = domain.StateBlocked
			status.Previous = domain.StateAvailable
			if err := states.SetResourceStatus(t.Context(), "reviewteam", "demo", "2.0.0", status); err != nil {
				t.Fatal(err)
			}
			module, err = st.GetModule(t.Context(), "reviewteam", "demo")
			if err != nil || module.LatestVersion != "1.0.0" {
				t.Fatalf("blocked latest = %#v %v", module, err)
			}
			if err := st.DeleteRelease(t.Context(), "reviewteam", "demo", "2.0.0"); err != nil {
				t.Fatal(err)
			}
			status, err = states.GetResourceStatus(t.Context(), "reviewteam", "demo", "2.0.0")
			if err != nil || status.State != domain.StateBlocked {
				t.Fatalf("delete removed the explicit block = %#v %v", status, err)
			}
		})
	}
}
