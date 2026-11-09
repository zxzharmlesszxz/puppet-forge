package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
	"github.com/zxzharmlesszxz/puppet-forge/internal/metrics"
	"github.com/zxzharmlesszxz/puppet-forge/internal/storage"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

var ErrUnavailable = errors.New("module or release is not available")
var ErrModerationForbidden = errors.New("moderation requires namespace administration; upstream resources require global administration")

func (s *ModuleService) ResourceStatus(ctx context.Context, owner, name, version string) (domain.ResourceStatus, error) {
	states, ok := s.modules.(store.LifecycleStore)
	if !ok {
		return domain.ResourceStatus{}, errors.New("lifecycle store is not available")
	}
	return states.GetResourceStatus(ctx, owner, name, version)
}

func (s *ModuleService) RequireAvailable(ctx context.Context, owner, name, version string) error {
	for _, key := range []string{"", version} {
		status, err := s.ResourceStatus(ctx, owner, name, key)
		if err != nil {
			return err
		}
		if !status.State.Readable() {
			return fmt.Errorf("%w: %s/%s %s is %s", ErrUnavailable, owner, name, key, status.State)
		}
		if key == "" && version == "" {
			break
		}
	}
	return nil
}

// Moderate serializes decisions with publication and binds approval to the reviewed bytes.
func (s *ModuleService) Moderate(ctx context.Context, principal auth.Principal, owner, name, version, action, reason, replacement, expectedSHA string) (err error) {
	operation := "other"
	switch action {
	case "approve", "deprecate", "restore", "block", "unblock":
		operation = action
	}
	defer metrics.ObserveOutcome(&err, controlOperationObserver(operation, version))
	if !principal.CanDeleteOwner(owner) {
		return ErrModerationForbidden
	}
	if strings.TrimSpace(reason) == "" || len(reason) > 500 || len(replacement) > 500 {
		return invalidInput("a reason of at most 500 characters is required")
	}
	states, ok := s.modules.(store.LifecycleStore)
	if !ok {
		return errors.New("lifecycle store is not available")
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
		return ErrModerationForbidden
	}
	status, err := states.GetResourceStatus(ctx, owner, name, version)
	if err != nil {
		return err
	}
	_, moduleErr := s.modules.GetModule(ctx, owner, name)
	if moduleErr != nil && (!errors.Is(moduleErr, store.ErrNotFound) || action != "unblock" || status.State != domain.StateBlocked) {
		return moduleErr
	}
	missing := moduleErr != nil
	if version != "" {
		release, err := s.modules.GetRelease(ctx, owner, name, version)
		if err != nil && (!errors.Is(err, store.ErrNotFound) || action != "unblock" || status.State != domain.StateBlocked) {
			return err
		}
		missing = err != nil
		if !missing && ((action == "approve" && expectedSHA == "") || expectedSHA != release.SHA256) {
			return fmt.Errorf("%w: release content changed; review again", store.ErrConflict)
		}
		status.SHA256 = release.SHA256
	}
	previous := status.State
	switch action {
	case "approve":
		if version == "" || status.State != domain.StatePending {
			return invalidInput("only a pending release can be approved")
		}
		status.State = domain.StateAvailable
	case "deprecate":
		if !status.State.Readable() {
			return invalidInput("only an available release or module can be deprecated")
		}
		status.State = domain.StateDeprecated
	case "restore":
		if status.State != domain.StateDeprecated {
			return invalidInput("only a deprecated resource can be restored")
		}
		status.State = domain.StateAvailable
	case "block":
		if status.State == domain.StateBlocked || status.State == domain.StateDeleted {
			return invalidInput("resource is already blocked or deleted")
		}
		status.Previous = status.State
		status.State = domain.StateBlocked
	case "unblock":
		if status.State != domain.StateBlocked {
			return invalidInput("resource is not blocked")
		}
		status.State = status.Previous
		if status.State == "" || missing {
			status.State = domain.StateAvailable
		}
		status.Previous = ""
	default:
		return invalidInput("unknown moderation action")
	}
	status.Reason = reason
	status.Replacement = replacement
	status.ChangedAt = time.Now().UTC()
	store.EnrichMutationAudit(ctx, map[string]string{"space": owner, "module": owner + "/" + name, "release": version, "from_state": string(previous), "to_state": string(status.State), "sha256": status.SHA256, "replacement": replacement})
	return states.SetResourceStatus(ctx, owner, name, version, status)
}

// ReviewArchive is deliberately separate from normal download paths. Its caller
// must enforce team administration and must never expose a storage URL.
func (s *ModuleService) ReviewArchive(ctx context.Context, owner, name, version string) (object storage.ObjectReader, err error) {
	defer metrics.ObserveOutcome(&err, controlOperationObserver("review_download", version))
	unlock, err := s.modules.LockModule(ctx, owner, name)
	if err != nil {
		return storage.ObjectReader{}, err
	}
	defer releaseModuleLock(unlock, owner, name)
	status, err := s.ResourceStatus(ctx, owner, name, version)
	if err != nil {
		return storage.ObjectReader{}, err
	}
	moduleStatus, err := s.ResourceStatus(ctx, owner, name, "")
	if err != nil {
		return storage.ObjectReader{}, err
	}
	if status.State != domain.StatePending || moduleStatus.State == domain.StateBlocked || moduleStatus.State == domain.StateDeleted {
		return storage.ObjectReader{}, ErrUnavailable
	}
	release, err := s.modules.GetRelease(ctx, owner, name, version)
	if err != nil {
		return storage.ObjectReader{}, err
	}
	return s.artifacts.Open(ctx, releaseArchivePath(release))
}
