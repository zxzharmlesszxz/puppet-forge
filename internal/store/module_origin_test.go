package store

import (
	"testing"

	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
)

func TestStoreParityUpstreamOriginIncludesHiddenReleases(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			st := tc.open(t)
			t.Cleanup(st.Close)
			origins, ok := st.(ModuleOriginStore)
			if !ok {
				t.Fatal("missing module origin store")
			}
			module, err := st.UpsertModule(t.Context(), "teamname", "demo")
			if err != nil {
				t.Fatal(err)
			}
			for _, source := range []string{"local", "upstream"} {
				version := "1.0.0"
				if source == "upstream" {
					version = "2.0.0"
				}
				release := NewRelease(module.ID, "teamname", "demo", version, "", "", "demo.tar.gz", "application/gzip", "md5", "sha", "modules/"+version, 1, nil)
				release.Source = source
				release.InitialState = domain.StateBlocked
				if _, err := st.CreateRelease(t.Context(), release); err != nil {
					t.Fatal(err)
				}
				upstream, err := origins.HasUpstreamReleases(ReadableCatalogContext(t.Context()), "teamname", "demo")
				if err != nil || upstream != (source == "upstream") {
					t.Fatalf("origin after %s = %v, %v", source, upstream, err)
				}
			}
			if upstream, err := origins.HasUpstreamReleases(t.Context(), "other", "demo"); err != nil || upstream {
				t.Fatalf("origin leaked across namespaces: %v, %v", upstream, err)
			}
		})
	}
}
