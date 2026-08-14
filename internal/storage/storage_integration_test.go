package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"testing"
)

type integrationArtifactStorage interface {
	ArtifactStorage
	RangeArtifactStorage
	ArtifactLister
	ArtifactMetadataLister
}

func verifyArtifactStorageLifecycle(t *testing.T, ctx context.Context, artifactStorage integrationArtifactStorage, key string, original []byte) {
	t.Helper()

	attrs, err := artifactStorage.Stat(ctx, key)
	if err != nil || attrs.Size != int64(len(original)) || attrs.ContentType != "application/gzip" {
		t.Fatalf("Stat() = %#v, %v", attrs, err)
	}
	object, err := artifactStorage.Open(ctx, key)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	body, readErr := io.ReadAll(object.Body)
	closeErr := object.Body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		t.Fatalf("read object error = %v", err)
	}
	if !bytes.Equal(body, original) {
		t.Fatalf("stored body = %q, want %q", body, original)
	}

	ranged, err := artifactStorage.OpenRange(ctx, key, 10, 8)
	if err != nil {
		t.Fatalf("OpenRange() error = %v", err)
	}
	rangeBody, readErr := io.ReadAll(ranged.Body)
	closeErr = ranged.Body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		t.Fatalf("read range error = %v", err)
	}
	if string(rangeBody) != "artifact" {
		t.Fatalf("range body = %q, want artifact", rangeBody)
	}

	paths, err := artifactStorage.ListObjects(ctx, "modules/teamname")
	if err != nil || !slices.Contains(paths, key) {
		t.Fatalf("ListObjects() = %#v, %v", paths, err)
	}
	listed, err := artifactStorage.ListObjectMetadata(ctx, "modules/teamname")
	if err != nil || len(listed) != 1 || listed[0].Path != key || listed[0].Size != int64(len(original)) || listed[0].UpdatedAt.IsZero() {
		t.Fatalf("ListObjectMetadata() = %#v, %v", listed, err)
	}

	if err := artifactStorage.Delete(ctx, key); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := artifactStorage.Delete(ctx, key); err != nil {
		t.Fatalf("Delete(idempotent) error = %v", err)
	}
	if _, err := artifactStorage.Stat(ctx, key); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("Stat(deleted) error = %v, want ErrObjectNotFound", err)
	}
}
