package service

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zxzharmlesszxz/puppet-forge/internal/testutil"
)

func TestExtractArchiveFileBoundsSkippedContent(t *testing.T) {
	archive, err := testutil.BuildTarGz(map[string]string{
		"root/large.txt": strings.Repeat("x", 8192),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"small.txt", "missing.txt"} {
		if _, _, err := extractArchiveFileWithLimit(t.Context(), bytes.NewReader(archive), target, 4096); err == nil || !strings.Contains(err.Error(), "expanded size") {
			t.Fatalf("extract %s must reject expanded archive: %v", target, err)
		}
	}
}

func TestExtractArchiveFileHonorsCancellation(t *testing.T) {
	archive, err := testutil.BuildTarGz(map[string]string{"root/file.txt": "body"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := extractArchiveFile(ctx, bytes.NewReader(archive), "file.txt"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled extraction = %v", err)
	}
}
