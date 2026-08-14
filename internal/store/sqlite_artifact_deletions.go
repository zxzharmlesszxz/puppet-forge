package store

import (
	"context"
	"fmt"
	"time"
)

func (s *SQLiteStore) ListArtifactDeletions(ctx context.Context, limit int) ([]ArtifactDeletion, error) {
	rows, err := s.db.QueryContext(ctx, `
		select storage_path, owner, name, created_at
		from artifact_deletions
		where next_attempt_at <= current_timestamp
		order by next_attempt_at, created_at, storage_path
		limit ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list artifact deletions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	deletions := make([]ArtifactDeletion, 0)
	for rows.Next() {
		var deletion ArtifactDeletion
		var createdAt sqliteTimestamp
		if err := rows.Scan(&deletion.StoragePath, &deletion.Owner, &deletion.Name, &createdAt); err != nil {
			return nil, fmt.Errorf("scan artifact deletion: %w", err)
		}
		deletion.CreatedAt = createdAt.Time
		deletions = append(deletions, deletion)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read artifact deletions: %w", err)
	}
	return deletions, nil
}

func (s *SQLiteStore) IsArtifactReferenced(ctx context.Context, storagePath string) (bool, error) {
	var referenced bool
	if err := s.db.QueryRowContext(ctx, `
		select exists(select 1 from releases where storage_path = ?)
	`, storagePath).Scan(&referenced); err != nil {
		return false, fmt.Errorf("check artifact reference: %w", err)
	}
	return referenced, nil
}

func (s *SQLiteStore) CountArtifactDeletions(ctx context.Context) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `select count(*) from artifact_deletions`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count artifact deletions: %w", err)
	}
	return count, nil
}

func (s *SQLiteStore) DeferArtifactDeletion(ctx context.Context, storagePath string, retryAt time.Time) error {
	if _, err := s.db.ExecContext(ctx, `
		update artifact_deletions
		set next_attempt_at = ?
		where storage_path = ?
	`, sqliteTime(retryAt.UTC()), storagePath); err != nil {
		return fmt.Errorf("defer artifact deletion: %w", err)
	}
	return nil
}

func (s *SQLiteStore) CompleteArtifactDeletion(ctx context.Context, storagePath string) error {
	if _, err := s.db.ExecContext(ctx, `delete from artifact_deletions where storage_path = ?`, storagePath); err != nil {
		return fmt.Errorf("complete artifact deletion: %w", err)
	}
	return nil
}
