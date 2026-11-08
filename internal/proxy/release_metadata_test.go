package proxy

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestFetchReleasePreservesAndValidatesMetadata(t *testing.T) {
	for _, tc := range []struct {
		name          string
		metadata      map[string]any
		version, slug string
		valid         bool
	}{
		{name: "full", version: "11.4.0", valid: true, metadata: map[string]any{"name": "puppetlabs-apt", "version": "11.4.0", "dependencies": []any{map[string]any{"name": "puppetlabs/stdlib", "version_requirement": ">= 9.0.0 < 11.0.0"}}, "requirements": []any{map[string]any{"name": "puppet", "version_requirement": ">= 7.0.0 < 9.0.0"}}, "custom": map[string]any{"nested": []any{"kept", true}}}},
		{name: "missing", version: "11.4.0"},
		{name: "missing version", version: "11.4.0", metadata: map[string]any{"name": "puppetlabs-apt"}},
		{name: "wrong module", version: "11.4.0", metadata: map[string]any{"name": "puppetlabs-stdlib", "version": "11.4.0"}},
		{name: "wrong metadata version", version: "11.4.0", metadata: map[string]any{"name": "puppetlabs-apt", "version": "1.0.0"}},
		{name: "wrong top version", version: "1.0.0", metadata: map[string]any{"name": "puppetlabs-apt", "version": "11.4.0"}},
		{name: "wrong slug", version: "11.4.0", slug: "puppetlabs-stdlib-11.4.0", metadata: map[string]any{"name": "puppetlabs-apt", "version": "11.4.0"}},
		{name: "legacy name without canonical envelope", version: "11.4.0", metadata: map[string]any{"name": "puppet-apt", "version": "11.4.0"}},
		{name: "different module with canonical envelope", version: "11.4.0", slug: "puppetlabs-apt-11.4.0", metadata: map[string]any{"name": "puppet-stdlib", "version": "11.4.0"}},
		{name: "invalid legacy owner", version: "11.4.0", slug: "puppetlabs-apt-11.4.0", metadata: map[string]any{"name": "../puppet-apt", "version": "11.4.0"}},
		{name: "legacy name wrong version", version: "11.4.0", slug: "puppetlabs-apt-11.4.0", metadata: map[string]any{"name": "puppet-apt", "version": "0.0.1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path != "/v3/releases/puppetlabs-apt-11.4.0" {
					t.Errorf("unexpected path: %s", req.URL.Path)
				}
				if req.Header.Get("Authorization") != "" {
					t.Error("client credential forwarded upstream")
				}
				if err := json.NewEncoder(w).Encode(map[string]any{"slug": tc.slug, "version": tc.version, "metadata": tc.metadata}); err != nil {
					t.Error(err)
				}
			}))
			t.Cleanup(upstream.Close)
			forge, err := NewForgeProxy(upstream.URL, time.Minute, 1<<20, newTestArtifactStorage(), "upstream-cache", WithHTTPClient(upstream.Client()), WithPrivateNetworks())
			if err != nil {
				t.Fatal(err)
			}
			release, err := forge.FetchRelease(t.Context(), "puppetlabs-apt-11.4.0")
			if (err == nil) != tc.valid {
				t.Fatalf("validation: %#v %v", release, err)
			}
			if tc.valid && !reflect.DeepEqual(release.Metadata, tc.metadata) {
				t.Fatalf("metadata lost fields: %#v", release.Metadata)
			}
		})
	}
}

func TestFetchReleaseNormalizesLegacyNamespaceWithoutLosingMetadata(t *testing.T) {
	for _, version := range []string{"0.0.1", "0.0.2", "10.0.0"} {
		t.Run(version, func(t *testing.T) {
			slug := "puppetlabs-apt-" + version
			metadataName := "puppet-apt"
			if version == "10.0.0" {
				metadataName = "puppetlabs-apt"
			}
			metadata := map[string]any{
				"name": metadataName, "version": version, "description": "APT Module for Puppet",
				"checksums":    map[string]any{"Modulefile": "legacy-checksum"},
				"dependencies": []any{map[string]any{"name": "puppetlabs/stdlib", "version_requirement": ">= 2.2.1"}},
				"custom":       map[string]any{"preserved": true},
			}
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls.Add(1)
				if req.URL.Path != "/v3/releases/"+slug {
					t.Errorf("unexpected archive or metadata request: %s", req.URL.Path)
				}
				if err := json.NewEncoder(w).Encode(map[string]any{"slug": slug, "version": version, "metadata": metadata, "file_uri": "/v3/files/" + slug + ".tar.gz", "file_md5": "original-md5", "file_sha256": "original-sha256"}); err != nil {
					t.Error(err)
				}
			}))
			t.Cleanup(upstream.Close)
			forge, err := NewForgeProxy(upstream.URL, time.Minute, 1<<20, newTestArtifactStorage(), "upstream-cache", WithHTTPClient(upstream.Client()), WithPrivateNetworks())
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				release, err := forge.FetchRelease(t.Context(), slug)
				if err != nil {
					t.Fatal(err)
				}
				expected := maps.Clone(metadata)
				expected["name"] = "puppetlabs-apt"
				if !reflect.DeepEqual(release.Metadata, expected) || release.FileMD5 != "original-md5" || release.FileSHA256 != "original-sha256" {
					t.Fatalf("legacy release normalization changed unrelated data: %#v", release)
				}
			}
			if calls.Load() != 2 {
				t.Fatalf("two direct metadata fetches made %d upstream requests", calls.Load())
			}
		})
	}
}
