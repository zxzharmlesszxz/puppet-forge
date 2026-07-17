package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
)

const postgresSchemaLockID int64 = 829522104050364091

var ErrNotFound = errors.New("not found")

type postgresExecutor interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type ModuleStore interface {
	Ping(ctx context.Context) error
	UpsertModule(ctx context.Context, owner, name string) (domain.Module, error)
	CreateRelease(ctx context.Context, release domain.Release) (domain.Release, error)
	DeleteModule(ctx context.Context, owner, name string) error
	DeleteRelease(ctx context.Context, owner, name, version string) error
	ListModules(ctx context.Context, limit int) ([]domain.Module, error)
	ListModulesPage(ctx context.Context, limit, offset int) ([]domain.Module, int, error)
	ListUpstreamModules(ctx context.Context, limit int) ([]domain.Module, error)
	ListReleases(ctx context.Context, owner, name string) ([]domain.ModuleVersion, error)
	ListAllReleases(ctx context.Context) ([]ReleaseSummary, error)
	GetModule(ctx context.Context, owner, name string) (domain.Module, error)
	GetRelease(ctx context.Context, owner, name, version string) (domain.Release, error)
}

type DeletedReleaseStore interface {
	IsReleaseDeleted(ctx context.Context, owner, name, version, source string) (bool, error)
}

type ReleaseUsageStore interface {
	MarkReleaseUsed(ctx context.Context, owner, name, version string) error
	IsReleaseActive(ctx context.Context, owner, name, version string, since time.Time) (bool, error)
	ListActiveReleases(ctx context.Context, since time.Time) ([]ReleaseSummary, error)
	PruneReleaseUsageBefore(ctx context.Context, before time.Time) error
}

type ReleaseMetricSummaryStore interface {
	ListReleaseMetricSummaries(ctx context.Context) ([]domain.ReleaseMetricSummary, error)
}

type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(ctx context.Context, dsn string) (*PostgresStore, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("create pg pool: %w", err)
	}

	store := &PostgresStore{pool: pool}
	if err := store.ensureOperationalTables(ctx); err != nil {
		pool.Close()
		return nil, err
	}

	return store, nil
}

func (s *PostgresStore) Close() {
	s.pool.Close()
}

func (s *PostgresStore) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

func (s *PostgresStore) ensureOperationalTables(ctx context.Context) error {
	const query = `
		create table if not exists modules (
			id text primary key,
			owner text not null,
			name text not null,
			latest_version text,
			created_at timestamptz not null default now(),
			updated_at timestamptz not null default now(),
			constraint modules_owner_name_unique unique (owner, name)
		);

		create table if not exists releases (
			id text primary key,
			module_id text not null references modules (id) on delete cascade,
			source text not null default 'local',
			version text not null,
			description text,
			readme text,
			file_name text not null,
			content_type text not null,
			size_bytes bigint not null,
			sha256 text not null,
			storage_path text not null,
			upstream_slug text,
			upstream_file_uri text,
			metadata jsonb not null default '{}'::jsonb,
			created_at timestamptz not null default now(),
			constraint releases_module_version_unique unique (module_id, version)
		);

		create table if not exists app_leases (
			name text primary key,
			holder text not null,
			lease_until timestamptz not null
		);

		create table if not exists access_teams (
			team text primary key
		);

		create table if not exists access_tokens (
			team text not null references access_teams (team) on delete cascade,
			token_type text not null,
			token text not null,
			primary key (token_type, token)
		);

		create table if not exists access_publish_owners (
			team text not null references access_teams (team) on delete cascade,
			owner text not null,
			primary key (team, owner)
		);

		create table if not exists access_oidc_mappings (
			team text not null references access_teams (team) on delete cascade,
			mapping_type text not null,
			value text not null,
			primary key (team, mapping_type, value)
		);

		create table if not exists deleted_releases (
			owner text not null,
			name text not null,
			version text not null,
			source text not null,
			deleted_at timestamptz not null default now(),
			primary key (owner, name, version, source)
		);

		create table if not exists release_usage (
			owner text not null,
			name text not null,
			version text not null,
			last_used_at timestamptz not null default now(),
			primary key (owner, name, version)
		);

		create index if not exists idx_modules_updated_at on modules (updated_at desc);
		create index if not exists idx_releases_module_id on releases (module_id);
	`

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire postgres schema setup connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `select pg_advisory_lock($1)`, postgresSchemaLockID); err != nil {
		return fmt.Errorf("lock postgres schema setup: %w", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), `select pg_advisory_unlock($1)`, postgresSchemaLockID) }()

	if _, err := conn.Exec(ctx, query); err != nil {
		return fmt.Errorf("ensure operational tables: %w", err)
	}
	migrateOIDC, err := needsAccessOIDCMappingsMigration(ctx, conn)
	if err != nil {
		return err
	}
	if migrateOIDC {
		if err := migrateAccessOIDCMappings(ctx, conn); err != nil {
			return err
		}
	}
	return nil
}

func needsAccessOIDCMappingsMigration(ctx context.Context, db postgresExecutor) (bool, error) {
	var constraintName string
	err := db.QueryRow(ctx, `
		select constraint_name
		from information_schema.table_constraints
		where table_schema = current_schema()
			and table_name = 'access_oidc_mappings'
			and constraint_type = 'PRIMARY KEY'
		limit 1
	`).Scan(&constraintName)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("check postgres oidc mappings migration state: %w", err)
	}
	return false, nil
}

func migrateAccessOIDCMappings(ctx context.Context, db postgresExecutor) error {
	const query = `
		create table if not exists access_oidc_mappings_next (
			team text not null references access_teams (team) on delete cascade,
			mapping_type text not null,
			value text not null,
			primary key (team, mapping_type, value)
		);
		insert into access_oidc_mappings_next (team, mapping_type, value)
		select team, mapping_type, value from access_oidc_mappings
		on conflict do nothing;
		drop table access_oidc_mappings;
		alter table access_oidc_mappings_next rename to access_oidc_mappings;
	`
	if _, err := db.Exec(ctx, query); err != nil {
		return fmt.Errorf("migrate postgres oidc mappings: %w", err)
	}
	return nil
}

func (s *PostgresStore) AcquireLease(ctx context.Context, name, holder string, duration time.Duration) (bool, error) {
	const query = `
		insert into app_leases (name, holder, lease_until)
		values ($1, $2, now() + $3::interval)
		on conflict (name)
		do update set
			holder = excluded.holder,
			lease_until = excluded.lease_until
		where app_leases.lease_until <= now() or app_leases.holder = excluded.holder
	`
	tag, err := s.pool.Exec(ctx, query, name, holder, formatPGInterval(duration))
	if err != nil {
		return false, fmt.Errorf("acquire lease: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

func (s *PostgresStore) ReleaseLease(ctx context.Context, name, holder string) error {
	const query = `delete from app_leases where name = $1 and holder = $2`
	if _, err := s.pool.Exec(ctx, query, name, holder); err != nil {
		return fmt.Errorf("release lease: %w", err)
	}
	return nil
}

func (s *PostgresStore) LoadTeamConfigs(ctx context.Context) ([]auth.TeamConfig, error) {
	rows, err := s.pool.Query(ctx, `select team from access_teams order by team`)
	if err != nil {
		return nil, fmt.Errorf("list access teams: %w", err)
	}
	defer rows.Close()

	return loadTeamConfigs(ctx, rows, s)
}

func (s *PostgresStore) ReplaceTeamConfigs(ctx context.Context, configs []auth.TeamConfig) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin access tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	if _, err := tx.Exec(ctx, `delete from access_teams`); err != nil {
		return fmt.Errorf("clear access teams: %w", err)
	}
	for _, cfg := range configs {
		if strings.TrimSpace(cfg.Team) == "" {
			return errors.New("team is required")
		}
		if _, err := tx.Exec(ctx, `insert into access_teams (team) values ($1)`, cfg.Team); err != nil {
			return fmt.Errorf("insert access team: %w", err)
		}
		for _, token := range cfg.ReadTokens {
			if err := insertPostgresAccessToken(ctx, tx, cfg.Team, "read", token); err != nil {
				return err
			}
		}
		for _, token := range cfg.PublishTokens {
			if err := insertPostgresAccessToken(ctx, tx, cfg.Team, "publish", token); err != nil {
				return err
			}
		}
		for _, owner := range cfg.PublishOwners {
			owner = strings.TrimSpace(owner)
			if owner == "" {
				continue
			}
			if _, err := tx.Exec(ctx, `insert into access_publish_owners (team, owner) values ($1, $2)`, cfg.Team, owner); err != nil {
				return fmt.Errorf("insert access owner: %w", err)
			}
		}
		for _, mapping := range accessOIDCMappings(cfg) {
			if _, err := tx.Exec(ctx, `insert into access_oidc_mappings (team, mapping_type, value) values ($1, $2, $3)`, cfg.Team, mapping.kind, mapping.value); err != nil {
				return fmt.Errorf("insert oidc mapping: %w", err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit access tx: %w", err)
	}
	return nil
}

func insertPostgresAccessToken(ctx context.Context, tx pgx.Tx, team, tokenType, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}
	if _, err := tx.Exec(ctx, `insert into access_tokens (team, token_type, token) values ($1, $2, $3)`, team, tokenType, token); err != nil {
		return fmt.Errorf("insert access token: %w", err)
	}
	return nil
}

func (s *PostgresStore) loadAccessTokens(ctx context.Context, configs []auth.TeamConfig, index map[string]int) error {
	rows, err := s.pool.Query(ctx, `select team, token_type, token from access_tokens order by team, token_type, token`)
	if err != nil {
		return fmt.Errorf("list access tokens: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var team, tokenType, token string
		if err := rows.Scan(&team, &tokenType, &token); err != nil {
			return fmt.Errorf("scan access token: %w", err)
		}
		if i, ok := index[team]; ok {
			applyAccessToken(&configs[i], tokenType, token)
		}
	}
	return rows.Err()
}

func (s *PostgresStore) loadAccessOwners(ctx context.Context, configs []auth.TeamConfig, index map[string]int) error {
	rows, err := s.pool.Query(ctx, `select team, owner from access_publish_owners order by team, owner`)
	if err != nil {
		return fmt.Errorf("list access owners: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var team, owner string
		if err := rows.Scan(&team, &owner); err != nil {
			return fmt.Errorf("scan access owner: %w", err)
		}
		if i, ok := index[team]; ok {
			configs[i].PublishOwners = append(configs[i].PublishOwners, owner)
		}
	}
	return rows.Err()
}

func (s *PostgresStore) loadAccessOIDC(ctx context.Context, configs []auth.TeamConfig, index map[string]int) error {
	rows, err := s.pool.Query(ctx, `select team, mapping_type, value from access_oidc_mappings order by team, mapping_type, value`)
	if err != nil {
		return fmt.Errorf("list access oidc mappings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var team, mappingType, value string
		if err := rows.Scan(&team, &mappingType, &value); err != nil {
			return fmt.Errorf("scan access oidc mapping: %w", err)
		}
		if i, ok := index[team]; ok {
			applyAccessOIDCMapping(&configs[i], mappingType, value)
		}
	}
	return rows.Err()
}

func formatPGInterval(duration time.Duration) string {
	return fmt.Sprintf("%f seconds", duration.Seconds())
}

func (s *PostgresStore) UpsertModule(ctx context.Context, owner, name string) (domain.Module, error) {
	const query = `
		insert into modules (id, owner, name, created_at, updated_at)
		values ($1, $2, $3, now(), now())
		on conflict (owner, name)
		do update set updated_at = now()
		returning id, owner, name, coalesce(latest_version, ''), created_at, updated_at
	`

	module := domain.Module{}
	err := s.pool.QueryRow(ctx, query, uuid.NewString(), owner, name).Scan(
		&module.ID,
		&module.Owner,
		&module.Name,
		&module.LatestVersion,
		&module.CreatedAt,
		&module.UpdatedAt,
	)
	if err != nil {
		return domain.Module{}, fmt.Errorf("upsert module: %w", err)
	}

	return module, nil
}

func (s *PostgresStore) CreateRelease(ctx context.Context, release domain.Release) (domain.Release, error) {
	metadataJSON, err := json.Marshal(release.Metadata)
	if err != nil {
		return domain.Release{}, fmt.Errorf("marshal metadata: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Release{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	const insertRelease = `
		insert into releases (
			id, module_id, source, version, description, readme, file_name, content_type, size_bytes,
			sha256, storage_path, upstream_slug, upstream_file_uri, metadata, created_at
		)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, now())
		on conflict (module_id, version)
		do update set
			source = excluded.source,
			description = excluded.description,
			readme = excluded.readme,
			file_name = excluded.file_name,
			content_type = excluded.content_type,
			size_bytes = excluded.size_bytes,
			sha256 = excluded.sha256,
			storage_path = excluded.storage_path,
			upstream_slug = excluded.upstream_slug,
			upstream_file_uri = excluded.upstream_file_uri,
			metadata = excluded.metadata
		returning created_at
	`

	err = tx.QueryRow(ctx, insertRelease,
		release.ID,
		release.ModuleID,
		release.Source,
		release.Version,
		release.Description,
		release.Readme,
		release.FileName,
		release.ContentType,
		release.SizeBytes,
		release.SHA256,
		release.StoragePath,
		release.UpstreamSlug,
		release.UpstreamFileURI,
		metadataJSON,
	).Scan(&release.CreatedAt)
	if err != nil {
		return domain.Release{}, fmt.Errorf("insert release: %w", err)
	}

	const updateModule = `
		update modules
		set latest_version = $1, updated_at = now()
		where id = $2
	`
	currentLatest, err := postgresCurrentLatestVersion(ctx, tx, release.ModuleID)
	if err != nil {
		return domain.Release{}, err
	}
	latest := latestVersionWithCandidate(currentLatest, release.Version)
	if _, err := tx.Exec(ctx, updateModule, latest, release.ModuleID); err != nil {
		return domain.Release{}, fmt.Errorf("update module latest version: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return domain.Release{}, fmt.Errorf("commit release tx: %w", err)
	}

	return release, nil
}

func (s *PostgresStore) ListModules(ctx context.Context, limit int) ([]domain.Module, error) {
	modules, _, err := s.ListModulesPage(ctx, limit, 0)
	return modules, err
}

func (s *PostgresStore) ListModulesPage(ctx context.Context, limit, offset int) ([]domain.Module, int, error) {
	total, err := s.countModulesWithReleases(ctx)
	if err != nil {
		return nil, 0, err
	}

	const query = `
		select id, owner, name, coalesce(latest_version, ''), created_at, updated_at
		from modules
		where exists (
			select 1
			from releases
			where releases.module_id = modules.id
		)
		order by updated_at desc
		limit $1
		offset $2
	`

	rows, err := s.pool.Query(ctx, query, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list modules: %w", err)
	}
	defer rows.Close()

	modules := make([]domain.Module, 0, limit)
	for rows.Next() {
		var module domain.Module
		if err := rows.Scan(
			&module.ID,
			&module.Owner,
			&module.Name,
			&module.LatestVersion,
			&module.CreatedAt,
			&module.UpdatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("scan module: %w", err)
		}
		modules = append(modules, module)
	}

	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return modules, total, nil
}

func (s *PostgresStore) countModulesWithReleases(ctx context.Context) (int, error) {
	const query = `
		select count(*)
		from modules
		where exists (
			select 1
			from releases
			where releases.module_id = modules.id
		)
	`
	var total int
	if err := s.pool.QueryRow(ctx, query).Scan(&total); err != nil {
		return 0, fmt.Errorf("count modules: %w", err)
	}
	return total, nil
}

func (s *PostgresStore) ListUpstreamModules(ctx context.Context, limit int) ([]domain.Module, error) {
	const query = `
		select distinct m.id, m.owner, m.name, coalesce(m.latest_version, ''), m.created_at, m.updated_at
		from modules m
		join releases r on r.module_id = m.id
		where coalesce(r.source, 'local') = 'upstream'
		order by m.updated_at desc
		limit $1
	`

	rows, err := s.pool.Query(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("list upstream modules: %w", err)
	}
	defer rows.Close()

	modules := make([]domain.Module, 0, limit)
	for rows.Next() {
		var module domain.Module
		if err := rows.Scan(
			&module.ID,
			&module.Owner,
			&module.Name,
			&module.LatestVersion,
			&module.CreatedAt,
			&module.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan upstream module: %w", err)
		}
		modules = append(modules, module)
	}

	return modules, rows.Err()
}

func (s *PostgresStore) DeleteModule(ctx context.Context, owner, name string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	const query = `
		delete from modules
		where owner = $1 and name = $2
	`

	tag, err := tx.Exec(ctx, query, owner, name)
	if err != nil {
		return fmt.Errorf("delete module: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `delete from deleted_releases where owner = $1 and name = $2`, owner, name); err != nil {
		return fmt.Errorf("delete module tombstones: %w", err)
	}
	if _, err := tx.Exec(ctx, `delete from release_usage where owner = $1 and name = $2`, owner, name); err != nil {
		return fmt.Errorf("delete module release usage: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit delete module tx: %w", err)
	}

	return nil
}

func (s *PostgresStore) DeleteRelease(ctx context.Context, owner, name, version string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	var source string
	const selectSource = `
		select r.source
		from releases r
		join modules m on m.id = r.module_id
		where m.owner = $1 and m.name = $2 and r.version = $3
	`
	if err := tx.QueryRow(ctx, selectSource, owner, name, version).Scan(&source); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("get release source: %w", err)
	}

	const deleteQuery = `
		delete from releases r
		using modules m
		where r.module_id = m.id
		  and m.owner = $1
		  and m.name = $2
		  and r.version = $3
	`

	tag, err := tx.Exec(ctx, deleteQuery, owner, name, version)
	if err != nil {
		return fmt.Errorf("delete release: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if source == "upstream" {
		const insertTombstone = `
			insert into deleted_releases (owner, name, version, source)
			values ($1, $2, $3, $4)
			on conflict(owner, name, version, source) do update set deleted_at = now()
		`
		if _, err := tx.Exec(ctx, insertTombstone, owner, name, version, source); err != nil {
			return fmt.Errorf("record deleted release: %w", err)
		}
	}

	moduleID, err := postgresModuleID(ctx, tx, owner, name)
	if err != nil {
		return err
	}
	latest, err := postgresLatestVersion(ctx, tx, moduleID)
	if err != nil {
		return err
	}

	const updateLatest = `
		update modules
		set latest_version = $1, updated_at = now()
		where id = $2
	`
	if _, err := tx.Exec(ctx, updateLatest, latest, moduleID); err != nil {
		return fmt.Errorf("update latest version: %w", err)
	}

	const deleteEmptyModule = `
		delete from modules
		where owner = $1
		  and name = $2
		  and not exists (
			select 1
			from releases
			where releases.module_id = modules.id
		  )
	`
	if _, err := tx.Exec(ctx, deleteEmptyModule, owner, name); err != nil {
		return fmt.Errorf("delete empty module: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit delete release tx: %w", err)
	}

	return nil
}

func (s *PostgresStore) IsReleaseDeleted(ctx context.Context, owner, name, version, source string) (bool, error) {
	var exists int
	err := s.pool.QueryRow(ctx, `
		select 1
		from deleted_releases
		where owner = $1 and name = $2 and version = $3 and source = $4
	`, owner, name, version, source).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check deleted release: %w", err)
	}
	return true, nil
}

func (s *PostgresStore) MarkReleaseUsed(ctx context.Context, owner, name, version string) error {
	_, err := s.pool.Exec(ctx, `
		insert into release_usage (owner, name, version, last_used_at)
		values ($1, $2, $3, now())
		on conflict (owner, name, version) do update set last_used_at = excluded.last_used_at
	`, owner, name, version)
	if err != nil {
		return fmt.Errorf("mark release used: %w", err)
	}
	return nil
}

func (s *PostgresStore) IsReleaseActive(ctx context.Context, owner, name, version string, since time.Time) (bool, error) {
	var exists int
	err := s.pool.QueryRow(ctx, `
		select 1
		from release_usage
		where owner = $1 and name = $2 and version = $3 and last_used_at >= $4
	`, owner, name, version, since.UTC()).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check release active: %w", err)
	}
	return true, nil
}

func (s *PostgresStore) ListActiveReleases(ctx context.Context, since time.Time) ([]ReleaseSummary, error) {
	rows, err := s.pool.Query(ctx, `
		select owner, name, version, last_used_at
		from release_usage
		where last_used_at >= $1
		order by owner, name, version
	`, since.UTC())
	if err != nil {
		return nil, fmt.Errorf("list active releases: %w", err)
	}
	defer rows.Close()

	var releases []ReleaseSummary
	for rows.Next() {
		var rel ReleaseSummary
		if err := rows.Scan(&rel.Owner, &rel.Name, &rel.Version, &rel.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan active release: %w", err)
		}
		releases = append(releases, rel)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active releases: %w", err)
	}
	return releases, nil
}

func (s *PostgresStore) PruneReleaseUsageBefore(ctx context.Context, before time.Time) error {
	if _, err := s.pool.Exec(ctx, `delete from release_usage where last_used_at < $1`, before.UTC()); err != nil {
		return fmt.Errorf("prune release usage: %w", err)
	}
	return nil
}

func postgresModuleID(ctx context.Context, tx pgx.Tx, owner, name string) (string, error) {
	var moduleID string
	if err := tx.QueryRow(ctx, `select id from modules where owner = $1 and name = $2`, owner, name).Scan(&moduleID); err != nil {
		return "", fmt.Errorf("get module id: %w", err)
	}
	return moduleID, nil
}

func postgresLatestVersion(ctx context.Context, tx pgx.Tx, moduleID string) (string, error) {
	rows, err := tx.Query(ctx, `
		select version from releases where module_id = $1
	`, moduleID)
	if err != nil {
		return "", fmt.Errorf("list versions for latest: %w", err)
	}
	defer rows.Close()

	var versions []string
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return "", fmt.Errorf("scan version for latest: %w", err)
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return latestVersion(versions), nil
}

func postgresCurrentLatestVersion(ctx context.Context, tx pgx.Tx, moduleID string) (string, error) {
	var latest string
	err := tx.QueryRow(ctx, `select coalesce(latest_version, '') from modules where id = $1 for update`, moduleID).Scan(&latest)
	if err != nil {
		return "", fmt.Errorf("get current latest version: %w", err)
	}
	return latest, nil
}

func (s *PostgresStore) GetModule(ctx context.Context, owner, name string) (domain.Module, error) {
	const query = `
		select id, owner, name, coalesce(latest_version, ''), created_at, updated_at
		from modules
		where owner = $1 and name = $2
	`

	var module domain.Module
	err := s.pool.QueryRow(ctx, query, owner, name).Scan(
		&module.ID,
		&module.Owner,
		&module.Name,
		&module.LatestVersion,
		&module.CreatedAt,
		&module.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Module{}, ErrNotFound
	}
	if err != nil {
		return domain.Module{}, fmt.Errorf("get module: %w", err)
	}

	return module, nil
}

func (s *PostgresStore) ListReleases(ctx context.Context, owner, name string) ([]domain.ModuleVersion, error) {
	const query = `
		select r.version, r.created_at
		from releases r
		join modules m on m.id = r.module_id
		where m.owner = $1 and m.name = $2
	`

	rows, err := s.pool.Query(ctx, query, owner, name)
	if err != nil {
		return nil, fmt.Errorf("list releases: %w", err)
	}
	defer rows.Close()

	var versions []domain.ModuleVersion
	for rows.Next() {
		var version domain.ModuleVersion
		if err := rows.Scan(&version.Version, &version.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan release version: %w", err)
		}
		versions = append(versions, version)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortModuleVersions(versions)
	return versions, nil
}

func (s *PostgresStore) ListAllReleases(ctx context.Context) ([]ReleaseSummary, error) {
	const query = `
		select m.owner, m.name, r.version, r.created_at
		from releases r
		join modules m on m.id = r.module_id
	`

	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list all releases: %w", err)
	}
	defer rows.Close()

	var releases []ReleaseSummary
	for rows.Next() {
		var item ReleaseSummary
		if err := rows.Scan(&item.Owner, &item.Name, &item.Version, &item.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan release summary: %w", err)
		}
		releases = append(releases, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortReleaseSummaries(releases)
	return releases, nil
}

func (s *PostgresStore) ListReleaseMetricSummaries(ctx context.Context) ([]domain.ReleaseMetricSummary, error) {
	const query = `
		select
			coalesce(r.source, 'local') as source,
			count(*) as releases,
			sum(case when r.version = coalesce(m.latest_version, '') then 1 else 0 end) as latest_releases
		from releases r
		join modules m on m.id = r.module_id
		group by coalesce(r.source, 'local')
		order by source
	`

	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list release metric summaries: %w", err)
	}
	defer rows.Close()

	var summaries []domain.ReleaseMetricSummary
	for rows.Next() {
		var summary domain.ReleaseMetricSummary
		if err := rows.Scan(&summary.Source, &summary.Releases, &summary.LatestReleases); err != nil {
			return nil, fmt.Errorf("scan release metric summary: %w", err)
		}
		summaries = append(summaries, summary)
	}
	return summaries, rows.Err()
}

func (s *PostgresStore) GetRelease(ctx context.Context, owner, name, version string) (domain.Release, error) {
	const query = `
		select
			r.id, r.module_id, m.owner, m.name, coalesce(r.source, 'local'), r.version, coalesce(r.description, ''), coalesce(r.readme, ''),
			r.file_name, r.content_type, r.size_bytes, r.sha256, r.storage_path, coalesce(r.upstream_slug, ''), coalesce(r.upstream_file_uri, ''),
			r.metadata, r.created_at
		from releases r
		join modules m on m.id = r.module_id
		where m.owner = $1 and m.name = $2 and r.version = $3
	`

	var (
		release      domain.Release
		metadataJSON []byte
	)

	err := s.pool.QueryRow(ctx, query, owner, name, version).Scan(
		&release.ID,
		&release.ModuleID,
		&release.Owner,
		&release.Name,
		&release.Source,
		&release.Version,
		&release.Description,
		&release.Readme,
		&release.FileName,
		&release.ContentType,
		&release.SizeBytes,
		&release.SHA256,
		&release.StoragePath,
		&release.UpstreamSlug,
		&release.UpstreamFileURI,
		&metadataJSON,
		&release.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Release{}, ErrNotFound
	}
	if err != nil {
		return domain.Release{}, fmt.Errorf("get release: %w", err)
	}

	if len(metadataJSON) > 0 {
		if err := json.Unmarshal(metadataJSON, &release.Metadata); err != nil {
			return domain.Release{}, fmt.Errorf("unmarshal metadata: %w", err)
		}
	}

	return release, nil
}

func NewRelease(moduleID, owner, name, version, description, readme, fileName, contentType, sha256, storagePath string, sizeBytes int64, metadata map[string]any) domain.Release {
	return domain.Release{
		ID:          uuid.NewString(),
		ModuleID:    moduleID,
		Owner:       owner,
		Name:        name,
		Source:      "local",
		Version:     version,
		Description: description,
		Readme:      readme,
		FileName:    fileName,
		ContentType: contentType,
		SizeBytes:   sizeBytes,
		SHA256:      sha256,
		StoragePath: storagePath,
		Metadata:    metadata,
		CreatedAt:   time.Time{},
	}
}
