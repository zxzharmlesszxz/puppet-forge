package app

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/config"
	"github.com/zxzharmlesszxz/puppet-forge/internal/service"
	artifactstorage "github.com/zxzharmlesszxz/puppet-forge/internal/storage"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

type artifactDeletionTestStorage struct {
	deleted  chan string
	deadline chan time.Time
}

type blockingLeaseStore struct {
	store.Store
	deadline chan time.Time
}

func (s *blockingLeaseStore) ReleaseLease(ctx context.Context, _, _ string) error {
	deadline, _ := ctx.Deadline()
	s.deadline <- deadline
	<-ctx.Done()
	return ctx.Err()
}

func (s *blockingLeaseStore) Close() {}

func (s *artifactDeletionTestStorage) Upload(context.Context, string, string, []byte) error {
	return nil
}

func (s *artifactDeletionTestStorage) UploadIfAbsent(context.Context, string, string, []byte) (bool, error) {
	return true, nil
}

func (s *artifactDeletionTestStorage) UploadReaderIfAbsent(context.Context, string, string, io.Reader) (bool, error) {
	return true, nil
}

func (s *artifactDeletionTestStorage) Delete(ctx context.Context, objectPath string) error {
	if s.deadline != nil {
		deadline, _ := ctx.Deadline()
		s.deadline <- deadline
	}
	s.deleted <- objectPath
	return nil
}

func (s *artifactDeletionTestStorage) Exists(context.Context, string) (bool, error) {
	return false, nil
}

func (s *artifactDeletionTestStorage) Download(context.Context, string) (artifactstorage.Object, error) {
	return artifactstorage.Object{}, nil
}

func (s *artifactDeletionTestStorage) Open(context.Context, string) (artifactstorage.ObjectReader, error) {
	return artifactstorage.ObjectReader{}, nil
}

func (s *artifactDeletionTestStorage) PublicURL(string) string {
	return ""
}

func (s *artifactDeletionTestStorage) Stat(context.Context, string) (artifactstorage.ObjectAttrs, error) {
	return artifactstorage.ObjectAttrs{}, nil
}

func TestRetentionCleanupRunsAsynchronouslyAfterStartup(t *testing.T) {
	t.Parallel()

	st, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	t.Cleanup(st.Close)

	ctx, cancel := context.WithCancel(context.Background())
	app := &App{store: st}
	completed := make(chan struct{})
	app.startRetentionCleanup(ctx, "test-retention-cleanup", time.Hour, "test retention", func(context.Context) error {
		close(completed)
		return nil
	})

	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("initial retention cleanup did not run asynchronously")
	}
	cancel()
	app.wg.Wait()
}

func TestPurgeReleaseUsageRemovesExpiredRows(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	t.Cleanup(st.Close)

	module, err := st.UpsertModule(ctx, "teamname", "archive")
	if err != nil {
		t.Fatalf("UpsertModule() error = %v", err)
	}
	if _, err := st.CreateRelease(ctx, store.NewRelease(
		module.ID, "teamname", "archive", "1.0.0", "", "", "archive.tar.gz",
		"application/gzip", "md5", "sha256", "modules/archive.tar.gz", 7, nil,
	)); err != nil {
		t.Fatalf("CreateRelease() error = %v", err)
	}
	if err := st.MarkReleaseUsed(ctx, "teamname", "archive", "1.0.0"); err != nil {
		t.Fatalf("MarkReleaseUsed() error = %v", err)
	}

	moduleService := service.NewModuleService(st, &artifactDeletionTestStorage{}, "modules", nil)
	time.Sleep(time.Millisecond)
	if err := purgeReleaseUsage(ctx, moduleService, time.Nanosecond); err != nil {
		t.Fatalf("purgeReleaseUsage() error = %v", err)
	}
	active, err := st.IsReleaseActive(ctx, "teamname", "archive", "1.0.0", time.Time{})
	if err != nil {
		t.Fatalf("IsReleaseActive() error = %v", err)
	}
	if active {
		t.Fatal("expired release usage row remains active after purge")
	}
}

func TestLeaseIdentityIsStableAndUniquePerApp(t *testing.T) {
	t.Parallel()

	first := &App{}
	second := &App{}
	firstIdentity := first.leaseIdentity()
	if firstIdentity == "" {
		t.Fatal("first lease identity is empty")
	}
	if got := first.leaseIdentity(); got != firstIdentity {
		t.Fatalf("lease identity changed from %q to %q", firstIdentity, got)
	}
	if secondIdentity := second.leaseIdentity(); secondIdentity == firstIdentity {
		t.Fatalf("distinct apps share lease identity %q", firstIdentity)
	}
}

func TestReleaseLeaseUsesBoundedContext(t *testing.T) {
	t.Parallel()

	st := &blockingLeaseStore{deadline: make(chan time.Time, 1)}
	application := &App{store: st, leaseTimeout: 20 * time.Millisecond}
	started := time.Now()
	application.releaseLease("test-lease", "test-holder")
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("releaseLease() took %s, want a bounded wait", elapsed)
	}
	deadline := <-st.deadline
	if budget := deadline.Sub(started); deadline.IsZero() || budget <= 0 || budget > 100*time.Millisecond {
		t.Fatalf("lease release deadline budget = %s, want (0, 100ms]", budget)
	}
}

func TestCloseStopsWaitingAtShutdownDeadline(t *testing.T) {
	t.Parallel()

	unblock := make(chan struct{})
	application := &App{closeTimeout: 20 * time.Millisecond}
	application.wg.Go(func() { <-unblock })
	t.Cleanup(func() { close(unblock) })

	started := time.Now()
	err := application.Close()
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("Close() took %s, want a bounded wait", elapsed)
	}
	if err == nil {
		t.Fatal("Close() error = nil, want shutdown deadline error")
	}
}

type closeTrackingStore struct {
	store.Store
	closed chan struct{}
}

func (s *closeTrackingStore) Close() { close(s.closed) }

func TestCloseEventuallyClosesDependenciesAfterWorkerUnblocks(t *testing.T) {
	t.Parallel()

	unblock := make(chan struct{})
	closed := make(chan struct{})
	application := &App{store: &closeTrackingStore{closed: closed}, closeTimeout: 10 * time.Millisecond}
	application.wg.Go(func() { <-unblock })
	if err := application.Close(); err == nil {
		t.Fatal("Close() error = nil, want shutdown deadline error")
	}
	select {
	case <-closed:
		t.Fatal("store closed while worker was still running")
	default:
	}
	close(unblock)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("store was not closed after worker stopped")
	}
	if err := application.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

func TestNewContextRejectsCanceledStartup(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewContext(ctx, config.Config{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("NewContext() error = %v, want context.Canceled", err)
	}
}

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
		AccessTokenPepper:       "test-access-token-pepper-32-bytes",
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
			_ = app.Close()
		}
		t.Fatal("expected New() to return proxy URL error")
	}
}

func TestNewRequiresAccessCredentialWhenAccessConfigIsEmpty(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		AppEnv:                  "test",
		HTTPAddr:                ":0",
		ReadTimeout:             time.Second,
		WriteTimeout:            time.Second,
		ShutdownTimeout:         time.Second,
		DatabaseDSN:             "sqlite://:memory:",
		AccessTokenPepper:       "test-access-token-pepper-32-bytes",
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
			_ = app.Close()
		}
		t.Fatal("expected access credential requirement error")
	}
}

func TestCloseGCSClientAcceptsNil(t *testing.T) {
	t.Parallel()

	closeGCSClient(nil)
}

func TestNewGCSClientDoesNotMutateEmulatorEnvironment(t *testing.T) {
	t.Setenv("STORAGE_EMULATOR_HOST", "preserve.example:4443")

	client, err := newGCSClient(context.Background(), "http://gcs.example:4443", true)
	if err != nil {
		t.Fatalf("newGCSClient() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if got := os.Getenv("STORAGE_EMULATOR_HOST"); got != "preserve.example:4443" {
		t.Fatalf("STORAGE_EMULATOR_HOST = %q, want preserved value", got)
	}
}

func TestBuildArtifactStorageRejectsUnsupportedBackend(t *testing.T) {
	t.Parallel()

	_, _, err := buildArtifactStorage(context.Background(), config.Config{ArtifactBackend: "unknown"})
	if err == nil {
		t.Fatal("expected unsupported backend error")
	}
}

func TestBuildGCSArtifactStorageDoesNotContactBucketByDefault(t *testing.T) {
	t.Parallel()

	artifacts, client, err := buildArtifactStorage(context.Background(), config.Config{
		ArtifactBackend:  "gcs",
		ArtifactEndpoint: "http://127.0.0.1:1",
		ArtifactBucket:   "existing-bucket",
	})
	if err != nil {
		t.Fatalf("buildArtifactStorage() error = %v", err)
	}
	if artifacts == nil || client == nil {
		t.Fatalf("buildArtifactStorage() = %#v, %#v", artifacts, client)
	}
	t.Cleanup(func() { _ = client.Close() })
}

func TestArtifactDeletionCleanupProcessesPendingObjects(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	defer st.Close()

	module, err := st.UpsertModule(ctx, "teamname", "archive")
	if err != nil {
		t.Fatalf("UpsertModule() error = %v", err)
	}
	const storagePath = "modules/teamname/archive/1.0.0/digest.tar.gz"
	if _, err := st.CreateRelease(ctx, store.NewRelease(
		module.ID, "teamname", "archive", "1.0.0", "", "", "archive.tar.gz",
		"application/gzip", "md5", "sha256", storagePath, 7, nil,
	)); err != nil {
		t.Fatalf("CreateRelease() error = %v", err)
	}
	if err := st.DeleteRelease(ctx, "teamname", "archive", "1.0.0"); err != nil {
		t.Fatalf("DeleteRelease() error = %v", err)
	}

	artifacts := &artifactDeletionTestStorage{
		deleted:  make(chan string, 1),
		deadline: make(chan time.Time, 1),
	}
	moduleService := service.NewModuleService(st, artifacts, "modules", nil)
	application := &App{store: st}
	application.startArtifactDeletionCleanup(ctx, moduleService)

	select {
	case deletedPath := <-artifacts.deleted:
		if deletedPath != storagePath {
			t.Fatalf("deleted path = %q, want %q", deletedPath, storagePath)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("artifact deletion worker did not process the pending object")
	}
	select {
	case cleanupDeadline := <-artifacts.deadline:
		remaining := time.Until(cleanupDeadline)
		if cleanupDeadline.IsZero() || remaining <= 0 || remaining > artifactDeletionCleanupTimeout {
			t.Fatalf("artifact deletion deadline remaining = %s, want (0, %s]", remaining, artifactDeletionCleanupTimeout)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("artifact deletion worker did not pass a bounded context to storage")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		pending, err := moduleService.CountArtifactDeletions(ctx)
		if err != nil {
			t.Fatalf("CountArtifactDeletions() error = %v", err)
		}
		if pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("artifact deletion queue still has %d item(s)", pending)
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	application.wg.Wait()
}
