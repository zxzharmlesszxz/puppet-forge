package storage

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
)

func TestS3StorageIntegrationLifecycle(t *testing.T) {
	endpoint := os.Getenv("PUPPET_FORGE_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("PUPPET_FORGE_TEST_S3_ENDPOINT is not set")
	}
	ctx := context.Background()
	bucket := "puppet-forge-test-" + uuid.NewString()
	storage, err := NewS3Storage(
		ctx,
		endpoint,
		"us-east-1",
		bucket,
		os.Getenv("PUPPET_FORGE_TEST_S3_ACCESS_KEY_ID"),
		os.Getenv("PUPPET_FORGE_TEST_S3_SECRET_ACCESS_KEY"),
		true,
	)
	if err != nil {
		t.Fatalf("NewS3Storage() error = %v", err)
	}
	if _, err := storage.client.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("CreateBucket() error = %v", err)
	}
	t.Cleanup(func() {
		objects, listErr := storage.ListObjects(context.Background(), "")
		if listErr == nil {
			for _, objectPath := range objects {
				_ = storage.Delete(context.Background(), objectPath)
			}
		}
		_, _ = storage.client.DeleteBucket(context.Background(), &awss3.DeleteBucketInput{Bucket: aws.String(bucket)})
	})

	key := "modules/teamname/module/1.0.0/sha256.tar.gz"
	original := []byte("immutable artifact body")
	created, err := storage.UploadReaderIfAbsent(ctx, key, "application/gzip", bytes.NewReader(original))
	if err != nil || !created {
		t.Fatalf("UploadReaderIfAbsent(first) = %v, %v", created, err)
	}
	created, err = storage.UploadReaderIfAbsent(ctx, key, "application/gzip", bytes.NewReader([]byte("replacement")))
	if err != nil || created {
		t.Fatalf("UploadReaderIfAbsent(existing) = %v, %v", created, err)
	}
	verifyArtifactStorageLifecycle(t, ctx, storage, key, original)

	concurrentKey := "modules/teamname/module/2.0.0/sha256.tar.gz"
	payloads := [][]byte{[]byte("first concurrent body"), []byte("second concurrent body")}
	createdResults := make([]bool, len(payloads))
	errorsByUpload := make([]error, len(payloads))
	var wg sync.WaitGroup
	for i, payload := range payloads {
		wg.Go(func() {
			createdResults[i], errorsByUpload[i] = storage.UploadReaderIfAbsent(ctx, concurrentKey, "application/gzip", bytes.NewReader(payload))
		})
	}
	wg.Wait()
	if errors.Join(errorsByUpload...) != nil {
		t.Fatalf("concurrent upload errors = %v", errorsByUpload)
	}
	if createdResults[0] == createdResults[1] {
		t.Fatalf("concurrent create results = %#v, want exactly one winner", createdResults)
	}
	concurrentObject, err := storage.Download(ctx, concurrentKey)
	if err != nil {
		t.Fatalf("Download(concurrent) error = %v", err)
	}
	if !bytes.Equal(concurrentObject.Body, payloads[0]) && !bytes.Equal(concurrentObject.Body, payloads[1]) {
		t.Fatalf("concurrent object contains unexpected bytes: %q", concurrentObject.Body)
	}

	cancelledKey := "modules/teamname/module/3.0.0/sha256.tar.gz"
	cancelledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := storage.UploadReaderIfAbsent(cancelledCtx, cancelledKey, "application/gzip", bytes.NewReader(original)); err == nil {
		t.Fatal("cancelled upload succeeded")
	}
	exists, err := storage.Exists(ctx, cancelledKey)
	if err != nil || exists {
		t.Fatalf("Exists(cancelled object) = %v, %v", exists, err)
	}
}
