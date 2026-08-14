package storage

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/google/uuid"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestGCSStorageIntegrationLifecycle(t *testing.T) {
	endpoint := os.Getenv("PUPPET_FORGE_TEST_GCS_ENDPOINT")
	if endpoint == "" {
		t.Skip("PUPPET_FORGE_TEST_GCS_ENDPOINT is not set")
	}
	ctx := context.Background()
	apiEndpoint := strings.TrimRight(endpoint, "/") + "/storage/v1/"
	client, err := storage.NewClient(ctx, option.WithEndpoint(apiEndpoint), option.WithoutAuthentication(), storage.WithJSONReads())
	if err != nil {
		t.Fatalf("storage.NewClient() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	bucket := "puppet-forge-test-" + uuid.NewString()
	artifactStorage, err := NewGCSStorage(client, bucket, "test-project", endpoint)
	if err != nil {
		t.Fatalf("NewGCSStorage() error = %v", err)
	}
	if err := artifactStorage.EnsureBucket(ctx); err != nil {
		t.Fatalf("EnsureBucket() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Bucket(bucket).Delete(context.Background()) })

	key := "modules/teamname/module/1.0.0/sha256.tar.gz"
	original := []byte("immutable artifact body")
	created, err := artifactStorage.UploadReaderIfAbsent(ctx, key, "application/gzip", bytes.NewReader(original))
	if err != nil || !created {
		t.Fatalf("UploadReaderIfAbsent(first) = %v, %v", created, err)
	}
	created, err = artifactStorage.UploadIfAbsent(ctx, key, "application/gzip", []byte("replacement"))
	if err != nil || created {
		t.Fatalf("UploadIfAbsent(existing) = %v, %v", created, err)
	}

	verifyArtifactStorageLifecycle(t, ctx, artifactStorage, key, original)
}

func TestGCSStoragePublicURL(t *testing.T) {
	t.Parallel()

	gcsStorage, err := NewGCSStorage(nil, "forge-artifacts", "project", "https://storage.googleapis.com")
	if err != nil {
		t.Fatalf("NewGCSStorage() error = %v", err)
	}
	got := gcsStorage.PublicURL("/modules/teamname/apache/1.2.3/archive.tar.gz")
	want := "https://storage.googleapis.com/forge-artifacts/modules/teamname/apache/1.2.3/archive.tar.gz"
	if got != want {
		t.Fatalf("unexpected public URL:\nwant %s\n got %s", want, got)
	}
}

func TestGCSStoragePublicURLUsesConfiguredEndpoint(t *testing.T) {
	t.Parallel()

	gcsStorage, err := NewGCSStorage(nil, "forge-artifacts", "project", "http://gcs.test:4443/base")
	if err != nil {
		t.Fatalf("NewGCSStorage() error = %v", err)
	}
	got := gcsStorage.PublicURL("modules/teamname/apache/1.2.3/archive.tar.gz")
	want := "http://gcs.test:4443/base/forge-artifacts/modules/teamname/apache/1.2.3/archive.tar.gz"
	if got != want {
		t.Fatalf("unexpected custom public URL:\nwant %s\n got %s", want, got)
	}
}

func TestNewGCSStorageRejectsRelativeEndpoint(t *testing.T) {
	t.Parallel()

	if _, err := NewGCSStorage(nil, "forge-artifacts", "project", "gcs.local"); err == nil {
		t.Fatal("NewGCSStorage() accepted a relative endpoint")
	}
	if _, err := NewGCSStorage(nil, "forge-artifacts", "project", "ftp://gcs.local"); err == nil {
		t.Fatal("NewGCSStorage() accepted a non-HTTP endpoint")
	}
}

func TestIsGCSPreconditionFailed(t *testing.T) {
	t.Parallel()

	if !isGCSPreconditionFailed(&googleapi.Error{Code: http.StatusPreconditionFailed}) {
		t.Fatal("expected HTTP 412 to be recognized")
	}
	if !isGCSPreconditionFailed(status.Error(codes.FailedPrecondition, "already exists")) {
		t.Fatal("expected gRPC FailedPrecondition to be recognized")
	}
	if isGCSPreconditionFailed(errors.New("other")) {
		t.Fatal("unexpected precondition match")
	}
}

func TestObjectWriterErrorIncludesWriteAndCloseErrors(t *testing.T) {
	t.Parallel()

	writeErr := errors.New("write failed")
	closeErr := errors.New("close failed")

	err := objectWriterError(writeErr, closeErr)
	if !errors.Is(err, writeErr) {
		t.Fatalf("expected write error in chain, got %v", err)
	}
	if !errors.Is(err, closeErr) {
		t.Fatalf("expected close error in chain, got %v", err)
	}
	if !strings.Contains(err.Error(), "write object") {
		t.Fatalf("expected write context, got %v", err)
	}
	if !strings.Contains(err.Error(), "close object writer") {
		t.Fatalf("expected close context, got %v", err)
	}
}
