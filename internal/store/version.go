package store

import (
	"sort"
	"strings"

	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
)

func latestVersion(versions []string) string {
	if len(versions) == 0 {
		return ""
	}
	latest := versions[0]
	for _, version := range versions[1:] {
		if compareVersions(version, latest) > 0 {
			latest = version
		}
	}
	return latest
}

func latestVersionWithCandidate(current, candidate string) string {
	if strings.TrimSpace(current) == "" {
		return candidate
	}
	if compareVersions(candidate, current) > 0 {
		return candidate
	}
	return current
}

func sortReleaseSummaries(releases []ReleaseSummary) {
	sort.SliceStable(releases, func(i, j int) bool {
		cmp := compareVersions(releases[i].Version, releases[j].Version)
		if cmp != 0 {
			return cmp > 0
		}
		return releases[i].CreatedAt.After(releases[j].CreatedAt)
	})
}

func sortModuleVersions(versions []domain.ModuleVersion) {
	sort.SliceStable(versions, func(i, j int) bool {
		cmp := compareVersions(versions[i].Version, versions[j].Version)
		if cmp != 0 {
			return cmp > 0
		}
		return versions[i].CreatedAt.After(versions[j].CreatedAt)
	})
}

func sortModuleReleaseSummaries(releases []ModuleReleaseSummary) {
	sort.SliceStable(releases, func(i, j int) bool {
		if releases[i].Owner != releases[j].Owner {
			return releases[i].Owner < releases[j].Owner
		}
		if releases[i].Name != releases[j].Name {
			return releases[i].Name < releases[j].Name
		}
		cmp := compareVersions(releases[i].Version, releases[j].Version)
		if cmp != 0 {
			return cmp > 0
		}
		return releases[i].CreatedAt.After(releases[j].CreatedAt)
	})
}

func compareVersions(left, right string) int {
	return domain.CompareVersions(left, right)
}
