package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
)

const upstreamCatalogSchema = `create table if not exists upstream_catalogs (
 owner text not null, name text not null, indexed_at text not null,
 primary key(owner,name)
);`

// CatalogLease fences a catalog write against an expired or replaced fetch leader.
type CatalogLease struct{ Name, Holder string }

type UpstreamCatalogStore interface {
	HasUpstreamCatalog(context.Context, string, string) (bool, error)
	// SaveUpstreamCatalog requires the module lock. All rows and the completion marker commit together.
	SaveUpstreamCatalog(context.Context, string, string, []string, CatalogLease) error
}

func (s *SQLiteStore) HasUpstreamCatalog(ctx context.Context, owner, name string) (bool, error) {
	var indexed bool
	err := s.db.QueryRowContext(ctx, `select exists(select 1 from upstream_catalogs where owner=? and name=?)`, owner, name).Scan(&indexed)
	return indexed, err
}

func (s *PostgresStore) HasUpstreamCatalog(ctx context.Context, owner, name string) (bool, error) {
	var indexed bool
	err := s.pool.QueryRow(ctx, `select exists(select 1 from upstream_catalogs where owner=$1 and name=$2)`, owner, name).Scan(&indexed)
	return indexed, err
}

type catalogMutationWriter interface {
	releaseMutationWriter
	fenceCatalog(CatalogLease) error
	catalogStatus(string, string) (domain.ResourceStatus, error)
	catalogModule(string, string) (string, error)
	insertCatalogVersion(string, string, string, string) error
	markCatalog(string, string) error
	localModule(string, string) (bool, error)
}

func saveUpstreamCatalog(owner, name string, versions []string, lease CatalogLease, writer catalogMutationWriter) error {
	if !domain.ValidModuleOwner(owner) || !domain.ValidModuleName(name) {
		return fmt.Errorf("invalid upstream catalog identity")
	}
	for _, version := range versions {
		if !domain.ValidModuleVersion(version) {
			return fmt.Errorf("invalid upstream catalog version %q", version)
		}
	}
	if err := writer.fenceCatalog(lease); err != nil {
		return err
	}
	local, err := writer.localModule(owner, name)
	if err != nil || local {
		return err
	}
	status, err := writer.catalogStatus(owner, name)
	if err != nil {
		return err
	}
	if !status.State.Readable() {
		return nil
	}
	moduleID, err := writer.catalogModule(owner, name)
	if err != nil {
		return err
	}
	for _, version := range versions {
		if err := writer.insertCatalogVersion(moduleID, owner, name, version); err != nil {
			return err
		}
	}
	latest, err := writer.currentLatestVersion(moduleID)
	if err != nil {
		return err
	}
	if err := writer.updateLatestVersion(moduleID, latest); err != nil {
		return err
	}
	if err := writer.markCatalog(owner, name); err != nil {
		return err
	}
	return writer.commit()
}

func (w sqliteMutationWriter) localModule(owner, name string) (bool, error) {
	var local bool
	err := w.tx.QueryRowContext(w.ctx, `select exists(select 1 from module_origins where owner=? and name=? and source='local')`, owner, name).Scan(&local)
	return local, err
}

func (w postgresMutationWriter) localModule(owner, name string) (bool, error) {
	var local bool
	err := w.tx.QueryRow(w.ctx, `select exists(select 1 from module_origins where owner=$1 and name=$2 and source='local')`, owner, name).Scan(&local)
	return local, err
}

func (s *SQLiteStore) SaveUpstreamCatalog(ctx context.Context, owner, name string, versions []string, lease CatalogLease) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	return saveUpstreamCatalog(owner, name, versions, lease, sqliteMutationWriter{ctx: ctx, tx: tx})
}

func (s *PostgresStore) SaveUpstreamCatalog(ctx context.Context, owner, name string, versions []string, lease CatalogLease) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return saveUpstreamCatalog(owner, name, versions, lease, postgresMutationWriter{ctx: ctx, tx: tx})
}

func (w sqliteMutationWriter) fenceCatalog(lease CatalogLease) error {
	if lease.Name == "" && lease.Holder == "" {
		return nil
	}
	result, err := w.tx.ExecContext(w.ctx, `update app_leases set holder=holder where name=? and holder=? and julianday(lease_until)>julianday('now')`, lease.Name, lease.Holder)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("%w: upstream catalog lease lost", context.DeadlineExceeded)
	}
	return nil
}

func (w postgresMutationWriter) fenceCatalog(lease CatalogLease) error {
	if lease.Name == "" && lease.Holder == "" {
		return nil
	}
	result, err := w.tx.Exec(w.ctx, `update app_leases set holder=holder where name=$1 and holder=$2 and lease_until>clock_timestamp()`, lease.Name, lease.Holder)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("%w: upstream catalog lease lost", context.DeadlineExceeded)
	}
	return nil
}

func (w sqliteMutationWriter) catalogStatus(owner, name string) (domain.ResourceStatus, error) {
	return scanResourceStatus(w.tx.QueryRowContext(w.ctx, `select state,previous_state,reason,replacement,sha256,changed_at from resource_states where owner=? and name=? and version=''`, owner, name))
}

func (w postgresMutationWriter) catalogStatus(owner, name string) (domain.ResourceStatus, error) {
	return scanResourceStatus(w.tx.QueryRow(w.ctx, `select state,previous_state,reason,replacement,sha256,changed_at from resource_states where owner=$1 and name=$2 and version=''`, owner, name))
}

func (w sqliteMutationWriter) catalogModule(owner, name string) (string, error) {
	if _, err := w.tx.ExecContext(w.ctx, `insert into modules(id,owner,name,slug) values(?,?,?,?) on conflict(owner,name) do nothing`, uuid.NewString(), owner, name, moduleSlug(owner, name)); err != nil {
		return "", err
	}
	return sqliteModuleID(w.ctx, w.tx, owner, name)
}

func (w postgresMutationWriter) catalogModule(owner, name string) (string, error) {
	if _, err := w.tx.Exec(w.ctx, `insert into modules(id,owner,name,slug) values($1,$2,$3,$4) on conflict(owner,name) do nothing`, uuid.NewString(), owner, name, moduleSlug(owner, name)); err != nil {
		return "", err
	}
	return postgresModuleID(w.ctx, w.tx, owner, name)
}

func (w sqliteMutationWriter) insertCatalogVersion(moduleID, owner, name, version string) error {
	slug := releaseSlug(owner, name, version)
	_, err := w.tx.ExecContext(w.ctx, `insert into releases(id,module_id,slug,source,version,file_name,content_type,size_bytes,sha256,storage_path,upstream_slug,upstream_file_uri,version_order)
 select ?,?,?,'upstream',?,?,'application/gzip',0,'','',?,'',?
 where not exists(select 1 from resource_states where owner=? and name=? and version=? and state not in ('available','deprecated'))
 on conflict(module_id,version) do nothing`, uuid.NewString(), moduleID, slug, version, slug+".tar.gz", slug, releaseVersionOrder(version), owner, name, version)
	return err
}

func (w postgresMutationWriter) insertCatalogVersion(moduleID, owner, name, version string) error {
	slug := releaseSlug(owner, name, version)
	_, err := w.tx.Exec(w.ctx, `insert into releases(id,module_id,slug,source,version,file_name,content_type,size_bytes,sha256,storage_path,upstream_slug,upstream_file_uri,version_order)
 select $1,$2,$3,'upstream',$4,$5,'application/gzip',0,'','',$3,'',$6
 where not exists(select 1 from resource_states where owner=$7 and name=$8 and version=$4 and state not in ('available','deprecated'))
 on conflict(module_id,version) do nothing`, uuid.NewString(), moduleID, slug, version, slug+".tar.gz", releaseVersionOrder(version), owner, name)
	return err
}

func (w sqliteMutationWriter) markCatalog(owner, name string) error {
	if _, err := w.tx.ExecContext(w.ctx, `insert into module_origins(owner,name,source) values(?,?,'upstream') on conflict(owner,name,source) do nothing`, owner, name); err != nil {
		return err
	}
	_, err := w.tx.ExecContext(w.ctx, `insert into upstream_catalogs(owner,name,indexed_at) values(?,?,?) on conflict(owner,name) do update set indexed_at=excluded.indexed_at`, owner, name, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (w postgresMutationWriter) markCatalog(owner, name string) error {
	if _, err := w.tx.Exec(w.ctx, `insert into module_origins(owner,name,source) values($1,$2,'upstream') on conflict(owner,name,source) do nothing`, owner, name); err != nil {
		return err
	}
	_, err := w.tx.Exec(w.ctx, `insert into upstream_catalogs(owner,name,indexed_at) values($1,$2,$3) on conflict(owner,name) do update set indexed_at=excluded.indexed_at`, owner, name, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}
