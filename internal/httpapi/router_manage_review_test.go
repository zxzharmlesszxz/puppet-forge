package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
	"github.com/zxzharmlesszxz/puppet-forge/internal/service"
	"github.com/zxzharmlesszxz/puppet-forge/internal/testutil"
)

func newReleaseReviewRouter(t *testing.T) (*Router, domain.Release) {
	t.Helper()
	st := newHTTPAPITestStore(t)
	archive, err := testutil.BuildTarGz(map[string]string{
		"teamname-demo-2.0.0/metadata.json":     `{"name":"teamname-demo","version":"2.0.0"}`,
		"teamname-demo-2.0.0/manifests/init.pp": "class demo { $value = '<script>alert(1)</script>' }\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := service.NewModuleService(st, fixedDownloadStorage{body: archive, contentType: "application/gzip"}, "modules", nil)
	module, err := st.UpsertModule(t.Context(), "teamname", "demo")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(archive)
	release, err := st.CreateRelease(t.Context(), domain.Release{ModuleID: module.ID, Owner: "teamname", Name: "demo", Source: "local", Version: "2.0.0", FileName: "module.tar.gz", ContentType: "application/gzip", StoragePath: "modules/review.tar.gz", SHA256: hex.EncodeToString(digest[:]), SizeBytes: int64(len(archive)), InitialState: domain.StatePending})
	if err != nil {
		t.Fatal(err)
	}
	return &Router{modules: svc}, release
}

func TestReleaseReviewRequiresAdministration(t *testing.T) {
	t.Parallel()
	r, _ := newReleaseReviewRouter(t)
	for _, principal := range []auth.Principal{{}, {CanRead: true}, {CanPublish: true, Team: "teamname"}, {CanManageTeam: true, Team: "other"}} {
		rec := httptest.NewRecorder()
		r.manageReleaseReview(rec, httptest.NewRequest(http.MethodGet, "/manage/modules/teamname/demo/review?version=2.0.0", nil), principal, "teamname", "demo")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("unauthorized status = %d", rec.Code)
		}
	}
}

func TestReleaseReviewRendersPendingContentAndBindsDecision(t *testing.T) {
	t.Parallel()
	r, release := newReleaseReviewRouter(t)
	principal := auth.Principal{CanRead: true, CanManageTeam: true, Team: "teamname"}
	rec := httptest.NewRecorder()
	r.manageReleaseReview(rec, httptest.NewRequest(http.MethodGet, "/manage/modules/teamname/demo/review?version=2.0.0&base=&file=manifests/init.pp", nil), principal, "teamname", "demo")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, expected := range []string{`id="release-review"`, `name="sha256" value="` + release.SHA256 + `"`, `Approve release`, `Download archive`, `data-review-file="manifests/init.pp"`, `syntax-`, `/assets/manage-review.js`} {
		if !strings.Contains(rec.Body.String(), expected) {
			t.Errorf("missing %q", expected)
		}
	}
	if strings.Contains(rec.Body.String(), "<script>alert(1)</script>") {
		t.Fatal("source executed as markup")
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("private review can be cached")
	}
	if module, err := r.modules.GetModule(t.Context(), "teamname", "demo"); err != nil || module.LatestVersion != "" {
		t.Fatalf("pending became latest: %#v %v", module, err)
	}
}

func TestReleaseReviewRejectsStaleIdentityAndPost(t *testing.T) {
	t.Parallel()
	r, _ := newReleaseReviewRouter(t)
	admin := auth.Principal{CanAdmin: true}
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "?version=2.0.0&sha256=old", http.StatusConflict},
		{http.MethodGet, "?version=2.0.0&file=../secret", http.StatusBadRequest},
		{http.MethodGet, "?version=2.0.0&mode=invalid", http.StatusBadRequest},
		{http.MethodPost, "?version=2.0.0", http.StatusMethodNotAllowed},
	} {
		rec := httptest.NewRecorder()
		r.manageReleaseReview(rec, httptest.NewRequest(tc.method, "/manage/modules/teamname/demo/review"+tc.path, nil), admin, "teamname", "demo")
		if rec.Code != tc.status {
			t.Errorf("%s %s: status %d, want %d", tc.method, tc.path, rec.Code, tc.status)
		}
	}
}

func TestReviewFilesIncludesRemovedAddedAndModeChanges(t *testing.T) {
	t.Parallel()
	result := service.ReleaseReview{Current: service.ArchiveReview{Files: []service.ArchiveFile{{Path: "added", SHA256: "new"}, {Path: "mode", SHA256: "same", Mode: 0o755}}}, Base: &service.ArchiveReview{Files: []service.ArchiveFile{{Path: "removed", SHA256: "old"}, {Path: "mode", SHA256: "same", Mode: 0o644}}}}
	files := reviewFiles(result)
	if len(files) != 3 || files[0].Status != "added" || files[1].Status != "modified" || files[2].Status != "removed" {
		t.Fatalf("manifest changes = %#v", files)
	}
	if tree := reviewTree(files); len(tree) != 3 {
		t.Fatalf("tree = %#v", tree)
	}
}
