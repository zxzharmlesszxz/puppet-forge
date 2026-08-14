package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/storage"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

type ReconciliationReport struct {
	Checked        int      `json:"checked"`
	MissingObjects []string `json:"missing_objects"`
	CorruptObjects []string `json:"corrupt_objects"`
	OrphanObjects  []string `json:"orphan_objects"`
	DeletedOrphans []string `json:"deleted_orphans,omitempty"`
}

func (s *ModuleService) ReconcileArtifacts(ctx context.Context, repair bool) (ReconciliationReport, error) {
	startedAt := time.Now().UTC()
	releaseStore, ok := s.modules.(store.ArtifactReleaseStore)
	if !ok {
		return ReconciliationReport{}, errors.New("module store does not support artifact reconciliation")
	}
	lister, ok := s.artifacts.(storage.ArtifactMetadataLister)
	if !ok {
		return ReconciliationReport{}, errors.New("artifact storage does not support object metadata listing")
	}
	releases, err := releaseStore.ListArtifactReleases(ctx)
	if err != nil {
		return ReconciliationReport{}, fmt.Errorf("list artifact releases: %w", err)
	}
	prefix := strings.Trim(s.prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	objects, err := lister.ListObjectMetadata(ctx, prefix)
	if err != nil {
		return ReconciliationReport{}, fmt.Errorf("list artifact objects: %w", err)
	}

	report := ReconciliationReport{Checked: len(releases)}
	expected := make(map[string]struct{}, len(releases))
	for _, release := range releases {
		expected[release.StoragePath] = struct{}{}
		object, openErr := s.artifacts.Open(ctx, release.StoragePath)
		if errors.Is(openErr, storage.ErrObjectNotFound) {
			report.MissingObjects = append(report.MissingObjects, release.StoragePath)
			continue
		}
		if openErr != nil {
			return report, fmt.Errorf("open artifact %q: %w", release.StoragePath, openErr)
		}
		hash := sha256.New()
		size, copyErr := io.Copy(hash, object.Body)
		closeErr := object.Body.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return report, fmt.Errorf("verify artifact %q: %w", release.StoragePath, err)
		}
		actualSHA := hex.EncodeToString(hash.Sum(nil))
		if size != release.SizeBytes || (release.SHA256 != "" && !strings.EqualFold(actualSHA, release.SHA256)) {
			report.CorruptObjects = append(report.CorruptObjects, release.StoragePath)
		}
	}
	orphans := make(map[string]storage.ListedObject)
	for _, object := range objects {
		if _, ok := expected[object.Path]; !ok {
			report.OrphanObjects = append(report.OrphanObjects, object.Path)
			orphans[object.Path] = object
		}
	}
	sort.Strings(report.MissingObjects)
	sort.Strings(report.CorruptObjects)
	sort.Strings(report.OrphanObjects)
	if repair {
		deletionStore, ok := s.modules.(store.ArtifactDeletionStore)
		if !ok {
			return report, errors.New("module store does not support artifact reference checks")
		}
		remainingOrphans := make([]string, 0, len(report.OrphanObjects))
		for _, objectPath := range report.OrphanObjects {
			deleted, referenced, err := s.repairOrphanArtifact(ctx, deletionStore, prefix, objectPath, orphans[objectPath], startedAt)
			if err != nil {
				return report, err
			}
			if referenced {
				continue
			}
			if deleted {
				report.DeletedOrphans = append(report.DeletedOrphans, objectPath)
			}
			remainingOrphans = append(remainingOrphans, objectPath)
		}
		report.OrphanObjects = remainingOrphans
	}
	return report, nil
}

func (s *ModuleService) repairOrphanArtifact(ctx context.Context, deletionStore store.ArtifactDeletionStore, prefix, objectPath string, object storage.ListedObject, startedAt time.Time) (bool, bool, error) {
	owner, name, ok := reconciliationObjectIdentity(prefix, objectPath)
	if !ok {
		return false, false, nil
	}
	unlock, err := s.modules.LockModule(ctx, owner, name)
	if err != nil {
		return false, false, fmt.Errorf("lock orphan artifact %q: %w", objectPath, err)
	}
	defer releaseModuleLock(unlock, owner, name)

	referenced, err := deletionStore.IsArtifactReferenced(ctx, objectPath)
	if err != nil {
		return false, false, fmt.Errorf("recheck orphan artifact %q: %w", objectPath, err)
	}
	if referenced {
		return false, true, nil
	}
	if object.UpdatedAt.IsZero() || !object.UpdatedAt.Before(startedAt) {
		return false, false, nil
	}
	if err := s.artifacts.Delete(ctx, objectPath); err != nil {
		return false, false, fmt.Errorf("delete orphan artifact %q: %w", objectPath, err)
	}
	return true, false, nil
}

func reconciliationObjectIdentity(prefix, objectPath string) (string, string, bool) {
	relative := strings.TrimPrefix(objectPath, prefix)
	if relative == objectPath || relative == "" {
		return "", "", false
	}
	parts := strings.Split(relative, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}
