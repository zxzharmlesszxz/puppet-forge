package service

import (
	"context"
	"errors"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

var ErrDeleteForbidden = errors.New("only global administrators may delete upstream resources; local resources require team administration")

func (s *ModuleService) CanDeleteModule(ctx context.Context, principal auth.Principal, owner, name string) (bool, error) {
	if principal.CanAdmin {
		return true, nil
	}
	if !principal.CanDeleteOwner(owner) {
		return false, nil
	}
	if origins, ok := s.modules.(store.ModuleOriginStore); ok {
		upstream, err := origins.HasUpstreamReleases(ctx, owner, name)
		return !upstream && err == nil, err
	}
	versions, err := s.modules.ListReleases(ctx, owner, name)
	if err != nil {
		return false, err
	}
	for _, version := range versions {
		release, err := s.modules.GetRelease(ctx, owner, name, version.Version)
		if err != nil {
			return false, err
		}
		if release.Source == "upstream" {
			return false, nil
		}
	}
	return true, nil
}

// DeleteAuthorized rechecks origin under the publication lock before deleting a resource.
func (s *ModuleService) DeleteAuthorized(ctx context.Context, principal auth.Principal, owner, name, version string, force bool, activeSince time.Time) error {
	if !principal.CanDeleteOwner(owner) {
		return ErrDeleteForbidden
	}
	unlock, err := s.modules.LockModule(ctx, owner, name)
	if err != nil {
		return err
	}
	defer releaseModuleLock(unlock, owner, name)
	allowed, err := s.CanDeleteModule(ctx, principal, owner, name)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrDeleteForbidden
	}
	if !force {
		return s.deleteUnusedLocked(ctx, owner, name, version, activeSince)
	}
	if version == "" {
		return s.deleteModuleLocked(ctx, owner, name)
	}
	return s.deleteReleaseLocked(ctx, owner, name, version)
}

func (s *ModuleService) deleteUnusedLocked(ctx context.Context, owner, name, version string, activeSince time.Time) error {
	versions := []domain.ModuleVersion{{Version: version}}
	if version == "" {
		var err error
		versions, err = s.modules.ListReleases(ctx, owner, name)
		if err != nil {
			return err
		}
	}
	for _, candidate := range versions {
		active, err := s.isReleaseActive(ctx, owner, name, candidate.Version, activeSince)
		if err != nil {
			return err
		}
		if active {
			prefix := "active release "
			if version == "" {
				prefix = "module contains active release "
			}
			return protectedDeleteError{message: prefix + owner + "/" + name + " " + candidate.Version + " cannot be deleted"}
		}
	}
	if version == "" {
		return s.deleteModuleLocked(ctx, owner, name)
	}
	return s.deleteReleaseLocked(ctx, owner, name, version)
}
