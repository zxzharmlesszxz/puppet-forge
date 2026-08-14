package domain

import (
	"strings"

	"golang.org/x/mod/semver"
)

func CompareVersions(left, right string) int {
	leftNormalized, leftValid := normalizedSemanticVersion(left)
	rightNormalized, rightValid := normalizedSemanticVersion(right)
	switch {
	case leftValid && rightValid:
		return semver.Compare(leftNormalized, rightNormalized)
	case leftValid:
		return 1
	case rightValid:
		return -1
	default:
		return strings.Compare(normalizeVersionText(left), normalizeVersionText(right))
	}
}

func ValidModuleVersion(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || strings.HasPrefix(value, "v") {
		return false
	}
	core := value
	if before, _, found := strings.Cut(core, "+"); found {
		core = before
	}
	if before, _, found := strings.Cut(core, "-"); found {
		core = before
	}
	return strings.Count(core, ".") == 2 && semver.IsValid("v"+value)
}

func normalizedSemanticVersion(value string) (string, bool) {
	normalized := normalizeVersionText(value)
	if normalized == "" {
		return "", false
	}
	if !strings.HasPrefix(normalized, "v") {
		normalized = "v" + normalized
	}
	return normalized, semver.IsValid(normalized)
}

func normalizeVersionText(value string) string {
	return strings.TrimSpace(value)
}
