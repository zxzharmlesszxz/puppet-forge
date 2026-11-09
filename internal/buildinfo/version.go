package buildinfo

import (
	"strings"
	"sync/atomic"
)

var currentVersion atomic.Pointer[string]

// SetVersion records the version supplied by the executable at startup.
func SetVersion(version string) {
	version = strings.TrimSpace(version)
	if version == "" {
		version = "dev"
	}
	currentVersion.Store(&version)
}

// Version returns the executable version, or dev for an unversioned process.
func Version() string {
	if version := currentVersion.Load(); version != nil {
		return *version
	}
	return "dev"
}
