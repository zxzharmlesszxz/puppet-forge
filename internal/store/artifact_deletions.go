package store

import (
	"context"
	"time"
)

type ArtifactDeletion struct {
	StoragePath string
	Owner       string
	Name        string
	CreatedAt   time.Time
}

type ArtifactDeletionStore interface {
	ListArtifactDeletions(ctx context.Context, limit int) ([]ArtifactDeletion, error)
	CountArtifactDeletions(ctx context.Context) (int, error)
	IsArtifactReferenced(ctx context.Context, storagePath string) (bool, error)
	DeferArtifactDeletion(ctx context.Context, storagePath string, retryAt time.Time) error
	CompleteArtifactDeletion(ctx context.Context, storagePath string) error
}
