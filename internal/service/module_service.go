package service

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- Puppet Forge protocol compatibility checksum; SHA-256 is also computed and used for integrity.
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"compress/gzip"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
	"github.com/zxzharmlesszxz/puppet-forge/internal/httputil"
	"github.com/zxzharmlesszxz/puppet-forge/internal/metrics"
	"github.com/zxzharmlesszxz/puppet-forge/internal/proxy"
	"github.com/zxzharmlesszxz/puppet-forge/internal/storage"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
	"github.com/zxzharmlesszxz/puppet-forge/internal/throttle"
)

var (
	moduleOwnerPattern = regexp.MustCompile(`^[A-Za-z0-9]+$`)
	moduleNamePattern  = regexp.MustCompile(`^[a-z0-9_]+$`)
)

const readinessCapabilityTTL = 5 * time.Minute

const (
	maxArchiveEntries             = 10000
	maxArchiveEntrySize           = 128 << 20
	maxArchiveExpandedSize        = 512 << 20
	maxArchiveMetadataSize        = 1 << 20
	maxArchiveReadmeSize          = 2 << 20
	maxArchiveFileSize            = 16 << 20
	defaultReleaseUsageMaxEntries = 10_000
	releaseUsageRecordInterval    = time.Minute
	upstreamRefreshAttemptTimeout = 5 * time.Second
)

var ErrProtectedDelete = errors.New("protected delete")
var ErrUpstreamHydration = errors.New("upstream release hydration failed")
var ErrUpstreamRestore = errors.New("upstream release restore failed")
var ErrValidation = errors.New("validation error")

type validationError struct {
	cause error
}

func (e validationError) Error() string {
	return e.cause.Error()
}

func (e validationError) Unwrap() error {
	return ErrValidation
}

func invalidInput(message string) error {
	return validationError{cause: errors.New(message)}
}

func invalidInputError(err error) error {
	return validationError{cause: err}
}

type protectedDeleteError struct {
	message string
}

func (e protectedDeleteError) Error() string {
	return e.message
}

func (e protectedDeleteError) Unwrap() error {
	return ErrProtectedDelete
}

type ModuleService struct {
	modules              store.ModuleStore
	access               store.AccessStore
	rateLimits           store.RateLimitStore
	artifacts            storage.ArtifactStorage
	prefix               string
	upstream             *proxy.ForgeProxy
	releaseUsageThrottle *throttle.ExpirySet
	readinessMu          sync.Mutex
	readinessCheckedAt   time.Time
}

func NewModuleService(modules store.ModuleStore, artifacts storage.ArtifactStorage, prefix string, upstream *proxy.ForgeProxy) *ModuleService {
	access, _ := modules.(store.AccessStore)
	rateLimits, _ := modules.(store.RateLimitStore)
	return &ModuleService{
		modules:    modules,
		access:     access,
		rateLimits: rateLimits,
		artifacts:  artifacts,
		prefix:     prefix,
		upstream:   upstream,
		releaseUsageThrottle: throttle.NewExpirySet(
			defaultReleaseUsageMaxEntries,
			releaseUsageRecordInterval,
		),
	}
}

func (s *ModuleService) ConsumeRateLimit(ctx context.Context, key string, limit int, window time.Duration, now time.Time) (bool, error) {
	if s == nil || s.rateLimits == nil {
		return false, errors.New("shared rate limit store is not available")
	}
	return s.rateLimits.ConsumeRateLimit(ctx, key, limit, window, now)
}

func (s *ModuleService) PurgeRateLimits(ctx context.Context, before time.Time) (int64, error) {
	if s == nil || s.rateLimits == nil {
		return 0, errors.New("shared rate limit store is not available")
	}
	return s.rateLimits.PurgeRateLimits(ctx, before)
}

type archiveModuleMetadata struct {
	Name        string         `json:"name"`
	Version     string         `json:"version"`
	Summary     string         `json:"summary"`
	Description string         `json:"description"`
	Metadata    map[string]any `json:"-"`
}

func (s *ModuleService) Publish(ctx context.Context, input domain.PublishModuleInput) (domain.Release, error) {
	input, err := s.normalizePublishInput(ctx, input)
	if err != nil {
		metrics.ObservePublish(err)
		return domain.Release{}, err
	}

	if !moduleOwnerPattern.MatchString(input.Owner) {
		err := invalidInput("invalid owner")
		metrics.ObservePublish(err)
		return domain.Release{}, err
	}
	if !moduleNamePattern.MatchString(input.Name) {
		err := invalidInput("invalid name")
		metrics.ObservePublish(err)
		return domain.Release{}, err
	}
	if input.Version == "" {
		err := errors.New("version is required")
		metrics.ObservePublish(err)
		return domain.Release{}, err
	}
	if input.File == nil {
		err := errors.New("artifact file is required")
		metrics.ObservePublish(err)
		return domain.Release{}, err
	}
	if input.FileName == "" {
		err := invalidInput("file name is required")
		metrics.ObservePublish(err)
		return domain.Release{}, err
	}
	input.FileName = releaseArchiveFileName(input.Owner, input.Name, input.Version)

	md5Hash := md5.New() // #nosec G401 -- compatibility checksum only; SHA-256 is computed by the same stream.
	shaHash := sha256.New()
	sizeBytes, err := io.Copy(io.MultiWriter(md5Hash, shaHash), contextReader{ctx: ctx, reader: input.File})
	if err != nil {
		err = fmt.Errorf("calculate artifact checksums: %w", err)
		metrics.ObservePublish(err)
		return domain.Release{}, err
	}
	input.SizeBytes = sizeBytes
	md5Hex := hex.EncodeToString(md5Hash.Sum(nil))
	shaHex := hex.EncodeToString(shaHash.Sum(nil))
	objectPath := path.Join(s.prefix, input.Owner, input.Name, input.Version, shaHex+".tar.gz")

	unlock, err := s.modules.LockModule(ctx, input.Owner, input.Name)
	if err != nil {
		metrics.ObservePublish(err)
		return domain.Release{}, err
	}
	defer releaseModuleLock(unlock, input.Owner, input.Name)

	existing, err := s.modules.GetRelease(ctx, input.Owner, input.Name, input.Version)
	if err == nil {
		if existing.Source == "local" && existing.SHA256 == shaHex {
			if err := s.verifyExistingArtifact(ctx, existing.StoragePath, existing.SHA256, existing.SizeBytes); err != nil {
				err = fmt.Errorf("verify registered artifact: %w", err)
				metrics.ObservePublish(err)
				return domain.Release{}, err
			}
			existing.DownloadURL = s.artifacts.PublicURL(existing.StoragePath)
			metrics.ObservePublish(nil)
			return existing, nil
		}
		err = fmt.Errorf("%w: release %s/%s %s already exists with different content", store.ErrConflict, input.Owner, input.Name, input.Version)
		metrics.ObservePublish(err)
		return domain.Release{}, err
	}
	if !errors.Is(err, store.ErrNotFound) {
		metrics.ObservePublish(err)
		return domain.Release{}, err
	}

	if err := rewindArchive(input.File); err != nil {
		metrics.ObservePublish(err)
		return domain.Release{}, err
	}
	created, err := s.artifacts.UploadReaderIfAbsent(ctx, objectPath, input.ContentType, input.File)
	if err != nil {
		err := fmt.Errorf("upload artifact: %w", err)
		metrics.ObservePublish(err)
		return domain.Release{}, err
	}
	if !created {
		if err := s.verifyExistingArtifact(ctx, objectPath, shaHex, sizeBytes); err != nil {
			metrics.ObservePublish(err)
			return domain.Release{}, err
		}
	}

	module, err := s.modules.UpsertModule(ctx, input.Owner, input.Name)
	if err != nil {
		err = s.compensatePublishFailure(ctx, input.Owner, input.Name, objectPath, err)
		metrics.ObservePublish(err)
		return domain.Release{}, err
	}

	release := store.NewRelease(
		module.ID,
		input.Owner,
		input.Name,
		input.Version,
		input.Description,
		input.Readme,
		input.FileName,
		input.ContentType,
		md5Hex,
		shaHex,
		objectPath,
		input.SizeBytes,
		input.Metadata,
	)

	release, err = s.modules.CreateReleaseIfAbsent(ctx, release)
	if err != nil {
		committed, getErr := s.modules.GetRelease(ctx, input.Owner, input.Name, input.Version)
		if getErr == nil &&
			committed.Source == "local" && committed.SHA256 == shaHex && committed.StoragePath == objectPath {
			committed.DownloadURL = s.artifacts.PublicURL(committed.StoragePath)
			metrics.ObservePublish(nil)
			return committed, nil
		}
		if getErr != nil && !errors.Is(getErr, store.ErrNotFound) {
			err = errors.Join(err, fmt.Errorf("verify release commit: %w", getErr))
			metrics.ObservePublish(err)
			return domain.Release{}, err
		}
		err = s.compensatePublishFailure(ctx, input.Owner, input.Name, objectPath, err)
		metrics.ObservePublish(err)
		return domain.Release{}, err
	}

	metrics.ObservePublish(nil)
	release.DownloadURL = s.artifacts.PublicURL(release.StoragePath)
	return release, nil
}

func (s *ModuleService) verifyExistingArtifact(ctx context.Context, objectPath, expectedSHA256 string, expectedSize int64) error {
	object, err := s.artifacts.Open(ctx, objectPath)
	if err != nil {
		return fmt.Errorf("open existing artifact: %w", err)
	}
	hash := sha256.New()
	size, copyErr := io.Copy(hash, contextReader{ctx: ctx, reader: object.Body})
	closeErr := object.Body.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return fmt.Errorf("verify existing artifact: %w", err)
	}
	actualSHA256 := hex.EncodeToString(hash.Sum(nil))
	if size != expectedSize || actualSHA256 != expectedSHA256 {
		return fmt.Errorf("existing artifact integrity mismatch for content-addressed object")
	}
	return nil
}

func releaseModuleLock(unlock store.ModuleUnlock, owner, name string) {
	if unlock == nil {
		return
	}
	if err := unlock(); err != nil {
		slog.Error("release module lock failed", "err", err, "owner", owner, "name", name)
	}
}

func (s *ModuleService) compensatePublishFailure(ctx context.Context, owner, name, objectPath string, publishErr error) error {
	errs := []error{publishErr}
	if err := s.artifacts.Delete(ctx, objectPath); err != nil {
		errs = append(errs, fmt.Errorf("remove uncommitted artifact: %w", err))
	}
	if err := s.modules.DeleteModuleIfEmpty(ctx, owner, name); err != nil {
		errs = append(errs, fmt.Errorf("remove empty module: %w", err))
	}
	return errors.Join(errs...)
}

func (s *ModuleService) NormalizePublishInput(input domain.PublishModuleInput) (domain.PublishModuleInput, error) {
	return s.normalizePublishInput(context.Background(), input)
}

func (s *ModuleService) normalizePublishInput(ctx context.Context, input domain.PublishModuleInput) (domain.PublishModuleInput, error) {
	input.Owner = strings.TrimSpace(input.Owner)
	if input.Owner == "" {
		return input, invalidInput("space is required")
	}
	if input.File == nil && len(input.FileBytes) > 0 {
		input.File = bytes.NewReader(input.FileBytes)
		input.SizeBytes = int64(len(input.FileBytes))
	}
	if input.File == nil {
		return input, invalidInput("artifact file is required")
	}
	if input.FileName == "" {
		return input, invalidInput("file name is required")
	}

	if err := rewindArchive(input.File); err != nil {
		return input, err
	}
	archiveInfo, err := inspectModuleArchive(contextReader{ctx: ctx, reader: input.File})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return input, err
		}
		return input, invalidInputError(fmt.Errorf("inspect module archive: %w", err))
	}
	if err := rewindArchive(input.File); err != nil {
		return input, err
	}

	if archiveInfo.Owner == "" {
		return input, invalidInput("metadata.json name must include a module namespace")
	}
	if archiveInfo.Name == "" {
		return input, invalidInput("metadata.json module name is required")
	}
	if archiveInfo.Version == "" {
		return input, invalidInput("metadata.json version is required")
	}
	if !domain.ValidModuleVersion(archiveInfo.Version) {
		return input, invalidInput("metadata.json version must be a valid MAJOR.MINOR.PATCH semantic version")
	}
	if archiveInfo.Owner != input.Owner {
		return input, invalidInputError(fmt.Errorf("module namespace %q does not match selected space %q", archiveInfo.Owner, input.Owner))
	}
	input.Name = archiveInfo.Name
	input.Version = archiveInfo.Version
	input.Description = archiveInfo.Description
	input.Readme = archiveInfo.Readme
	input.Metadata = archiveInfo.Metadata

	return input, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.reader.Read(buffer)
	}
}

func rewindArchive(file io.Seeker) error {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind artifact file: %w", err)
	}
	return nil
}

func (s *ModuleService) ListModules(ctx context.Context, limit int) ([]domain.Module, error) {
	return s.modules.ListModules(ctx, limit)
}

func (s *ModuleService) ListModulesPage(ctx context.Context, limit, offset int) ([]domain.Module, int, error) {
	return s.modules.ListModulesPage(ctx, limit, offset)
}

func (s *ModuleService) ListModulesPageFiltered(ctx context.Context, owners []string, query string, limit, offset int) ([]domain.Module, int, error) {
	return s.modules.ListModulesPageFiltered(ctx, owners, query, limit, offset)
}

func (s *ModuleService) ListModulesPagePrioritized(ctx context.Context, owners, priorityOwners []string, query string, limit, offset int) ([]domain.Module, int, error) {
	return s.modules.ListModulesPagePrioritized(ctx, owners, priorityOwners, query, limit, offset)
}

func (s *ModuleService) CountModulesByOwner(ctx context.Context, owners []string) (map[string]int, error) {
	return s.modules.CountModulesByOwner(ctx, owners)
}

func (s *ModuleService) CountUpstreamModulesByOwner(ctx context.Context) (map[string]int, error) {
	return s.modules.CountUpstreamModulesByOwner(ctx)
}

func (s *ModuleService) ListUpstreamModules(ctx context.Context, limit int) ([]domain.Module, error) {
	return s.modules.ListUpstreamModules(ctx, limit)
}

func (s *ModuleService) DeleteModule(ctx context.Context, owner, name string) error {
	unlock, err := s.modules.LockModule(ctx, owner, name)
	if err != nil {
		return err
	}
	defer releaseModuleLock(unlock, owner, name)
	return s.deleteModuleLocked(ctx, owner, name)
}

func (s *ModuleService) DeleteModuleIfAllowed(ctx context.Context, owner, name string, activeSince time.Time) error {
	unlock, err := s.modules.LockModule(ctx, owner, name)
	if err != nil {
		return err
	}
	defer releaseModuleLock(unlock, owner, name)

	versions, err := s.modules.ListReleases(ctx, owner, name)
	if err != nil {
		return err
	}
	for _, version := range versions {
		active, err := s.isReleaseActive(ctx, owner, name, version.Version, activeSince)
		if err != nil {
			return err
		}
		if active {
			return protectedDeleteError{message: fmt.Sprintf("module contains active release %s/%s %s cannot be deleted", owner, name, version.Version)}
		}
	}
	return s.deleteModuleLocked(ctx, owner, name)
}

func (s *ModuleService) deleteModuleLocked(ctx context.Context, owner, name string) error {
	return s.modules.DeleteModule(ctx, owner, name)
}

func (s *ModuleService) DeleteRelease(ctx context.Context, owner, name, version string) error {
	unlock, err := s.modules.LockModule(ctx, owner, name)
	if err != nil {
		return err
	}
	defer releaseModuleLock(unlock, owner, name)
	return s.deleteReleaseLocked(ctx, owner, name, version)
}

func (s *ModuleService) DeleteReleaseIfAllowed(ctx context.Context, owner, name, version string, activeSince time.Time) error {
	unlock, err := s.modules.LockModule(ctx, owner, name)
	if err != nil {
		return err
	}
	defer releaseModuleLock(unlock, owner, name)

	module, err := s.modules.GetModule(ctx, owner, name)
	if err != nil {
		return err
	}
	if module.LatestVersion == version {
		return protectedDeleteError{message: fmt.Sprintf("latest release %s/%s %s cannot be deleted", owner, name, version)}
	}
	active, err := s.isReleaseActive(ctx, owner, name, version, activeSince)
	if err != nil {
		return err
	}
	if active {
		return protectedDeleteError{message: fmt.Sprintf("active release %s/%s %s cannot be deleted", owner, name, version)}
	}
	return s.deleteReleaseLocked(ctx, owner, name, version)
}

func (s *ModuleService) deleteReleaseLocked(ctx context.Context, owner, name, version string) error {
	return s.modules.DeleteRelease(ctx, owner, name, version)
}

func (s *ModuleService) isReleaseActive(ctx context.Context, owner, name, version string, since time.Time) (bool, error) {
	usageStore, ok := s.modules.(store.ReleaseUsageStore)
	if !ok {
		return false, nil
	}
	return usageStore.IsReleaseActive(ctx, owner, name, version, since)
}

type ArtifactDeletionResult struct {
	Attempted int
	Deleted   int
	Canceled  int
	Failed    int
	Pending   int
}

const artifactDeletionRetryDelay = time.Minute

func (s *ModuleService) CountArtifactDeletions(ctx context.Context) (int, error) {
	deletionStore, ok := s.modules.(store.ArtifactDeletionStore)
	if !ok {
		return 0, errors.New("artifact deletion store is not available")
	}
	return deletionStore.CountArtifactDeletions(ctx)
}

func (s *ModuleService) ProcessArtifactDeletions(ctx context.Context, limit int) (ArtifactDeletionResult, error) {
	deletionStore, ok := s.modules.(store.ArtifactDeletionStore)
	if !ok {
		return ArtifactDeletionResult{}, errors.New("artifact deletion store is not available")
	}
	if limit <= 0 {
		return ArtifactDeletionResult{}, errors.New("artifact deletion limit must be greater than zero")
	}

	deletions, err := deletionStore.ListArtifactDeletions(ctx, limit)
	if err != nil {
		return ArtifactDeletionResult{}, err
	}
	result := ArtifactDeletionResult{Attempted: len(deletions)}
	errs := make([]error, 0)
	for _, deletion := range deletions {
		canceled, err := s.processArtifactDeletion(ctx, deletionStore, deletion)
		if err != nil {
			result.Failed++
			metrics.ObserveArtifactDeletion("error")
			errs = append(errs, err)
			if deferErr := deletionStore.DeferArtifactDeletion(ctx, deletion.StoragePath, time.Now().Add(artifactDeletionRetryDelay)); deferErr != nil {
				errs = append(errs, fmt.Errorf("defer failed artifact deletion %q: %w", deletion.StoragePath, deferErr))
			}
			continue
		}
		if canceled {
			result.Canceled++
			metrics.ObserveArtifactDeletion("canceled")
		} else {
			result.Deleted++
			metrics.ObserveArtifactDeletion("deleted")
		}
	}
	result.Pending, err = deletionStore.CountArtifactDeletions(ctx)
	if err != nil {
		errs = append(errs, err)
	} else {
		metrics.SetArtifactDeletionsPending(result.Pending)
	}
	return result, errors.Join(errs...)
}

func (s *ModuleService) processArtifactDeletion(ctx context.Context, deletionStore store.ArtifactDeletionStore, deletion store.ArtifactDeletion) (bool, error) {
	unlock, err := s.modules.LockModule(ctx, deletion.Owner, deletion.Name)
	if err != nil {
		return false, fmt.Errorf("lock module for artifact deletion %q: %w", deletion.StoragePath, err)
	}
	defer releaseModuleLock(unlock, deletion.Owner, deletion.Name)

	referenced, err := deletionStore.IsArtifactReferenced(ctx, deletion.StoragePath)
	if err != nil {
		return false, fmt.Errorf("check artifact deletion reference %q: %w", deletion.StoragePath, err)
	}
	if !referenced {
		if err := s.artifacts.Delete(ctx, deletion.StoragePath); err != nil {
			return false, fmt.Errorf("delete queued artifact %q: %w", deletion.StoragePath, err)
		}
	}
	if err := deletionStore.CompleteArtifactDeletion(ctx, deletion.StoragePath); err != nil {
		return false, fmt.Errorf("complete artifact deletion %q: %w", deletion.StoragePath, err)
	}
	return referenced, nil
}

func (s *ModuleService) ListReleases(ctx context.Context, owner, name string) ([]domain.ModuleVersion, error) {
	return s.modules.ListReleases(ctx, owner, name)
}

func (s *ModuleService) ListReleasesForModules(ctx context.Context, modules []domain.Module) (map[string][]domain.ModuleVersion, error) {
	grouped := make(map[string][]domain.ModuleVersion, len(modules))
	if batchStore, ok := s.modules.(store.ModuleReleaseBatchStore); ok {
		releases, err := batchStore.ListReleasesForModules(ctx, modules)
		if err != nil {
			return nil, err
		}
		for _, release := range releases {
			key := release.Owner + "\x00" + release.Name
			grouped[key] = append(grouped[key], domain.ModuleVersion{Version: release.Version, CreatedAt: release.CreatedAt})
		}
		return grouped, nil
	}
	for _, module := range modules {
		versions, err := s.modules.ListReleases(ctx, module.Owner, module.Name)
		if err != nil {
			return nil, err
		}
		grouped[module.Owner+"\x00"+module.Name] = versions
	}
	return grouped, nil
}

func (s *ModuleService) CountReleasesForModules(ctx context.Context, modules []domain.Module) (map[string]int, error) {
	batchStore, ok := s.modules.(store.ModuleReleaseBatchStore)
	if !ok {
		counts := make(map[string]int, len(modules))
		for _, module := range modules {
			versions, err := s.modules.ListReleases(ctx, module.Owner, module.Name)
			if err != nil {
				return nil, err
			}
			counts[module.Owner+"\x00"+module.Name] = len(versions)
		}
		return counts, nil
	}
	rows, err := batchStore.CountReleasesForModules(ctx, modules)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int, len(rows))
	for _, row := range rows {
		counts[row.Owner+"\x00"+row.Name] = row.Count
	}
	return counts, nil
}

func (s *ModuleService) ListAllReleases(ctx context.Context) ([]store.ReleaseSummary, error) {
	return s.modules.ListAllReleases(ctx)
}

func (s *ModuleService) ListReleaseMetricSummaries(ctx context.Context) ([]domain.ReleaseMetricSummary, error) {
	summaryStore, ok := s.modules.(store.ReleaseMetricSummaryStore)
	if !ok {
		return nil, nil
	}
	return summaryStore.ListReleaseMetricSummaries(ctx)
}

func (s *ModuleService) MarkReleaseUsed(ctx context.Context, owner, name, version string) error {
	usageStore, ok := s.modules.(store.ReleaseUsageStore)
	if !ok {
		return nil
	}
	key := owner + "\x00" + name + "\x00" + version
	if !s.releaseUsageThrottle.Record(key, time.Now()) {
		return nil
	}
	err := usageStore.MarkReleaseUsed(ctx, owner, name, version)
	metrics.ObserveReleaseUsageMark(err)
	if err != nil {
		s.releaseUsageThrottle.Forget(key)
	}
	return err
}

func (s *ModuleService) IsReleaseActive(ctx context.Context, owner, name, version string, since time.Time) (bool, error) {
	return s.isReleaseActive(ctx, owner, name, version, since)
}

func (s *ModuleService) ListActiveReleases(ctx context.Context, since time.Time) ([]store.ReleaseSummary, error) {
	usageStore, ok := s.modules.(store.ReleaseUsageStore)
	if !ok {
		return nil, nil
	}
	return usageStore.ListActiveReleases(ctx, since)
}

func (s *ModuleService) PurgeReleaseUsage(ctx context.Context, before time.Time) error {
	usageStore, ok := s.modules.(store.ReleaseUsageStore)
	if !ok {
		return nil
	}
	return usageStore.PruneReleaseUsageBefore(ctx, before)
}

func (s *ModuleService) ListActiveReleasesForModules(ctx context.Context, since time.Time, modules []domain.Module) ([]store.ReleaseSummary, error) {
	usageStore, ok := s.modules.(store.ReleaseUsageStore)
	if !ok {
		return nil, nil
	}
	return usageStore.ListActiveReleasesForModules(ctx, since, modules)
}

func (s *ModuleService) GetModule(ctx context.Context, owner, name string) (domain.Module, error) {
	return s.modules.GetModule(ctx, owner, name)
}

func (s *ModuleService) GetModuleBySlug(ctx context.Context, slug string) (domain.Module, error) {
	slugStore, ok := s.modules.(store.SlugStore)
	if !ok {
		return domain.Module{}, store.ErrNotFound
	}
	return slugStore.GetModuleBySlug(ctx, slug)
}

func (s *ModuleService) GetReleaseBySlug(ctx context.Context, slug string) (domain.Release, error) {
	slugStore, ok := s.modules.(store.SlugStore)
	if !ok {
		return domain.Release{}, store.ErrNotFound
	}
	release, err := slugStore.GetReleaseBySlug(ctx, slug)
	if err == nil {
		return s.GetRelease(ctx, release.Owner, release.Name, release.Version)
	}
	if !errors.Is(err, store.ErrNotFound) || s.upstream == nil {
		return release, err
	}
	module, err := slugStore.GetModuleForReleaseSlug(ctx, slug)
	if err != nil {
		return domain.Release{}, err
	}
	prefix := module.Owner + "-" + module.Name + "-"
	version := strings.TrimPrefix(slug, prefix)
	if version == "" || version == slug {
		return domain.Release{}, store.ErrNotFound
	}
	return s.restoreUpstreamRelease(ctx, module.Owner, module.Name, version)
}

func (s *ModuleService) GetRelease(ctx context.Context, owner, name, version string) (domain.Release, error) {
	release, err := s.modules.GetRelease(ctx, owner, name, version)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) || s.upstream == nil {
			return domain.Release{}, err
		}
		release, err = s.restoreUpstreamRelease(ctx, owner, name, version)
		if err != nil {
			return domain.Release{}, err
		}
	}

	if release.Source == "upstream" {
		// Empty descriptive fields are valid upstream values. The artifact URI is
		// the durable marker that a release reference has been hydrated.
		if s.upstream != nil && release.UpstreamFileURI == "" {
			slug := release.UpstreamSlug
			if slug == "" {
				slug = owner + "-" + name + "-" + version
			}

			upstreamRelease, err := s.upstream.FetchRelease(ctx, slug)
			if err != nil {
				return domain.Release{}, fmt.Errorf("%w for %s: %w", ErrUpstreamHydration, slug, err)
			}
			release.Description = fallbackString(upstreamRelease.Description, release.Description)
			release.Readme = fallbackString(upstreamRelease.Readme, release.Readme)
			release.UpstreamSlug = fallbackString(upstreamRelease.Slug, release.UpstreamSlug)
			release.UpstreamFileURI = fallbackString(upstreamRelease.FileURI, release.UpstreamFileURI)
			release.FileName = fallbackString(upstreamRelease.FileName, release.FileName)
			if upstreamRelease.FileSize > 0 {
				release.SizeBytes = upstreamRelease.FileSize
			}
			release.MD5 = fallbackString(upstreamRelease.FileMD5, release.MD5)
			release.SHA256 = fallbackString(upstreamRelease.FileSHA256, release.SHA256)
			if saved, saveErr := s.modules.CreateRelease(ctx, release); saveErr == nil {
				release = saved
			} else {
				slog.Warn("persist hydrated upstream release failed", "owner", owner, "name", name, "version", version, "err", saveErr)
			}
		}

		release.DownloadURL = release.UpstreamFileURI
		return release, nil
	}

	release.DownloadURL = s.artifacts.PublicURL(release.StoragePath)
	return release, nil
}

func (s *ModuleService) restoreUpstreamRelease(ctx context.Context, owner, name, version string) (domain.Release, error) {
	slug := owner + "-" + name + "-" + version
	upstreamRelease, err := s.upstream.FetchRelease(ctx, slug)
	if err != nil {
		if errors.Is(err, proxy.ErrUpstreamNotFound) {
			return domain.Release{}, store.ErrNotFound
		}
		return domain.Release{}, fmt.Errorf("%w: %w for %s: %w", ErrUpstreamHydration, ErrUpstreamRestore, slug, err)
	}
	if upstreamRelease.Version == "" {
		upstreamRelease.Version = version
	}
	if upstreamRelease.Version != version {
		return domain.Release{}, store.ErrNotFound
	}
	if upstreamRelease.Slug == "" {
		upstreamRelease.Slug = slug
	}

	module, err := s.modules.UpsertModule(ctx, owner, name)
	if err != nil {
		return domain.Release{}, err
	}

	release := upstreamDomainRelease(module, upstreamRelease)
	saved, err := s.modules.CreateReleaseIfAbsent(ctx, release)
	if errors.Is(err, store.ErrConflict) {
		saved, err = s.modules.GetRelease(ctx, owner, name, version)
	}
	if err != nil {
		return domain.Release{}, err
	}
	saved.DownloadURL = saved.UpstreamFileURI
	return saved, nil
}

func (s *ModuleService) ReadReleaseFile(ctx context.Context, owner, name, version, filePath string) (storage.Object, error) {
	release, err := s.GetRelease(ctx, owner, name, version)
	if err != nil {
		return storage.Object{}, err
	}
	if release.Source == "upstream" {
		release, err = s.EnsureReleaseChecksums(ctx, release)
		if err != nil {
			return storage.Object{}, err
		}
	}
	archive, err := s.openReleaseArchive(ctx, release)
	if err != nil {
		return storage.Object{}, err
	}
	body, contentType, extractErr := extractArchiveFile(archive.Body, filePath)
	closeErr := archive.Body.Close()
	if err := errors.Join(extractErr, closeErr); err != nil {
		return storage.Object{}, err
	}

	return storage.Object{Body: body, ContentType: contentType}, nil
}

func (s *ModuleService) OpenReleaseArchive(ctx context.Context, owner, name, version string) (storage.ObjectReader, error) {
	release, err := s.GetRelease(ctx, owner, name, version)
	if err != nil {
		return storage.ObjectReader{}, err
	}
	return s.openReleaseArchive(ctx, release)
}

func (s *ModuleService) openReleaseArchive(ctx context.Context, release domain.Release) (storage.ObjectReader, error) {
	archivePath := releaseArchivePath(release)
	if archivePath == "" {
		return storage.ObjectReader{}, store.ErrNotFound
	}

	object, err := s.artifacts.Open(ctx, archivePath)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotFound) {
			return storage.ObjectReader{}, store.ErrNotFound
		}
		return storage.ObjectReader{}, fmt.Errorf("open release archive: %w", err)
	}

	return object, nil
}

func (s *ModuleService) StatReleaseArchive(ctx context.Context, release domain.Release) (storage.ObjectAttrs, error) {
	archivePath := releaseArchivePath(release)
	if archivePath == "" {
		return storage.ObjectAttrs{}, store.ErrNotFound
	}
	attrs, err := s.artifacts.Stat(ctx, archivePath)
	if errors.Is(err, storage.ErrObjectNotFound) {
		return storage.ObjectAttrs{}, store.ErrNotFound
	}
	if err != nil {
		return storage.ObjectAttrs{}, fmt.Errorf("stat release archive: %w", err)
	}
	return attrs, nil
}

func (s *ModuleService) OpenReleaseArchiveRange(ctx context.Context, release domain.Release, offset, length int64) (storage.ObjectReader, error) {
	rangeStorage, ok := s.artifacts.(storage.RangeArtifactStorage)
	if !ok {
		return storage.ObjectReader{}, errors.New("artifact storage does not support range reads")
	}
	archivePath := releaseArchivePath(release)
	if archivePath == "" {
		return storage.ObjectReader{}, store.ErrNotFound
	}
	object, err := rangeStorage.OpenRange(ctx, archivePath, offset, length)
	if errors.Is(err, storage.ErrObjectNotFound) {
		return storage.ObjectReader{}, store.ErrNotFound
	}
	if err != nil {
		return storage.ObjectReader{}, fmt.Errorf("open release archive range: %w", err)
	}
	return object, nil
}

func releaseArchivePath(release domain.Release) string {
	if release.StoragePath != "" {
		return release.StoragePath
	}
	if release.Source == "upstream" {
		return cachedUpstreamArtifactPath(release.UpstreamFileURI)
	}
	return ""
}

func (s *ModuleService) EnsureReleaseChecksums(ctx context.Context, release domain.Release) (domain.Release, error) {
	checksumsComplete := release.MD5 != "" && release.SHA256 != "" && release.SizeBytes > 0
	if checksumsComplete && (release.Source != "upstream" || release.StoragePath != "") {
		return release, nil
	}
	if release.Source == "upstream" {
		if s.upstream == nil || release.UpstreamFileURI == "" {
			return release, store.ErrNotFound
		}
		for range 2 {
			var err error
			if release.SHA256 != "" && release.SizeBytes > 0 {
				err = s.upstream.EnsureArtifactIntegrity(ctx, release.UpstreamFileURI, release.SHA256, release.SizeBytes)
			} else {
				err = s.upstream.EnsureArtifact(ctx, release.UpstreamFileURI)
			}
			if err != nil {
				return release, fmt.Errorf("%w: cache upstream release artifact: %w", ErrUpstreamHydration, err)
			}

			var verified domain.Release
			err = s.upstream.WithCachedArtifactLease(ctx, releaseArchivePath(release), func(leaseCtx context.Context) error {
				var calculateErr error
				verified, calculateErr = s.calculateReleaseChecksums(leaseCtx, release)
				return calculateErr
			})
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return verified, err
		}
		return release, fmt.Errorf("%w: cached upstream artifact disappeared during verification", ErrUpstreamHydration)
	}
	return s.calculateReleaseChecksums(ctx, release)
}

func (s *ModuleService) calculateReleaseChecksums(ctx context.Context, release domain.Release) (domain.Release, error) {
	expectedSHA256 := release.SHA256
	expectedSize := release.SizeBytes
	object, err := s.openReleaseArchive(ctx, release)
	if err != nil {
		return release, err
	}
	md5Hash := md5.New() // #nosec G401 -- compatibility checksum only; SHA-256 is authoritative.
	shaHash := sha256.New()
	sizeBytes, copyErr := io.Copy(io.MultiWriter(md5Hash, shaHash), contextReader{ctx: ctx, reader: object.Body})
	closeErr := object.Body.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return release, fmt.Errorf("calculate release checksums: %w", err)
	}
	release.MD5 = hex.EncodeToString(md5Hash.Sum(nil))
	release.SHA256 = hex.EncodeToString(shaHash.Sum(nil))
	release.SizeBytes = sizeBytes
	release.StoragePath = releaseArchivePath(release)
	if release.Source == "upstream" && ((expectedSHA256 != "" && !strings.EqualFold(expectedSHA256, release.SHA256)) || (expectedSize > 0 && expectedSize != release.SizeBytes)) {
		return domain.Release{}, fmt.Errorf("%w: cached upstream artifact does not match release metadata", ErrUpstreamHydration)
	}

	if checksumStore, ok := s.modules.(store.ReleaseChecksumStore); ok {
		if err := checksumStore.UpdateReleaseChecksums(
			ctx,
			release.Owner,
			release.Name,
			release.Version,
			release.MD5,
			release.SHA256,
			release.StoragePath,
			release.SizeBytes,
		); err != nil {
			return domain.Release{}, err
		}
	}
	return release, nil
}

func (s *ModuleService) Ready(ctx context.Context) error {
	if s == nil || s.modules == nil {
		return errors.New("module store is not available")
	}
	if err := s.modules.Ping(ctx); err != nil {
		return fmt.Errorf("metadata store readiness: %w", err)
	}
	if s.artifacts == nil {
		return errors.New("artifact storage is not available")
	}
	s.readinessMu.Lock()
	defer s.readinessMu.Unlock()
	if !s.readinessCheckedAt.IsZero() && time.Since(s.readinessCheckedAt) < readinessCapabilityTTL {
		return nil
	}
	probeID := make([]byte, 16)
	if _, err := rand.Read(probeID); err != nil {
		return fmt.Errorf("generate artifact storage readiness probe: %w", err)
	}
	readinessPath := path.Join(s.prefix, ".readiness-probes", hex.EncodeToString(probeID))
	created, err := s.artifacts.UploadReaderIfAbsent(ctx, readinessPath, "application/octet-stream", strings.NewReader("ready"))
	if err != nil {
		return fmt.Errorf("write artifact storage readiness probe: %w", err)
	}
	if !created {
		return errors.New("artifact storage readiness probe path already exists")
	}
	object, openErr := s.artifacts.Open(ctx, readinessPath)
	var readErr error
	if openErr == nil {
		_, readErr = io.Copy(io.Discard, io.LimitReader(object.Body, 16))
		readErr = errors.Join(readErr, object.Body.Close())
	}
	deleteErr := s.artifacts.Delete(ctx, readinessPath)
	if err := errors.Join(openErr, readErr, deleteErr); err != nil {
		return fmt.Errorf("verify artifact storage readiness probe: %w", err)
	}
	s.readinessCheckedAt = time.Now()
	return nil
}

func (s *ModuleService) LoadTeamConfigs(ctx context.Context) ([]auth.TeamConfig, error) {
	if s.access == nil {
		return nil, errors.New("access store is not available")
	}
	return s.access.LoadTeamConfigs(ctx)
}

func (s *ModuleService) LockAccessConfig(ctx context.Context) (store.AccessConfigUnlock, error) {
	if s.access == nil {
		return nil, errors.New("access store is not available")
	}
	return s.access.LockAccessConfig(ctx)
}

func (s *ModuleService) ReplaceTeamConfigs(ctx context.Context, configs []auth.TeamConfig) error {
	if s.access == nil {
		return errors.New("access store is not available")
	}
	return s.access.ReplaceTeamConfigs(ctx, configs)
}

func (s *ModuleService) IsAccessTokenActive(ctx context.Context, tokenID string, now time.Time) (bool, error) {
	if s.access == nil {
		return false, errors.New("access store is not available")
	}
	return s.access.IsAccessTokenActive(ctx, tokenID, now)
}

func (s *ModuleService) MarkAccessTokenUsed(ctx context.Context, tokenID string, usedAt time.Time) error {
	if s.access == nil {
		return errors.New("access store is not available")
	}
	return s.access.MarkAccessTokenUsed(ctx, tokenID, usedAt)
}

func (s *ModuleService) PurgeAccessTokenHistory(ctx context.Context, cutoff time.Time) (int64, error) {
	if s.access == nil {
		return 0, errors.New("access store is not available")
	}
	return s.access.PurgeAccessTokenHistory(ctx, cutoff)
}

func (s *ModuleService) PurgeDeletedReleases(ctx context.Context, cutoff time.Time) (int64, error) {
	backend, ok := s.modules.(store.DeletedReleaseStore)
	if !ok {
		return 0, errors.New("deleted release store is not available")
	}
	return backend.PurgeDeletedReleases(ctx, cutoff)
}

func (s *ModuleService) CreateManageSession(ctx context.Context, session store.ManageSession) error {
	backend, ok := s.modules.(store.ManageSessionStore)
	if !ok {
		return errors.New("manage session store is not available")
	}
	return backend.CreateManageSession(ctx, session)
}

func (s *ModuleService) GetManageSession(ctx context.Context, sessionHash string, now time.Time) (store.ManageSession, error) {
	backend, ok := s.modules.(store.ManageSessionStore)
	if !ok {
		return store.ManageSession{}, errors.New("manage session store is not available")
	}
	return backend.GetManageSession(ctx, sessionHash, now)
}

func (s *ModuleService) RevokeManageSession(ctx context.Context, sessionHash string, revokedAt time.Time) error {
	backend, ok := s.modules.(store.ManageSessionStore)
	if !ok {
		return errors.New("manage session store is not available")
	}
	return backend.RevokeManageSession(ctx, sessionHash, revokedAt)
}

func (s *ModuleService) PurgeSessionState(ctx context.Context, now, historyBefore time.Time) (store.SessionCleanupResult, error) {
	backend, ok := s.modules.(store.SessionMaintenanceStore)
	if !ok {
		return store.SessionCleanupResult{}, errors.New("session maintenance store is not available")
	}
	return backend.PurgeSessionState(ctx, now, historyBefore)
}

func (s *ModuleService) SyncUpstreamModule(ctx context.Context, owner, name string) error {
	return s.syncUpstreamModule(ctx, owner, name, "single")
}

func (s *ModuleService) syncUpstreamModule(ctx context.Context, owner, name, trigger string) error {
	if s.upstream == nil {
		err := store.ErrNotFound
		metrics.ObserveUpstreamSync(trigger, err)
		return err
	}

	upstreamModule, err := s.upstream.FetchModule(ctx, owner+"-"+name)
	if err != nil {
		metrics.ObserveUpstreamSync(trigger, err)
		return err
	}

	err = s.IndexUpstreamModule(ctx, upstreamModule)
	metrics.ObserveUpstreamSync(trigger, err)
	return err
}

type upstreamRefreshResult struct {
	module domain.Module
	err    error
}

func (s *ModuleService) RefreshCachedUpstreamModules(ctx context.Context, limit, concurrency int) error {
	start := time.Now()
	if s.upstream == nil {
		metrics.ObserveUpstreamRefresh(start, 0, 0, 0)
		return nil
	}

	modules, err := s.modules.ListUpstreamModules(ctx, limit)
	if err != nil {
		metrics.ObserveUpstreamRefresh(start, 0, 0, 1)
		return err
	}

	workerCount := min(max(concurrency, 1), len(modules))
	slog.Default().Info("starting upstream module refresh cycle", "modules", len(modules), "limit", limit, "concurrency", workerCount)

	jobs := make(chan domain.Module)
	results := make(chan upstreamRefreshResult, workerCount)
	var workers sync.WaitGroup
	for range workerCount {
		workers.Go(func() {
			for module := range jobs {
				slog.Default().Debug("refreshing upstream module", "owner", module.Owner, "name", module.Name)
				refreshErr := s.syncUpstreamModule(ctx, module.Owner, module.Name, "refresh")
				markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), upstreamRefreshAttemptTimeout)
				markErr := s.modules.MarkUpstreamModuleRefreshAttempt(markCtx, module.Owner, module.Name, time.Now())
				cancel()
				results <- upstreamRefreshResult{
					module: module,
					err:    errors.Join(refreshErr, markErr),
				}
			}
		})
	}

	go func() {
		defer close(jobs)
		for _, module := range modules {
			select {
			case jobs <- module:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		workers.Wait()
		close(results)
	}()

	attempted := 0
	succeeded := 0
	failed := 0
	interrupted := false
	var refreshErrors []error
	for result := range results {
		attempted++
		if result.err != nil {
			failed++
			refreshErrors = append(refreshErrors, fmt.Errorf("refresh %s/%s: %w", result.module.Owner, result.module.Name, result.err))
			if errors.Is(result.err, context.Canceled) || errors.Is(result.err, context.DeadlineExceeded) {
				interrupted = true
			}
			slog.Default().Error("upstream refresh failed", "owner", result.module.Owner, "name", result.module.Name, "err", result.err)
			continue
		}
		succeeded++
	}

	slog.Default().Info("finished upstream module refresh cycle", "modules", len(modules), "attempted", attempted, "succeeded", succeeded, "failed", failed, "duration", time.Since(start).Round(time.Millisecond))
	metrics.ObserveUpstreamRefresh(start, attempted, succeeded, failed)

	if attempted < len(modules) {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("upstream refresh interrupted after %d of %d modules: %w", attempted, len(modules), err)
		}
		return fmt.Errorf("upstream refresh incomplete: attempted %d of %d modules", attempted, len(modules))
	}
	if interrupted || len(refreshErrors) > 0 {
		return errors.Join(refreshErrors...)
	}
	return nil
}

func (s *ModuleService) IndexUpstreamModule(ctx context.Context, upstreamModule proxy.UpstreamModule) error {
	owner := upstreamModule.Owner
	name := upstreamModule.Name
	if !moduleOwnerPattern.MatchString(owner) {
		return errors.New("invalid upstream module owner")
	}
	if !moduleNamePattern.MatchString(name) {
		return errors.New("invalid upstream module name")
	}
	unlock, err := s.modules.LockModule(ctx, owner, name)
	if err != nil {
		return err
	}
	defer releaseModuleLock(unlock, owner, name)

	module, err := s.modules.UpsertModule(ctx, owner, name)
	if err != nil {
		return err
	}

	currentVersion := upstreamModule.CurrentRelease.Version
	if currentVersion == "" {
		currentVersion = httputil.ReleaseVersionFromSlug(upstreamModule.Slug, upstreamModule.CurrentRelease.Slug)
	}

	for _, ref := range upstreamModule.Releases {
		if ref.Version == "" {
			ref.Version = httputil.ReleaseVersionFromSlug(upstreamModule.Slug, ref.Slug)
		}
		if ref.Version == "" {
			continue
		}
		deleted, err := s.isDeletedUpstreamRelease(ctx, owner, name, ref.Version)
		if err != nil {
			return err
		}
		if deleted {
			continue
		}

		release := upstreamDomainRelease(module, proxy.UpstreamRelease{
			Slug:    fallbackString(ref.Slug, owner+"-"+name+"-"+ref.Version),
			Version: ref.Version,
		})

		if err := s.createUpstreamReleaseIfAbsent(ctx, release); err != nil {
			return err
		}
	}

	if currentVersion != "" {
		deleted, err := s.isDeletedUpstreamRelease(ctx, owner, name, currentVersion)
		if err != nil {
			return err
		}
		if deleted {
			return nil
		}
		release := upstreamDomainRelease(module, proxy.UpstreamRelease{
			Slug:    fallbackString(upstreamModule.CurrentRelease.Slug, owner+"-"+name+"-"+currentVersion),
			Version: currentVersion,
		})
		if err := s.createUpstreamReleaseIfAbsent(ctx, release); err != nil {
			return err
		}
	}

	return nil
}

func (s *ModuleService) createUpstreamReleaseIfAbsent(ctx context.Context, release domain.Release) error {
	_, err := s.modules.CreateReleaseIfAbsent(ctx, release)
	if errors.Is(err, store.ErrConflict) {
		return nil
	}
	return err
}

func upstreamDomainRelease(module domain.Module, upstreamRelease proxy.UpstreamRelease) domain.Release {
	fileName := fallbackString(upstreamRelease.FileName, module.Name+"-"+upstreamRelease.Version+".tar.gz")
	return domain.Release{
		ID:              module.ID + ":" + upstreamRelease.Version,
		ModuleID:        module.ID,
		Owner:           module.Owner,
		Name:            module.Name,
		Source:          "upstream",
		Version:         upstreamRelease.Version,
		Description:     upstreamRelease.Description,
		Readme:          upstreamRelease.Readme,
		FileName:        fileName,
		ContentType:     "application/gzip",
		SizeBytes:       upstreamRelease.FileSize,
		MD5:             upstreamRelease.FileMD5,
		SHA256:          upstreamRelease.FileSHA256,
		StoragePath:     "",
		UpstreamSlug:    fallbackString(upstreamRelease.Slug, module.Owner+"-"+module.Name+"-"+upstreamRelease.Version),
		UpstreamFileURI: upstreamRelease.FileURI,
		Metadata:        map[string]any{},
	}
}

func (s *ModuleService) isDeletedUpstreamRelease(ctx context.Context, owner, name, version string) (bool, error) {
	deletedStore, ok := s.modules.(store.DeletedReleaseStore)
	if !ok {
		return false, nil
	}
	return deletedStore.IsReleaseDeleted(ctx, owner, name, version, "upstream")
}

type inspectedModuleArchive struct {
	Owner       string
	Name        string
	Version     string
	Description string
	Readme      string
	Metadata    map[string]any
}

func inspectModuleArchive(archive io.Reader) (inspectedModuleArchive, error) {
	return inspectModuleArchiveWithLimit(archive, maxArchiveExpandedSize)
}

func inspectModuleArchiveWithLimit(archive io.Reader, maxExpandedSize int64) (inspectedModuleArchive, error) {
	gzipReader, err := gzip.NewReader(archive)
	if err != nil {
		return inspectedModuleArchive{}, err
	}
	defer func() {
		_ = gzipReader.Close()
	}()

	tarReader := tar.NewReader(gzipReader)
	info := inspectedModuleArchive{
		Metadata: map[string]any{},
	}
	metadataFound := false
	readmeFound := false
	entryCount := 0
	expandedSize := int64(0)
	root := ""
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			if !metadataFound {
				return inspectedModuleArchive{}, errors.New("metadata.json is required")
			}
			if info.Owner != "" && info.Name != "" && info.Version != "" {
				expectedRoot := info.Owner + "-" + info.Name + "-" + info.Version
				if root != expectedRoot {
					return inspectedModuleArchive{}, fmt.Errorf("archive root %q does not match module identity %q", root, expectedRoot)
				}
			}
			return info, nil
		}
		if err != nil {
			return inspectedModuleArchive{}, err
		}
		entryCount++
		if err := enforceArchiveEntryLimit(entryCount, maxArchiveEntries); err != nil {
			return inspectedModuleArchive{}, err
		}
		cleanName, entryRoot, err := validateArchiveEntry(header)
		if err != nil {
			return inspectedModuleArchive{}, err
		}
		if root == "" {
			root = entryRoot
		} else if entryRoot != root {
			return inspectedModuleArchive{}, errors.New("archive entries must share one root directory")
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Size > maxArchiveEntrySize {
			return inspectedModuleArchive{}, fmt.Errorf("archive entry %q exceeds maximum size of %d bytes", header.Name, maxArchiveEntrySize)
		}
		if header.Size < 0 || expandedSize > maxExpandedSize-header.Size {
			return inspectedModuleArchive{}, fmt.Errorf("archive expanded size exceeds maximum of %d bytes", maxExpandedSize)
		}
		expandedSize += header.Size

		relativeName := strings.TrimPrefix(cleanName, root+"/")
		base := strings.ToLower(path.Base(relativeName))
		switch {
		case isReadmeFile(base):
			if strings.Contains(relativeName, "/") {
				continue
			}
			if readmeFound {
				return inspectedModuleArchive{}, errors.New("archive contains multiple root README files")
			}
			readmeFound = true
			body, err := readLimitedArchiveEntry(tarReader, header, maxArchiveReadmeSize)
			if err != nil {
				return inspectedModuleArchive{}, err
			}
			info.Readme = string(body)
		case base == "metadata.json":
			if relativeName != "metadata.json" {
				return inspectedModuleArchive{}, errors.New("metadata.json must be in the archive root directory")
			}
			if metadataFound {
				return inspectedModuleArchive{}, errors.New("archive contains multiple metadata.json files")
			}
			metadataFound = true
			body, err := readLimitedArchiveEntry(tarReader, header, maxArchiveMetadataSize)
			if err != nil {
				return inspectedModuleArchive{}, err
			}
			metadata, err := parseArchiveMetadata(body)
			if err != nil {
				return inspectedModuleArchive{}, err
			}
			info.Owner = metadata.Owner()
			info.Name = metadata.ModuleName()
			info.Version = metadata.Version
			info.Description = metadata.EffectiveDescription()
			info.Metadata = metadata.Metadata
		}
	}
}

func validateArchiveEntry(header *tar.Header) (cleanName, root string, err error) {
	if header.Name == "" || strings.Contains(header.Name, "\\") || path.IsAbs(header.Name) {
		return "", "", fmt.Errorf("archive entry has invalid path %q", header.Name)
	}
	for part := range strings.SplitSeq(strings.TrimSuffix(header.Name, "/"), "/") {
		if part == "." || part == ".." {
			return "", "", fmt.Errorf("archive entry has invalid path %q", header.Name)
		}
	}
	cleanName = path.Clean(header.Name)
	if cleanName == "." || cleanName == ".." || strings.HasPrefix(cleanName, "../") {
		return "", "", fmt.Errorf("archive entry has invalid path %q", header.Name)
	}
	parts := strings.Split(cleanName, "/")
	if parts[0] == "" {
		return "", "", fmt.Errorf("archive entry %q is outside the module root directory", header.Name)
	}
	if len(parts) == 1 && header.Typeflag != tar.TypeDir {
		return "", "", fmt.Errorf("archive entry %q is outside the module root directory", header.Name)
	}
	switch header.Typeflag {
	case 0, tar.TypeReg, tar.TypeDir:
	default:
		return "", "", fmt.Errorf("archive entry %q uses unsupported type %d", header.Name, header.Typeflag)
	}
	return cleanName, parts[0], nil
}

func extractArchiveFile(archive io.Reader, filePath string) ([]byte, string, error) {
	cleanPath := path.Clean(strings.TrimPrefix(filePath, "/"))
	if cleanPath == "." || cleanPath == "" || strings.HasPrefix(cleanPath, "../") {
		return nil, "", errors.New("invalid file path")
	}

	gzipReader, err := gzip.NewReader(archive)
	if err != nil {
		return nil, "", fmt.Errorf("open archive: %w", err)
	}
	defer func() {
		_ = gzipReader.Close()
	}()

	tarReader := tar.NewReader(gzipReader)
	entryCount := 0
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			return nil, "", store.ErrNotFound
		}
		if err != nil {
			return nil, "", fmt.Errorf("read archive: %w", err)
		}
		entryCount++
		if err := enforceArchiveEntryLimit(entryCount, maxArchiveEntries); err != nil {
			return nil, "", err
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}

		headerPath := normalizeArchivePath(header.Name)
		if headerPath != cleanPath {
			continue
		}

		body, err := readLimitedArchiveEntry(tarReader, header, maxArchiveFileSize)
		if err != nil {
			return nil, "", fmt.Errorf("read archive file: %w", err)
		}

		contentType := mime.TypeByExtension(filepath.Ext(cleanPath))
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		return body, contentType, nil
	}
}

func enforceArchiveEntryLimit(entryCount, maxEntries int) error {
	if maxEntries <= 0 || entryCount <= maxEntries {
		return nil
	}
	return fmt.Errorf("archive contains more than %d entries", maxEntries)
}

func readLimitedArchiveEntry(r io.Reader, header *tar.Header, limit int64) ([]byte, error) {
	if limit <= 0 {
		return io.ReadAll(r)
	}
	if header.Size > limit {
		return nil, fmt.Errorf("archive entry %q exceeds maximum size of %d bytes", header.Name, limit)
	}
	limited := io.LimitReader(r, limit+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("archive entry %q exceeds maximum size of %d bytes", header.Name, limit)
	}
	return body, nil
}

func parseArchiveMetadata(body []byte) (archiveModuleMetadata, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return archiveModuleMetadata{}, fmt.Errorf("parse metadata.json: %w", err)
	}

	var metadata archiveModuleMetadata
	if err := json.Unmarshal(body, &metadata); err != nil {
		return archiveModuleMetadata{}, fmt.Errorf("decode metadata.json: %w", err)
	}
	metadata.Metadata = raw
	return metadata, nil
}

func isReadmeFile(name string) bool {
	switch {
	case strings.HasPrefix(name, "readme."):
		return true
	case name == "readme":
		return true
	default:
		return false
	}
}

func (m archiveModuleMetadata) Owner() string {
	owner, _ := splitModuleIdentity(m.Name)
	return owner
}

func (m archiveModuleMetadata) ModuleName() string {
	_, name := splitModuleIdentity(m.Name)
	return name
}

func (m archiveModuleMetadata) EffectiveDescription() string {
	if strings.TrimSpace(m.Summary) != "" {
		return m.Summary
	}
	return m.Description
}

func fallbackString(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

// splitModuleIdentity accepts Puppet module identities only in owner-name,
// owner/name, or bare name form. Owners and module names are validated later by
// moduleOwnerPattern and moduleNamePattern, so nested owner/name/path identities are intentionally rejected.
func splitModuleIdentity(raw string) (owner, name string) {
	if left, right, ok := strings.Cut(raw, "-"); ok {
		return left, right
	}
	if left, right, ok := strings.Cut(raw, "/"); ok {
		return left, right
	}
	return "", raw
}

func releaseArchiveFileName(owner, name, version string) string {
	return owner + "-" + name + "-" + version + ".tar.gz"
}

func normalizeArchivePath(name string) string {
	clean := path.Clean(strings.TrimPrefix(name, "/"))
	parts := strings.Split(clean, "/")
	if len(parts) > 1 {
		return path.Join(parts[1:]...)
	}
	return clean
}

func cachedUpstreamArtifactPath(fileURI string) string {
	if fileURI == "" {
		return ""
	}
	trimmed := fileURI
	if strings.Contains(trimmed, "/v3/files/") {
		trimmed = trimmed[strings.Index(trimmed, "/v3/files/"):]
	}
	trimmed = strings.TrimPrefix(trimmed, "/")
	if trimmed == "" {
		return ""
	}
	return path.Join("upstream-cache", trimmed)
}
