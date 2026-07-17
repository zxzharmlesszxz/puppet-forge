package storage

import (
	"path"
	"strings"
)

func cleanObjectPath(objectPath string) string {
	return strings.TrimPrefix(path.Clean(objectPath), "/")
}
