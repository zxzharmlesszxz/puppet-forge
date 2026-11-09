package store

import (
	"reflect"
	"testing"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
)

func TestStoreParityResourceMetricStates(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			st := tc.open(t)
			t.Cleanup(st.Close)
			ctx := t.Context()
			states := st.(LifecycleStore)
			pendingAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
			for _, state := range []domain.ResourceState{domain.StateAvailable, domain.StatePending, domain.StateDeprecated, domain.StateBlocked} {
				module, err := st.UpsertModule(ctx, "metricteam", string(state))
				if err != nil {
					t.Fatal(err)
				}
				for _, version := range []string{"1.0.0", "2.0.0"} {
					release := NewRelease(module.ID, module.Owner, module.Name, version, "", "", "demo.tar.gz", "application/gzip", "", "sha", "modules/"+module.Name+version, 1, nil)
					if _, err := st.CreateRelease(ctx, release); err != nil {
						t.Fatal(err)
					}
				}
				status := domain.ResourceStatus{State: state, ChangedAt: pendingAt}
				if err := states.SetResourceStatus(ctx, module.Owner, module.Name, "", status); err != nil {
					t.Fatal(err)
				}
			}
			// The release's own block wins over a parent's Pending state.
			if err := states.SetResourceStatus(ctx, "metricteam", "pending", "2.0.0", domain.ResourceStatus{State: domain.StateBlocked, ChangedAt: pendingAt}); err != nil {
				t.Fatal(err)
			}
			if err := saveTestCatalog(t, ctx, st, "metricupstream", "demo", []string{"1.0.0"}, CatalogLease{}); err != nil {
				t.Fatal(err)
			}
			backend := st.(ResourceMetricStore)
			summaries, err := backend.ListResourceMetricSummaries(ReadableCatalogContext(ctx))
			if err != nil {
				t.Fatal(err)
			}
			counts := make(map[[3]string]int)
			for _, summary := range summaries {
				counts[[3]string{summary.Kind, summary.Source, string(summary.State)}] = summary.Count
				if summary.State == domain.StatePending && !summary.OldestPendingAt.Equal(pendingAt) {
					t.Fatalf("pending age lost: %#v", summary)
				}
			}
			want := map[[3]string]int{
				{"module", "local", "available"}: 1, {"module", "local", "pending"}: 1,
				{"module", "local", "deprecated"}: 1, {"module", "local", "blocked"}: 1,
				{"release", "local", "available"}: 2, {"release", "local", "pending"}: 1,
				{"release", "local", "deprecated"}: 2, {"release", "local", "blocked"}: 3,
				{"module", "upstream", "available"}: 1, {"release", "upstream", "available"}: 1,
			}
			if !reflect.DeepEqual(counts, want) {
				t.Fatalf("resource counts=%v want=%v", counts, want)
			}
			if err := st.DeleteModule(ctx, "metricteam", "blocked"); err != nil {
				t.Fatal(err)
			}
			summaries, err = backend.ListResourceMetricSummaries(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, summary := range summaries {
				if summary.Kind == "module" && summary.Source == "local" && summary.State == domain.StateBlocked {
					t.Fatal("deleted module's retained block was counted as catalog data")
				}
			}
		})
	}
}
