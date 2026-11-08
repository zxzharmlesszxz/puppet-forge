package service

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
	"github.com/zxzharmlesszxz/puppet-forge/internal/testutil"
)

func TestScanReviewArchiveRejectsAmbiguityAndEntryFloods(t *testing.T) {
	t.Parallel()
	root := "teamname-demo-1.0.0/"
	for _, entries := range [][]testTarEntry{
		{{header: tar.Header{Name: root + "file"}, body: "one"}, {header: tar.Header{Name: root + "file"}, body: "two"}},
		{{header: tar.Header{Name: root + "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}}},
		{{header: tar.Header{Name: root + "file"}, body: "one"}, {header: tar.Header{Name: root + "file/child"}, body: "two"}},
		{{header: tar.Header{Name: root + strings.Repeat("part/", 33), Typeflag: tar.TypeDir}}},
	} {
		archive := buildTarGzEntries(t, entries)
		if _, _, err := scanReviewArchive(t.Context(), bytes.NewReader(archive), strings.TrimSuffix(root, "/"), "file", ReviewMaxExpanded); err == nil {
			t.Fatal("ambiguous or unsafe archive accepted")
		}
	}
	entries := make([]testTarEntry, ReviewMaxEntries+1)
	for index := range entries {
		entries[index].header.Name = fmt.Sprintf("%sfile%d", root, index)
	}
	archive := buildTarGzEntries(t, entries)
	if _, _, err := scanReviewArchive(t.Context(), bytes.NewReader(archive), strings.TrimSuffix(root, "/"), "file0", ReviewMaxExpanded); !errors.Is(err, ErrReviewLimit) {
		t.Fatalf("entry limit = %v", err)
	}
}

func TestScanReviewArchiveBoundsAndUnsafeEntries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		files   map[string]string
		limit   int64
		wantErr bool
	}{
		{"valid", map[string]string{"teamname-demo-1.0.0/metadata.json": "{}", "teamname-demo-1.0.0/manifests/init.pp": "class demo {}"}, 1 << 20, false},
		{"traversal", map[string]string{"teamname-demo-1.0.0/../secret": "hidden"}, 1 << 20, true},
		{"wrong root", map[string]string{"another/file": "bad"}, 1 << 20, true},
		{"expanded limit", map[string]string{"teamname-demo-1.0.0/file": strings.Repeat("x", 4096)}, 1024, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			archive, err := testutil.BuildTarGz(tc.files)
			if err != nil {
				t.Fatal(err)
			}
			result, _, err := scanReviewArchive(t.Context(), bytes.NewReader(archive), "teamname-demo-1.0.0", "metadata.json", tc.limit)
			if (err != nil) != tc.wantErr {
				t.Fatalf("scan = %#v, %v", result, err)
			}
		})
	}
}

func TestScanReviewArchiveEnforcesCombinedExpandedLimit(t *testing.T) {
	t.Parallel()
	archive, err := testutil.BuildTarGz(map[string]string{
		"teamname-demo-1.0.0/first":  strings.Repeat("x", 2048),
		"teamname-demo-1.0.0/second": strings.Repeat("x", 2048),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = scanReviewArchive(t.Context(), bytes.NewReader(archive), "teamname-demo-1.0.0", "first", 4096)
	if !errors.Is(err, ErrReviewLimit) {
		t.Fatalf("combined expanded limit error = %v, want ErrReviewLimit", err)
	}
}

func TestReviewBinaryAndLargeFilesRemainInManifest(t *testing.T) {
	t.Parallel()
	for _, content := range []string{"a\x00b", strings.Repeat("x", ReviewMaxText+1), strings.Repeat("x\n", ReviewMaxLines+1)} {
		archive, err := testutil.BuildTarGz(map[string]string{"teamname-demo-1.0.0/file": content})
		if err != nil {
			t.Fatal(err)
		}
		result, _, err := scanReviewArchive(t.Context(), bytes.NewReader(archive), "teamname-demo-1.0.0", "file", ReviewMaxExpanded)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Files) != 1 || result.Notice == "" || !result.FilePresent || result.Text != "" {
			t.Fatalf("missing explicit preview exclusion: %#v", result)
		}
	}
}

func newReviewService(t *testing.T) (*ModuleService, *store.SQLiteStore, domain.Release) {
	t.Helper()
	st, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	svc := NewModuleService(st, &testArtifactStorage{}, "modules", nil)
	release, err := svc.Publish(t.Context(), domain.PublishModuleInput{FileBytes: buildPublishArchive(t, "teamname-demo", "1.0.0", "review"), FileName: "module.tar.gz", AwaitApproval: true})
	if err != nil {
		t.Fatal(err)
	}
	return svc, st, release
}

func reviewAdmin() auth.Principal {
	return auth.Principal{CanRead: true, CanManageTeam: true, Team: "teamname", ManagedTeams: map[string]struct{}{"teamname": {}}}
}

func TestReviewReleasesAuthorizationStatesAndSHA(t *testing.T) {
	t.Parallel()
	svc, st, release := newReviewService(t)
	for _, principal := range []auth.Principal{{CanRead: true}, {CanPublish: true, Team: "teamname"}, {CanManageTeam: true, Team: "other"}} {
		if _, _, err := svc.ReviewReleases(t.Context(), principal, "teamname", "demo", "1.0.0", "", "metadata.json", "", ""); !errors.Is(err, ErrReviewForbidden) {
			t.Fatalf("unauthorized review = %v", err)
		}
	}
	result, done, err := svc.ReviewReleases(t.Context(), reviewAdmin(), "teamname", "demo", "1.0.0", "", "metadata.json", release.SHA256, "")
	if err != nil {
		t.Fatal(err)
	}
	done()
	if !result.Current.FilePresent || result.Current.Release.Status.State != domain.StatePending {
		t.Fatalf("pending review = %#v", result)
	}
	if active, err := st.IsReleaseActive(t.Context(), "teamname", "demo", "1.0.0", time.Now().Add(-time.Hour)); err != nil || active {
		t.Fatalf("review recorded installation usage: %v %v", active, err)
	}
	if _, _, err := svc.ReviewReleases(t.Context(), reviewAdmin(), "teamname", "demo", "1.0.0", "", "metadata.json", "stale", ""); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale review = %v", err)
	}
	if err := svc.Moderate(t.Context(), reviewAdmin(), "teamname", "demo", "1.0.0", "approve", "reviewed", "", "stale"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale approval = %v", err)
	}
	if err := svc.Moderate(t.Context(), reviewAdmin(), "teamname", "demo", "1.0.0", "approve", "reviewed", "", release.SHA256); err != nil {
		t.Fatal(err)
	}
	if module, err := svc.GetModule(t.Context(), "teamname", "demo"); err != nil || module.LatestVersion != "1.0.0" || module.Status.State != domain.StateAvailable {
		t.Fatalf("approved module = %#v %v", module, err)
	}
	if err := svc.Moderate(t.Context(), reviewAdmin(), "teamname", "demo", "1.0.0", "block", "broken", "", release.SHA256); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ReviewReleases(t.Context(), reviewAdmin(), "teamname", "demo", "1.0.0", "", "metadata.json", "", ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("blocked review = %v", err)
	}
}

func TestReviewCapacityAndCanceledContext(t *testing.T) {
	t.Parallel()
	svc, _, _ := newReviewService(t)
	svc.reviewSlots <- struct{}{}
	svc.reviewSlots <- struct{}{}
	if _, _, err := svc.ReviewReleases(t.Context(), reviewAdmin(), "teamname", "demo", "1.0.0", "", "metadata.json", "", ""); !errors.Is(err, ErrReviewBusy) {
		t.Fatalf("capacity = %v", err)
	}
	<-svc.reviewSlots
	<-svc.reviewSlots
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := svc.ReviewReleases(ctx, reviewAdmin(), "teamname", "demo", "1.0.0", "", "metadata.json", "", ""); err == nil {
		t.Fatal("canceled review succeeded")
	}
	if len(svc.reviewSlots) != 0 {
		t.Fatal("review capacity leaked")
	}
}

func TestReviewReleasesReadsBothVersionedArchives(t *testing.T) {
	t.Parallel()
	svc, _, first := newReviewService(t)
	second, err := svc.Publish(t.Context(), domain.PublishModuleInput{FileBytes: buildPublishArchive(t, "teamname-demo", "2.0.0", "second payload"), FileName: "module.tar.gz", AwaitApproval: true})
	if err != nil {
		t.Fatal(err)
	}
	result, done, err := svc.ReviewReleases(t.Context(), reviewAdmin(), "teamname", "demo", "2.0.0", "1.0.0", "payload.txt", second.SHA256, first.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	if result.Base == nil || result.Base.Text != "review" || result.Current.Text != "second payload" {
		t.Fatalf("wrong comparison snapshots: %#v", result)
	}
}
