package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
)

const postgresSchemaLockID int64 = 829522104050364091
const postgresAdvisoryUnlockTimeout = 5 * time.Second

var ErrNotFound = errors.New("not found")
var ErrConflict = errors.New("conflict")

type ModuleUnlock func() error

type postgresExecutor interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type postgresQueryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

type postgresAccessConfigLoader struct {
	queryer postgresQueryer
}

type ModuleStore interface {
	Ping(ctx context.Context) error
	LockModule(ctx context.Context, owner, name string) (ModuleUnlock, error)
	UpsertModule(ctx context.Context, owner, name string) (domain.Module, error)
	CreateRelease(ctx context.Context, release domain.Release) (domain.Release, error)
	CreateReleaseIfAbsent(ctx context.Context, release domain.Release) (domain.Release, error)
	DeleteModuleIfEmpty(ctx context.Context, owner, name string) error
	DeleteModule(ctx context.Context, owner, name string) error
	DeleteRelease(ctx context.Context, owner, name, version string) error
	ListModules(ctx context.Context, limit int) ([]domain.Module, error)
	ListModulesPage(ctx context.Context, limit, offset int) ([]domain.Module, int, error)
	ListModulesPageFiltered(ctx context.Context, owners []string, query string, limit, offset int) ([]domain.Module, int, error)
	ListModulesPagePrioritized(ctx context.Context, owners, priorityOwners []string, query string, limit, offset int) ([]domain.Module, int, error)
	// CountModulesByOwner counts every owner when owners is nil and limits the
	// result to the explicit owner set otherwise.
	CountModulesByOwner(ctx context.Context, owners []string) (map[string]int, error)
	CountUpstreamModulesByOwner(ctx context.Context) (map[string]int, error)
	ListUpstreamModules(ctx context.Context, limit int) ([]domain.Module, error)
	MarkUpstreamModuleRefreshAttempt(ctx context.Context, owner, name string, attemptedAt time.Time) error
	ListReleases(ctx context.Context, owner, name string) ([]domain.ModuleVersion, error)
	ListAllReleases(ctx context.Context) ([]ReleaseSummary, error)
	GetModule(ctx context.Context, owner, name string) (domain.Module, error)
	GetRelease(ctx context.Context, owner, name, version string) (domain.Release, error)
}

type SlugStore interface {
	GetModuleBySlug(ctx context.Context, slug string) (domain.Module, error)
	GetModuleForReleaseSlug(ctx context.Context, releaseSlug string) (domain.Module, error)
	GetReleaseBySlug(ctx context.Context, slug string) (domain.Release, error)
}

func moduleSlug(owner, name string) string {
	return owner + "-" + name
}

func releaseSlug(owner, name, version string) string {
	return moduleSlug(owner, name) + "-" + version
}

type DeletedReleaseStore interface {
	IsReleaseDeleted(ctx context.Context, owner, name, version, source string) (bool, error)
	PurgeDeletedReleases(ctx context.Context, cutoff time.Time) (int64, error)
}

type ReleaseUsageStore interface {
	MarkReleaseUsed(ctx context.Context, owner, name, version string) error
	IsReleaseActive(ctx context.Context, owner, name, version string, since time.Time) (bool, error)
	ListActiveReleases(ctx context.Context, since time.Time) ([]ReleaseSummary, error)
	ListActiveReleasesForModules(ctx context.Context, since time.Time, modules []domain.Module) ([]ReleaseSummary, error)
	PruneReleaseUsageBefore(ctx context.Context, before time.Time) error
}

type ReleaseMetricSummaryStore interface {
	ListReleaseMetricSummaries(ctx context.Context) ([]domain.ReleaseMetricSummary, error)
}

type ReleaseChecksumStore interface {
	UpdateReleaseChecksums(ctx context.Context, owner, name, version, md5, sha256, storagePath string, sizeBytes int64) error
}

type PostgresStore struct {
	pool        *pgxpool.Pool
	tokenHasher *auth.TokenHasher
}

func (s *PostgresStore) LockAccessConfig(ctx context.Context) (AccessConfigUnlock, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire access config lock connection: %w", err)
	}
	const lockName = "puppet-forge/access-config"
	if _, err := conn.Exec(ctx, `select pg_advisory_lock(hashtextextended($1, 0))`, lockName); err != nil {
		conn.Release()
		return nil, fmt.Errorf("acquire access config lock: %w", err)
	}

	var once sync.Once
	var unlockErr error
	return func() error {
		once.Do(func() {
			unlockCtx, cancel := context.WithTimeout(context.Background(), postgresAdvisoryUnlockTimeout)
			defer cancel()
			var unlocked bool
			if err := conn.QueryRow(unlockCtx, `select pg_advisory_unlock(hashtextextended($1, 0))`, lockName).Scan(&unlocked); err != nil {
				unlockErr = fmt.Errorf("release access config lock: %w", err)
				closePostgresConnection(conn)
			} else if !unlocked {
				unlockErr = errors.New("release access config lock: lock was not held")
			}
			conn.Release()
		})
		return unlockErr
	}, nil
}

func NewPostgresStore(ctx context.Context, dsn string, tokenHashers ...*auth.TokenHasher) (*PostgresStore, error) {
	var tokenHasher *auth.TokenHasher
	if len(tokenHashers) > 0 {
		tokenHasher = tokenHashers[0]
	}
	return newPostgresStore(ctx, dsn, tokenHasher, nil)
}

func newPostgresStore(ctx context.Context, dsn string, tokenHasher *auth.TokenHasher, poolOptions *PostgresPoolConfig) (*PostgresStore, error) {
	poolConfig, err := postgresPoolConfig(dsn, poolOptions)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("create pg pool: %w", err)
	}

	store := &PostgresStore{pool: pool, tokenHasher: tokenHasher}
	if err := store.ensureOperationalTables(ctx); err != nil {
		pool.Close()
		return nil, err
	}

	return store, nil
}

func postgresPoolConfig(dsn string, options *PostgresPoolConfig) (*pgxpool.Config, error) {
	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse pg pool config: %w", err)
	}
	if options != nil {
		poolConfig.MaxConns = options.MaxConns
		poolConfig.MinConns = options.MinConns
		poolConfig.MaxConnLifetime = options.MaxConnLifetime
		poolConfig.MaxConnIdleTime = options.MaxConnIdleTime
	}
	return poolConfig, nil
}

func (s *PostgresStore) Close() {
	s.pool.Close()
}

func (s *PostgresStore) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

func (s *PostgresStore) LockModule(ctx context.Context, owner, name string) (ModuleUnlock, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire module lock connection: %w", err)
	}
	lockKey := owner + "/" + name
	if _, err := conn.Exec(ctx, `select pg_advisory_lock(hashtextextended($1, 0))`, lockKey); err != nil {
		conn.Release()
		return nil, fmt.Errorf("acquire module lock: %w", err)
	}

	var once sync.Once
	var unlockErr error
	return func() error {
		once.Do(func() {
			unlockCtx, cancel := context.WithTimeout(context.Background(), postgresAdvisoryUnlockTimeout)
			defer cancel()
			var unlocked bool
			if err := conn.QueryRow(unlockCtx, `select pg_advisory_unlock(hashtextextended($1, 0))`, lockKey).Scan(&unlocked); err != nil {
				unlockErr = fmt.Errorf("release module lock: %w", err)
				closePostgresConnection(conn)
			} else if !unlocked {
				unlockErr = errors.New("release module lock: lock was not held")
			}
			conn.Release()
		})
		return unlockErr
	}, nil
}

func (s *PostgresStore) ensureOperationalTables(ctx context.Context) (returnErr error) {
	const query = `
		create table if not exists modules (
			id text primary key,
			owner text not null,
			name text not null,
			slug text not null unique,
			latest_version text,
			upstream_refreshed_at timestamptz,
			created_at timestamptz not null default now(),
			updated_at timestamptz not null default now(),
			constraint modules_owner_name_unique unique (owner, name),
			constraint modules_owner_nonempty check (btrim(owner) <> ''),
			constraint modules_name_nonempty check (btrim(name) <> ''),
			constraint modules_slug_nonempty check (btrim(slug) <> '')
		);

		create table if not exists releases (
			id text primary key,
			module_id text not null references modules (id) on delete cascade,
			slug text not null unique,
			source text not null default 'local',
			version text not null,
			description text,
			readme text,
			file_name text not null,
			content_type text not null,
			size_bytes bigint not null,
			md5 text not null default '',
			sha256 text not null,
			storage_path text not null,
			upstream_slug text,
			upstream_file_uri text,
			metadata jsonb not null default '{}'::jsonb,
			created_at timestamptz not null default now(),
			constraint releases_module_version_unique unique (module_id, version),
			constraint releases_version_nonempty check (btrim(version) <> ''),
			constraint releases_slug_nonempty check (btrim(slug) <> ''),
			constraint releases_size_nonnegative check (size_bytes >= 0),
			constraint releases_source_valid check (source in ('local', 'upstream'))
		);

		create table if not exists schema_metadata (
			id smallint primary key check (id = 1),
			version integer not null check (version >= 0)
		);
		insert into schema_metadata (id, version) values (1, 0) on conflict (id) do nothing;

		create table if not exists app_leases (
			name text primary key,
			holder text not null,
			lease_until timestamptz not null
		);

		create table if not exists rate_limits (
			limiter_key text primary key,
			request_count integer not null,
			reset_at timestamptz not null,
			updated_at timestamptz not null
		);

		create table if not exists access_teams (
			team text primary key
		);

		create table if not exists access_tokens (
			token_id text primary key,
			team text not null references access_teams (team) on delete cascade,
			token_type text not null,
			token_prefix text not null,
			token_hash text not null unique,
			description text not null default '',
			created_at timestamptz not null default now(),
			expires_at timestamptz,
			revoked_at timestamptz,
			last_used_at timestamptz
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

		create table if not exists manage_sessions (
			session_hash text primary key,
			credential_hash text not null,
			credential_id text not null default '',
			auth_method text not null,
			csrf_secret text not null,
			created_at timestamptz not null,
			expires_at timestamptz not null,
			last_seen_at timestamptz not null,
			revoked_at timestamptz
		);

		create table if not exists oidc_states (
			state_hash text primary key,
			nonce text not null,
			pkce_verifier text not null,
			next_path text not null,
			created_at timestamptz not null,
			expires_at timestamptz not null,
			consumed_at timestamptz
		);

		create table if not exists oidc_sessions (
			session_hash text primary key,
			subject text not null,
			email text not null,
			name text not null,
			groups_json jsonb not null,
			csrf_secret text not null,
			created_at timestamptz not null,
			expires_at timestamptz not null,
			last_seen_at timestamptz not null,
			revoked_at timestamptz
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

		create table if not exists artifact_deletions (
			storage_path text primary key,
			owner text not null,
			name text not null,
			created_at timestamptz not null default now(),
			next_attempt_at timestamptz not null default now()
		);

		create index if not exists idx_modules_updated_at on modules (updated_at desc);
		create index if not exists idx_releases_module_id on releases (module_id);
		create index if not exists idx_releases_storage_path on releases (storage_path);
		create index if not exists idx_manage_sessions_expires_at on manage_sessions (expires_at);
		create index if not exists idx_oidc_states_expires_at on oidc_states (expires_at);
		create index if not exists idx_oidc_sessions_expires_at on oidc_sessions (expires_at);
		create index if not exists idx_artifact_deletions_due on artifact_deletions (next_attempt_at, created_at, storage_path);
		alter table releases add column if not exists md5 text not null default '';
		alter table modules add column if not exists upstream_refreshed_at timestamptz;
	`

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire postgres schema setup connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `select pg_advisory_lock($1)`, postgresSchemaLockID); err != nil {
		return fmt.Errorf("lock postgres schema setup: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), postgresAdvisoryUnlockTimeout)
		defer cancel()
		var unlocked bool
		if err := conn.QueryRow(unlockCtx, `select pg_advisory_unlock($1)`, postgresSchemaLockID).Scan(&unlocked); err != nil {
			closePostgresConnection(conn)
			returnErr = errors.Join(returnErr, fmt.Errorf("unlock postgres schema setup: %w", err))
		} else if !unlocked {
			closePostgresConnection(conn)
			returnErr = errors.Join(returnErr, errors.New("unlock postgres schema setup: lock was not held"))
		}
	}()

	if _, err := conn.Exec(ctx, query); err != nil {
		return fmt.Errorf("ensure operational tables: %w", err)
	}
	if err := rejectNewerPostgresSchema(ctx, conn); err != nil {
		return err
	}
	if err := migratePostgresCanonicalSlugs(ctx, conn); err != nil {
		return err
	}
	if err := migratePostgresDataConstraints(ctx, conn); err != nil {
		return err
	}
	if err := migratePostgresAccessTokens(ctx, conn, s.tokenHasher); err != nil {
		return err
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
	if _, err := conn.Exec(ctx, `
		create index if not exists idx_modules_listing on modules (updated_at desc, owner, name, id);
		create index if not exists idx_modules_upstream_refresh on modules (upstream_refreshed_at asc nulls first, owner, name, id);
		create index if not exists idx_releases_module_created on releases (module_id, created_at desc, version);
		create index if not exists idx_release_usage_last_used_at on release_usage (last_used_at);
		create index if not exists idx_access_tokens_team_type on access_tokens (team, token_type);
		create index if not exists idx_access_tokens_expires_at on access_tokens (expires_at) where expires_at is not null;
		create index if not exists idx_access_tokens_revoked_at on access_tokens (revoked_at) where revoked_at is not null;
		create index if not exists idx_rate_limits_reset_at on rate_limits (reset_at);
	`); err != nil {
		return fmt.Errorf("ensure postgres query indexes: %w", err)
	}
	if _, err := conn.Exec(ctx, `update schema_metadata set version = $1 where id = 1`, currentSchemaVersion); err != nil {
		return fmt.Errorf("update postgres schema version: %w", err)
	}
	return nil
}

func (s *PostgresStore) ConsumeRateLimit(ctx context.Context, key string, limit int, window time.Duration, now time.Time) (bool, error) {
	if strings.TrimSpace(key) == "" || limit <= 0 || window <= 0 {
		return false, errors.New("invalid rate limit")
	}
	var allowed bool
	err := s.pool.QueryRow(ctx, `
		insert into rate_limits (limiter_key, request_count, reset_at, updated_at)
		values ($1, 1, $2, $3)
		on conflict (limiter_key) do update set
			request_count = case
				when rate_limits.reset_at <= $3 then 1
				else least(rate_limits.request_count + 1, $4 + 1)
			end,
			reset_at = case when rate_limits.reset_at <= $3 then $2 else rate_limits.reset_at end,
			updated_at = $3
		returning request_count <= $4
	`, key, now.UTC().Add(window), now.UTC(), limit).Scan(&allowed)
	if err != nil {
		return false, fmt.Errorf("consume rate limit: %w", err)
	}
	return allowed, nil
}

func (s *PostgresStore) PurgeRateLimits(ctx context.Context, before time.Time) (int64, error) {
	result, err := s.pool.Exec(ctx, `delete from rate_limits where reset_at < $1`, before.UTC())
	if err != nil {
		return 0, fmt.Errorf("purge rate limits: %w", err)
	}
	return result.RowsAffected(), nil
}

func closePostgresConnection(conn *pgxpool.Conn) {
	closeCtx, cancel := context.WithTimeout(context.Background(), postgresAdvisoryUnlockTimeout)
	defer cancel()
	_ = conn.Conn().Close(closeCtx)
}

func rejectNewerPostgresSchema(ctx context.Context, conn *pgxpool.Conn) error {
	var version int
	if err := conn.QueryRow(ctx, `select version from schema_metadata where id = 1`).Scan(&version); err != nil {
		return fmt.Errorf("read postgres schema version: %w", err)
	}
	if version > currentSchemaVersion {
		return fmt.Errorf("postgres schema version %d is newer than supported version %d", version, currentSchemaVersion)
	}
	return nil
}

func migratePostgresDataConstraints(ctx context.Context, conn *pgxpool.Conn) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin postgres data constraint migration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		do $$ begin
			if not exists (select 1 from pg_constraint where conrelid = 'modules'::regclass and conname = 'modules_owner_nonempty') then
				alter table modules add constraint modules_owner_nonempty check (btrim(owner) <> '');
			end if;
			if not exists (select 1 from pg_constraint where conrelid = 'modules'::regclass and conname = 'modules_name_nonempty') then
				alter table modules add constraint modules_name_nonempty check (btrim(name) <> '');
			end if;
			if not exists (select 1 from pg_constraint where conrelid = 'modules'::regclass and conname = 'modules_slug_nonempty') then
				alter table modules add constraint modules_slug_nonempty check (btrim(slug) <> '');
			end if;
			if not exists (select 1 from pg_constraint where conrelid = 'releases'::regclass and conname = 'releases_version_nonempty') then
				alter table releases add constraint releases_version_nonempty check (btrim(version) <> '');
			end if;
			if not exists (select 1 from pg_constraint where conrelid = 'releases'::regclass and conname = 'releases_slug_nonempty') then
				alter table releases add constraint releases_slug_nonempty check (btrim(slug) <> '');
			end if;
			if not exists (select 1 from pg_constraint where conrelid = 'releases'::regclass and conname = 'releases_size_nonnegative') then
				alter table releases add constraint releases_size_nonnegative check (size_bytes >= 0);
			end if;
			if not exists (select 1 from pg_constraint where conrelid = 'releases'::regclass and conname = 'releases_source_valid') then
				alter table releases add constraint releases_source_valid check (source in ('local', 'upstream'));
			end if;
		end $$;
	`); err != nil {
		return fmt.Errorf("migrate postgres data constraints: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit postgres data constraint migration: %w", err)
	}
	return nil
}

func migratePostgresCanonicalSlugs(ctx context.Context, conn *pgxpool.Conn) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin postgres canonical slug migration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		alter table modules add column if not exists slug text;
		alter table releases add column if not exists slug text;
		update modules set slug = owner || '-' || name where slug is null or slug = '';
		update releases r set slug = m.slug || '-' || r.version from modules m
		where m.id = r.module_id and (r.slug is null or r.slug = '');
		alter table modules alter column slug set not null;
		alter table releases alter column slug set not null;
		create unique index if not exists idx_modules_slug_unique on modules (slug);
		create unique index if not exists idx_releases_slug_unique on releases (slug);
	`); err != nil {
		return fmt.Errorf("migrate postgres canonical slugs (canonical slug collision): %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit postgres canonical slug migration: %w", err)
	}
	return nil
}

func migratePostgresAccessTokens(ctx context.Context, conn *pgxpool.Conn, tokenHasher *auth.TokenHasher) error {
	var legacy bool
	if err := conn.QueryRow(ctx, `
		select exists (
			select 1 from information_schema.columns
			where table_schema = current_schema() and table_name = 'access_tokens' and column_name = 'token'
		)
	`).Scan(&legacy); err != nil {
		return fmt.Errorf("check postgres access token migration state: %w", err)
	}
	if !legacy {
		return nil
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin postgres access token migration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `select team, token_type, token from access_tokens order by team, token_type, token`)
	if err != nil {
		return fmt.Errorf("read legacy postgres access tokens: %w", err)
	}
	type legacyToken struct{ team, kind, raw string }
	var tokens []legacyToken
	for rows.Next() {
		var token legacyToken
		if err := rows.Scan(&token.team, &token.kind, &token.raw); err != nil {
			rows.Close()
			return fmt.Errorf("scan legacy postgres access token: %w", err)
		}
		tokens = append(tokens, token)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate legacy postgres access tokens: %w", err)
	}
	if len(tokens) > 0 && tokenHasher == nil {
		return errors.New("ACCESS_TOKEN_PEPPER is required to migrate legacy access tokens")
	}
	if _, err := tx.Exec(ctx, `
		create table access_tokens_next (
			token_id text primary key,
			team text not null references access_teams (team) on delete cascade,
			token_type text not null,
			token_prefix text not null,
			token_hash text not null unique,
			description text not null default '',
			created_at timestamptz not null default now(),
			expires_at timestamptz,
			revoked_at timestamptz,
			last_used_at timestamptz
		)
	`); err != nil {
		return fmt.Errorf("create postgres access token migration table: %w", err)
	}
	for _, token := range tokens {
		record, err := accessTokenRecordFromRaw(tokenHasher, token.kind, token.raw)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `insert into access_tokens_next (token_id, team, token_type, token_prefix, token_hash, created_at) values ($1, $2, $3, $4, $5, $6)`,
			record.ID, token.team, token.kind, record.Prefix, record.Digest, record.CreatedAt); err != nil {
			return fmt.Errorf("migrate postgres access token: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `drop table access_tokens; alter table access_tokens_next rename to access_tokens`); err != nil {
		return fmt.Errorf("replace postgres access token table: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit postgres access token migration: %w", err)
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
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin access snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `select team from access_teams order by team`)
	if err != nil {
		return nil, fmt.Errorf("list access teams: %w", err)
	}
	defer rows.Close()

	configs, err := loadTeamConfigs(ctx, rows, postgresAccessConfigLoader{queryer: tx})
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit access snapshot: %w", err)
	}
	return configs, nil
}

func (s *PostgresStore) ReplaceTeamConfigs(ctx context.Context, configs []auth.TeamConfig) error {
	if _, err := auth.NewAuthorizer(configs); err != nil {
		return fmt.Errorf("validate access config: %w", err)
	}

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
	writer := postgresAccessConfigWriter{ctx: ctx, tx: tx}
	if err := persistTeamConfigs(configs, s.tokenHasher, writer); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit access tx: %w", err)
	}
	return nil
}

type postgresAccessConfigWriter struct {
	ctx context.Context
	tx  pgx.Tx
}

func (w postgresAccessConfigWriter) insertTeam(team string) error {
	if _, err := w.tx.Exec(w.ctx, `insert into access_teams (team) values ($1)`, team); err != nil {
		return fmt.Errorf("insert access team: %w", err)
	}
	return nil
}

func (w postgresAccessConfigWriter) insertToken(team, tokenType string, token auth.AccessTokenRecord) error {
	return insertPostgresAccessTokenRecord(w.ctx, w.tx, team, tokenType, token)
}

func (w postgresAccessConfigWriter) insertOwner(team, owner string) error {
	if _, err := w.tx.Exec(w.ctx, `insert into access_publish_owners (team, owner) values ($1, $2)`, team, owner); err != nil {
		return fmt.Errorf("insert access owner: %w", err)
	}
	return nil
}

func (w postgresAccessConfigWriter) insertOIDCMapping(team string, mapping accessOIDCMapping) error {
	if _, err := w.tx.Exec(w.ctx, `insert into access_oidc_mappings (team, mapping_type, value) values ($1, $2, $3)`, team, mapping.kind, mapping.value); err != nil {
		return fmt.Errorf("insert oidc mapping: %w", err)
	}
	return nil
}

func (s *PostgresStore) IsAccessTokenActive(ctx context.Context, tokenID string, now time.Time) (bool, error) {
	if strings.TrimSpace(tokenID) == "" {
		return false, errors.New("access token id is required")
	}
	var active bool
	if err := s.pool.QueryRow(ctx, `
		select exists (
			select 1
			from access_tokens
			where token_id = $1
			  and revoked_at is null
			  and (expires_at is null or expires_at > $2)
		)
	`, tokenID, now.UTC()).Scan(&active); err != nil {
		return false, fmt.Errorf("check access token activity: %w", err)
	}
	return active, nil
}

func (s *PostgresStore) MarkAccessTokenUsed(ctx context.Context, tokenID string, usedAt time.Time) error {
	if strings.TrimSpace(tokenID) == "" {
		return errors.New("access token id is required")
	}
	usedAt = usedAt.UTC()
	if _, err := s.pool.Exec(ctx, `
		update access_tokens
		set last_used_at = $2
		where token_id = $1
		  and revoked_at is null
		  and (last_used_at is null or last_used_at < $3)
	`, tokenID, usedAt, usedAt.Add(-time.Minute)); err != nil {
		return fmt.Errorf("mark access token used: %w", err)
	}
	return nil
}

func (s *PostgresStore) CreateManageSession(ctx context.Context, session ManageSession) error {
	if session.SessionHash == "" || session.CredentialHash == "" || session.CSRFSecret == "" || session.AuthMethod == "" {
		return errors.New("manage session is incomplete")
	}
	if _, err := s.pool.Exec(ctx, `
		insert into manage_sessions (session_hash, credential_hash, credential_id, auth_method, csrf_secret, created_at, expires_at, last_seen_at, revoked_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, session.SessionHash, session.CredentialHash, session.CredentialID, session.AuthMethod, session.CSRFSecret,
		session.CreatedAt, session.ExpiresAt, session.LastSeenAt, session.RevokedAt); err != nil {
		return fmt.Errorf("create manage session: %w", err)
	}
	return nil
}

func (s *PostgresStore) GetManageSession(ctx context.Context, sessionHash string, now time.Time) (ManageSession, error) {
	var session ManageSession
	var revokedAt sql.NullTime
	err := s.pool.QueryRow(ctx, `
		select session_hash, credential_hash, credential_id, auth_method, csrf_secret, created_at, expires_at, last_seen_at, revoked_at
		from manage_sessions
		where session_hash = $1 and revoked_at is null and expires_at > $2
	`, sessionHash, now.UTC()).Scan(
		&session.SessionHash, &session.CredentialHash, &session.CredentialID, &session.AuthMethod, &session.CSRFSecret,
		&session.CreatedAt, &session.ExpiresAt, &session.LastSeenAt, &revokedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ManageSession{}, ErrNotFound
	}
	if err != nil {
		return ManageSession{}, fmt.Errorf("get manage session: %w", err)
	}
	session.RevokedAt = nullablePostgresTime(revokedAt)
	if session.LastSeenAt.Before(now.UTC().Add(-time.Minute)) {
		if _, err := s.pool.Exec(ctx, `update manage_sessions set last_seen_at = $2 where session_hash = $1 and last_seen_at < $3`,
			sessionHash, now.UTC(), now.UTC().Add(-time.Minute)); err != nil {
			slog.Warn("touch manage session failed", "err", err)
		}
	}
	return session, nil
}

func (s *PostgresStore) RevokeManageSession(ctx context.Context, sessionHash string, revokedAt time.Time) error {
	if _, err := s.pool.Exec(ctx, `update manage_sessions set revoked_at = $2 where session_hash = $1 and revoked_at is null`, sessionHash, revokedAt.UTC()); err != nil {
		return fmt.Errorf("revoke manage session: %w", err)
	}
	return nil
}

func (s *PostgresStore) CreateOIDCState(ctx context.Context, state OIDCState) error {
	if state.StateHash == "" || state.Nonce == "" || state.PKCEVerifier == "" {
		return errors.New("oidc state is incomplete")
	}
	if _, err := s.pool.Exec(ctx, `
		insert into oidc_states (state_hash, nonce, pkce_verifier, next_path, created_at, expires_at)
		values ($1, $2, $3, $4, $5, $6)
	`, state.StateHash, state.Nonce, state.PKCEVerifier, state.NextPath, state.CreatedAt.UTC(), state.ExpiresAt.UTC()); err != nil {
		return fmt.Errorf("create oidc state: %w", err)
	}
	return nil
}

func (s *PostgresStore) ConsumeOIDCState(ctx context.Context, stateHash string, now time.Time) (OIDCState, error) {
	var state OIDCState
	err := s.pool.QueryRow(ctx, `
		update oidc_states
		set consumed_at = $2
		where state_hash = $1 and consumed_at is null and expires_at > $2
		returning state_hash, nonce, pkce_verifier, next_path, created_at, expires_at
	`, stateHash, now.UTC()).Scan(
		&state.StateHash, &state.Nonce, &state.PKCEVerifier, &state.NextPath, &state.CreatedAt, &state.ExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return OIDCState{}, ErrNotFound
	}
	if err != nil {
		return OIDCState{}, fmt.Errorf("consume oidc state: %w", err)
	}
	return state, nil
}

func (s *PostgresStore) CreateOIDCSession(ctx context.Context, session OIDCSession) error {
	if session.SessionHash == "" || session.Subject == "" || session.CSRFSecret == "" {
		return errors.New("oidc session is incomplete")
	}
	groupsJSON, err := json.Marshal(session.Groups)
	if err != nil {
		return fmt.Errorf("encode oidc session groups: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `
		insert into oidc_sessions (session_hash, subject, email, name, groups_json, csrf_secret, created_at, expires_at, last_seen_at, revoked_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, session.SessionHash, session.Subject, session.Email, session.Name, groupsJSON, session.CSRFSecret,
		session.CreatedAt.UTC(), session.ExpiresAt.UTC(), session.LastSeenAt.UTC(), session.RevokedAt); err != nil {
		return fmt.Errorf("create oidc session: %w", err)
	}
	return nil
}

func (s *PostgresStore) GetOIDCSession(ctx context.Context, sessionHash string, now time.Time) (OIDCSession, error) {
	var session OIDCSession
	var groupsJSON []byte
	var revokedAt sql.NullTime
	err := s.pool.QueryRow(ctx, `
		select session_hash, subject, email, name, groups_json, csrf_secret, created_at, expires_at, last_seen_at, revoked_at
		from oidc_sessions
		where session_hash = $1 and revoked_at is null and expires_at > $2
	`, sessionHash, now.UTC()).Scan(
		&session.SessionHash, &session.Subject, &session.Email, &session.Name, &groupsJSON, &session.CSRFSecret,
		&session.CreatedAt, &session.ExpiresAt, &session.LastSeenAt, &revokedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return OIDCSession{}, ErrNotFound
	}
	if err != nil {
		return OIDCSession{}, fmt.Errorf("get oidc session: %w", err)
	}
	if err := json.Unmarshal(groupsJSON, &session.Groups); err != nil {
		return OIDCSession{}, fmt.Errorf("decode oidc session groups: %w", err)
	}
	session.RevokedAt = nullablePostgresTime(revokedAt)
	if session.LastSeenAt.Before(now.UTC().Add(-time.Minute)) {
		if _, err := s.pool.Exec(ctx, `update oidc_sessions set last_seen_at = $2 where session_hash = $1 and last_seen_at < $3`,
			sessionHash, now.UTC(), now.UTC().Add(-time.Minute)); err != nil {
			slog.Warn("touch oidc session failed", "err", err)
		}
	}
	return session, nil
}

func (s *PostgresStore) PurgeSessionState(ctx context.Context, now, historyBefore time.Time) (SessionCleanupResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SessionCleanupResult{}, fmt.Errorf("begin session cleanup: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var result SessionCleanupResult
	manageTag, err := tx.Exec(ctx, `delete from manage_sessions where expires_at < $1 or (revoked_at is not null and revoked_at < $2)`, now.UTC(), historyBefore.UTC())
	if err != nil {
		return result, fmt.Errorf("purge manage sessions: %w", err)
	}
	result.ManageSessions = manageTag.RowsAffected()
	stateTag, err := tx.Exec(ctx, `delete from oidc_states where expires_at < $1 or (consumed_at is not null and consumed_at < $2)`, now.UTC(), historyBefore.UTC())
	if err != nil {
		return result, fmt.Errorf("purge oidc states: %w", err)
	}
	result.OIDCStates = stateTag.RowsAffected()
	sessionTag, err := tx.Exec(ctx, `delete from oidc_sessions where expires_at < $1 or (revoked_at is not null and revoked_at < $2)`, now.UTC(), historyBefore.UTC())
	if err != nil {
		return result, fmt.Errorf("purge oidc sessions: %w", err)
	}
	result.OIDCSessions = sessionTag.RowsAffected()
	if err := tx.Commit(ctx); err != nil {
		return result, fmt.Errorf("commit session cleanup: %w", err)
	}
	return result, nil
}

func (s *PostgresStore) RevokeOIDCSession(ctx context.Context, sessionHash string, revokedAt time.Time) error {
	if _, err := s.pool.Exec(ctx, `update oidc_sessions set revoked_at = $2 where session_hash = $1 and revoked_at is null`, sessionHash, revokedAt.UTC()); err != nil {
		return fmt.Errorf("revoke oidc session: %w", err)
	}
	return nil
}

func insertPostgresAccessTokenRecord(ctx context.Context, tx pgx.Tx, team, tokenType string, token auth.AccessTokenRecord) error {
	if token.ID == "" || token.Digest == "" || token.Prefix == "" {
		return errors.New("access token record is incomplete")
	}
	if _, err := tx.Exec(ctx, `insert into access_tokens (token_id, team, token_type, token_prefix, token_hash, description, created_at, expires_at, revoked_at, last_used_at) values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		token.ID, team, tokenType, token.Prefix, token.Digest, token.Description, token.CreatedAt, token.ExpiresAt, token.RevokedAt, token.LastUsedAt); err != nil {
		return fmt.Errorf("insert access token: %w", err)
	}
	return nil
}

func (l postgresAccessConfigLoader) loadAccessTokens(ctx context.Context, configs []auth.TeamConfig, index map[string]int) error {
	rows, err := l.queryer.Query(ctx, `select team, token_type, token_id, token_prefix, token_hash, description, created_at, expires_at, revoked_at, last_used_at from access_tokens order by team, token_type, token_prefix`)
	if err != nil {
		return fmt.Errorf("list access tokens: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var team, tokenType string
		var token auth.AccessTokenRecord
		var expiresAt, revokedAt, lastUsedAt sql.NullTime
		if err := rows.Scan(&team, &tokenType, &token.ID, &token.Prefix, &token.Digest, &token.Description, &token.CreatedAt, &expiresAt, &revokedAt, &lastUsedAt); err != nil {
			return fmt.Errorf("scan access token: %w", err)
		}
		token.ExpiresAt = nullablePostgresTime(expiresAt)
		token.RevokedAt = nullablePostgresTime(revokedAt)
		token.LastUsedAt = nullablePostgresTime(lastUsedAt)
		if i, ok := index[team]; ok {
			applyAccessToken(&configs[i], tokenType, token)
		}
	}
	return rows.Err()
}

func (s *PostgresStore) PurgeAccessTokenHistory(ctx context.Context, cutoff time.Time) (int64, error) {
	result, err := s.pool.Exec(ctx, `
		delete from access_tokens
		where (revoked_at is not null and revoked_at < $1)
		   or (revoked_at is null and expires_at is not null and expires_at < $1)
	`, cutoff.UTC())
	if err != nil {
		return 0, fmt.Errorf("purge access token history: %w", err)
	}
	return result.RowsAffected(), nil
}

func nullablePostgresTime(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	parsed := value.Time.UTC()
	return &parsed
}

func (l postgresAccessConfigLoader) loadAccessOwners(ctx context.Context, configs []auth.TeamConfig, index map[string]int) error {
	rows, err := l.queryer.Query(ctx, `select team, owner from access_publish_owners order by team, owner`)
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

func (l postgresAccessConfigLoader) loadAccessOIDC(ctx context.Context, configs []auth.TeamConfig, index map[string]int) error {
	rows, err := l.queryer.Query(ctx, `select team, mapping_type, value from access_oidc_mappings order by team, mapping_type, value`)
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
		insert into modules (id, owner, name, slug, created_at, updated_at)
		values ($1, $2, $3, $4, now(), now())
		on conflict (owner, name)
		do update set slug = excluded.slug
		returning id, owner, name, coalesce(latest_version, ''), created_at, updated_at
	`

	module := domain.Module{}
	err := s.pool.QueryRow(ctx, query, uuid.NewString(), owner, name, moduleSlug(owner, name)).Scan(
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
	return s.createRelease(ctx, release, false)
}

func (s *PostgresStore) CreateReleaseIfAbsent(ctx context.Context, release domain.Release) (domain.Release, error) {
	return s.createRelease(ctx, release, true)
}

func (s *PostgresStore) createRelease(ctx context.Context, release domain.Release, createOnly bool) (domain.Release, error) {
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

	insertRelease := `
		insert into releases (
			id, module_id, slug, source, version, description, readme, file_name, content_type, size_bytes,
			md5, sha256, storage_path, upstream_slug, upstream_file_uri, metadata, created_at
		)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, now())
		on conflict (module_id, version) `
	if createOnly {
		insertRelease += `do nothing returning created_at`
	} else {
		insertRelease += `do update set
			source = excluded.source,
			description = excluded.description,
			readme = excluded.readme,
			file_name = excluded.file_name,
			content_type = excluded.content_type,
			size_bytes = excluded.size_bytes,
			md5 = excluded.md5,
			sha256 = excluded.sha256,
			storage_path = excluded.storage_path,
			upstream_slug = excluded.upstream_slug,
			upstream_file_uri = excluded.upstream_file_uri,
			metadata = excluded.metadata
		returning created_at`
	}

	err = tx.QueryRow(ctx, insertRelease,
		release.ID,
		release.ModuleID,
		releaseSlug(release.Owner, release.Name, release.Version),
		release.Source,
		release.Version,
		release.Description,
		release.Readme,
		release.FileName,
		release.ContentType,
		release.SizeBytes,
		release.MD5,
		release.SHA256,
		release.StoragePath,
		release.UpstreamSlug,
		release.UpstreamFileURI,
		metadataJSON,
	).Scan(&release.CreatedAt)
	if err != nil {
		if createOnly && errors.Is(err, pgx.ErrNoRows) {
			return domain.Release{}, ErrConflict
		}
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

func (s *PostgresStore) UpdateReleaseChecksums(ctx context.Context, owner, name, version, md5, sha256, storagePath string, sizeBytes int64) error {
	tag, err := s.pool.Exec(ctx, `
		update releases r
		set md5 = $4, sha256 = $5, storage_path = $6, size_bytes = $7
		from modules m
		where r.module_id = m.id
			and m.owner = $1
			and m.name = $2
			and r.version = $3
	`, owner, name, version, md5, sha256, storagePath, sizeBytes)
	if err != nil {
		return fmt.Errorf("update release checksums: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) ListModules(ctx context.Context, limit int) ([]domain.Module, error) {
	modules, _, err := s.ListModulesPage(ctx, limit, 0)
	return modules, err
}

func (s *PostgresStore) ListModulesPage(ctx context.Context, limit, offset int) ([]domain.Module, int, error) {
	return s.ListModulesPageFiltered(ctx, nil, "", limit, offset)
}

func (s *PostgresStore) ListModulesPageFiltered(ctx context.Context, owners []string, search string, limit, offset int) ([]domain.Module, int, error) {
	return s.ListModulesPagePrioritized(ctx, owners, nil, search, limit, offset)
}

func (s *PostgresStore) ListModulesPagePrioritized(ctx context.Context, owners, priorityOwners []string, search string, limit, offset int) ([]domain.Module, int, error) {
	if err := validateModulePagination(limit, offset); err != nil {
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
			and ($1 = '' or strpos(lower(owner || '/' || name), lower($1)) > 0)
			and (coalesce(cardinality($2::text[]), 0) = 0 or owner = any($2::text[]))
		order by case when owner = any($3::text[]) then 0 else 1 end,
			updated_at desc, owner asc, name asc, id asc
		limit $4
		offset $5
	`

	search = strings.TrimSpace(search)
	total, err := s.countFilteredModulesWithReleases(ctx, owners, search)
	if err != nil {
		return nil, 0, err
	}
	rows, err := s.pool.Query(ctx, query, search, owners, priorityOwners, limit, offset)
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

func (s *PostgresStore) countFilteredModulesWithReleases(ctx context.Context, owners []string, search string) (int, error) {
	const query = `
		select count(*)
		from modules
		where exists (
			select 1
			from releases
			where releases.module_id = modules.id
		)
			and ($1 = '' or strpos(lower(owner || '/' || name), lower($1)) > 0)
			and (coalesce(cardinality($2::text[]), 0) = 0 or owner = any($2::text[]))
	`
	var total int
	if err := s.pool.QueryRow(ctx, query, search, owners).Scan(&total); err != nil {
		return 0, fmt.Errorf("count modules: %w", err)
	}
	return total, nil
}

func (s *PostgresStore) CountModulesByOwner(ctx context.Context, owners []string) (map[string]int, error) {
	if owners != nil && len(owners) == 0 {
		return map[string]int{}, nil
	}
	rows, err := s.pool.Query(ctx, `
		select owner, count(*)
		from modules
		where exists (
			select 1
			from releases
			where releases.module_id = modules.id
		)
		  and ($1::text[] is null or owner = any($1::text[]))
		group by owner
	`, owners)
	if err != nil {
		return nil, fmt.Errorf("count modules by owner: %w", err)
	}
	defer rows.Close()

	counts := map[string]int{}
	for rows.Next() {
		var owner string
		var count int
		if err := rows.Scan(&owner, &count); err != nil {
			return nil, fmt.Errorf("scan module owner count: %w", err)
		}
		counts[owner] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read module owner counts: %w", err)
	}
	return counts, nil
}

func (s *PostgresStore) CountUpstreamModulesByOwner(ctx context.Context) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, `
		select m.owner, count(*)
		from modules m
		where exists (
			select 1
			from releases r
			where r.module_id = m.id and r.source = 'upstream'
		)
		group by m.owner
	`)
	if err != nil {
		return nil, fmt.Errorf("count upstream modules by owner: %w", err)
	}
	defer rows.Close()

	counts := map[string]int{}
	for rows.Next() {
		var owner string
		var count int
		if err := rows.Scan(&owner, &count); err != nil {
			return nil, fmt.Errorf("scan upstream module owner count: %w", err)
		}
		counts[owner] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read upstream module owner counts: %w", err)
	}
	return counts, nil
}

func (s *PostgresStore) ListUpstreamModules(ctx context.Context, limit int) ([]domain.Module, error) {
	const query = `
		select m.id, m.owner, m.name, coalesce(m.latest_version, ''), m.created_at, m.updated_at
		from modules m
		where exists (
			select 1 from releases r
			where r.module_id = m.id and coalesce(r.source, 'local') = 'upstream'
		)
		order by m.upstream_refreshed_at asc nulls first, m.owner, m.name, m.id
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

func (s *PostgresStore) MarkUpstreamModuleRefreshAttempt(ctx context.Context, owner, name string, attemptedAt time.Time) error {
	result, err := s.pool.Exec(ctx, `
		update modules
		set upstream_refreshed_at = $1
		where owner = $2 and name = $3
	`, attemptedAt.UTC(), owner, name)
	if err != nil {
		return fmt.Errorf("mark upstream module refresh attempt: %w", err)
	}
	if result.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) DeleteModule(ctx context.Context, owner, name string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	if _, err := tx.Exec(ctx, `
		insert into artifact_deletions (storage_path, owner, name)
		select r.storage_path, m.owner, m.name
		from releases r
		join modules m on m.id = r.module_id
		where m.owner = $1 and m.name = $2 and r.source = 'local' and r.storage_path <> ''
		on conflict(storage_path) do nothing
	`, owner, name); err != nil {
		return fmt.Errorf("queue module artifact deletions: %w", err)
	}

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

func (s *PostgresStore) DeleteModuleIfEmpty(ctx context.Context, owner, name string) error {
	_, err := s.pool.Exec(ctx, `
		delete from modules m
		where m.owner = $1 and m.name = $2
		  and not exists (select 1 from releases r where r.module_id = m.id)
	`, owner, name)
	if err != nil {
		return fmt.Errorf("delete empty module: %w", err)
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

	var source, storagePath string
	const selectSource = `
		select r.source, r.storage_path
		from releases r
		join modules m on m.id = r.module_id
		where m.owner = $1 and m.name = $2 and r.version = $3
	`
	if err := tx.QueryRow(ctx, selectSource, owner, name, version).Scan(&source, &storagePath); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("get release source: %w", err)
	}
	if source == "local" && storagePath != "" {
		if _, err := tx.Exec(ctx, `
			insert into artifact_deletions (storage_path, owner, name)
			values ($1, $2, $3)
			on conflict(storage_path) do nothing
		`, storagePath, owner, name); err != nil {
			return fmt.Errorf("queue release artifact deletion: %w", err)
		}
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

func (s *PostgresStore) PurgeDeletedReleases(ctx context.Context, cutoff time.Time) (int64, error) {
	result, err := s.pool.Exec(ctx, `delete from deleted_releases where deleted_at < $1`, cutoff.UTC())
	if err != nil {
		return 0, fmt.Errorf("purge deleted releases: %w", err)
	}
	return result.RowsAffected(), nil
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

func (s *PostgresStore) ListActiveReleasesForModules(ctx context.Context, since time.Time, modules []domain.Module) ([]ReleaseSummary, error) {
	if len(modules) == 0 {
		return nil, nil
	}
	owners := make([]string, len(modules))
	names := make([]string, len(modules))
	for i, module := range modules {
		owners[i] = module.Owner
		names[i] = module.Name
	}
	rows, err := s.pool.Query(ctx, `
		with selected(owner, name) as (
			select * from unnest($2::text[], $3::text[])
		)
		select usage.owner, usage.name, usage.version, usage.last_used_at
		from release_usage usage
		join selected using (owner, name)
		where usage.last_used_at >= $1
		order by usage.owner, usage.name, usage.version
	`, since.UTC(), owners, names)
	if err != nil {
		return nil, fmt.Errorf("list active releases for modules: %w", err)
	}
	defer rows.Close()

	var releases []ReleaseSummary
	for rows.Next() {
		var release ReleaseSummary
		if err := rows.Scan(&release.Owner, &release.Name, &release.Version, &release.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan active release for module: %w", err)
		}
		releases = append(releases, release)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read active releases for modules: %w", err)
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

	latest := ""
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return "", fmt.Errorf("scan version for latest: %w", err)
		}
		latest = latestVersionWithCandidate(latest, version)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return latest, nil
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

func (s *PostgresStore) GetModuleBySlug(ctx context.Context, slug string) (domain.Module, error) {
	const query = `
		select id, owner, name, coalesce(latest_version, ''), created_at, updated_at
		from modules
		where slug = $1
	`
	var module domain.Module
	err := s.pool.QueryRow(ctx, query, slug).Scan(
		&module.ID, &module.Owner, &module.Name, &module.LatestVersion, &module.CreatedAt, &module.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Module{}, ErrNotFound
	}
	if err != nil {
		return domain.Module{}, fmt.Errorf("get module by slug: %w", err)
	}
	return module, nil
}

func (s *PostgresStore) GetModuleForReleaseSlug(ctx context.Context, releaseSlug string) (domain.Module, error) {
	const query = `
		select id, owner, name, coalesce(latest_version, ''), created_at, updated_at
		from modules
		where $1 like slug || '-%'
		order by length(slug) desc
		limit 1
	`
	var module domain.Module
	err := s.pool.QueryRow(ctx, query, releaseSlug).Scan(
		&module.ID, &module.Owner, &module.Name, &module.LatestVersion, &module.CreatedAt, &module.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Module{}, ErrNotFound
	}
	if err != nil {
		return domain.Module{}, fmt.Errorf("get module for release slug: %w", err)
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

func (s *PostgresStore) ListReleasesForModules(ctx context.Context, modules []domain.Module) ([]ModuleReleaseSummary, error) {
	if len(modules) == 0 {
		return nil, nil
	}
	owners := make([]string, len(modules))
	names := make([]string, len(modules))
	for i, module := range modules {
		owners[i] = module.Owner
		names[i] = module.Name
	}
	const query = `
		select m.owner, m.name, r.version, r.created_at
		from unnest($1::text[], $2::text[]) as selected(owner, name)
		join modules m on m.owner = selected.owner and m.name = selected.name
		join releases r on r.module_id = m.id
		order by m.owner, m.name, r.created_at desc, r.version desc
	`
	rows, err := s.pool.Query(ctx, query, owners, names)
	if err != nil {
		return nil, fmt.Errorf("list releases for modules: %w", err)
	}
	defer rows.Close()

	var releases []ModuleReleaseSummary
	for rows.Next() {
		var release ModuleReleaseSummary
		if err := rows.Scan(&release.Owner, &release.Name, &release.Version, &release.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan module release summary: %w", err)
		}
		releases = append(releases, release)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortModuleReleaseSummaries(releases)
	return releases, nil
}

func (s *PostgresStore) CountReleasesForModules(ctx context.Context, modules []domain.Module) ([]ModuleReleaseCount, error) {
	if len(modules) == 0 {
		return nil, nil
	}
	owners := make([]string, len(modules))
	names := make([]string, len(modules))
	for i, module := range modules {
		owners[i] = module.Owner
		names[i] = module.Name
	}
	const query = `
		select selected.owner, selected.name, count(r.id)
		from unnest($1::text[], $2::text[]) as selected(owner, name)
		left join modules m on m.owner = selected.owner and m.name = selected.name
		left join releases r on r.module_id = m.id
		group by selected.owner, selected.name
		order by selected.owner, selected.name
	`
	rows, err := s.pool.Query(ctx, query, owners, names)
	if err != nil {
		return nil, fmt.Errorf("count releases for modules: %w", err)
	}
	defer rows.Close()
	var counts []ModuleReleaseCount
	for rows.Next() {
		var count ModuleReleaseCount
		if err := rows.Scan(&count.Owner, &count.Name, &count.Count); err != nil {
			return nil, fmt.Errorf("scan module release count: %w", err)
		}
		counts = append(counts, count)
	}
	return counts, rows.Err()
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

func (s *PostgresStore) ListArtifactReleases(ctx context.Context) ([]ArtifactReleaseRecord, error) {
	const query = `
		select m.owner, m.name, r.version, r.storage_path, r.sha256, r.size_bytes
		from releases r
		join modules m on m.id = r.module_id
		where r.storage_path <> ''
		order by r.storage_path
	`
	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list artifact releases: %w", err)
	}
	defer rows.Close()
	return scanArtifactReleaseRows(rows)
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
			r.file_name, r.content_type, r.size_bytes, r.md5, r.sha256, r.storage_path, coalesce(r.upstream_slug, ''), coalesce(r.upstream_file_uri, ''),
			r.metadata, r.created_at
		from releases r
		join modules m on m.id = r.module_id
		where m.owner = $1 and m.name = $2 and r.version = $3
	`

	return scanPostgresRelease(s.pool.QueryRow(ctx, query, owner, name, version), "get release")
}

func scanPostgresRelease(scanner moduleScanner, action string) (domain.Release, error) {
	var release domain.Release
	var metadataJSON []byte
	err := scanner.Scan(
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
		&release.MD5,
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
		return domain.Release{}, fmt.Errorf("%s: %w", action, err)
	}

	if len(metadataJSON) > 0 {
		if err := json.Unmarshal(metadataJSON, &release.Metadata); err != nil {
			return domain.Release{}, fmt.Errorf("unmarshal metadata: %w", err)
		}
	}
	normalizeReleaseMetadata(&release)

	return release, nil
}

func (s *PostgresStore) GetReleaseBySlug(ctx context.Context, slug string) (domain.Release, error) {
	const query = `
		select
			r.id, r.module_id, m.owner, m.name, coalesce(r.source, 'local'), r.version, coalesce(r.description, ''), coalesce(r.readme, ''),
			r.file_name, r.content_type, r.size_bytes, r.md5, r.sha256, r.storage_path, coalesce(r.upstream_slug, ''), coalesce(r.upstream_file_uri, ''),
			r.metadata, r.created_at
		from releases r
		join modules m on m.id = r.module_id
		where r.slug = $1
	`
	return scanPostgresRelease(s.pool.QueryRow(ctx, query, slug), "get release by slug")
}

func NewRelease(moduleID, owner, name, version, description, readme, fileName, contentType, md5, sha256, storagePath string, sizeBytes int64, metadata map[string]any) domain.Release {
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
		MD5:         md5,
		SHA256:      sha256,
		StoragePath: storagePath,
		Metadata:    metadata,
		CreatedAt:   time.Time{},
	}
}
