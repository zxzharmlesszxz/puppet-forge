package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
)

const lifecycleSchema = `create table if not exists resource_states (
 owner text not null, name text not null, version text not null,
 state text not null check (state in ('available','pending','deprecated','blocked','deleted')),
 previous_state text not null default '', reason text not null default '',
 replacement text not null default '', sha256 text not null default '', changed_at text not null,
 primary key (owner,name,version)
);`

type readableCatalogKey struct{}

func ReadableCatalogContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, readableCatalogKey{}, true)
}

func IsReadableCatalog(ctx context.Context) bool {
	value, _ := ctx.Value(readableCatalogKey{}).(bool)
	return value
}

const readableModulePredicate = `not exists (select 1 from resource_states s where s.owner=modules.owner and s.name=modules.name and s.version='' and s.state not in ('available','deprecated')) and exists (select 1 from releases r where r.module_id=modules.id and not exists (select 1 from resource_states s where s.owner=modules.owner and s.name=modules.name and s.version=r.version and s.state not in ('available','deprecated')))`

const readableReleaseFilter = ` and not exists (select 1 from resource_states s where s.owner=m.owner and s.name=m.name and (s.version='' or s.version=r.version) and s.state not in ('available','deprecated'))`

type LifecycleStore interface {
	GetResourceStatus(context.Context, string, string, string) (domain.ResourceStatus, error)
	SetResourceStatus(context.Context, string, string, string, domain.ResourceStatus) error
}

func initialResourceStatus(release domain.Release) domain.ResourceStatus {
	return domain.ResourceStatus{State: release.InitialState, SHA256: release.SHA256, ChangedAt: time.Now().UTC()}
}

func initializeSQLiteStatus(ctx context.Context, tx *sql.Tx, release domain.Release) error {
	if release.InitialState == "" {
		return nil
	}
	status := initialResourceStatus(release)
	if err := writeSQLiteStatus(ctx, tx, release.Owner, release.Name, release.Version, status); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `insert into resource_states(owner,name,version,state,changed_at)
	select ?,?,'',?,? where (select count(*) from releases where module_id=?)=1
	on conflict(owner,name,version) do update set state=excluded.state,changed_at=excluded.changed_at where resource_states.state='deleted'`, release.Owner, release.Name, release.InitialState, status.ChangedAt.Format(time.RFC3339Nano), release.ModuleID)
	return err
}

func initializePostgresStatus(ctx context.Context, tx pgx.Tx, release domain.Release) error {
	if release.InitialState == "" {
		return nil
	}
	status := initialResourceStatus(release)
	if err := writePostgresStatus(ctx, tx, release.Owner, release.Name, release.Version, status); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `insert into resource_states(owner,name,version,state,changed_at)
	select $1,$2,'',$3,$4 where (select count(*) from releases where module_id=$5)=1
	on conflict(owner,name,version) do update set state=excluded.state,changed_at=excluded.changed_at where resource_states.state='deleted'`, release.Owner, release.Name, release.InitialState, status.ChangedAt.Format(time.RFC3339Nano), release.ModuleID)
	return err
}

type statusScanner interface{ Scan(...any) error }

func scanResourceStatus(row statusScanner) (domain.ResourceStatus, error) {
	var status domain.ResourceStatus
	var changed string
	err := row.Scan(&status.State, &status.Previous, &status.Reason, &status.Replacement, &status.SHA256, &changed)
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, pgx.ErrNoRows) {
		return domain.ResourceStatus{State: domain.StateAvailable}, nil
	}
	if err != nil {
		return status, err
	}
	status.ChangedAt, err = time.Parse(time.RFC3339Nano, changed)
	return status, err
}

func (s *SQLiteStore) GetResourceStatus(ctx context.Context, owner, name, version string) (domain.ResourceStatus, error) {
	return scanResourceStatus(s.db.QueryRowContext(ctx, `select state,previous_state,reason,replacement,sha256,changed_at from resource_states where owner=? and name=? and version=?`, owner, name, version))
}

func (s *PostgresStore) GetResourceStatus(ctx context.Context, owner, name, version string) (domain.ResourceStatus, error) {
	return scanResourceStatus(s.pool.QueryRow(ctx, `select state,previous_state,reason,replacement,sha256,changed_at from resource_states where owner=$1 and name=$2 and version=$3`, owner, name, version))
}

func writeSQLiteStatus(ctx context.Context, tx *sql.Tx, owner, name, version string, status domain.ResourceStatus) error {
	_, err := tx.ExecContext(ctx, `insert into resource_states(owner,name,version,state,previous_state,reason,replacement,sha256,changed_at) values(?,?,?,?,?,?,?,?,?) on conflict(owner,name,version) do update set state=excluded.state,previous_state=excluded.previous_state,reason=excluded.reason,replacement=excluded.replacement,sha256=excluded.sha256,changed_at=excluded.changed_at`, owner, name, version, status.State, status.Previous, status.Reason, status.Replacement, status.SHA256, status.ChangedAt.UTC().Format(time.RFC3339Nano))
	if err == nil && version != "" && status.State == domain.StateAvailable {
		_, err = tx.ExecContext(ctx, `update resource_states set state='available',changed_at=? where owner=? and name=? and version='' and state='pending'`, status.ChangedAt.UTC().Format(time.RFC3339Nano), owner, name)
	}
	return err
}

func writePostgresStatus(ctx context.Context, tx pgx.Tx, owner, name, version string, status domain.ResourceStatus) error {
	_, err := tx.Exec(ctx, `insert into resource_states(owner,name,version,state,previous_state,reason,replacement,sha256,changed_at) values($1,$2,$3,$4,$5,$6,$7,$8,$9) on conflict(owner,name,version) do update set state=excluded.state,previous_state=excluded.previous_state,reason=excluded.reason,replacement=excluded.replacement,sha256=excluded.sha256,changed_at=excluded.changed_at`, owner, name, version, status.State, status.Previous, status.Reason, status.Replacement, status.SHA256, status.ChangedAt.UTC().Format(time.RFC3339Nano))
	if err == nil && version != "" && status.State == domain.StateAvailable {
		_, err = tx.Exec(ctx, `update resource_states set state='available',changed_at=$1 where owner=$2 and name=$3 and version='' and state='pending'`, status.ChangedAt.UTC().Format(time.RFC3339Nano), owner, name)
	}
	return err
}

type resourceStatusMutationWriter interface {
	releaseMutationWriter
	writeResourceStatus(owner, name, version string, status domain.ResourceStatus) error
	moduleID(owner, name string) (string, error)
}

func (w sqliteMutationWriter) writeResourceStatus(owner, name, version string, status domain.ResourceStatus) error {
	return writeSQLiteStatus(w.ctx, w.tx, owner, name, version, status)
}

func (w postgresMutationWriter) writeResourceStatus(owner, name, version string, status domain.ResourceStatus) error {
	return writePostgresStatus(w.ctx, w.tx, owner, name, version, status)
}

func (w sqliteMutationWriter) moduleID(owner, name string) (string, error) {
	return sqliteModuleID(w.ctx, w.tx, owner, name)
}

func (w postgresMutationWriter) moduleID(owner, name string) (string, error) {
	return postgresModuleID(w.ctx, w.tx, owner, name)
}

func finishResourceStatusMutation(owner, name, version string, status domain.ResourceStatus, writer resourceStatusMutationWriter) error {
	if err := writer.writeResourceStatus(owner, name, version, status); err != nil {
		return err
	}
	moduleID, err := writer.moduleID(owner, name)
	if err != nil {
		return err
	}
	latest, err := writer.currentLatestVersion(moduleID)
	if err != nil {
		return err
	}
	if err := writer.updateLatestVersion(moduleID, latest); err != nil {
		return err
	}
	return writer.commit()
}

func (s *SQLiteStore) SetResourceStatus(ctx context.Context, owner, name, version string, status domain.ResourceStatus) error {
	if err := validateResourceStatus(status); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	return finishResourceStatusMutation(owner, name, version, status, sqliteMutationWriter{ctx: ctx, tx: tx})
}

func (s *PostgresStore) SetResourceStatus(ctx context.Context, owner, name, version string, status domain.ResourceStatus) error {
	if err := validateResourceStatus(status); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return finishResourceStatusMutation(owner, name, version, status, postgresMutationWriter{ctx: ctx, tx: tx})
}

func validateResourceStatus(status domain.ResourceStatus) error {
	switch status.State {
	case domain.StateAvailable, domain.StatePending, domain.StateDeprecated, domain.StateBlocked, domain.StateDeleted:
	default:
		return fmt.Errorf("invalid resource state %q", status.State)
	}
	if status.ChangedAt.IsZero() {
		return errors.New("resource state requires a timestamp")
	}
	return nil
}
