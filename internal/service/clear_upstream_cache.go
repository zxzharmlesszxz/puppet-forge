package service

import (
	"context"
	"errors"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/metrics"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

var ErrCacheClearForbidden = errors.New("only global administrators may clear upstream archive caches")

func (s *ModuleService) CanClearUpstreamCache(ctx context.Context, principal auth.Principal, owner, name string) (bool, error) {
	if !principal.CanAdmin {
		return false, nil
	}
	origins, ok := s.modules.(store.ModuleOriginStore)
	if !ok {
		return false, nil
	}
	localOrigins, ok := s.modules.(store.LocalModuleOriginStore)
	if !ok {
		return false, nil
	}
	local, err := localOrigins.HasLocalModuleOrigin(ctx, owner, name)
	if err != nil || local {
		return false, err
	}
	return origins.HasUpstreamReleases(ctx, owner, name)
}

// ClearUpstreamCache queues registered archive copies without deleting releases or moderation state.
func (s *ModuleService) ClearUpstreamCache(ctx context.Context, principal auth.Principal, owner, name string) (err error) {
	defer metrics.ObserveOutcome(&err, controlOperationObserver("clear_cache", ""))
	if !principal.CanAdmin {
		return ErrCacheClearForbidden
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	unlock, err := s.modules.LockModule(ctx, owner, name)
	if err != nil {
		return err
	}
	defer releaseModuleLock(unlock, owner, name)
	allowed, err := s.CanClearUpstreamCache(ctx, principal, owner, name)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrCacheClearForbidden
	}
	backend, ok := s.modules.(store.UpstreamCacheStore)
	if !ok {
		return errors.New("upstream cache store is not available")
	}
	return backend.ClearUpstreamArtifactCache(ctx, owner, name)
}
