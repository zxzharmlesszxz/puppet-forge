package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
)

const sqliteSchema = `
pragma foreign_keys = on;

create table if not exists modules (
    id text primary key,
    owner text not null,
    name text not null,
    latest_version text,
    created_at text not null default current_timestamp,
    updated_at text not null default current_timestamp,
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
    size_bytes integer not null,
    sha256 text not null,
    storage_path text not null,
    upstream_slug text,
    upstream_file_uri text,
    metadata text not null default '{}',
    created_at text not null default current_timestamp,
    constraint releases_module_version_unique unique (module_id, version)
);

create table if not exists app_leases (
    name text primary key,
    holder text not null,
    lease_until text not null
);

create index if not exists idx_modules_updated_at on modules (updated_at desc);
create index if not exists idx_releases_module_id on releases (module_id);

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
	db *sql.DB
}

func NewSQLiteStore(dsn string) (*SQLiteStore, error) {
	path, err := sqlitePathFromDSN(dsn)
	if err != nil {
		return nil, err
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("create sqlite directory: %w", err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite db: %w", err)
	}

	if _, err := db.Exec(sqliteSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init sqlite schema: %w", err)
	}
	migrateOIDC, err := sqliteNeedsAccessOIDCMappingsMigration(db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if migrateOIDC {
		if _, err := db.Exec(sqliteAccessOIDCMappingsMigration); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("migrate sqlite oidc mappings: %w", err)
		}
	}

	return &SQLiteStore{db: db}, nil
}

func sqliteNeedsAccessOIDCMappingsMigration(db *sql.DB) (bool, error) {
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
		values (?, ?, datetime('now', '+' || ? || ' seconds'))
		on conflict(name) do update set
			holder = excluded.holder,
			lease_until = excluded.lease_until
		where app_leases.lease_until <= current_timestamp or app_leases.holder = excluded.holder
	`, name, holder, int(duration.Seconds()))
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
	rows, err := s.db.QueryContext(ctx, `select team from access_teams order by team`)
	if err != nil {
		return nil, fmt.Errorf("list access teams: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	return loadTeamConfigs(ctx, rows, s)
}

func (s *SQLiteStore) ReplaceTeamConfigs(ctx context.Context, configs []auth.TeamConfig) error {
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
	for _, cfg := range configs {
		if strings.TrimSpace(cfg.Team) == "" {
			return errors.New("team is required")
		}
		if _, err := tx.ExecContext(ctx, `insert into access_teams (team) values (?)`, cfg.Team); err != nil {
			return fmt.Errorf("insert access team: %w", err)
		}
		for _, token := range cfg.ReadTokens {
			if err := insertSQLiteAccessToken(ctx, tx, cfg.Team, "read", token); err != nil {
				return err
			}
		}
		for _, token := range cfg.PublishTokens {
			if err := insertSQLiteAccessToken(ctx, tx, cfg.Team, "publish", token); err != nil {
				return err
			}
		}
		for _, owner := range cfg.PublishOwners {
			owner = strings.TrimSpace(owner)
			if owner == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, `insert into access_publish_owners (team, owner) values (?, ?)`, cfg.Team, owner); err != nil {
				return fmt.Errorf("insert access owner: %w", err)
			}
		}
		for _, mapping := range accessOIDCMappings(cfg) {
			if _, err := tx.ExecContext(ctx, `insert into access_oidc_mappings (team, mapping_type, value) values (?, ?, ?)`, cfg.Team, mapping.kind, mapping.value); err != nil {
				return fmt.Errorf("insert oidc mapping: %w", err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit access tx: %w", err)
	}
	return nil
}

func (s *SQLiteStore) loadAccessTokens(ctx context.Context, configs []auth.TeamConfig, index map[string]int) error {
	rows, err := s.db.QueryContext(ctx, `select team, token_type, token from access_tokens order by team, token_type, token`)
	if err != nil {
		return fmt.Errorf("list access tokens: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()
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

func (s *SQLiteStore) loadAccessOwners(ctx context.Context, configs []auth.TeamConfig, index map[string]int) error {
	rows, err := s.db.QueryContext(ctx, `select team, owner from access_publish_owners order by team, owner`)
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

func (s *SQLiteStore) loadAccessOIDC(ctx context.Context, configs []auth.TeamConfig, index map[string]int) error {
	rows, err := s.db.QueryContext(ctx, `select team, mapping_type, value from access_oidc_mappings order by team, mapping_type, value`)
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
		insert into modules (id, owner, name, created_at, updated_at)
		values (?, ?, ?, current_timestamp, current_timestamp)
		on conflict (owner, name)
		do update set updated_at = current_timestamp
	`
	if _, err := s.db.ExecContext(ctx, upsert, uuid.NewString(), owner, name); err != nil {
		return domain.Module{}, fmt.Errorf("upsert module: %w", err)
	}
	return s.GetModule(ctx, owner, name)
}

func (s *SQLiteStore) CreateRelease(ctx context.Context, release domain.Release) (domain.Release, error) {
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

	const insertRelease = `
		insert into releases (
			id, module_id, source, version, description, readme, file_name, content_type, size_bytes,
			sha256, storage_path, upstream_slug, upstream_file_uri, metadata, created_at
		)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, current_timestamp)
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
	`
	if _, err := tx.ExecContext(ctx, insertRelease,
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
		string(metadataJSON),
	); err != nil {
		return domain.Release{}, fmt.Errorf("insert release: %w", err)
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

func (s *SQLiteStore) DeleteModule(ctx context.Context, owner, name string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

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

func (s *SQLiteStore) DeleteRelease(ctx context.Context, owner, name, version string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	var source string
	const selectSource = `
		select r.source
		from releases r
		join modules m on m.id = r.module_id
		where m.owner = ? and m.name = ? and r.version = ?
	`
	if err := tx.QueryRowContext(ctx, selectSource, owner, name, version).Scan(&source); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("get release source: %w", err)
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
		where owner = ? and name = ? and version = ? and unixepoch(last_used_at) >= ?
	`, owner, name, version, since.UTC().Unix()).Scan(&exists)
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
		select owner, name, version, unixepoch(last_used_at)
		from release_usage
		where unixepoch(last_used_at) >= ?
		order by owner, name, version
	`, since.UTC().Unix())
	if err != nil {
		return nil, fmt.Errorf("list active releases: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	var releases []ReleaseSummary
	for rows.Next() {
		var rel ReleaseSummary
		var lastUsedAt int64
		if err := rows.Scan(&rel.Owner, &rel.Name, &rel.Version, &lastUsedAt); err != nil {
			return nil, fmt.Errorf("scan active release: %w", err)
		}
		rel.CreatedAt = time.Unix(lastUsedAt, 0).UTC()
		releases = append(releases, rel)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active releases: %w", err)
	}
	return releases, nil
}

func (s *SQLiteStore) PruneReleaseUsageBefore(ctx context.Context, before time.Time) error {
	if _, err := s.db.ExecContext(ctx, `delete from release_usage where unixepoch(last_used_at) < ?`, before.UTC().Unix()); err != nil {
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
	// SQLite не має вбудованого semver-порівняння, тому завантажуємо всі версії
	// та порівнюємо в Go — коректно для pre-release та різної кількості частин.
	rows, err := tx.QueryContext(ctx, `select version from releases where module_id = ?`, moduleID)
	if err != nil {
		return "", fmt.Errorf("list versions for latest: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

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
	total, err := s.countModulesWithReleases(ctx)
	if err != nil {
		return nil, 0, err
	}

	rows, err := s.db.QueryContext(ctx, `
		select id, owner, name, coalesce(latest_version, ''), unixepoch(created_at), unixepoch(updated_at)
		from modules
		where exists (
			select 1
			from releases
			where releases.module_id = modules.id
		)
		order by updated_at desc
		limit ?
		offset ?
	`, limit, offset)
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

func (s *SQLiteStore) countModulesWithReleases(ctx context.Context) (int, error) {
	var total int
	err := s.db.QueryRowContext(ctx, `
		select count(*)
		from modules
		where exists (
			select 1
			from releases
			where releases.module_id = modules.id
		)
	`).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("count modules: %w", err)
	}
	return total, nil
}

func (s *SQLiteStore) ListUpstreamModules(ctx context.Context, limit int) ([]domain.Module, error) {
	rows, err := s.db.QueryContext(ctx, `
		select distinct m.id, m.owner, m.name, coalesce(m.latest_version, ''), unixepoch(m.created_at), unixepoch(m.updated_at)
		from modules m
		join releases r on r.module_id = m.id
		where coalesce(r.source, 'local') = 'upstream'
		order by m.updated_at desc
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

func (s *SQLiteStore) ListReleases(ctx context.Context, owner, name string) ([]domain.ModuleVersion, error) {
	rows, err := s.db.QueryContext(ctx, `
		select r.version, unixepoch(r.created_at)
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
		var createdAt int64
		if err := rows.Scan(&version.Version, &createdAt); err != nil {
			return nil, fmt.Errorf("scan release version: %w", err)
		}
		version.CreatedAt = time.Unix(createdAt, 0).UTC()
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortModuleVersions(versions)
	return versions, nil
}

func (s *SQLiteStore) ListAllReleases(ctx context.Context) ([]ReleaseSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		select m.owner, m.name, r.version, unixepoch(r.created_at)
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
		var createdAt int64
		if err := rows.Scan(&item.Owner, &item.Name, &item.Version, &createdAt); err != nil {
			return nil, fmt.Errorf("scan release summary: %w", err)
		}
		item.CreatedAt = time.Unix(createdAt, 0).UTC()
		releases = append(releases, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortReleaseSummaries(releases)
	return releases, nil
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
		select id, owner, name, coalesce(latest_version, ''), unixepoch(created_at), unixepoch(updated_at)
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

func (s *SQLiteStore) GetRelease(ctx context.Context, owner, name, version string) (domain.Release, error) {
	row := s.db.QueryRowContext(ctx, `
		select
			r.id, r.module_id, m.owner, m.name, coalesce(r.source, 'local'), r.version, coalesce(r.description, ''), coalesce(r.readme, ''),
			r.file_name, r.content_type, r.size_bytes, r.sha256, r.storage_path, coalesce(r.upstream_slug, ''), coalesce(r.upstream_file_uri, ''),
			r.metadata, unixepoch(r.created_at)
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

type moduleScanner interface {
	Scan(dest ...any) error
}

func scanModule(scanner moduleScanner) (domain.Module, error) {
	var module domain.Module
	var createdAt int64
	var updatedAt int64
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
	module.CreatedAt = time.Unix(createdAt, 0).UTC()
	module.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	return module, nil
}

func scanRelease(scanner moduleScanner) (domain.Release, error) {
	var release domain.Release
	var metadataJSON string
	var createdAt int64
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
		&release.SHA256,
		&release.StoragePath,
		&release.UpstreamSlug,
		&release.UpstreamFileURI,
		&metadataJSON,
		&createdAt,
	); err != nil {
		return domain.Release{}, err
	}
	release.CreatedAt = time.Unix(createdAt, 0).UTC()
	if metadataJSON != "" {
		if err := json.Unmarshal([]byte(metadataJSON), &release.Metadata); err != nil {
			return domain.Release{}, fmt.Errorf("unmarshal metadata: %w", err)
		}
	}
	if release.Metadata == nil {
		release.Metadata = map[string]any{}
	}
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
