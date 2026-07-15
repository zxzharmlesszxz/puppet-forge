package app

import (
	"context"
	"testing"
	"time"

	"puppet-forge/internal/config"
)

func TestNewWithS3BackendAndProxyErrorDoesNotPanicClosingNilGCSClient(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		AppEnv:                  "test",
		HTTPAddr:                ":0",
		ReadTimeout:             time.Second,
		WriteTimeout:            time.Second,
		ShutdownTimeout:         time.Second,
		DatabaseDSN:             "sqlite://:memory:",
		AdminToken:              "admin-token",
		ArtifactBackend:         "s3",
		ArtifactEndpoint:        "http://127.0.0.1:9",
		ArtifactBucket:          "test-bucket",
		ArtifactRegion:          "us-east-1",
		ArtifactAccessKeyID:     "test",
		ArtifactSecretAccessKey: "test",
		ArtifactPathStyle:       true,
		PublicBaseURL:           "http://localhost",
		UpstreamURL:             "://bad-url",
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("New() panicked: %v", recovered)
		}
	}()

	app, err := New(cfg)
	if err == nil {
		if app != nil {
			app.Close()
		}
		t.Fatal("expected New() to return proxy URL error")
	}
}

func TestNewRequiresAdminTokenWhenAccessConfigIsEmpty(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		AppEnv:                  "test",
		HTTPAddr:                ":0",
		ReadTimeout:             time.Second,
		WriteTimeout:            time.Second,
		ShutdownTimeout:         time.Second,
		DatabaseDSN:             "sqlite://:memory:",
		ArtifactBackend:         "s3",
		ArtifactEndpoint:        "http://127.0.0.1:9",
		ArtifactBucket:          "test-bucket",
		ArtifactRegion:          "us-east-1",
		ArtifactAccessKeyID:     "test",
		ArtifactSecretAccessKey: "test",
		ArtifactPathStyle:       true,
		PublicBaseURL:           "http://localhost",
		UpstreamURL:             "https://forgeapi.puppetlabs.com",
	}

	app, err := New(cfg)
	if err == nil {
		if app != nil {
			app.Close()
		}
		t.Fatal("expected ADMIN_TOKEN requirement error")
	}
}

func TestCloseGCSClientAcceptsNil(t *testing.T) {
	t.Parallel()

	closeGCSClient(nil)
}

func TestBuildArtifactStorageRejectsUnsupportedBackend(t *testing.T) {
	t.Parallel()

	_, _, err := buildArtifactStorage(context.Background(), config.Config{ArtifactBackend: "unknown"})
	if err == nil {
		t.Fatal("expected unsupported backend error")
	}
}
