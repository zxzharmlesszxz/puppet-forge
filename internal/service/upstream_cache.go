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

type UpstreamCachePruneResult struct {
	Scanned int
	Deleted int
	Failed  int
}

func (s *ModuleService) PruneUpstreamArtifactCache(ctx context.Context, cutoff time.Time) (UpstreamCachePruneResult, error) {
	releaseStore, ok := s.modules.(store.ArtifactReleaseStore)
	if !ok {
		return UpstreamCachePruneResult{}, errors.New("module store does not support artifact references")
	}
	lister, ok := s.artifacts.(storage.ArtifactMetadataIterator)
	if !ok {
		return UpstreamCachePruneResult{}, errors.New("artifact storage does not support object metadata iteration")
	}
	result := UpstreamCachePruneResult{}
	var deleteErrors []error
	err := lister.IterateObjectMetadata(ctx, upstreamArtifactCachePrefix, func(object storage.ListedObject) error {
		result.Scanned++
		if object.UpdatedAt.IsZero() || !object.UpdatedAt.Before(cutoff) {
			return nil
		}
		referenced, err := releaseStore.IsArtifactPathReferenced(ctx, object.Path)
		if err != nil {
			result.Failed++
			deleteErrors = append(deleteErrors, fmt.Errorf("check upstream cache object %q reference: %w", object.Path, err))
			return nil
		}
		if referenced {
			return nil
		}
		if err := s.artifacts.Delete(ctx, object.Path); err != nil {
			result.Failed++
			deleteErrors = append(deleteErrors, fmt.Errorf("delete orphan upstream cache object %q: %w", object.Path, err))
			return nil
		}
		result.Deleted++
		return nil
	})
	if err != nil {
		deleteErrors = append(deleteErrors, fmt.Errorf("list upstream cache objects: %w", err))
	}
	return result, errors.Join(deleteErrors...)
}
