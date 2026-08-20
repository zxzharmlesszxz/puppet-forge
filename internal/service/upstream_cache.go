package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/storage"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

const upstreamArtifactCachePrefix = "upstream-cache/"
const upstreamCacheCleanupErrorLimit = 20

type UpstreamCachePruneResult struct {
	Scanned int
	Deleted int
	Failed  int
}

func (s *ModuleService) PruneUpstreamArtifactCache(ctx context.Context, cutoff time.Time) (UpstreamCachePruneResult, error) {
	lister, ok := s.artifacts.(storage.ArtifactMetadataIterator)
	if !ok {
		return UpstreamCachePruneResult{}, errors.New("artifact storage does not support object metadata iteration")
	}
	referenceStore, ok := s.modules.(store.ArtifactDeletionStore)
	if !ok {
		return UpstreamCachePruneResult{}, errors.New("module store does not support artifact reference checks")
	}
	result := UpstreamCachePruneResult{}
	var deleteErrors []error
	omittedErrors := 0
	recordDeleteError := func(err error) {
		if len(deleteErrors) < upstreamCacheCleanupErrorLimit {
			deleteErrors = append(deleteErrors, err)
			return
		}
		omittedErrors++
	}
	err := lister.IterateObjectMetadata(ctx, upstreamArtifactCachePrefix, func(object storage.ListedObject) error {
		result.Scanned++
		if object.UpdatedAt.IsZero() || !object.UpdatedAt.Before(cutoff) {
			return nil
		}
		deleted := false
		deleteObject := func(deleteCtx context.Context) error {
			referenced, err := referenceStore.IsArtifactReferenced(deleteCtx, object.Path)
			if err != nil {
				return fmt.Errorf("recheck orphan upstream cache object %q: %w", object.Path, err)
			}
			if referenced {
				return nil
			}
			if err := s.artifacts.Delete(deleteCtx, object.Path); err != nil {
				return err
			}
			deleted = true
			return nil
		}
		var deleteErr error
		if s.upstream != nil {
			deleteErr = s.upstream.WithCachedArtifactLease(ctx, object.Path, deleteObject)
		} else {
			deleteErr = deleteObject(ctx)
		}
		if deleteErr != nil {
			result.Failed++
			recordDeleteError(fmt.Errorf("delete orphan upstream cache object %q: %w", object.Path, deleteErr))
			return nil
		}
		if deleted {
			result.Deleted++
		}
		return nil
	})
	if err != nil {
		recordDeleteError(fmt.Errorf("list upstream cache objects: %w", err))
	}
	if omittedErrors > 0 {
		deleteErrors = append(deleteErrors, fmt.Errorf("%d additional upstream cache cleanup errors omitted", omittedErrors))
	}
	return result, errors.Join(deleteErrors...)
}
