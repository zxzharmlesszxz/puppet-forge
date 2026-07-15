package store

import (
	"sort"
	"strconv"
	"strings"

	"puppet-forge/internal/domain"
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

func compareVersions(left, right string) int {
	left = strings.TrimPrefix(strings.TrimSpace(left), "v")
	right = strings.TrimPrefix(strings.TrimSpace(right), "v")
	leftCore, leftPre, _ := strings.Cut(left, "-")
	rightCore, rightPre, _ := strings.Cut(right, "-")

	leftParts := strings.Split(leftCore, ".")
	rightParts := strings.Split(rightCore, ".")
	maxParts := len(leftParts)
	if len(rightParts) > maxParts {
		maxParts = len(rightParts)
	}
	for i := 0; i < maxParts; i++ {
		leftPart := versionPart(leftParts, i)
		rightPart := versionPart(rightParts, i)
		if leftPart != rightPart {
			if leftPart > rightPart {
				return 1
			}
			return -1
		}
	}

	leftPre = strings.Split(leftPre, "+")[0]
	rightPre = strings.Split(rightPre, "+")[0]
	switch {
	case leftPre == "" && rightPre != "":
		return 1
	case leftPre != "" && rightPre == "":
		return -1
	case leftPre == rightPre:
		return 0
	}

	leftIDs := strings.Split(leftPre, ".")
	rightIDs := strings.Split(rightPre, ".")
	maxIDs := len(leftIDs)
	if len(rightIDs) > maxIDs {
		maxIDs = len(rightIDs)
	}
	for i := 0; i < maxIDs; i++ {
		var leftID, rightID string
		if i < len(leftIDs) {
			leftID = leftIDs[i]
		}
		if i < len(rightIDs) {
			rightID = rightIDs[i]
		}
		if leftID == rightID {
			continue
		}
		if leftID == "" {
			return -1
		}
		if rightID == "" {
			return 1
		}
		leftNum, leftErr := strconv.Atoi(leftID)
		rightNum, rightErr := strconv.Atoi(rightID)
		if leftErr == nil && rightErr == nil {
			if leftNum > rightNum {
				return 1
			}
			return -1
		}
		if leftErr == nil {
			return -1
		}
		if rightErr == nil {
			return 1
		}
		if leftID > rightID {
			return 1
		}
		return -1
	}
	return 0
}

func versionPart(parts []string, index int) int {
	if index >= len(parts) {
		return 0
	}
	value, err := strconv.Atoi(parts[index])
	if err != nil {
		return 0
	}
	return value
}
