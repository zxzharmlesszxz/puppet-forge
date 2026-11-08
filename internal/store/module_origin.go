package store

import "context"

const moduleOriginSchema = `create table if not exists module_origins (
 owner text not null, name text not null, source text not null check(source in ('local','upstream')),
 primary key(owner,name,source)
);
insert into module_origins(owner,name,source)
 select distinct m.owner,m.name,r.source from modules m join releases r on r.module_id=m.id
 where r.source in ('local','upstream') on conflict(owner,name,source) do nothing;
insert into module_origins(owner,name,source)
 select owner,name,'upstream' from upstream_catalogs where true on conflict(owner,name,source) do nothing;`

type LocalModuleOriginStore interface {
	HasLocalModuleOrigin(context.Context, string, string) (bool, error)
}

func (s *SQLiteStore) HasLocalModuleOrigin(ctx context.Context, owner, name string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `select exists(select 1 from module_origins where owner=? and name=? and source='local')`, owner, name).Scan(&exists)
	return exists, err
}

func (s *PostgresStore) HasLocalModuleOrigin(ctx context.Context, owner, name string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `select exists(select 1 from module_origins where owner=$1 and name=$2 and source='local')`, owner, name).Scan(&exists)
	return exists, err
}

type ModuleOriginStore interface {
	HasUpstreamReleases(ctx context.Context, owner, name string) (bool, error)
}

func (s *SQLiteStore) HasUpstreamReleases(ctx context.Context, owner, name string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `select exists(select 1 from module_origins where owner=? and name=? and source='upstream') or exists(select 1 from upstream_catalogs where owner=? and name=?)`, owner, name, owner, name).Scan(&exists)
	return exists, err
}

func (s *PostgresStore) HasUpstreamReleases(ctx context.Context, owner, name string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `select exists(select 1 from module_origins where owner=$1 and name=$2 and source='upstream') or exists(select 1 from upstream_catalogs where owner=$1 and name=$2)`, owner, name).Scan(&exists)
	return exists, err
}
