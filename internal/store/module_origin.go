package store

import "context"

type ModuleOriginStore interface {
	HasUpstreamReleases(ctx context.Context, owner, name string) (bool, error)
}

func (s *SQLiteStore) HasUpstreamReleases(ctx context.Context, owner, name string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `select exists(select 1 from releases r join modules m on m.id=r.module_id where m.owner=? and m.name=? and r.source='upstream')`, owner, name).Scan(&exists)
	return exists, err
}

func (s *PostgresStore) HasUpstreamReleases(ctx context.Context, owner, name string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `select exists(select 1 from releases r join modules m on m.id=r.module_id where m.owner=$1 and m.name=$2 and r.source='upstream')`, owner, name).Scan(&exists)
	return exists, err
}
