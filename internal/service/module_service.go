package service

import (
	"archive/tar"
	"bytes"
	"context"
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
	"time"

	"compress/gzip"

	"puppet-forge/internal/auth"
	"puppet-forge/internal/domain"
	"puppet-forge/internal/httputil"
	"puppet-forge/internal/metrics"
	"puppet-forge/internal/proxy"
	"puppet-forge/internal/storage"
	"puppet-forge/internal/store"
)

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

const (
	maxArchiveEntries      = 10000
	maxArchiveEntrySize    = 128 << 20
	maxArchiveMetadataSize = 1 << 20
	maxArchiveReadmeSize   = 2 << 20
	maxArchiveFileSize     = 16 << 20
)

type ModuleService struct {
	modules   store.ModuleStore
	access    store.AccessStore
	artifacts storage.ArtifactStorage
	prefix    string
	upstream  *proxy.ForgeProxy
}

func NewModuleService(modules store.ModuleStore, artifacts storage.ArtifactStorage, prefix string, upstream *proxy.ForgeProxy) *ModuleService {
	access, _ := modules.(store.AccessStore)
	return &ModuleService{
		modules:   modules,
		access:    access,
		artifacts: artifacts,
		prefix:    prefix,
		upstream:  upstream,
	}
}

type archiveModuleMetadata struct {
	Name        string         `json:"name"`
	Version     string         `json:"version"`
	Summary     string         `json:"summary"`
	Description string         `json:"description"`
	Metadata    map[string]any `json:"-"`
}

func (s *ModuleService) Publish(ctx context.Context, input domain.PublishModuleInput) (domain.Release, error) {
	owner := input.Owner
	input, err := s.NormalizePublishInput(input)
	if err != nil {
		metrics.ObservePublish(owner, err)
		return domain.Release{}, err
	}
	owner = input.Owner

	if !slugPattern.MatchString(input.Owner) {
		err := errors.New("invalid owner")
		metrics.ObservePublish(owner, err)
		return domain.Release{}, err
	}
	if !slugPattern.MatchString(input.Name) {
		err := errors.New("invalid name")
		metrics.ObservePublish(owner, err)
		return domain.Release{}, err
	}
	if input.Version == "" {
		err := errors.New("version is required")
		metrics.ObservePublish(owner, err)
		return domain.Release{}, err
	}
	if len(input.FileBytes) == 0 {
		err := errors.New("artifact file is required")
		metrics.ObservePublish(owner, err)
		return domain.Release{}, err
	}
	if input.FileName == "" {
		err := errors.New("file name is required")
		metrics.ObservePublish(owner, err)
		return domain.Release{}, err
	}

	module, err := s.modules.UpsertModule(ctx, input.Owner, input.Name)
	if err != nil {
		metrics.ObservePublish(owner, err)
		return domain.Release{}, err
	}

	sha := sha256.Sum256(input.FileBytes)
	shaHex := hex.EncodeToString(sha[:])
	objectPath := path.Join(s.prefix, input.Owner, input.Name, input.Version, input.FileName)

	if err := s.artifacts.Upload(ctx, objectPath, input.ContentType, input.FileBytes); err != nil {
		err := fmt.Errorf("upload artifact: %w", err)
		metrics.ObservePublish(owner, err)
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
		shaHex,
		objectPath,
		int64(len(input.FileBytes)),
		input.Metadata,
	)

	release, err = s.modules.CreateRelease(ctx, release)
	if err != nil {
		metrics.ObservePublish(owner, err)
		return domain.Release{}, err
	}

	metrics.ObservePublish(owner, nil)
	release.DownloadURL = s.artifacts.PublicURL(release.StoragePath)
	return release, nil
}

func (s *ModuleService) NormalizePublishInput(input domain.PublishModuleInput) (domain.PublishModuleInput, error) {
	if len(input.FileBytes) == 0 {
		return input, errors.New("artifact file is required")
	}
	if input.FileName == "" {
		return input, errors.New("file name is required")
	}

	archiveInfo, err := inspectModuleArchive(input.FileBytes)
	if err != nil {
		return input, fmt.Errorf("inspect module archive: %w", err)
	}

	if input.Owner == "" {
		input.Owner = archiveInfo.Owner
	}
	if input.Name == "" {
		input.Name = archiveInfo.Name
	}
	if input.Version == "" {
		input.Version = archiveInfo.Version
	}
	if input.Description == "" {
		input.Description = archiveInfo.Description
	}
	input.Readme = archiveInfo.Readme
	input.Metadata = mergeMetadata(archiveInfo.Metadata, input.Metadata)

	return input, nil
}

func (s *ModuleService) ListModules(ctx context.Context, limit int) ([]domain.Module, error) {
	return s.modules.ListModules(ctx, limit)
}

func (s *ModuleService) ListModulesPage(ctx context.Context, limit, offset int) ([]domain.Module, int, error) {
	return s.modules.ListModulesPage(ctx, limit, offset)
}

func (s *ModuleService) ListUpstreamModules(ctx context.Context, limit int) ([]domain.Module, error) {
	return s.modules.ListUpstreamModules(ctx, limit)
}

func (s *ModuleService) DeleteModule(ctx context.Context, owner, name string) error {
	return s.modules.DeleteModule(ctx, owner, name)
}

func (s *ModuleService) DeleteRelease(ctx context.Context, owner, name, version string) error {
	return s.modules.DeleteRelease(ctx, owner, name, version)
}

func (s *ModuleService) ListReleases(ctx context.Context, owner, name string) ([]domain.ModuleVersion, error) {
	return s.modules.ListReleases(ctx, owner, name)
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
	err := usageStore.MarkReleaseUsed(ctx, owner, name, version)
	metrics.ObserveReleaseUsageMark(owner, err)
	return err
}

func (s *ModuleService) MarkUpstreamModuleCurrentReleaseUsed(ctx context.Context, upstreamModule proxy.UpstreamModule) error {
	owner := fallbackString(upstreamModule.Owner, extractOwnerFromSlug(upstreamModule.Slug))
	name := fallbackString(upstreamModule.Name, extractNameFromSlug(upstreamModule.Slug))
	version := upstreamModule.CurrentRelease.Version
	if version == "" {
		version = httputil.ReleaseVersionFromSlug(upstreamModule.Slug, upstreamModule.CurrentRelease.Slug)
	}
	if owner == "" || name == "" || version == "" {
		return nil
	}
	return s.MarkReleaseUsed(ctx, owner, name, version)
}

func (s *ModuleService) IsReleaseActive(ctx context.Context, owner, name, version string, since time.Time) (bool, error) {
	usageStore, ok := s.modules.(store.ReleaseUsageStore)
	if !ok {
		return false, nil
	}
	return usageStore.IsReleaseActive(ctx, owner, name, version, since)
}

func (s *ModuleService) ListActiveReleases(ctx context.Context, since time.Time) ([]store.ReleaseSummary, error) {
	usageStore, ok := s.modules.(store.ReleaseUsageStore)
	if !ok {
		return nil, nil
	}
	if err := usageStore.PruneReleaseUsageBefore(ctx, since); err != nil {
		return nil, err
	}
	return usageStore.ListActiveReleases(ctx, since)
}

func (s *ModuleService) GetModule(ctx context.Context, owner, name string) (domain.Module, error) {
	return s.modules.GetModule(ctx, owner, name)
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
		if s.upstream != nil && (release.Readme == "" || release.UpstreamFileURI == "" || release.Description == "") {
			slug := release.UpstreamSlug
			if slug == "" {
				slug = owner + "-" + name + "-" + version
			}

			upstreamRelease, err := s.upstream.FetchRelease(ctx, slug)
			if err == nil {
				release.Description = fallbackString(upstreamRelease.Description, release.Description)
				release.Readme = fallbackString(upstreamRelease.Readme, release.Readme)
				release.UpstreamSlug = fallbackString(upstreamRelease.Slug, release.UpstreamSlug)
				release.UpstreamFileURI = fallbackString(upstreamRelease.FileURI, release.UpstreamFileURI)
				release.FileName = fallbackString(upstreamRelease.FileName, release.FileName)
				release.SHA256 = fallbackString(upstreamRelease.FileSHA256, release.SHA256)
				if _, saveErr := s.modules.CreateRelease(ctx, release); saveErr == nil {
					release, _ = s.modules.GetRelease(ctx, owner, name, version)
				}
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
		return domain.Release{}, store.ErrNotFound
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
	saved, err := s.modules.CreateRelease(ctx, release)
	if err != nil {
		return domain.Release{}, err
	}
	saved.DownloadURL = saved.UpstreamFileURI
	return saved, nil
}

func (s *ModuleService) ReadReleaseFile(ctx context.Context, owner, name, version, filePath string) (storage.Object, error) {
	object, err := s.ReadReleaseArchive(ctx, owner, name, version)
	if err != nil {
		return storage.Object{}, err
	}

	body, contentType, err := extractArchiveFile(object.Body, filePath)
	if err != nil {
		return storage.Object{}, err
	}

	return storage.Object{
		Body:        body,
		ContentType: contentType,
	}, nil
}

func (s *ModuleService) ReadReleaseArchive(ctx context.Context, owner, name, version string) (storage.Object, error) {
	release, err := s.GetRelease(ctx, owner, name, version)
	if err != nil {
		return storage.Object{}, err
	}

	archivePath := release.StoragePath
	if release.Source == "upstream" {
		archivePath = cachedUpstreamArtifactPath(release.UpstreamFileURI)
	}
	if archivePath == "" {
		return storage.Object{}, store.ErrNotFound
	}

	object, err := s.artifacts.Download(ctx, archivePath)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotFound) {
			return storage.Object{}, store.ErrNotFound
		}
		return storage.Object{}, fmt.Errorf("download release archive: %w", err)
	}

	return object, nil
}

func (s *ModuleService) Ready(ctx context.Context) error {
	return s.modules.Ping(ctx)
}

func (s *ModuleService) LoadTeamConfigs(ctx context.Context) ([]auth.TeamConfig, error) {
	if s.access == nil {
		return nil, errors.New("access store is not available")
	}
	return s.access.LoadTeamConfigs(ctx)
}

func (s *ModuleService) ReplaceTeamConfigs(ctx context.Context, configs []auth.TeamConfig) error {
	if s.access == nil {
		return errors.New("access store is not available")
	}
	return s.access.ReplaceTeamConfigs(ctx, configs)
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

func (s *ModuleService) RefreshCachedUpstreamModules(ctx context.Context, limit int) error {
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

	slog.Default().Info("starting upstream module refresh cycle", "modules", len(modules), "limit", limit)

	succeeded := 0
	failed := 0
	for _, module := range modules {
		slog.Default().Info("refreshing upstream module", "owner", module.Owner, "name", module.Name)
		if err := s.syncUpstreamModule(ctx, module.Owner, module.Name, "refresh"); err != nil {
			failed++
			slog.Default().Error("upstream refresh failed", "owner", module.Owner, "name", module.Name, "err", err)
			continue
		}
		succeeded++
	}

	slog.Default().Info("finished upstream module refresh cycle", "modules", len(modules))
	metrics.ObserveUpstreamRefresh(start, len(modules), succeeded, failed)

	return nil
}

func (s *ModuleService) IndexUpstreamModule(ctx context.Context, upstreamModule proxy.UpstreamModule) error {
	owner := fallbackString(upstreamModule.Owner, extractOwnerFromSlug(upstreamModule.Slug))
	name := fallbackString(upstreamModule.Name, extractNameFromSlug(upstreamModule.Slug))
	if owner == "" || name == "" {
		return errors.New("invalid upstream module identity")
	}

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

		if _, err := s.modules.CreateRelease(ctx, release); err != nil {
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
		if _, err := s.modules.CreateRelease(ctx, release); err != nil {
			return err
		}
	}

	return nil
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
		SizeBytes:       0,
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

func inspectModuleArchive(archive []byte) (inspectedModuleArchive, error) {
	gzipReader, err := gzip.NewReader(bytes.NewReader(archive))
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
	entryCount := 0
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			return info, nil
		}
		if err != nil {
			return inspectedModuleArchive{}, err
		}
		entryCount++
		if err := enforceArchiveEntryLimit(entryCount, maxArchiveEntries); err != nil {
			return inspectedModuleArchive{}, err
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		if header.Size > maxArchiveEntrySize {
			return inspectedModuleArchive{}, fmt.Errorf("archive entry %q exceeds maximum size of %d bytes", header.Name, maxArchiveEntrySize)
		}

		base := strings.ToLower(filepath.Base(header.Name))
		switch {
		case isReadmeFile(base) && info.Readme == "":
			body, err := readLimitedArchiveEntry(tarReader, header, maxArchiveReadmeSize)
			if err != nil {
				return inspectedModuleArchive{}, err
			}
			info.Readme = string(body)
		case base == "metadata.json":
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

func extractArchiveFile(archive []byte, filePath string) ([]byte, string, error) {
	cleanPath := path.Clean(strings.TrimPrefix(filePath, "/"))
	if cleanPath == "." || cleanPath == "" || strings.HasPrefix(cleanPath, "../") {
		return nil, "", errors.New("invalid file path")
	}

	gzipReader, err := gzip.NewReader(bytes.NewReader(archive))
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
// slugPattern, so nested owner/name/path identities are intentionally rejected.
func splitModuleIdentity(raw string) (owner, name string) {
	if left, right, ok := strings.Cut(raw, "-"); ok {
		return left, right
	}
	if left, right, ok := strings.Cut(raw, "/"); ok {
		return left, right
	}
	return "", raw
}

func mergeMetadata(base, override map[string]any) map[string]any {
	if len(base) == 0 && len(override) == 0 {
		return map[string]any{}
	}

	merged := make(map[string]any, len(base)+len(override))
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range override {
		merged[key] = value
	}
	return merged
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

func extractOwnerFromSlug(slug string) string {
	parts := strings.SplitN(slug, "-", 2)
	if len(parts) != 2 {
		return ""
	}
	return parts[0]
}

func extractNameFromSlug(slug string) string {
	parts := strings.SplitN(slug, "-", 2)
	if len(parts) != 2 {
		return ""
	}
	return parts[1]
}
