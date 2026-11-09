package service

import (
	"context"
	"errors"

	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
	"github.com/zxzharmlesszxz/puppet-forge/internal/metrics"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

func (s *ModuleService) ListResourceMetricSummaries(ctx context.Context) ([]domain.ResourceMetricSummary, error) {
	backend, ok := s.modules.(store.ResourceMetricStore)
	if !ok {
		return nil, errors.New("resource metrics store is not available")
	}
	return backend.ListResourceMetricSummaries(ctx)
}

func observeControlOperation(operation, version string, err error) {
	kind := "release"
	if version == "" {
		kind = "module"
	}
	metrics.ObserveControlOperation(operation, kind, controlOperationResult(err))
}

func controlOperationObserver(operation, version string) func(error) {
	return func(err error) { observeControlOperation(operation, version, err) }
}

func controlOperationResult(err error) string {
	result := "success"
	if err != nil {
		result = "error"
		if errors.Is(err, ErrValidation) || errors.Is(err, ErrModerationForbidden) || errors.Is(err, ErrDeleteForbidden) || errors.Is(err, ErrCacheClearForbidden) || errors.Is(err, ErrReviewForbidden) || errors.Is(err, ErrUnavailable) || errors.Is(err, ErrReviewBusy) || errors.Is(err, ErrReviewLimit) || errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) || errors.Is(err, ErrProtectedDelete) || errors.Is(err, context.Canceled) {
			result = "rejected"
		}
	}
	return result
}
