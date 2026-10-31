package store

import (
	"strings"
	"testing"
)

func TestReleaseVersionOrderMatchesSemanticComparison(t *testing.T) {
	versions := []string{"bad", "", "v1", "1.0.0", "1.0.0+build.2", "1.2.3", "2.0.0", "10.0.0", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha-1", "1.0.0-1", "1.0.0-2", "1.0.0-10", "1.0.0-rc.2", "1.0.0-rc.10", "999999999999999999999.0.0", " 1.0.0 "}
	for _, left := range versions {
		for _, right := range versions {
			if got, want := strings.Compare(releaseVersionOrder(left), releaseVersionOrder(right)), compareVersions(left, right); got != want {
				t.Errorf("ordering %q / %q: got %d, want %d", left, right, got, want)
			}
		}
	}
}

func TestStoreParityReleasePagination(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			st := tc.open(t)
			t.Cleanup(st.Close)
			ctx := t.Context()
			module, err := st.UpsertModule(ctx, "teamname", "module")
			if err != nil {
				t.Fatal(err)
			}
			for _, version := range []string{"1.0.0", "2.0.0-rc.2", "2.0.0-rc.10", "2.0.0", "10.0.0"} {
				if _, err := st.CreateRelease(ctx, NewRelease(module.ID, module.Owner, module.Name, version, "", "", "archive.tar.gz", "application/gzip", "", "", "", 0, map[string]any{})); err != nil {
					t.Fatal(err)
				}
			}
			pageStore := st.(ReleasePageStore)
			releases, total, err := pageStore.ListReleasesPageFiltered(ctx, "teamname-module", "", 2, 1)
			if err != nil || total != 5 || len(releases) != 2 {
				t.Fatalf("release page = %#v, total=%d err=%v", releases, total, err)
			}
			if releases[0].Version != "2.0.0" || releases[1].Version != "2.0.0-rc.10" {
				t.Fatalf("semantic pagination order = %#v", releases)
			}
			for _, filter := range []struct {
				module, version string
				total           int
			}{
				{"", "1.0.0", 1}, {"missing", "", 0}, {"teamname-module", "missing", 0},
			} {
				rows, total, err := pageStore.ListReleasesPageFiltered(ctx, filter.module, filter.version, 10, 0)
				if err != nil || total != filter.total || len(rows) != filter.total {
					t.Fatalf("filtered page = %#v total=%d err=%v", rows, total, err)
				}
			}
			if _, _, err := pageStore.ListReleasesPageFiltered(ctx, "", "", 0, 0); err == nil {
				t.Fatal("invalid pagination accepted")
			}
		})
	}
}
