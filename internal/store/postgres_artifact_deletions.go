package store

import (
	"context"
	"fmt"
	"time"
)

func (s *PostgresStore) ListArtifactDeletions(ctx context.Context, limit int) ([]ArtifactDeletion, error) {
	rows, err := s.pool.Query(ctx, `
		select storage_path, owner, name, created_at
		from artifact_deletions
		where next_attempt_at <= now()
		order by next_attempt_at, created_at, storage_path
		limit $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list artifact deletions: %w", err)
	}
	defer rows.Close()

	deletions := make([]ArtifactDeletion, 0)
	for rows.Next() {
		var deletion ArtifactDeletion
		if err := rows.Scan(&deletion.StoragePath, &deletion.Owner, &deletion.Name, &deletion.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan artifact deletion: %w", err)
		}
		deletions = append(deletions, deletion)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read artifact deletions: %w", err)
	}
	return deletions, nil
}

func (s *PostgresStore) IsArtifactReferenced(ctx context.Context, storagePath string) (bool, error) {
	var referenced bool
	if err := s.pool.QueryRow(ctx, `
		select exists(select 1 from releases where storage_path = $1)
	`, storagePath).Scan(&referenced); err != nil {
		return false, fmt.Errorf("check artifact reference: %w", err)
	}
	return referenced, nil
}

func (s *PostgresStore) CountArtifactDeletions(ctx context.Context) (int, error) {
	var count int
	if err := s.pool.QueryRow(ctx, `select count(*) from artifact_deletions`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count artifact deletions: %w", err)
	}
	return count, nil
}

func (s *PostgresStore) DeferArtifactDeletion(ctx context.Context, storagePath string, retryAt time.Time) error {
	if _, err := s.pool.Exec(ctx, `
		update artifact_deletions
		set next_attempt_at = $1
		where storage_path = $2
	`, retryAt, storagePath); err != nil {
		return fmt.Errorf("defer artifact deletion: %w", err)
	}
	return nil
}

func (s *PostgresStore) CompleteArtifactDeletion(ctx context.Context, storagePath string) error {
	if _, err := s.pool.Exec(ctx, `delete from artifact_deletions where storage_path = $1`, storagePath); err != nil {
		return fmt.Errorf("complete artifact deletion: %w", err)
	}
	return nil
}
