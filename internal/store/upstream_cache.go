package store

import (
	"context"
	"fmt"
)

type UpstreamCacheStore interface {
	// ClearUpstreamArtifactCache requires the module lock and retains catalog/lifecycle data.
	ClearUpstreamArtifactCache(context.Context, string, string) error
}

type upstreamCacheMutationWriter interface {
	moduleID(string, string) (string, error)
	localModule(string, string) (bool, error)
	queueUpstreamCache(string, string, string) error
	detachUpstreamCache(string) error
	commit() error
}

func clearUpstreamArtifactCache(owner, name string, writer upstreamCacheMutationWriter) error {
	moduleID, err := writer.moduleID(owner, name)
	if err != nil {
		return err
	}
	local, err := writer.localModule(owner, name)
	if err != nil {
		return err
	}
	if local {
		return fmt.Errorf("%w: local module archives are not a cache", ErrConflict)
	}
	if err := writer.queueUpstreamCache(moduleID, owner, name); err != nil {
		return err
	}
	if err := writer.detachUpstreamCache(moduleID); err != nil {
		return err
	}
	return writer.commit()
}

func (s *SQLiteStore) ClearUpstreamArtifactCache(ctx context.Context, owner, name string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	return clearUpstreamArtifactCache(owner, name, sqliteMutationWriter{ctx: ctx, tx: tx})
}

func (s *PostgresStore) ClearUpstreamArtifactCache(ctx context.Context, owner, name string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return clearUpstreamArtifactCache(owner, name, postgresMutationWriter{ctx: ctx, tx: tx})
}

func (w sqliteMutationWriter) queueUpstreamCache(moduleID, owner, name string) error {
	_, err := w.tx.ExecContext(w.ctx, `insert into artifact_deletions(storage_path,owner,name)
 select distinct storage_path,?,? from releases
 where module_id=? and source='upstream'
 and substr(storage_path,1,length('upstream-cache/'))='upstream-cache/' and storage_path not like '%..%'
 on conflict(storage_path) do update set next_attempt_at=current_timestamp`, owner, name, moduleID)
	return err
}

func (w postgresMutationWriter) queueUpstreamCache(moduleID, owner, name string) error {
	_, err := w.tx.Exec(w.ctx, `insert into artifact_deletions(storage_path,owner,name)
 select distinct storage_path,$1,$2 from releases
 where module_id=$3 and source='upstream'
 and substr(storage_path,1,length('upstream-cache/'))='upstream-cache/' and storage_path not like '%..%'
 on conflict(storage_path) do update set next_attempt_at=now()`, owner, name, moduleID)
	return err
}

func (w sqliteMutationWriter) detachUpstreamCache(moduleID string) error {
	_, err := w.tx.ExecContext(w.ctx, `update releases set storage_path=''
 where module_id=? and source='upstream'
 and substr(storage_path,1,length('upstream-cache/'))='upstream-cache/' and storage_path not like '%..%'`, moduleID)
	return err
}

func (w postgresMutationWriter) detachUpstreamCache(moduleID string) error {
	_, err := w.tx.Exec(w.ctx, `update releases set storage_path=''
 where module_id=$1 and source='upstream'
 and substr(storage_path,1,length('upstream-cache/'))='upstream-cache/' and storage_path not like '%..%'`, moduleID)
	return err
}
