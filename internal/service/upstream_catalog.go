package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
	"github.com/zxzharmlesszxz/puppet-forge/internal/httputil"
	"github.com/zxzharmlesszxz/puppet-forge/internal/proxy"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

const upstreamCatalogTimeout = 20 * time.Second
const upstreamCatalogLeaseTTL = time.Minute
const maxUpstreamCatalogVersions = 10000

type catalogLeaseStore interface {
	AcquireLease(context.Context, string, string, time.Duration) (bool, error)
	ReleaseLease(context.Context, string, string) error
}

func upstreamCatalogVersions(module proxy.UpstreamModule) ([]string, error) {
	if !domain.ValidModuleOwner(module.Owner) {
		return nil, errors.New("invalid upstream module owner")
	}
	if !domain.ValidModuleName(module.Name) {
		return nil, errors.New("invalid upstream module name")
	}
	slug := module.Owner + "-" + module.Name
	if module.Slug != "" && module.Slug != slug {
		return nil, errors.New("upstream module slug does not match identity")
	}
	if len(module.Releases) > maxUpstreamCatalogVersions {
		return nil, errors.New("upstream module catalog exceeds release limit")
	}
	refs := append([]proxy.UpstreamReleaseRef(nil), module.Releases...)
	if module.CurrentRelease.Version != "" || module.CurrentRelease.Slug != "" {
		refs = append(refs, module.CurrentRelease)
	}
	versions := make([]string, 0, len(refs))
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		version := ref.Version
		fromSlug := httputil.ReleaseVersionFromSlug(slug, ref.Slug)
		if version == "" {
			version = fromSlug
		}
		if !domain.ValidModuleVersion(version) || ref.Slug != "" && fromSlug != version {
			return nil, fmt.Errorf("invalid upstream release reference for %s", slug)
		}
		if !seen[version] {
			seen[version] = true
			versions = append(versions, version)
		}
	}
	if len(versions) > maxUpstreamCatalogVersions {
		return nil, errors.New("upstream module catalog exceeds release limit")
	}
	return versions, nil
}

func (s *ModuleService) requireUpstreamOrigin(ctx context.Context, owner, name string) error {
	if origins, ok := s.modules.(store.LocalModuleOriginStore); ok {
		local, err := origins.HasLocalModuleOrigin(ctx, owner, name)
		if err != nil {
			return err
		}
		if local {
			return store.ErrNotFound
		}
	}
	return nil
}

func (s *ModuleService) upstreamCatalogReady(ctx context.Context, backend store.UpstreamCatalogStore, owner, name string) (bool, error) {
	if err := s.requireUpstreamOrigin(ctx, owner, name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return true, nil
		}
		return false, err
	}
	status, err := s.ResourceStatus(ctx, owner, name, "")
	if err != nil {
		return false, err
	}
	if !status.State.Readable() {
		return true, nil
	}
	indexed, err := backend.HasUpstreamCatalog(ctx, owner, name)
	if err != nil || indexed {
		return indexed, err
	}
	if _, err := s.modules.GetModule(ctx, owner, name); errors.Is(err, store.ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	origins, ok := s.modules.(store.ModuleOriginStore)
	if !ok {
		return false, errors.New("module origin store is not available")
	}
	upstream, err := origins.HasUpstreamReleases(ctx, owner, name)
	return !upstream, err
}

// EnsureUpstreamCatalog indexes an exact module filter independently of version/offset results.
func (s *ModuleService) EnsureUpstreamCatalog(ctx context.Context, filter string) error {
	parts := strings.SplitN(filter, "-", 2)
	if s.upstream == nil || len(parts) != 2 || !domain.ValidModuleOwner(parts[0]) || !domain.ValidModuleName(parts[1]) {
		return nil
	}
	backend, ok := s.modules.(store.UpstreamCatalogStore)
	if !ok {
		return errors.New("upstream catalog store is not available")
	}
	leases, ok := s.modules.(catalogLeaseStore)
	if !ok {
		return errors.New("catalog lease store is not available")
	}
	ctx, cancel := context.WithTimeout(ctx, upstreamCatalogTimeout)
	defer cancel()
	owner, name := parts[0], parts[1]
	digest := sha256.Sum256([]byte(owner + "/" + name))
	lease := store.CatalogLease{Name: fmt.Sprintf("upstream-catalog-%x", digest), Holder: rand.Text()}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		ready, err := s.upstreamCatalogReady(ctx, backend, owner, name)
		if err != nil || ready {
			return err
		}
		acquired, err := leases.AcquireLease(ctx, lease.Name, lease.Holder, upstreamCatalogLeaseTTL)
		if err != nil {
			return err
		}
		if acquired {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := leases.ReleaseLease(releaseCtx, lease.Name, lease.Holder); err != nil {
			slog.Warn("release upstream catalog lease", "owner", owner, "name", name, "err", err)
		}
	}()
	ready, err := s.upstreamCatalogReady(ctx, backend, owner, name)
	if err != nil || ready {
		return err
	}
	module, err := s.upstream.FetchModule(ctx, filter)
	if errors.Is(err, proxy.ErrUpstreamNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: fetch catalog %s: %w", ErrUpstreamHydration, filter, err)
	}
	if module.Owner != owner || module.Name != name {
		return fmt.Errorf("%w: upstream catalog identity mismatch", ErrUpstreamHydration)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Publication/moderation use the normal module lock, never the network lease.
	if err := s.indexUpstreamCatalog(ctx, module, lease); err != nil {
		return fmt.Errorf("%w: index catalog %s: %w", ErrUpstreamHydration, filter, err)
	}
	return nil
}
