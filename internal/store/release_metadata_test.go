package store

import (
	"reflect"
	"testing"
)

func TestStoreParityRepairsUpstreamMetadataWithoutChangingArtifact(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			st := tc.open(t)
			t.Cleanup(st.Close)
			module, err := st.UpsertModule(t.Context(), "puppetlabs", "apt")
			if err != nil {
				t.Fatal(err)
			}
			initial := NewRelease(module.ID, module.Owner, module.Name, "11.4.0", "", "", "apt.tar.gz", "application/gzip", "md5", "sha", "cached-object", 7, nil)
			initial.Source, initial.UpstreamFileURI = "upstream", "/v3/files/puppetlabs-apt-11.4.0.tar.gz"
			initial, err = st.CreateRelease(t.Context(), initial)
			if err != nil {
				t.Fatal(err)
			}
			metadata := map[string]any{"name": "puppetlabs-apt", "version": "11.4.0", "dependencies": []any{map[string]any{"name": "puppetlabs/stdlib", "version_requirement": ">= 10.1.0"}}, "requirements": []any{map[string]any{"name": "puppet", "version_requirement": ">= 8.0.0"}}, "custom": map[string]any{"kept": true}}
			repaired := initial
			repaired.Metadata = metadata
			if _, err := st.CreateRelease(t.Context(), repaired); err != nil {
				t.Fatal(err)
			}
			loaded, err := st.GetRelease(t.Context(), module.Owner, module.Name, initial.Version)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(loaded.Metadata, metadata) || loaded.ID != initial.ID || loaded.StoragePath != initial.StoragePath || loaded.MD5 != initial.MD5 || loaded.SHA256 != initial.SHA256 || loaded.SizeBytes != initial.SizeBytes || loaded.UpstreamFileURI != initial.UpstreamFileURI {
				t.Fatalf("repair changed metadata or artifact: %#v", loaded)
			}
		})
	}
}
