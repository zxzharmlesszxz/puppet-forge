package service

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

func TestControlOperationResult(t *testing.T) {
	for _, err := range []error{ErrValidation, ErrModerationForbidden, ErrDeleteForbidden, ErrCacheClearForbidden, ErrReviewForbidden, ErrUnavailable, ErrReviewBusy, ErrReviewLimit, ErrProtectedDelete, store.ErrConflict, store.ErrNotFound, context.Canceled} {
		if got := controlOperationResult(fmt.Errorf("wrapped: %w", err)); got != "rejected" {
			t.Errorf("%v classified as %q", err, got)
		}
	}
	if got := controlOperationResult(nil); got != "success" {
		t.Fatal(got)
	}
	for _, err := range []error{errors.New("SQL unavailable"), context.DeadlineExceeded} {
		if got := controlOperationResult(err); got != "error" {
			t.Fatalf("internal failure classified as %q", got)
		}
	}
}
