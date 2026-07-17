package store

import (
	"testing"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
)

func TestLatestVersionUsesSemanticVersionOrder(t *testing.T) {
	t.Parallel()

	got := latestVersion([]string{"1.0.0", "1.10.0", "1.2.0", "2.0.0-rc1", "2.0.0"})
	if got != "2.0.0" {
		t.Fatalf("latestVersion() = %q, want 2.0.0", got)
	}
}

func TestLatestVersionReturnsEmptyForEmptySlice(t *testing.T) {
	t.Parallel()

	if got := latestVersion(nil); got != "" {
		t.Fatalf("latestVersion(nil) = %q, want empty", got)
	}
	if got := latestVersion([]string{}); got != "" {
		t.Fatalf("latestVersion([]) = %q, want empty", got)
	}
}

func TestLatestVersionWithCandidateUsesCandidateWhenCurrentIsEmpty(t *testing.T) {
	t.Parallel()

	if got := latestVersionWithCandidate("", "0.0.0"); got != "0.0.0" {
		t.Fatalf("latestVersionWithCandidate(empty, 0.0.0) = %q, want 0.0.0", got)
	}
}

func TestLatestVersionWithCandidateKeepsCurrentWhenCandidateIsOlder(t *testing.T) {
	t.Parallel()

	if got := latestVersionWithCandidate("2.0.0", "1.9.9"); got != "2.0.0" {
		t.Fatalf("latestVersionWithCandidate(2.0.0, 1.9.9) = %q, want 2.0.0", got)
	}
}

func TestLatestVersionHandlesPreRelease(t *testing.T) {
	t.Parallel()

	got := latestVersion([]string{"2.0.0-rc1", "2.0.0-rc2", "2.0.0"})
	if got != "2.0.0" {
		t.Fatalf("expected release to win over pre-release, got %q", got)
	}

	got = latestVersion([]string{"1.0.0-alpha", "1.0.0-beta", "1.0.0"})
	if got != "1.0.0" {
		t.Fatalf("expected release to win over pre-release, got %q", got)
	}

	got = latestVersion([]string{"2.0.0-10", "2.0.0-2"})
	if got != "2.0.0-10" {
		t.Fatalf("expected numeric pre-release 10 > 2, got %q", got)
	}
}

func TestLatestVersionHandlesMajorVersionBump(t *testing.T) {
	t.Parallel()

	got := latestVersion([]string{"1.9.9", "2.0.0", "1.10.0"})
	if got != "2.0.0" {
		t.Fatalf("expected 2.0.0 to win, got %q", got)
	}
}

func TestLatestVersionIgnoresBuildMetadata(t *testing.T) {
	t.Parallel()

	got := latestVersion([]string{"1.0.0+build1", "1.0.0+build2"})
	if got != "1.0.0+build1" {
		t.Fatalf("expected first build entry (build metadata ignored), got %q", got)
	}
}

func TestCompareVersions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		left  string
		right string
		want  int
	}{
		{"1.0.0", "1.0.0", 0},
		{"2.0.0", "1.0.0", 1},
		{"1.0.0", "2.0.0", -1},
		{"1.10.0", "1.2.0", 1},
		{"1.2.0", "1.10.0", -1},
		{"2.0.0", "2.0.0-rc1", 1},
		{"2.0.0-rc1", "2.0.0", -1},
		{"1.0.0-alpha", "1.0.0-beta", -1},
		{"1.0.0-rc2", "1.0.0-rc10", 1},
		{"1.0.0-2", "1.0.0-10", -1},
		{"v1.0.0", "1.0.0", 0},
		{"1.0.0+build1", "1.0.0+build2", 0},
		{"3.0.0", "2.9.9", 1},
		{"1.0.0-rc1", "1.0.0-rc1.1", -1},
		{"1.0.0-rc1.1", "1.0.0-rc1", 1},
		{"1.0.0-rc1", "1.0.0-rc1", 0},
	}

	for _, tt := range tests {
		got := compareVersions(tt.left, tt.right)
		if got != tt.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", tt.left, tt.right, got, tt.want)
		}
	}
}

func TestSortModuleVersions(t *testing.T) {
	t.Parallel()

	now := time.Now()
	versions := []domain.ModuleVersion{
		{Version: "1.0.0", CreatedAt: now},
		{Version: "2.0.0", CreatedAt: now.Add(-time.Hour)},
		{Version: "1.0.0-rc1", CreatedAt: now.Add(-time.Minute)},
	}
	sortModuleVersions(versions)

	if versions[0].Version != "2.0.0" {
		t.Fatalf("expected first to be 2.0.0, got %q", versions[0].Version)
	}
	if versions[1].Version != "1.0.0" {
		t.Fatalf("expected second to be 1.0.0, got %q", versions[1].Version)
	}
	if versions[2].Version != "1.0.0-rc1" {
		t.Fatalf("expected third to be 1.0.0-rc1, got %q", versions[2].Version)
	}
}

func TestSortModuleVersionsTiesBrokenByCreatedAt(t *testing.T) {
	t.Parallel()

	now := time.Now()
	versions := []domain.ModuleVersion{
		{Version: "1.0.0", CreatedAt: now.Add(-time.Hour)},
		{Version: "1.0.0", CreatedAt: now},
	}
	sortModuleVersions(versions)

	if versions[0].CreatedAt != now {
		t.Fatal("expected later CreatedAt first when versions are equal")
	}
}

func TestVersionPart(t *testing.T) {
	t.Parallel()

	tests := []struct {
		parts []string
		index int
		want  int
	}{
		{[]string{"1", "2", "3"}, 0, 1},
		{[]string{"1", "2", "3"}, 1, 2},
		{[]string{"1", "2", "3"}, 2, 3},
		{[]string{"1", "2"}, 2, 0},
		{[]string{"1"}, 1, 0},
		{[]string{"abc"}, 0, 0},
	}

	for _, tt := range tests {
		got := versionPart(tt.parts, tt.index)
		if got != tt.want {
			t.Errorf("versionPart(%v, %d) = %d, want %d", tt.parts, tt.index, got, tt.want)
		}
	}
}
