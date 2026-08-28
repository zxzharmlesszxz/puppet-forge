package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
)

const currentSchemaVersion = 3

type sqliteQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

type sqliteAccessConfigLoader struct {
	queryer sqliteQueryer
}

const sqliteSchema = `
pragma foreign_keys = on;

create table if not exists modules (
    id text primary key,
    owner text not null,
    name text not null,
	slug text not null unique,
    latest_version text,
	upstream_refreshed_at text,
    created_at text not null default current_timestamp,
    updated_at text not null default current_timestamp,
	constraint modules_owner_name_unique unique (owner, name),
	constraint modules_owner_nonempty check (trim(owner) <> ''),
	constraint modules_name_nonempty check (trim(name) <> ''),
	constraint modules_slug_nonempty check (trim(slug) <> '')
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
    size_bytes integer not null,
    md5 text not null default '',
    sha256 text not null,
    storage_path text not null,
    upstream_slug text,
    upstream_file_uri text,
    metadata text not null default '{}',
    created_at text not null default current_timestamp,
	constraint releases_module_version_unique unique (module_id, version),
	constraint releases_version_nonempty check (trim(version) <> ''),
	constraint releases_slug_nonempty check (trim(slug) <> ''),
	constraint releases_size_nonnegative check (size_bytes >= 0),
	constraint releases_source_valid check (source in ('local', 'upstream'))
);

create table if not exists schema_metadata (
	id integer primary key check (id = 1),
	version integer not null check (version >= 0)
);
insert or ignore into schema_metadata (id, version) values (1, 0);

create table if not exists app_leases (
    name text primary key,
    holder text not null,
    lease_until text not null
);

create table if not exists rate_limits (
    limiter_key text primary key,
    request_count integer not null,
    reset_at text not null,
    updated_at text not null
);

create index if not exists idx_rate_limits_reset_at on rate_limits (reset_at);

create index if not exists idx_modules_updated_at on modules (updated_at desc);
create index if not exists idx_releases_module_id on releases (module_id);
create index if not exists idx_releases_storage_path on releases (storage_path);

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
    created_at text not null default current_timestamp,
    expires_at text,
    revoked_at text,
    last_used_at text
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
    created_at text not null,
    expires_at text not null,
    last_seen_at text not null,
    revoked_at text
);

create index if not exists idx_manage_sessions_expires_at on manage_sessions (expires_at);

create table if not exists oidc_states (
    state_hash text primary key,
    nonce text not null,
    pkce_verifier text not null,
    next_path text not null,
    created_at text not null,
    expires_at text not null,
    consumed_at text
);

create index if not exists idx_oidc_states_expires_at on oidc_states (expires_at);

create table if not exists oidc_sessions (
    session_hash text primary key,
    subject text not null,
    email text not null,
    name text not null,
    groups_json text not null,
    csrf_secret text not null,
    created_at text not null,
    expires_at text not null,
    last_seen_at text not null,
    revoked_at text
);

create index if not exists idx_oidc_sessions_expires_at on oidc_sessions (expires_at);

create table if not exists deleted_releases (
    owner text not null,
    name text not null,
    version text not null,
    source text not null,
    deleted_at text not null default current_timestamp,
    primary key (owner, name, version, source)
);

create table if not exists release_usage (
    owner text not null,
    name text not null,
    version text not null,
    last_used_at text not null default current_timestamp,
    primary key (owner, name, version)
);

create table if not exists artifact_deletions (
	storage_path text primary key,
	owner text not null,
	name text not null,
	created_at text not null default current_timestamp,
	next_attempt_at text not null default current_timestamp
);

create index if not exists idx_artifact_deletions_due
on artifact_deletions (next_attempt_at, created_at, storage_path);
`

const sqliteAccessOIDCMappingsMigration = `
create table if not exists access_oidc_mappings_next (
    team text not null references access_teams (team) on delete cascade,
    mapping_type text not null,
    value text not null,
    primary key (team, mapping_type, value)
);
insert or ignore into access_oidc_mappings_next (team, mapping_type, value)
select team, mapping_type, value from access_oidc_mappings;
drop table access_oidc_mappings;
alter table access_oidc_mappings_next rename to access_oidc_mappings;
`

type SQLiteStore struct {
	db             *sql.DB
	tokenHasher    *auth.TokenHasher
	accessConfigMu sync.Mutex
	moduleLocksMu  sync.Mutex
	moduleLocks    map[string]*sqliteModuleLock
}

func (s *SQLiteStore) LockAccessConfig(context.Context) (AccessConfigUnlock, error) {
	s.accessConfigMu.Lock()
	var once sync.Once
	return func() error {
		once.Do(s.accessConfigMu.Unlock)
		return nil
	}, nil
}

func (s *SQLiteStore) ConsumeRateLimit(ctx context.Context, key string, limit int, window time.Duration, now time.Time) (bool, error) {
	if strings.TrimSpace(key) == "" || limit <= 0 || window <= 0 {
		return false, errors.New("invalid rate limit")
	}
	var allowed bool
	err := s.db.QueryRowContext(ctx, `
		insert into rate_limits (limiter_key, request_count, reset_at, updated_at)
		values (?, 1, ?, ?)
		on conflict (limiter_key) do update set
			request_count = case
				when rate_limits.reset_at <= excluded.updated_at then 1
				else min(rate_limits.request_count + 1, ? + 1)
			end,
			reset_at = case when rate_limits.reset_at <= excluded.updated_at then excluded.reset_at else rate_limits.reset_at end,
			updated_at = excluded.updated_at
		returning request_count <= ?
	`, key, sqliteTime(now.UTC().Add(window)), sqliteTime(now.UTC()), limit, limit).Scan(&allowed)
	if err != nil {
		return false, fmt.Errorf("consume rate limit: %w", err)
	}
	return allowed, nil
}

func (s *SQLiteStore) PurgeRateLimits(ctx context.Context, before time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `delete from rate_limits where reset_at < ?`, sqliteTime(before.UTC()))
	if err != nil {
		return 0, fmt.Errorf("purge rate limits: %w", err)
	}
	return result.RowsAffected()
}

type sqliteModuleLock struct {
	semaphore  chan struct{}
	references int
}

type sqliteMigrationExecutor interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
}

func NewSQLiteStore(dsn string, tokenHashers ...*auth.TokenHasher) (*SQLiteStore, error) {
	var tokenHasher *auth.TokenHasher
	if len(tokenHashers) > 0 {
		tokenHasher = tokenHashers[0]
	}
	path, err := sqlitePathFromDSN(dsn)
	if err != nil {
		return nil, err
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, fmt.Errorf("create sqlite directory: %w", err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite db: %w", err)
	}
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(sqliteSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init sqlite schema: %w", err)
	}
	if err := rejectNewerSQLiteSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrateSQLiteSchema(db, tokenHasher); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec(`
		create index if not exists idx_modules_listing on modules (updated_at desc, owner, name, id);
		create index if not exists idx_modules_upstream_refresh on modules (upstream_refreshed_at is not null, upstream_refreshed_at, owner, name, id);
		create index if not exists idx_releases_module_created on releases (module_id, created_at desc, version);
		create index if not exists idx_release_usage_last_used_at on release_usage (last_used_at);
		create index if not exists idx_access_tokens_team_type on access_tokens (team, token_type);
		create index if not exists idx_access_tokens_expires_at on access_tokens (expires_at) where expires_at is not null;
		create index if not exists idx_access_tokens_revoked_at on access_tokens (revoked_at) where revoked_at is not null;
	`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ensure sqlite query indexes: %w", err)
	}

	return &SQLiteStore{db: db, tokenHasher: tokenHasher, moduleLocks: make(map[string]*sqliteModuleLock)}, nil
}

func (s *SQLiteStore) LockModule(ctx context.Context, owner, name string) (ModuleUnlock, error) {
	key := owner + "\x00" + name
	s.moduleLocksMu.Lock()
	entry := s.moduleLocks[key]
	if entry == nil {
		entry = &sqliteModuleLock{semaphore: make(chan struct{}, 1)}
		entry.semaphore <- struct{}{}
		s.moduleLocks[key] = entry
	}
	entry.references++
	s.moduleLocksMu.Unlock()

	select {
	case <-ctx.Done():
		s.releaseModuleLockReference(key, entry)
		return nil, fmt.Errorf("acquire module lock: %w", ctx.Err())
	case <-entry.semaphore:
	}

	var once sync.Once
	return func() error {
		once.Do(func() {
			entry.semaphore <- struct{}{}
			s.releaseModuleLockReference(key, entry)
		})
		return nil
	}, nil
}

func (s *SQLiteStore) releaseModuleLockReference(key string, entry *sqliteModuleLock) {
	s.moduleLocksMu.Lock()
	entry.references--
	if entry.references == 0 {
		delete(s.moduleLocks, key)
	}
	s.moduleLocksMu.Unlock()
}

func migrateSQLiteSchema(db *sql.DB, tokenHasher *auth.TokenHasher) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin sqlite schema migration: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	if err := sqliteEnsureReleaseMD5Column(tx); err != nil {
		return err
	}
	if err := sqliteEnsureUpstreamRefreshColumn(tx); err != nil {
		return err
	}
	if err := sqliteEnsureCanonicalSlugs(tx); err != nil {
		return err
	}
	if err := sqliteEnsureDataConstraints(tx); err != nil {
		return err
	}
	if err := migrateSQLiteAccessTokens(tx, tokenHasher); err != nil {
		return err
	}
	migrateOIDC, err := sqliteNeedsAccessOIDCMappingsMigration(tx)
	if err != nil {
		return err
	}
	if migrateOIDC {
		if _, err := tx.Exec(sqliteAccessOIDCMappingsMigration); err != nil {
			return fmt.Errorf("migrate sqlite oidc mappings: %w", err)
		}
	}
	if _, err := tx.Exec(`update schema_metadata set version = ? where id = 1`, currentSchemaVersion); err != nil {
		return fmt.Errorf("update sqlite schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit sqlite schema migration: %w", err)
	}
	return nil
}

func sqliteEnsureUpstreamRefreshColumn(db sqliteMigrationExecutor) error {
	hasColumn, err := sqliteTableHasColumn(db, "modules", "upstream_refreshed_at")
	if err != nil {
		return err
	}
	if hasColumn {
		return nil
	}
	if _, err := db.Exec(`alter table modules add column upstream_refreshed_at text`); err != nil {
		return fmt.Errorf("add sqlite upstream refresh timestamp: %w", err)
	}
	return nil
}

func rejectNewerSQLiteSchema(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`select version from schema_metadata where id = 1`).Scan(&version); err != nil {
		return fmt.Errorf("read sqlite schema version: %w", err)
	}
	if version > currentSchemaVersion {
		return fmt.Errorf("sqlite schema version %d is newer than supported version %d", version, currentSchemaVersion)
	}
	return nil
}

func sqliteEnsureDataConstraints(db sqliteMigrationExecutor) error {
	if _, err := db.Exec(`
		create trigger if not exists modules_validate_insert before insert on modules
		when trim(new.owner) = '' or trim(new.name) = '' or trim(new.slug) = ''
		begin select raise(abort, 'invalid module identity'); end;
		create trigger if not exists modules_validate_update before update on modules
		when trim(new.owner) = '' or trim(new.name) = '' or trim(new.slug) = ''
		begin select raise(abort, 'invalid module identity'); end;
		create trigger if not exists releases_validate_insert before insert on releases
		when trim(new.version) = '' or trim(new.slug) = '' or new.size_bytes < 0 or new.source not in ('local', 'upstream')
		begin select raise(abort, 'invalid release data'); end;
		create trigger if not exists releases_validate_update before update on releases
		when trim(new.version) = '' or trim(new.slug) = '' or new.size_bytes < 0 or new.source not in ('local', 'upstream')
		begin select raise(abort, 'invalid release data'); end;
	`); err != nil {
		return fmt.Errorf("create sqlite data constraint triggers: %w", err)
	}
	return nil
}

func sqliteEnsureCanonicalSlugs(db sqliteMigrationExecutor) error {
	for _, table := range []string{"modules", "releases"} {
		hasSlug, err := sqliteTableHasColumn(db, table, "slug")
		if err != nil {
			return err
		}
		if !hasSlug {
			if _, err := db.Exec(`alter table ` + table + ` add column slug text`); err != nil {
				return fmt.Errorf("add sqlite %s slug: %w", table, err)
			}
		}
	}
	if _, err := db.Exec(`update modules set slug = owner || '-' || name where slug is null or slug = ''`); err != nil {
		return fmt.Errorf("backfill sqlite module slugs: %w", err)
	}
	if _, err := db.Exec(`
		update releases
		set slug = (select modules.slug || '-' || releases.version from modules where modules.id = releases.module_id)
		where slug is null or slug = ''
	`); err != nil {
		return fmt.Errorf("backfill sqlite release slugs: %w", err)
	}
	if _, err := db.Exec(`create unique index if not exists idx_modules_slug_unique on modules (slug)`); err != nil {
		return fmt.Errorf("create sqlite module slug index (canonical slug collision): %w", err)
	}
	if _, err := db.Exec(`create unique index if not exists idx_releases_slug_unique on releases (slug)`); err != nil {
		return fmt.Errorf("create sqlite release slug index (canonical slug collision): %w", err)
	}
	return nil
}

func sqliteTableHasColumn(db sqliteMigrationExecutor, table, column string) (bool, error) {
	rows, err := db.Query(`pragma table_info(` + table + `)`)
	if err != nil {
		return false, fmt.Errorf("inspect sqlite %s columns: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var cid, notNull, primaryKeyPosition int
		var name, dataType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKeyPosition); err != nil {
			return false, fmt.Errorf("scan sqlite %s columns: %w", table, err)
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func migrateSQLiteAccessTokens(tx *sql.Tx, tokenHasher *auth.TokenHasher) error {
	rows, err := tx.Query(`pragma table_info(access_tokens)`)
	if err != nil {
		return fmt.Errorf("check sqlite access token migration state: %w", err)
	}
	legacy := false
	for rows.Next() {
		var cid, notNull, primaryKeyPosition int
		var name, dataType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKeyPosition); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan sqlite access token migration state: %w", err)
		}
		legacy = legacy || name == "token"
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close sqlite access token migration rows: %w", err)
	}
	if !legacy {
		return nil
	}

	legacyRows, err := tx.Query(`select team, token_type, token from access_tokens order by team, token_type, token`)
	if err != nil {
		return fmt.Errorf("read legacy sqlite access tokens: %w", err)
	}
	type legacyToken struct{ team, kind, raw string }
	var tokens []legacyToken
	for legacyRows.Next() {
		var token legacyToken
		if err := legacyRows.Scan(&token.team, &token.kind, &token.raw); err != nil {
			_ = legacyRows.Close()
			return fmt.Errorf("scan legacy sqlite access token: %w", err)
		}
		tokens = append(tokens, token)
	}
	if err := legacyRows.Close(); err != nil {
		return fmt.Errorf("close legacy sqlite access tokens: %w", err)
	}
	if len(tokens) > 0 && tokenHasher == nil {
		return errors.New("ACCESS_TOKEN_PEPPER is required to migrate legacy access tokens")
	}
	if _, err := tx.Exec(`
		create table access_tokens_next (
			token_id text primary key,
			team text not null references access_teams (team) on delete cascade,
			token_type text not null,
			token_prefix text not null,
			token_hash text not null unique,
			description text not null default '',
			created_at text not null default current_timestamp,
			expires_at text,
			revoked_at text,
			last_used_at text
		)
	`); err != nil {
		return fmt.Errorf("create sqlite access token migration table: %w", err)
	}
	for _, token := range tokens {
		record, err := accessTokenRecordFromRaw(tokenHasher, token.kind, token.raw)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`insert into access_tokens_next (token_id, team, token_type, token_prefix, token_hash, created_at) values (?, ?, ?, ?, ?, ?)`,
			record.ID, token.team, token.kind, record.Prefix, record.Digest, record.CreatedAt.Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("migrate sqlite access token: %w", err)
		}
	}
	if _, err := tx.Exec(`drop table access_tokens; alter table access_tokens_next rename to access_tokens`); err != nil {
		return fmt.Errorf("replace sqlite access token table: %w", err)
	}
	return nil
}

func sqliteEnsureReleaseMD5Column(db sqliteMigrationExecutor) error {
	rows, err := db.Query(`pragma table_info(releases)`)
	if err != nil {
		return fmt.Errorf("check sqlite release checksum migration state: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	for rows.Next() {
		var cid int
		var name string
		var dataType string
		var notNull int
		var defaultValue sql.NullString
		var primaryKeyPosition int
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKeyPosition); err != nil {
			return fmt.Errorf("scan sqlite release checksum migration state: %w", err)
		}
		if name == "md5" {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read sqlite release checksum migration state: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close sqlite release checksum migration rows: %w", err)
	}
	if _, err := db.Exec(`alter table releases add column md5 text not null default ''`); err != nil {
		return fmt.Errorf("migrate sqlite release checksums: %w", err)
	}
	return nil
}

func sqliteNeedsAccessOIDCMappingsMigration(db sqliteMigrationExecutor) (bool, error) {
	rows, err := db.Query(`
		pragma table_info(access_oidc_mappings)
	`)
	if err != nil {
		return false, fmt.Errorf("check sqlite oidc mappings migration state: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	primaryKeyColumns := 0
	for rows.Next() {
		var cid int
		var name string
		var dataType string
		var notNull int
		var defaultValue sql.NullString
		var primaryKeyPosition int
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKeyPosition); err != nil {
			return false, fmt.Errorf("scan sqlite oidc mappings migration state: %w", err)
		}
		if primaryKeyPosition > 0 {
			primaryKeyColumns++
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate sqlite oidc mappings migration state: %w", err)
	}
	return primaryKeyColumns == 0, nil
}

func (s *SQLiteStore) Close() {
	_ = s.db.Close()
}

func (s *SQLiteStore) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *SQLiteStore) AcquireLease(ctx context.Context, name, holder string, duration time.Duration) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
		insert into app_leases (name, holder, lease_until)
		values (?, ?, strftime('%Y-%m-%d %H:%M:%f', julianday('now') + (? / 86400.0)))
		on conflict(name) do update set
			holder = excluded.holder,
			lease_until = excluded.lease_until
		where julianday(app_leases.lease_until) <= julianday('now') or app_leases.holder = excluded.holder
	`, name, holder, duration.Seconds())
	if err != nil {
		return false, fmt.Errorf("acquire lease: %w", err)
	}
	rows, _ := result.RowsAffected()
	return rows > 0, nil
}

func (s *SQLiteStore) ReleaseLease(ctx context.Context, name, holder string) error {
	if _, err := s.db.ExecContext(ctx, `delete from app_leases where name = ? and holder = ?`, name, holder); err != nil {
		return fmt.Errorf("release lease: %w", err)
	}
	return nil
}

func (s *SQLiteStore) LoadTeamConfigs(ctx context.Context) ([]auth.TeamConfig, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin access snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `select team from access_teams order by team`)
	if err != nil {
		return nil, fmt.Errorf("list access teams: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	configs, err := loadTeamConfigs(ctx, rows, sqliteAccessConfigLoader{queryer: tx})
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit access snapshot: %w", err)
	}
	return configs, nil
}

func (s *SQLiteStore) ReplaceTeamConfigs(ctx context.Context, configs []auth.TeamConfig) error {
	if _, err := auth.NewAuthorizer(configs); err != nil {
		return fmt.Errorf("validate access config: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin access tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	if _, err := tx.ExecContext(ctx, `delete from access_teams`); err != nil {
		return fmt.Errorf("clear access teams: %w", err)
	}
	writer := sqliteAccessConfigWriter{ctx: ctx, tx: tx}
	if err := persistTeamConfigs(configs, s.tokenHasher, writer); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit access tx: %w", err)
	}
	return nil
}

type sqliteAccessConfigWriter struct {
	ctx context.Context
	tx  *sql.Tx
}

func (w sqliteAccessConfigWriter) insertTeam(team string) error {
	if _, err := w.tx.ExecContext(w.ctx, `insert into access_teams (team) values (?)`, team); err != nil {
		return fmt.Errorf("insert access team: %w", err)
	}
	return nil
}

func (w sqliteAccessConfigWriter) insertToken(team, tokenType string, token auth.AccessTokenRecord) error {
	return insertSQLiteAccessTokenRecord(w.ctx, w.tx, team, tokenType, token)
}

func (w sqliteAccessConfigWriter) insertOwner(team, owner string) error {
	if _, err := w.tx.ExecContext(w.ctx, `insert into access_publish_owners (team, owner) values (?, ?)`, team, owner); err != nil {
		return fmt.Errorf("insert access owner: %w", err)
	}
	return nil
}

func (w sqliteAccessConfigWriter) insertOIDCMapping(team string, mapping accessOIDCMapping) error {
	if _, err := w.tx.ExecContext(w.ctx, `insert into access_oidc_mappings (team, mapping_type, value) values (?, ?, ?)`, team, mapping.kind, mapping.value); err != nil {
		return fmt.Errorf("insert oidc mapping: %w", err)
	}
	return nil
}

func (s *SQLiteStore) IsAccessTokenActive(ctx context.Context, tokenID string, now time.Time) (bool, error) {
	if strings.TrimSpace(tokenID) == "" {
		return false, errors.New("access token id is required")
	}
	var active bool
	if err := s.db.QueryRowContext(ctx, `
		select exists (
			select 1
			from access_tokens
			where token_id = ?
			  and revoked_at is null
			  and (expires_at is null or expires_at > ?)
		)
	`, tokenID, sqliteTime(now.UTC())).Scan(&active); err != nil {
		return false, fmt.Errorf("check access token activity: %w", err)
	}
	return active, nil
}

func (s *SQLiteStore) MarkAccessTokenUsed(ctx context.Context, tokenID string, usedAt time.Time) error {
	if strings.TrimSpace(tokenID) == "" {
		return errors.New("access token id is required")
	}
	usedAt = usedAt.UTC()
	if _, err := s.db.ExecContext(ctx, `
		update access_tokens
		set last_used_at = ?
		where token_id = ?
		  and revoked_at is null
		  and (last_used_at is null or last_used_at < ?)
	`, sqliteTime(usedAt), tokenID, sqliteTime(usedAt.Add(-time.Minute))); err != nil {
		return fmt.Errorf("mark access token used: %w", err)
	}
	return nil
}

func (s *SQLiteStore) CreateManageSession(ctx context.Context, session ManageSession) error {
	if session.SessionHash == "" || session.CredentialHash == "" || session.CSRFSecret == "" || session.AuthMethod == "" {
		return errors.New("manage session is incomplete")
	}
	if _, err := s.db.ExecContext(ctx, `
		insert into manage_sessions (session_hash, credential_hash, credential_id, auth_method, csrf_secret, created_at, expires_at, last_seen_at, revoked_at)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, session.SessionHash, session.CredentialHash, session.CredentialID, session.AuthMethod, session.CSRFSecret,
		sqliteTime(session.CreatedAt), sqliteTime(session.ExpiresAt), sqliteTime(session.LastSeenAt), nullableSQLiteTime(session.RevokedAt)); err != nil {
		return fmt.Errorf("create manage session: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetManageSession(ctx context.Context, sessionHash string, now time.Time) (ManageSession, error) {
	var session ManageSession
	var createdAt, expiresAt, lastSeenAt, revokedAt sqliteTimestamp
	err := s.db.QueryRowContext(ctx, `
		select session_hash, credential_hash, credential_id, auth_method, csrf_secret,
		       created_at, expires_at, last_seen_at, revoked_at
		from manage_sessions
		where session_hash = ? and revoked_at is null and expires_at > ?
	`, sessionHash, sqliteTime(now.UTC())).Scan(
		&session.SessionHash, &session.CredentialHash, &session.CredentialID, &session.AuthMethod, &session.CSRFSecret,
		&createdAt, &expiresAt, &lastSeenAt, &revokedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ManageSession{}, ErrNotFound
	}
	if err != nil {
		return ManageSession{}, fmt.Errorf("get manage session: %w", err)
	}
	session.CreatedAt = createdAt.Time
	session.ExpiresAt = expiresAt.Time
	session.LastSeenAt = lastSeenAt.Time
	session.RevokedAt = revokedAt.Pointer()
	if session.LastSeenAt.Before(now.UTC().Add(-time.Minute)) {
		if _, err := s.db.ExecContext(ctx, `update manage_sessions set last_seen_at = ? where session_hash = ? and last_seen_at < ?`,
			sqliteTime(now.UTC()), sessionHash, sqliteTime(now.UTC().Add(-time.Minute))); err != nil {
			slog.Warn("touch manage session failed", "err", err)
		}
	}
	return session, nil
}

func (s *SQLiteStore) RevokeManageSession(ctx context.Context, sessionHash string, revokedAt time.Time) error {
	if _, err := s.db.ExecContext(ctx, `update manage_sessions set revoked_at = ? where session_hash = ? and revoked_at is null`, sqliteTime(revokedAt.UTC()), sessionHash); err != nil {
		return fmt.Errorf("revoke manage session: %w", err)
	}
	return nil
}

func (s *SQLiteStore) CreateOIDCState(ctx context.Context, state OIDCState) error {
	if state.StateHash == "" || state.Nonce == "" || state.PKCEVerifier == "" {
		return errors.New("oidc state is incomplete")
	}
	if _, err := s.db.ExecContext(ctx, `
		insert into oidc_states (state_hash, nonce, pkce_verifier, next_path, created_at, expires_at)
		values (?, ?, ?, ?, ?, ?)
	`, state.StateHash, state.Nonce, state.PKCEVerifier, state.NextPath, sqliteTime(state.CreatedAt), sqliteTime(state.ExpiresAt)); err != nil {
		return fmt.Errorf("create oidc state: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ConsumeOIDCState(ctx context.Context, stateHash string, now time.Time) (OIDCState, error) {
	var state OIDCState
	var createdAt, expiresAt sqliteTimestamp
	err := s.db.QueryRowContext(ctx, `
		update oidc_states
		set consumed_at = ?
		where state_hash = ? and consumed_at is null and expires_at > ?
		returning state_hash, nonce, pkce_verifier, next_path, created_at, expires_at
	`, sqliteTime(now.UTC()), stateHash, sqliteTime(now.UTC())).Scan(
		&state.StateHash, &state.Nonce, &state.PKCEVerifier, &state.NextPath, &createdAt, &expiresAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return OIDCState{}, ErrNotFound
	}
	if err != nil {
		return OIDCState{}, fmt.Errorf("consume oidc state: %w", err)
	}
	state.CreatedAt = createdAt.Time
	state.ExpiresAt = expiresAt.Time
	return state, nil
}

func (s *SQLiteStore) CreateOIDCSession(ctx context.Context, session OIDCSession) error {
	if session.SessionHash == "" || session.Subject == "" || session.CSRFSecret == "" {
		return errors.New("oidc session is incomplete")
	}
	groupsJSON, err := json.Marshal(session.Groups)
	if err != nil {
		return fmt.Errorf("encode oidc session groups: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		insert into oidc_sessions (session_hash, subject, email, name, groups_json, csrf_secret, created_at, expires_at, last_seen_at, revoked_at)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, session.SessionHash, session.Subject, session.Email, session.Name, string(groupsJSON), session.CSRFSecret,
		sqliteTime(session.CreatedAt), sqliteTime(session.ExpiresAt), sqliteTime(session.LastSeenAt), nullableSQLiteTime(session.RevokedAt)); err != nil {
		return fmt.Errorf("create oidc session: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetOIDCSession(ctx context.Context, sessionHash string, now time.Time) (OIDCSession, error) {
	var session OIDCSession
	var groupsJSON string
	var createdAt, expiresAt, lastSeenAt, revokedAt sqliteTimestamp
	err := s.db.QueryRowContext(ctx, `
		select session_hash, subject, email, name, groups_json, csrf_secret,
		       created_at, expires_at, last_seen_at, revoked_at
		from oidc_sessions
		where session_hash = ? and revoked_at is null and expires_at > ?
	`, sessionHash, sqliteTime(now.UTC())).Scan(
		&session.SessionHash, &session.Subject, &session.Email, &session.Name, &groupsJSON, &session.CSRFSecret,
		&createdAt, &expiresAt, &lastSeenAt, &revokedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return OIDCSession{}, ErrNotFound
	}
	if err != nil {
		return OIDCSession{}, fmt.Errorf("get oidc session: %w", err)
	}
	if err := json.Unmarshal([]byte(groupsJSON), &session.Groups); err != nil {
		return OIDCSession{}, fmt.Errorf("decode oidc session groups: %w", err)
	}
	session.CreatedAt = createdAt.Time
	session.ExpiresAt = expiresAt.Time
	session.LastSeenAt = lastSeenAt.Time
	session.RevokedAt = revokedAt.Pointer()
	if session.LastSeenAt.Before(now.UTC().Add(-time.Minute)) {
		if _, err := s.db.ExecContext(ctx, `update oidc_sessions set last_seen_at = ? where session_hash = ? and last_seen_at < ?`,
			sqliteTime(now.UTC()), sessionHash, sqliteTime(now.UTC().Add(-time.Minute))); err != nil {
			slog.Warn("touch oidc session failed", "err", err)
		}
	}
	return session, nil
}

func (s *SQLiteStore) PurgeSessionState(ctx context.Context, now, historyBefore time.Time) (SessionCleanupResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SessionCleanupResult{}, fmt.Errorf("begin session cleanup: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var result SessionCleanupResult
	manageResult, err := tx.ExecContext(ctx, `delete from manage_sessions where expires_at < ? or (revoked_at is not null and revoked_at < ?)`, sqliteTime(now.UTC()), sqliteTime(historyBefore.UTC()))
	if err != nil {
		return result, fmt.Errorf("purge manage sessions: %w", err)
	}
	result.ManageSessions, _ = manageResult.RowsAffected()
	stateResult, err := tx.ExecContext(ctx, `delete from oidc_states where expires_at < ? or (consumed_at is not null and consumed_at < ?)`, sqliteTime(now.UTC()), sqliteTime(historyBefore.UTC()))
	if err != nil {
		return result, fmt.Errorf("purge oidc states: %w", err)
	}
	result.OIDCStates, _ = stateResult.RowsAffected()
	sessionResult, err := tx.ExecContext(ctx, `delete from oidc_sessions where expires_at < ? or (revoked_at is not null and revoked_at < ?)`, sqliteTime(now.UTC()), sqliteTime(historyBefore.UTC()))
	if err != nil {
		return result, fmt.Errorf("purge oidc sessions: %w", err)
	}
	result.OIDCSessions, _ = sessionResult.RowsAffected()
	if err := tx.Commit(); err != nil {
		return result, fmt.Errorf("commit session cleanup: %w", err)
	}
	return result, nil
}

func (s *SQLiteStore) RevokeOIDCSession(ctx context.Context, sessionHash string, revokedAt time.Time) error {
	if _, err := s.db.ExecContext(ctx, `update oidc_sessions set revoked_at = ? where session_hash = ? and revoked_at is null`, sqliteTime(revokedAt.UTC()), sessionHash); err != nil {
		return fmt.Errorf("revoke oidc session: %w", err)
	}
	return nil
}

func (l sqliteAccessConfigLoader) loadAccessTokens(ctx context.Context, configs []auth.TeamConfig, index map[string]int) error {
	rows, err := l.queryer.QueryContext(ctx, `select team, token_type, token_id, token_prefix, token_hash, description, created_at, expires_at, revoked_at, last_used_at from access_tokens order by team, token_type, token_prefix`)
	if err != nil {
		return fmt.Errorf("list access tokens: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()
	for rows.Next() {
		var team, tokenType string
		var token auth.AccessTokenRecord
		var createdAt, expiresAt, revokedAt, lastUsedAt sqliteTimestamp
		if err := rows.Scan(&team, &tokenType, &token.ID, &token.Prefix, &token.Digest, &token.Description, &createdAt, &expiresAt, &revokedAt, &lastUsedAt); err != nil {
			return fmt.Errorf("scan access token: %w", err)
		}
		token.CreatedAt = createdAt.Time
		token.ExpiresAt = expiresAt.Pointer()
		token.RevokedAt = revokedAt.Pointer()
		token.LastUsedAt = lastUsedAt.Pointer()
		if i, ok := index[team]; ok {
			applyAccessToken(&configs[i], tokenType, token)
		}
	}
	return rows.Err()
}

func (s *SQLiteStore) PurgeAccessTokenHistory(ctx context.Context, cutoff time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
		delete from access_tokens
		where (revoked_at is not null and revoked_at < ?)
		   or (revoked_at is null and expires_at is not null and expires_at < ?)
	`, sqliteTime(cutoff.UTC()), sqliteTime(cutoff.UTC()))
	if err != nil {
		return 0, fmt.Errorf("purge access token history: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count purged access token history: %w", err)
	}
	return deleted, nil
}

func insertSQLiteAccessTokenRecord(ctx context.Context, tx *sql.Tx, team, tokenType string, token auth.AccessTokenRecord) error {
	if token.ID == "" || token.Digest == "" || token.Prefix == "" {
		return errors.New("access token record is incomplete")
	}
	if _, err := tx.ExecContext(ctx, `insert into access_tokens (token_id, team, token_type, token_prefix, token_hash, description, created_at, expires_at, revoked_at, last_used_at) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		token.ID, team, tokenType, token.Prefix, token.Digest, token.Description, sqliteTime(token.CreatedAt), nullableSQLiteTime(token.ExpiresAt), nullableSQLiteTime(token.RevokedAt), nullableSQLiteTime(token.LastUsedAt)); err != nil {
		return fmt.Errorf("insert access token: %w", err)
	}
	return nil
}

type sqliteTimestamp struct {
	Time  time.Time
	Valid bool
}

func (timestamp *sqliteTimestamp) Scan(value any) error {
	if value == nil {
		timestamp.Time = time.Time{}
		timestamp.Valid = false
		return nil
	}

	var raw string
	switch typed := value.(type) {
	case string:
		raw = typed
	case []byte:
		raw = string(typed)
	case time.Time:
		timestamp.Time = typed.UTC()
		timestamp.Valid = true
		return nil
	default:
		return fmt.Errorf("scan sqlite timestamp from %T", value)
	}
	var parsed time.Time
	var err error
	for _, layout := range []string{time.RFC3339Nano, sqliteTimestampZoneLayout, sqliteTimestampLayout} {
		parsed, err = time.ParseInLocation(layout, raw, time.UTC)
		if err == nil {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("parse sqlite timestamp %q: %w", raw, err)
	}
	timestamp.Time = parsed.UTC()
	timestamp.Valid = true
	return nil
}

func (timestamp *sqliteTimestamp) Pointer() *time.Time {
	if !timestamp.Valid {
		return nil
	}
	value := timestamp.Time
	return &value
}

func nullableSQLiteTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return sqliteTime(*value)
}

const sqliteTimestampLayout = "2006-01-02 15:04:05.999999999"
const sqliteTimestampZoneLayout = "2006-01-02 15:04:05.999999999Z07:00"

func sqliteTime(value time.Time) string {
	return value.UTC().Format(sqliteTimestampLayout)
}

func (l sqliteAccessConfigLoader) loadAccessOwners(ctx context.Context, configs []auth.TeamConfig, index map[string]int) error {
	rows, err := l.queryer.QueryContext(ctx, `select team, owner from access_publish_owners order by team, owner`)
	if err != nil {
		return fmt.Errorf("list access owners: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()
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

func (l sqliteAccessConfigLoader) loadAccessOIDC(ctx context.Context, configs []auth.TeamConfig, index map[string]int) error {
	rows, err := l.queryer.QueryContext(ctx, `select team, mapping_type, value from access_oidc_mappings order by team, mapping_type, value`)
	if err != nil {
		return fmt.Errorf("list access oidc mappings: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()
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

func (s *SQLiteStore) UpsertModule(ctx context.Context, owner, name string) (domain.Module, error) {
	const upsert = `
		insert into modules (id, owner, name, slug, created_at, updated_at)
		values (?, ?, ?, ?, current_timestamp, current_timestamp)
		on conflict (owner, name)
		do update set slug = excluded.slug
	`
	if _, err := s.db.ExecContext(ctx, upsert, uuid.NewString(), owner, name, moduleSlug(owner, name)); err != nil {
		return domain.Module{}, fmt.Errorf("upsert module: %w", err)
	}
	return s.GetModule(ctx, owner, name)
}

func (s *SQLiteStore) CreateRelease(ctx context.Context, release domain.Release) (domain.Release, error) {
	return s.createRelease(ctx, release, false)
}

func (s *SQLiteStore) CreateReleaseIfAbsent(ctx context.Context, release domain.Release) (domain.Release, error) {
	return s.createRelease(ctx, release, true)
}

func (s *SQLiteStore) createRelease(ctx context.Context, release domain.Release, createOnly bool) (domain.Release, error) {
	metadataJSON, err := json.Marshal(release.Metadata)
	if err != nil {
		return domain.Release{}, fmt.Errorf("marshal metadata: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Release{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	var previousSource, previousStoragePath string
	if !createOnly {
		err := tx.QueryRowContext(ctx, `
			select source, storage_path
			from releases
			where module_id = ? and version = ?
		`, release.ModuleID, release.Version).Scan(&previousSource, &previousStoragePath)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return domain.Release{}, fmt.Errorf("read replaced release artifact: %w", err)
		}
	}

	insertRelease := `
		insert into releases (
			id, module_id, slug, source, version, description, readme, file_name, content_type, size_bytes,
			md5, sha256, storage_path, upstream_slug, upstream_file_uri, metadata, created_at
		)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, current_timestamp)
		on conflict (module_id, version) `
	if createOnly {
		insertRelease += `do nothing`
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
			metadata = excluded.metadata`
	}
	result, err := tx.ExecContext(ctx, insertRelease,
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
		string(metadataJSON),
	)
	if err != nil {
		return domain.Release{}, fmt.Errorf("insert release: %w", err)
	}
	if createOnly {
		rows, err := result.RowsAffected()
		if err != nil {
			return domain.Release{}, fmt.Errorf("read inserted release rows: %w", err)
		}
		if rows == 0 {
			return domain.Release{}, ErrConflict
		}
	}
	if release.Source == "local" && release.StoragePath != "" {
		if _, err := tx.ExecContext(ctx, `delete from artifact_deletions where storage_path = ?`, release.StoragePath); err != nil {
			return domain.Release{}, fmt.Errorf("cancel current artifact deletion: %w", err)
		}
	}
	if previousSource == "local" && previousStoragePath != "" && previousStoragePath != release.StoragePath {
		if _, err := tx.ExecContext(ctx, `
			insert into artifact_deletions (storage_path, owner, name)
			values (?, ?, ?)
			on conflict(storage_path) do nothing
		`, previousStoragePath, release.Owner, release.Name); err != nil {
			return domain.Release{}, fmt.Errorf("queue replaced artifact deletion: %w", err)
		}
	}

	const updateModule = `
		update modules
		set latest_version = ?, updated_at = current_timestamp
		where id = ?
	`
	currentLatest, err := sqliteCurrentLatestVersion(ctx, tx, release.ModuleID)
	if err != nil {
		return domain.Release{}, err
	}
	latest := latestVersionWithCandidate(currentLatest, release.Version)
	if _, err := tx.ExecContext(ctx, updateModule, latest, release.ModuleID); err != nil {
		return domain.Release{}, fmt.Errorf("update module latest version: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return domain.Release{}, fmt.Errorf("commit release tx: %w", err)
	}

	return s.GetRelease(ctx, release.Owner, release.Name, release.Version)
}

func (s *SQLiteStore) UpdateReleaseChecksums(ctx context.Context, owner, name, version, md5, sha256, storagePath string, sizeBytes int64) error {
	result, err := s.db.ExecContext(ctx, `
		update releases
		set md5 = ?, sha256 = ?, storage_path = ?, size_bytes = ?
		where module_id = (select id from modules where owner = ? and name = ?)
			and version = ?
	`, md5, sha256, storagePath, sizeBytes, owner, name, version)
	if err != nil {
		return fmt.Errorf("update release checksums: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read updated release checksum rows: %w", err)
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) DeleteModule(ctx context.Context, owner, name string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	if _, err := tx.ExecContext(ctx, `
		insert into artifact_deletions (storage_path, owner, name)
		select r.storage_path, m.owner, m.name
		from releases r
		join modules m on m.id = r.module_id
		where m.owner = ? and m.name = ? and r.source = 'local' and r.storage_path <> ''
		on conflict(storage_path) do nothing
	`, owner, name); err != nil {
		return fmt.Errorf("queue module artifact deletions: %w", err)
	}

	result, err := tx.ExecContext(ctx, `delete from modules where owner = ? and name = ?`, owner, name)
	if err != nil {
		return fmt.Errorf("delete module: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `delete from deleted_releases where owner = ? and name = ?`, owner, name); err != nil {
		return fmt.Errorf("delete module tombstones: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `delete from release_usage where owner = ? and name = ?`, owner, name); err != nil {
		return fmt.Errorf("delete module release usage: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete module tx: %w", err)
	}
	return nil
}

func (s *SQLiteStore) DeleteModuleIfEmpty(ctx context.Context, owner, name string) error {
	_, err := s.db.ExecContext(ctx, `
		delete from modules
		where owner = ? and name = ?
		  and not exists (select 1 from releases where module_id = modules.id)
	`, owner, name)
	if err != nil {
		return fmt.Errorf("delete empty module: %w", err)
	}
	return nil
}

func (s *SQLiteStore) DeleteRelease(ctx context.Context, owner, name, version string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	var source, storagePath string
	const selectSource = `
		select r.source, r.storage_path
		from releases r
		join modules m on m.id = r.module_id
		where m.owner = ? and m.name = ? and r.version = ?
	`
	if err := tx.QueryRowContext(ctx, selectSource, owner, name, version).Scan(&source, &storagePath); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("get release source: %w", err)
	}
	if source == "local" && storagePath != "" {
		if _, err := tx.ExecContext(ctx, `
			insert into artifact_deletions (storage_path, owner, name)
			values (?, ?, ?)
			on conflict(storage_path) do nothing
		`, storagePath, owner, name); err != nil {
			return fmt.Errorf("queue release artifact deletion: %w", err)
		}
	}

	const deleteQuery = `
		delete from releases
		where module_id = (select id from modules where owner = ? and name = ?)
		  and version = ?
	`
	result, err := tx.ExecContext(ctx, deleteQuery, owner, name, version)
	if err != nil {
		return fmt.Errorf("delete release: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return ErrNotFound
	}
	if source == "upstream" {
		const insertTombstone = `
			insert into deleted_releases (owner, name, version, source)
			values (?, ?, ?, ?)
			on conflict(owner, name, version, source) do update set deleted_at = current_timestamp
		`
		if _, err := tx.ExecContext(ctx, insertTombstone, owner, name, version, source); err != nil {
			return fmt.Errorf("record deleted release: %w", err)
		}
	}

	moduleID, err := sqliteModuleID(ctx, tx, owner, name)
	if err != nil {
		return err
	}
	latest, err := sqliteLatestVersion(ctx, tx, moduleID)
	if err != nil {
		return err
	}

	const updateLatest = `
		update modules
		set latest_version = ?, updated_at = current_timestamp
		where id = ?
	`
	if _, err := tx.ExecContext(ctx, updateLatest, latest, moduleID); err != nil {
		return fmt.Errorf("update latest version: %w", err)
	}

	const deleteEmptyModule = `
		delete from modules
		where owner = ? and name = ?
		  and not exists (
			select 1
			from releases
			where releases.module_id = modules.id
		  )
	`
	if _, err := tx.ExecContext(ctx, deleteEmptyModule, owner, name); err != nil {
		return fmt.Errorf("delete empty module: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete release tx: %w", err)
	}
	return nil
}

func (s *SQLiteStore) IsReleaseDeleted(ctx context.Context, owner, name, version, source string) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, `
		select 1
		from deleted_releases
		where owner = ? and name = ? and version = ? and source = ?
	`, owner, name, version, source).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check deleted release: %w", err)
	}
	return true, nil
}

func (s *SQLiteStore) PurgeDeletedReleases(ctx context.Context, cutoff time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `delete from deleted_releases where deleted_at < ?`, sqliteTime(cutoff.UTC()))
	if err != nil {
		return 0, fmt.Errorf("purge deleted releases: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count purged deleted releases: %w", err)
	}
	return deleted, nil
}

func (s *SQLiteStore) MarkReleaseUsed(ctx context.Context, owner, name, version string) error {
	_, err := s.db.ExecContext(ctx, `
		insert into release_usage (owner, name, version, last_used_at)
		values (?, ?, ?, current_timestamp)
		on conflict(owner, name, version) do update set last_used_at = excluded.last_used_at
	`, owner, name, version)
	if err != nil {
		return fmt.Errorf("mark release used: %w", err)
	}
	return nil
}

func (s *SQLiteStore) IsReleaseActive(ctx context.Context, owner, name, version string, since time.Time) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, `
		select 1
		from release_usage
		where owner = ? and name = ? and version = ? and julianday(last_used_at) >= julianday(?)
	`, owner, name, version, sqliteTime(since.UTC())).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check release active: %w", err)
	}
	return true, nil
}

func (s *SQLiteStore) ListActiveReleases(ctx context.Context, since time.Time) ([]ReleaseSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		select owner, name, version, last_used_at
		from release_usage
		where julianday(last_used_at) >= julianday(?)
		order by owner, name, version
	`, sqliteTime(since.UTC()))
	if err != nil {
		return nil, fmt.Errorf("list active releases: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	var releases []ReleaseSummary
	for rows.Next() {
		var rel ReleaseSummary
		var lastUsedAt sqliteTimestamp
		if err := rows.Scan(&rel.Owner, &rel.Name, &rel.Version, &lastUsedAt); err != nil {
			return nil, fmt.Errorf("scan active release: %w", err)
		}
		rel.CreatedAt = lastUsedAt.Time
		releases = append(releases, rel)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active releases: %w", err)
	}
	return releases, nil
}

func (s *SQLiteStore) ListActiveReleasesForModules(ctx context.Context, since time.Time, modules []domain.Module) ([]ReleaseSummary, error) {
	if len(modules) == 0 {
		return nil, nil
	}
	var selected strings.Builder
	args := make([]any, 0, 1+len(modules)*2)
	args = append(args, sqliteTime(since.UTC()))
	for i, module := range modules {
		if i > 0 {
			selected.WriteString(" or ")
		}
		selected.WriteString("(owner = ? and name = ?)")
		args = append(args, module.Owner, module.Name)
	}
	// #nosec G202 -- selected contains only fixed predicates and placeholders; values stay bound in args.
	rows, err := s.db.QueryContext(ctx, `
		select owner, name, version, last_used_at
		from release_usage
		where julianday(last_used_at) >= julianday(?) and (`+selected.String()+`)
		order by owner, name, version
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("list active releases for modules: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	var releases []ReleaseSummary
	for rows.Next() {
		var release ReleaseSummary
		var usedAt sqliteTimestamp
		if err := rows.Scan(&release.Owner, &release.Name, &release.Version, &usedAt); err != nil {
			return nil, fmt.Errorf("scan active release for module: %w", err)
		}
		release.CreatedAt = usedAt.Time
		releases = append(releases, release)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read active releases for modules: %w", err)
	}
	return releases, nil
}

func (s *SQLiteStore) PruneReleaseUsageBefore(ctx context.Context, before time.Time) error {
	if _, err := s.db.ExecContext(ctx, `delete from release_usage where julianday(last_used_at) < julianday(?)`, sqliteTime(before.UTC())); err != nil {
		return fmt.Errorf("prune release usage: %w", err)
	}
	return nil
}

func sqliteModuleID(ctx context.Context, tx *sql.Tx, owner, name string) (string, error) {
	var moduleID string
	err := tx.QueryRowContext(ctx, `select id from modules where owner = ? and name = ?`, owner, name).Scan(&moduleID)
	if err != nil {
		return "", fmt.Errorf("get module id: %w", err)
	}
	return moduleID, nil
}

func sqliteLatestVersion(ctx context.Context, tx *sql.Tx, moduleID string) (string, error) {
	// SQLite has no built-in semantic-version comparison, so keep the current
	// maximum while streaming rows to preserve prerelease ordering in O(1) memory.
	rows, err := tx.QueryContext(ctx, `select version from releases where module_id = ?`, moduleID)
	if err != nil {
		return "", fmt.Errorf("list versions for latest: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

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

func sqliteCurrentLatestVersion(ctx context.Context, tx *sql.Tx, moduleID string) (string, error) {
	var latest string
	err := tx.QueryRowContext(ctx, `select coalesce(latest_version, '') from modules where id = ?`, moduleID).Scan(&latest)
	if err != nil {
		return "", fmt.Errorf("get current latest version: %w", err)
	}
	return latest, nil
}

func (s *SQLiteStore) ListModules(ctx context.Context, limit int) ([]domain.Module, error) {
	modules, _, err := s.ListModulesPage(ctx, limit, 0)
	return modules, err
}

func (s *SQLiteStore) ListModulesPage(ctx context.Context, limit, offset int) ([]domain.Module, int, error) {
	return s.ListModulesPageFiltered(ctx, nil, "", limit, offset)
}

func (s *SQLiteStore) ListModulesPageFiltered(ctx context.Context, owners []string, search string, limit, offset int) ([]domain.Module, int, error) {
	return s.ListModulesPagePrioritized(ctx, owners, nil, search, limit, offset)
}

func (s *SQLiteStore) ListModulesPagePrioritized(ctx context.Context, owners, priorityOwners []string, search string, limit, offset int) ([]domain.Module, int, error) {
	if err := validateModulePagination(limit, offset); err != nil {
		return nil, 0, err
	}
	filter, args := sqliteModuleFilter(owners, search)
	total, err := s.countFilteredModulesWithReleases(ctx, filter, args)
	if err != nil {
		return nil, 0, err
	}

	priorityOrder, priorityArgs := sqliteModulePriorityOrder(priorityOwners)
	args = append(args, priorityArgs...)
	// #nosec G202 -- the filter and priority helpers emit only fixed SQL tokens and placeholders; all user values remain bound parameters.
	query := `
		select id, owner, name, coalesce(latest_version, ''), created_at, updated_at
		from modules
		where exists (
			select 1
			from releases
			where releases.module_id = modules.id
		)
		` + filter + `
		order by ` + priorityOrder + `, updated_at desc, owner asc, name asc, id asc
		limit ?
		offset ?
	`
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list modules: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	var modules []domain.Module
	for rows.Next() {
		module, err := scanModule(rows)
		if err != nil {
			return nil, 0, err
		}
		modules = append(modules, module)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return modules, total, nil
}

func sqliteModulePriorityOrder(owners []string) (string, []any) {
	if len(owners) == 0 {
		return "case when 1 = 1 then 1 else 1 end", nil
	}
	var placeholders strings.Builder
	args := make([]any, 0, len(owners))
	for i, owner := range owners {
		if i > 0 {
			placeholders.WriteString(", ")
		}
		placeholders.WriteByte('?')
		args = append(args, owner)
	}
	return "case when owner in (" + placeholders.String() + ") then 0 else 1 end", args
}

func (s *SQLiteStore) countFilteredModulesWithReleases(ctx context.Context, filter string, args []any) (int, error) {
	var total int
	query := `
		select count(*)
		from modules
		where exists (
			select 1
			from releases
			where releases.module_id = modules.id
		)
		` + filter
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("count modules: %w", err)
	}
	return total, nil
}

func (s *SQLiteStore) CountModulesByOwner(ctx context.Context, owners []string) (map[string]int, error) {
	if owners != nil && len(owners) == 0 {
		return map[string]int{}, nil
	}
	query := `
		select owner, count(*)
		from modules
		where exists (
			select 1
			from releases
			where releases.module_id = modules.id
		)
	`
	args := make([]any, 0, len(owners))
	if owners != nil {
		// #nosec G202 -- only placeholder tokens are constructed here; owner values remain bound parameters.
		query += " and owner in (" + strings.TrimRight(strings.Repeat("?,", len(owners)), ",") + ")"
		for _, owner := range owners {
			args = append(args, owner)
		}
	}
	query += " group by owner"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("count modules by owner: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

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

func (s *SQLiteStore) CountUpstreamModulesByOwner(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
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
	defer func() {
		_ = rows.Close()
	}()

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

func sqliteModuleFilter(owners []string, search string) (string, []any) {
	var filter strings.Builder
	args := make([]any, 0, len(owners)+1)
	if search = strings.TrimSpace(search); search != "" {
		filter.WriteString(` and instr(lower(owner || '/' || name), ?) > 0`)
		args = append(args, strings.ToLower(search))
	}
	if len(owners) > 0 {
		filter.WriteString(` and owner in (`)
		for i, owner := range owners {
			if i > 0 {
				filter.WriteString(", ")
			}
			filter.WriteByte('?')
			args = append(args, owner)
		}
		filter.WriteByte(')')
	}
	return filter.String(), args
}

func (s *SQLiteStore) ListUpstreamModules(ctx context.Context, limit int) ([]domain.Module, error) {
	rows, err := s.db.QueryContext(ctx, `
		select m.id, m.owner, m.name, coalesce(m.latest_version, ''), m.created_at, m.updated_at
		from modules m
		where exists (
			select 1 from releases r
			where r.module_id = m.id and coalesce(r.source, 'local') = 'upstream'
		)
		order by m.upstream_refreshed_at is not null, m.upstream_refreshed_at, m.owner, m.name, m.id
		limit ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list upstream modules: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	var modules []domain.Module
	for rows.Next() {
		module, err := scanModule(rows)
		if err != nil {
			return nil, err
		}
		modules = append(modules, module)
	}
	return modules, rows.Err()
}

func (s *SQLiteStore) MarkUpstreamModuleRefreshAttempt(ctx context.Context, owner, name string, attemptedAt time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		update modules
		set upstream_refreshed_at = ?
		where owner = ? and name = ?
	`, sqliteTime(attemptedAt), owner, name)
	if err != nil {
		return fmt.Errorf("mark upstream module refresh attempt: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read upstream refresh update result: %w", err)
	}
	if rowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) ListReleases(ctx context.Context, owner, name string) ([]domain.ModuleVersion, error) {
	rows, err := s.db.QueryContext(ctx, `
		select r.version, r.created_at
		from releases r
		join modules m on m.id = r.module_id
		where m.owner = ? and m.name = ?
	`, owner, name)
	if err != nil {
		return nil, fmt.Errorf("list releases: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	var versions []domain.ModuleVersion
	for rows.Next() {
		var version domain.ModuleVersion
		var createdAt sqliteTimestamp
		if err := rows.Scan(&version.Version, &createdAt); err != nil {
			return nil, fmt.Errorf("scan release version: %w", err)
		}
		version.CreatedAt = createdAt.Time
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortModuleVersions(versions)
	return versions, nil
}

func (s *SQLiteStore) ListReleasesForModules(ctx context.Context, modules []domain.Module) ([]ModuleReleaseSummary, error) {
	if len(modules) == 0 {
		return nil, nil
	}
	var selected strings.Builder
	args := make([]any, 0, len(modules)*2)
	for i, module := range modules {
		if i > 0 {
			selected.WriteString(" or ")
		}
		selected.WriteString("(m.owner = ? and m.name = ?)")
		args = append(args, module.Owner, module.Name)
	}
	// #nosec G202 -- selected contains only fixed predicates/placeholders; module identities remain bound parameters.
	rows, err := s.db.QueryContext(ctx, `
		select m.owner, m.name, r.version, r.created_at
		from releases r
		join modules m on m.id = r.module_id
		where `+selected.String()+`
		order by m.owner, m.name, r.created_at desc, r.version desc
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("list releases for modules: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var releases []ModuleReleaseSummary
	for rows.Next() {
		var release ModuleReleaseSummary
		var createdAt sqliteTimestamp
		if err := rows.Scan(&release.Owner, &release.Name, &release.Version, &createdAt); err != nil {
			return nil, fmt.Errorf("scan module release summary: %w", err)
		}
		release.CreatedAt = createdAt.Time
		releases = append(releases, release)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortModuleReleaseSummaries(releases)
	return releases, nil
}

func (s *SQLiteStore) CountReleasesForModules(ctx context.Context, modules []domain.Module) ([]ModuleReleaseCount, error) {
	if len(modules) == 0 {
		return nil, nil
	}
	var selected strings.Builder
	args := make([]any, 0, len(modules)*2)
	for i, module := range modules {
		if i > 0 {
			selected.WriteString(" union all ")
		}
		selected.WriteString("select ? as owner, ? as name")
		args = append(args, module.Owner, module.Name)
	}
	// #nosec G202 -- selected contains only fixed SELECT clauses and placeholders; identities remain bound parameters.
	rows, err := s.db.QueryContext(ctx, `
		with selected(owner, name) as (`+selected.String()+`)
		select selected.owner, selected.name, count(r.id)
		from selected
		left join modules m on m.owner = selected.owner and m.name = selected.name
		left join releases r on r.module_id = m.id
		group by selected.owner, selected.name
		order by selected.owner, selected.name
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("count releases for modules: %w", err)
	}
	defer func() { _ = rows.Close() }()
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

func (s *SQLiteStore) ListAllReleases(ctx context.Context) ([]ReleaseSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		select m.owner, m.name, r.version, r.created_at
		from releases r
		join modules m on m.id = r.module_id
	`)
	if err != nil {
		return nil, fmt.Errorf("list all releases: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	var releases []ReleaseSummary
	for rows.Next() {
		var item ReleaseSummary
		var createdAt sqliteTimestamp
		if err := rows.Scan(&item.Owner, &item.Name, &item.Version, &createdAt); err != nil {
			return nil, fmt.Errorf("scan release summary: %w", err)
		}
		item.CreatedAt = createdAt.Time
		releases = append(releases, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortReleaseSummaries(releases)
	return releases, nil
}

func (s *SQLiteStore) ListArtifactReleases(ctx context.Context) ([]ArtifactReleaseRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		select m.owner, m.name, r.version, r.storage_path, r.sha256, r.size_bytes
		from releases r
		join modules m on m.id = r.module_id
		where r.storage_path <> ''
		order by r.storage_path
	`)
	if err != nil {
		return nil, fmt.Errorf("list artifact releases: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanArtifactReleaseRows(rows)
}

func (s *SQLiteStore) ListReleaseMetricSummaries(ctx context.Context) ([]domain.ReleaseMetricSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		select
			coalesce(r.source, 'local') as source,
			count(*) as releases,
			sum(case when r.version = coalesce(m.latest_version, '') then 1 else 0 end) as latest_releases
		from releases r
		join modules m on m.id = r.module_id
		group by coalesce(r.source, 'local')
		order by source
	`)
	if err != nil {
		return nil, fmt.Errorf("list release metric summaries: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

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

func (s *SQLiteStore) GetModule(ctx context.Context, owner, name string) (domain.Module, error) {
	row := s.db.QueryRowContext(ctx, `
		select id, owner, name, coalesce(latest_version, ''), created_at, updated_at
		from modules
		where owner = ? and name = ?
	`, owner, name)
	module, err := scanModule(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Module{}, ErrNotFound
	}
	if err != nil {
		return domain.Module{}, fmt.Errorf("get module: %w", err)
	}
	return module, nil
}

func (s *SQLiteStore) GetModuleBySlug(ctx context.Context, slug string) (domain.Module, error) {
	row := s.db.QueryRowContext(ctx, `
		select id, owner, name, coalesce(latest_version, ''), created_at, updated_at
		from modules
		where slug = ?
	`, slug)
	module, err := scanModule(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Module{}, ErrNotFound
	}
	if err != nil {
		return domain.Module{}, fmt.Errorf("get module by slug: %w", err)
	}
	return module, nil
}

func (s *SQLiteStore) GetModuleForReleaseSlug(ctx context.Context, releaseSlug string) (domain.Module, error) {
	row := s.db.QueryRowContext(ctx, `
		select id, owner, name, coalesce(latest_version, ''), created_at, updated_at
		from modules
		where ? like slug || '-%'
		order by length(slug) desc
		limit 1
	`, releaseSlug)
	module, err := scanModule(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Module{}, ErrNotFound
	}
	if err != nil {
		return domain.Module{}, fmt.Errorf("get module for release slug: %w", err)
	}
	return module, nil
}

func (s *SQLiteStore) GetRelease(ctx context.Context, owner, name, version string) (domain.Release, error) {
	row := s.db.QueryRowContext(ctx, `
		select
			r.id, r.module_id, m.owner, m.name, coalesce(r.source, 'local'), r.version, coalesce(r.description, ''), coalesce(r.readme, ''),
			r.file_name, r.content_type, r.size_bytes, r.md5, r.sha256, r.storage_path, coalesce(r.upstream_slug, ''), coalesce(r.upstream_file_uri, ''),
			r.metadata, r.created_at
		from releases r
		join modules m on m.id = r.module_id
		where m.owner = ? and m.name = ? and r.version = ?
	`, owner, name, version)
	release, err := scanRelease(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Release{}, ErrNotFound
	}
	if err != nil {
		return domain.Release{}, fmt.Errorf("get release: %w", err)
	}
	return release, nil
}

func (s *SQLiteStore) GetReleaseBySlug(ctx context.Context, slug string) (domain.Release, error) {
	row := s.db.QueryRowContext(ctx, `
		select
			r.id, r.module_id, m.owner, m.name, coalesce(r.source, 'local'), r.version, coalesce(r.description, ''), coalesce(r.readme, ''),
			r.file_name, r.content_type, r.size_bytes, r.md5, r.sha256, r.storage_path, coalesce(r.upstream_slug, ''), coalesce(r.upstream_file_uri, ''),
			r.metadata, r.created_at
		from releases r
		join modules m on m.id = r.module_id
		where r.slug = ?
	`, slug)
	release, err := scanRelease(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Release{}, ErrNotFound
	}
	if err != nil {
		return domain.Release{}, fmt.Errorf("get release by slug: %w", err)
	}
	return release, nil
}

type moduleScanner interface {
	Scan(dest ...any) error
}

func scanModule(scanner moduleScanner) (domain.Module, error) {
	var module domain.Module
	var createdAt, updatedAt sqliteTimestamp
	if err := scanner.Scan(
		&module.ID,
		&module.Owner,
		&module.Name,
		&module.LatestVersion,
		&createdAt,
		&updatedAt,
	); err != nil {
		return domain.Module{}, err
	}
	module.CreatedAt = createdAt.Time
	module.UpdatedAt = updatedAt.Time
	return module, nil
}

func scanRelease(scanner moduleScanner) (domain.Release, error) {
	var release domain.Release
	var metadataJSON string
	var createdAt sqliteTimestamp
	if err := scanner.Scan(
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
		&createdAt,
	); err != nil {
		return domain.Release{}, err
	}
	release.CreatedAt = createdAt.Time
	if metadataJSON != "" {
		if err := json.Unmarshal([]byte(metadataJSON), &release.Metadata); err != nil {
			return domain.Release{}, fmt.Errorf("unmarshal metadata: %w", err)
		}
	}
	normalizeReleaseMetadata(&release)
	return release, nil
}

func sqlitePathFromDSN(dsn string) (string, error) {
	const prefix = "sqlite://"
	if !strings.HasPrefix(dsn, prefix) {
		return "", errors.New("invalid sqlite DATABASE_DSN")
	}
	raw := strings.TrimPrefix(dsn, prefix)
	if raw == "" {
		return "", errors.New("sqlite DATABASE_DSN path is required")
	}
	if raw == ":memory:" {
		return raw, nil
	}
	if strings.HasPrefix(raw, "/") {
		return raw, nil
	}
	return raw, nil
}
