package domain

import "regexp"

var (
	moduleOwnerPattern = regexp.MustCompile(`^[A-Za-z0-9]+$`)
	moduleNamePattern  = regexp.MustCompile(`^[a-z0-9_]+$`)
)

func ValidModuleOwner(value string) bool {
	return moduleOwnerPattern.MatchString(value)
}

func ValidModuleName(value string) bool {
	return moduleNamePattern.MatchString(value)
}
