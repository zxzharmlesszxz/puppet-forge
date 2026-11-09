package service

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
	"github.com/zxzharmlesszxz/puppet-forge/internal/testutil"
)

func TestPublishRequiresNonBlankRootReadmeBeforePersistence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, filename, body string
		valid                bool
	}{
		{name: "missing"},
		{name: "empty", filename: "README.md"},
		{name: "whitespace", filename: "README.md", body: " \t\r\n"},
		{name: "unicode whitespace", filename: "README.md", body: "\u00a0\u2003\n"},
		{name: "nested only", filename: "docs/README.md", body: "# Nested documentation"},
		{name: "too large", filename: "README.md", body: strings.Repeat("x", maxArchiveReadmeSize+1)},
		{name: "canonical", filename: "README.md", body: " \n# Demo\n ", valid: true},
		{name: "lowercase", filename: "readme.md", body: "# Demo", valid: true},
		{name: "mixed case", filename: "Readme.md", body: "# Demo", valid: true},
		{name: "uppercase extension", filename: "README.MD", body: "# Demo", valid: true},
		{name: "extensionless", filename: "README", body: "# Demo", valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := map[string]string{"teamname-demo-1.0.0/metadata.json": `{"name":"teamname-demo","version":"1.0.0"}`}
			if tc.filename != "" {
				files["teamname-demo-1.0.0/"+tc.filename] = tc.body
			}
			archive, err := testutil.BuildTarGz(files)
			if err != nil {
				t.Fatal(err)
			}
			svc, artifacts := newSQLiteModuleService(t)
			release, err := svc.Publish(t.Context(), domain.PublishModuleInput{
				FileName: "module.tar.gz", FileBytes: archive,
				Readme: "A supplied field cannot replace the README inside the archive",
			})
			if tc.valid {
				if err != nil {
					t.Fatal(err)
				}
				if release.Readme != tc.body || !bytes.Equal(artifacts.body, archive) {
					t.Fatal("publication changed README content or archive bytes")
				}
				return
			}
			if !errors.Is(err, ErrValidation) || !strings.Contains(err.Error(), "README") {
				t.Fatalf("invalid README error=%v", err)
			}
			if artifacts.uploads != 0 || artifacts.objectCount() != 0 {
				t.Fatal("invalid publication wrote an archive")
			}
			if _, err := svc.GetModule(t.Context(), "teamname", "demo"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("invalid publication created a module: %v", err)
			}
		})
	}
}

func TestInvalidReadmeCannotReplaceExistingRelease(t *testing.T) {
	t.Parallel()
	svc, artifacts := newSQLiteModuleService(t)
	original, err := svc.Publish(t.Context(), domain.PublishModuleInput{FileName: "module.tar.gz", FileBytes: buildPublishArchive(t, "teamname-demo", "1.0.0", "original")})
	if err != nil {
		t.Fatal(err)
	}
	uploads := artifacts.uploads
	archive, err := testutil.BuildTarGz(map[string]string{"teamname-demo-1.0.0/metadata.json": `{"name":"teamname-demo","version":"1.0.0"}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Publish(t.Context(), domain.PublishModuleInput{FileName: "module.tar.gz", FileBytes: archive, Replace: true}); !errors.Is(err, ErrValidation) {
		t.Fatalf("replacement without README error=%v", err)
	}
	stored, err := svc.GetRelease(t.Context(), original.Owner, original.Name, original.Version)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ID != original.ID || stored.SHA256 != original.SHA256 || stored.StoragePath != original.StoragePath || stored.Readme != original.Readme || artifacts.uploads != uploads || artifacts.objectCount() != 1 {
		t.Fatal("rejected replacement changed the existing release or storage")
	}
}

func TestReadmeRequirementDoesNotAffectLegacyOrUpstreamReads(t *testing.T) {
	t.Parallel()
	svc, artifacts := newSQLiteModuleService(t)
	st := svc.modules.(*store.SQLiteStore)
	module, err := st.UpsertModule(t.Context(), "teamname", "legacy")
	if err != nil {
		t.Fatal(err)
	}
	legacy := store.NewRelease(module.ID, module.Owner, module.Name, "1.0.0", "", "", "legacy.tar.gz", "application/gzip", "", "", "modules/legacy.tar.gz", 1, nil)
	if _, err := st.CreateRelease(t.Context(), legacy); err != nil {
		t.Fatal(err)
	}
	svc = newUpstreamModuleService(t, st, artifacts, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v3/releases/puppetlabs-demo-1.0.0" {
			http.NotFound(w, req)
			return
		}
		if _, err := io.WriteString(w, `{"slug":"puppetlabs-demo-1.0.0","version":"1.0.0","metadata":{"name":"puppetlabs-demo","version":"1.0.0"},"file_uri":"/v3/files/puppetlabs-demo-1.0.0.tar.gz"}`); err != nil {
			t.Error(err)
		}
	}))
	for _, identity := range []struct{ owner, name string }{{"teamname", "legacy"}, {"puppetlabs", "demo"}} {
		release, err := svc.GetRelease(t.Context(), identity.owner, identity.name, "1.0.0")
		if err != nil {
			t.Fatal(err)
		}
		if release.Readme != "" {
			t.Fatalf("unexpected README backfill for %s/%s", identity.owner, identity.name)
		}
	}
	stored, err := st.GetRelease(t.Context(), legacy.Owner, legacy.Name, legacy.Version)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ID != legacy.ID || stored.StoragePath != legacy.StoragePath || stored.Readme != "" {
		t.Fatalf("legacy release was modified: %#v %v", stored, err)
	}
}
