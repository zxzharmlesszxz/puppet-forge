package storage

import (
	"path"
	"strings"
)

func cleanObjectPath(objectPath string) string {
	if strings.TrimSpace(objectPath) == "" {
		return ""
	}
	return strings.TrimPrefix(path.Clean(objectPath), "/")
}
