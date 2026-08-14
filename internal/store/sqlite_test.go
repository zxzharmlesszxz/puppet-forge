package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
)

func TestSQLiteReleaseMD5MigrationAddsColumnToExistingTable(t *testing.T) {
	t.Parallel()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer func() {
		_ = db.Close()
	}()
	if _, err := db.Exec(`create table releases (id text primary key, sha256 text not null)`); err != nil {
		t.Fatalf("create legacy releases table error = %v", err)
	}

	if err := sqliteEnsureReleaseMD5Column(db); err != nil {
		t.Fatalf("sqliteEnsureReleaseMD5Column() error = %v", err)
	}
	if err := sqliteEnsureReleaseMD5Column(db); err != nil {
		t.Fatalf("sqliteEnsureReleaseMD5Column() second call error = %v", err)
	}

	var md5Value string
	if err := db.QueryRow(`select md5 from releases limit 1`).Scan(&md5Value); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("query migrated md5 column error = %v, want sql.ErrNoRows", err)
	}
}

func TestSQLiteTimestampPreservesFractionalSeconds(t *testing.T) {
	t.Parallel()

	want := time.Date(2026, time.August, 9, 12, 34, 56, 123456789, time.UTC)
	for _, raw := range []string{
		sqliteTime(want),
		"2026-08-09 12:34:56.123456789+00:00",
		"2026-08-09T12:34:56.123456789Z",
	} {
		var timestamp sqliteTimestamp
		if err := timestamp.Scan(raw); err != nil {
			t.Fatalf("Scan(%q) error = %v", raw, err)
		}
		if !timestamp.Valid || !timestamp.Time.Equal(want) {
			t.Fatalf("timestamp for %q = %#v, want %s", raw, timestamp, want)
		}
	}
	var timestamp sqliteTimestamp
	if err := timestamp.Scan(nil); err != nil {
		t.Fatalf("Scan(nil) error = %v", err)
	}
	if timestamp.Valid || timestamp.Pointer() != nil {
		t.Fatalf("nil timestamp = %#v", timestamp)
	}
}

func TestSQLiteReleaseUsagePreservesCompatibleFractionalTimestamp(t *testing.T) {
	t.Parallel()

	s := newSQLiteTestStore(t)
	want := time.Date(2026, time.August, 9, 12, 34, 56, 123456000, time.UTC)
	if _, err := s.db.Exec(`insert into release_usage (owner, name, version, last_used_at) values (?, ?, ?, ?)`,
		"teamname", "module", "1.0.0", "2026-08-09 12:34:56.123456+00:00"); err != nil {
		t.Fatalf("insert release usage error = %v", err)
	}

	releases, err := s.ListActiveReleases(context.Background(), want.Add(-time.Millisecond))
	if err != nil {
		t.Fatalf("ListActiveReleases() error = %v", err)
	}
	if len(releases) != 1 || !releases[0].CreatedAt.Equal(want) {
		t.Fatalf("ListActiveReleases() = %#v, want timestamp %s", releases, want)
	}
	if err := s.PruneReleaseUsageBefore(context.Background(), want.Add(time.Millisecond)); err != nil {
		t.Fatalf("PruneReleaseUsageBefore() error = %v", err)
	}
	releases, err = s.ListActiveReleases(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("ListActiveReleases(after prune) error = %v", err)
	}
	if len(releases) != 0 {
		t.Fatalf("ListActiveReleases(after prune) = %#v, want empty", releases)
	}
}

func TestSQLiteDeferArtifactDeletionPreservesFractionalTimestamp(t *testing.T) {
	t.Parallel()

	s := newSQLiteTestStore(t)
	const storagePath = "modules/teamname/module/1.0.0.tar.gz"
	if _, err := s.db.Exec(`insert into artifact_deletions (storage_path, owner, name) values (?, ?, ?)`,
		storagePath, "teamname", "module"); err != nil {
		t.Fatalf("insert artifact deletion error = %v", err)
	}
	retryAt := time.Date(2026, time.August, 9, 12, 34, 56, 123456789, time.UTC)
	if err := s.DeferArtifactDeletion(context.Background(), storagePath, retryAt); err != nil {
		t.Fatalf("DeferArtifactDeletion() error = %v", err)
	}

	var stored sqliteTimestamp
	if err := s.db.QueryRow(`select next_attempt_at from artifact_deletions where storage_path = ?`, storagePath).Scan(&stored); err != nil {
		t.Fatalf("read deferred timestamp error = %v", err)
	}
	if !stored.Valid || !stored.Time.Equal(retryAt) {
		t.Fatalf("next_attempt_at = %#v, want %s", stored, retryAt)
	}
}

func TestSQLiteOIDCMigrationRollsBackOnFailure(t *testing.T) {
	t.Parallel()

	dsn := "sqlite://" + t.TempDir() + "/forge.db"
	path, err := sqlitePathFromDSN(dsn)
	if err != nil {
		t.Fatalf("sqlitePathFromDSN() error = %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := db.Exec(`
		create table access_teams (team text primary key);
		create table access_oidc_mappings (
			team text not null,
			mapping_type text not null
		);
	`); err != nil {
		t.Fatalf("create malformed legacy schema error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close malformed legacy db error = %v", err)
	}

	if _, err := NewSQLiteStore(dsn); err == nil || !strings.Contains(err.Error(), "migrate sqlite oidc mappings") {
		t.Fatalf("NewSQLiteStore() error = %v, want migration failure", err)
	}

	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("reopen sqlite db error = %v", err)
	}
	defer func() {
		_ = db.Close()
	}()
	var originalTables int
	if err := db.QueryRow(`
		select count(*)
		from sqlite_master
		where type = 'table' and name = 'access_oidc_mappings'
	`).Scan(&originalTables); err != nil {
		t.Fatalf("query original migration table error = %v", err)
	}
	var temporaryTables int
	if err := db.QueryRow(`
		select count(*)
		from sqlite_master
		where type = 'table' and name = 'access_oidc_mappings_next'
	`).Scan(&temporaryTables); err != nil {
		t.Fatalf("query temporary migration table error = %v", err)
	}
	if originalTables != 1 || temporaryTables != 0 {
		t.Fatalf("failed migration was not rolled back: original=%d temporary=%d", originalTables, temporaryTables)
	}
}

func TestSQLiteSchemaVersionIsMonotonicAndRejectsNewerSchema(t *testing.T) {
	t.Parallel()

	dsn := "sqlite://" + t.TempDir() + "/forge.db"
	s, err := NewSQLiteStore(dsn, testAccessTokenHasher(t))
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	var version int
	if err := s.db.QueryRow(`select version from schema_metadata where id = 1`).Scan(&version); err != nil {
		t.Fatalf("read schema version error = %v", err)
	}
	if version != currentSchemaVersion {
		t.Fatalf("schema version = %d, want %d", version, currentSchemaVersion)
	}
	s.Close()

	path, err := sqlitePathFromDSN(dsn)
	if err != nil {
		t.Fatalf("sqlitePathFromDSN() error = %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := db.Exec(`update schema_metadata set version = ? where id = 1`, currentSchemaVersion+1); err != nil {
		t.Fatalf("set newer schema version error = %v", err)
	}
	_ = db.Close()
	if _, err := NewSQLiteStore(dsn, testAccessTokenHasher(t)); err == nil || !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("NewSQLiteStore(newer schema) error = %v", err)
	}
}

func TestSQLiteForeignKeysRemainEnabled(t *testing.T) {
	t.Parallel()

	s, err := NewSQLiteStore("sqlite://:memory:", testAccessTokenHasher(t))
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	defer s.Close()
	_, err = s.db.Exec(`
		insert into releases (id, module_id, slug, source, version, file_name, content_type, size_bytes, sha256, storage_path)
		values ('orphan', 'missing', 'missing-module-1.0.0', 'local', '1.0.0', 'module.tar.gz', 'application/gzip', 1, 'sha', 'path')
	`)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		t.Fatalf("orphan release insert error = %v, want foreign key failure", err)
	}
}

func TestSQLiteDeleteLastReleaseRemovesModule(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	hasher := testAccessTokenHasher(t)
	s, err := NewSQLiteStore("sqlite://:memory:", hasher)
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	defer s.Close()

	module, err := s.UpsertModule(ctx, "teamname", "testdelete")
	if err != nil {
		t.Fatalf("UpsertModule() error = %v", err)
	}

	_, err = s.CreateRelease(ctx, NewRelease(
		module.ID,
		"teamname",
		"testdelete",
		"0.0.1",
		"Delete test module",
		"",
		"teamname-testdelete-0.0.1.tar.gz",
		"application/gzip",
		"",
		"deadbeef",
		"modules/teamname/testdelete/0.0.1/teamname-testdelete-0.0.1.tar.gz",
		123,
		map[string]any{},
	))
	if err != nil {
		t.Fatalf("CreateRelease() error = %v", err)
	}

	if err := s.DeleteRelease(ctx, "teamname", "testdelete", "0.0.1"); err != nil {
		t.Fatalf("DeleteRelease() error = %v", err)
	}

	_, err = s.GetModule(ctx, "teamname", "testdelete")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetModule() error = %v, want %v", err, ErrNotFound)
	}
}

func TestSQLiteCanonicalSlugLookupSupportsHyphens(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, err := NewSQLiteStore("sqlite://:memory:", testAccessTokenHasher(t))
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	defer s.Close()
	module, err := s.UpsertModule(ctx, "platform-core", "reverse-proxy-module")
	if err != nil {
		t.Fatalf("UpsertModule() error = %v", err)
	}
	_, err = s.CreateRelease(ctx, NewRelease(
		module.ID, module.Owner, module.Name, "1.2.3-rc.1", "", "", "archive.tar.gz",
		"application/gzip", "md5", "sha256", "modules/archive.tar.gz", 7, map[string]any{},
	))
	if err != nil {
		t.Fatalf("CreateRelease() error = %v", err)
	}
	gotModule, err := s.GetModuleBySlug(ctx, "platform-core-reverse-proxy-module")
	if err != nil {
		t.Fatalf("GetModuleBySlug() error = %v", err)
	}
	if gotModule.Owner != module.Owner || gotModule.Name != module.Name {
		t.Fatalf("GetModuleBySlug() = %s/%s", gotModule.Owner, gotModule.Name)
	}
	gotRelease, err := s.GetReleaseBySlug(ctx, "platform-core-reverse-proxy-module-1.2.3-rc.1")
	if err != nil {
		t.Fatalf("GetReleaseBySlug() error = %v", err)
	}
	if gotRelease.Owner != module.Owner || gotRelease.Name != module.Name || gotRelease.Version != "1.2.3-rc.1" {
		t.Fatalf("GetReleaseBySlug() = %#v", gotRelease)
	}
}

func TestSQLiteRejectsAmbiguousCanonicalModuleSlug(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, err := NewSQLiteStore("sqlite://:memory:", testAccessTokenHasher(t))
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	defer s.Close()
	if _, err := s.UpsertModule(ctx, "platform-core", "nginx"); err != nil {
		t.Fatalf("UpsertModule(first) error = %v", err)
	}
	if _, err := s.UpsertModule(ctx, "platform", "core-nginx"); err == nil {
		t.Fatal("ambiguous canonical module slug was accepted")
	}
	got, err := s.GetModuleBySlug(ctx, "platform-core-nginx")
	if err != nil {
		t.Fatalf("GetModuleBySlug() error = %v", err)
	}
	if got.Owner != "platform-core" || got.Name != "nginx" {
		t.Fatalf("ambiguous slug resolved to %s/%s", got.Owner, got.Name)
	}
}

func TestSQLiteDeleteOldReleaseKeepsLatestSemanticVersion(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, err := NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	defer s.Close()

	module, err := s.UpsertModule(ctx, "teamname", "versions")
	if err != nil {
		t.Fatalf("UpsertModule() error = %v", err)
	}
	for _, version := range []string{"1.0.0", "2.0.0", "1.5.0"} {
		_, err = s.CreateRelease(ctx, NewRelease(
			module.ID,
			"teamname",
			"versions",
			version,
			"Version test module",
			"",
			"teamname-versions-"+version+".tar.gz",
			"application/gzip",
			"",
			"deadbeef",
			"modules/teamname/versions/"+version+"/teamname-versions-"+version+".tar.gz",
			123,
			map[string]any{},
		))
		if err != nil {
			t.Fatalf("CreateRelease(%s) error = %v", version, err)
		}
	}

	got, err := s.GetModule(ctx, "teamname", "versions")
	if err != nil {
		t.Fatalf("GetModule() error = %v", err)
	}
	if got.LatestVersion != "2.0.0" {
		t.Fatalf("LatestVersion after create = %q, want 2.0.0", got.LatestVersion)
	}

	if err := s.DeleteRelease(ctx, "teamname", "versions", "1.0.0"); err != nil {
		t.Fatalf("DeleteRelease(oldest) error = %v", err)
	}
	got, err = s.GetModule(ctx, "teamname", "versions")
	if err != nil {
		t.Fatalf("GetModule() after deleting oldest error = %v", err)
	}
	if got.LatestVersion != "2.0.0" {
		t.Fatalf("LatestVersion after deleting oldest = %q, want 2.0.0", got.LatestVersion)
	}

	if err := s.DeleteRelease(ctx, "teamname", "versions", "2.0.0"); err != nil {
		t.Fatalf("DeleteRelease(latest) error = %v", err)
	}
	got, err = s.GetModule(ctx, "teamname", "versions")
	if err != nil {
		t.Fatalf("GetModule() after deleting latest error = %v", err)
	}
	if got.LatestVersion != "1.5.0" {
		t.Fatalf("LatestVersion after deleting latest = %q, want 1.5.0", got.LatestVersion)
	}
}

func newSQLiteTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func createPuppetlabsStdlibRelease(t *testing.T, ctx context.Context, s *SQLiteStore) domain.Module {
	t.Helper()
	module, err := s.UpsertModule(ctx, "puppetlabs", "stdlib")
	if err != nil {
		t.Fatalf("UpsertModule() error = %v", err)
	}
	_, err = s.CreateRelease(ctx, domain.Release{
		ID:          "release-1",
		ModuleID:    module.ID,
		Owner:       "puppetlabs",
		Name:        "stdlib",
		Source:      "upstream",
		Version:     "1.0.0",
		FileName:    "stdlib-1.0.0.tar.gz",
		ContentType: "application/gzip",
		Metadata:    map[string]any{},
	})
	if err != nil {
		t.Fatalf("CreateRelease() error = %v", err)
	}
	return module
}

func TestSQLiteDeleteUpstreamReleaseRecordsTombstone(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := newSQLiteTestStore(t)
	createPuppetlabsStdlibRelease(t, ctx, s)

	if err := s.DeleteRelease(ctx, "puppetlabs", "stdlib", "1.0.0"); err != nil {
		t.Fatalf("DeleteRelease() error = %v", err)
	}
	deleted, err := s.IsReleaseDeleted(ctx, "puppetlabs", "stdlib", "1.0.0", "upstream")
	if err != nil {
		t.Fatalf("IsReleaseDeleted() error = %v", err)
	}
	if !deleted {
		t.Fatal("expected upstream release tombstone")
	}
}

func TestSQLiteDeleteModuleClearsReleaseTombstonesAndUsage(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := newSQLiteTestStore(t)
	module := createPuppetlabsStdlibRelease(t, ctx, s)
	_, err := s.CreateRelease(ctx, domain.Release{
		ID:          "release-2",
		ModuleID:    module.ID,
		Owner:       "puppetlabs",
		Name:        "stdlib",
		Source:      "upstream",
		Version:     "2.0.0",
		FileName:    "stdlib-2.0.0.tar.gz",
		ContentType: "application/gzip",
		Metadata:    map[string]any{},
	})
	if err != nil {
		t.Fatalf("CreateRelease(second) error = %v", err)
	}
	if err := s.MarkReleaseUsed(ctx, "puppetlabs", "stdlib", "1.0.0"); err != nil {
		t.Fatalf("MarkReleaseUsed() error = %v", err)
	}
	if err := s.DeleteRelease(ctx, "puppetlabs", "stdlib", "1.0.0"); err != nil {
		t.Fatalf("DeleteRelease() error = %v", err)
	}

	if err := s.DeleteModule(ctx, "puppetlabs", "stdlib"); err != nil {
		t.Fatalf("DeleteModule() error = %v", err)
	}

	deleted, err := s.IsReleaseDeleted(ctx, "puppetlabs", "stdlib", "1.0.0", "upstream")
	if err != nil {
		t.Fatalf("IsReleaseDeleted() error = %v", err)
	}
	if deleted {
		t.Fatal("expected module delete to clear release tombstone")
	}
	active, err := s.IsReleaseActive(ctx, "puppetlabs", "stdlib", "1.0.0", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("IsReleaseActive() error = %v", err)
	}
	if active {
		t.Fatal("expected module delete to clear release usage")
	}
}

func TestSQLiteReleaseUsageRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, err := NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	defer s.Close()

	if err := s.MarkReleaseUsed(ctx, "teamname", "apache", "1.2.3"); err != nil {
		t.Fatalf("MarkReleaseUsed() error = %v", err)
	}

	active, err := s.IsReleaseActive(ctx, "teamname", "apache", "1.2.3", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("IsReleaseActive(recent) error = %v", err)
	}
	if !active {
		t.Fatal("release should be active after mark")
	}

	active, err = s.IsReleaseActive(ctx, "teamname", "apache", "1.2.3", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("IsReleaseActive(future) error = %v", err)
	}
	if active {
		t.Fatal("release should not be active for future cutoff")
	}
}

func TestSQLitePruneReleaseUsageBefore(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, err := NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	defer s.Close()

	if err := s.MarkReleaseUsed(ctx, "teamname", "apache", "1.2.3"); err != nil {
		t.Fatalf("MarkReleaseUsed() error = %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		update release_usage
		set last_used_at = datetime('now', '-48 hours')
		where owner = ? and name = ? and version = ?
	`, "teamname", "apache", "1.2.3"); err != nil {
		t.Fatalf("age release usage row: %v", err)
	}
	if err := s.PruneReleaseUsageBefore(ctx, time.Now().Add(-24*time.Hour)); err != nil {
		t.Fatalf("PruneReleaseUsageBefore() error = %v", err)
	}

	active, err := s.IsReleaseActive(ctx, "teamname", "apache", "1.2.3", time.Now().Add(-72*time.Hour))
	if err != nil {
		t.Fatalf("IsReleaseActive() error = %v", err)
	}
	if active {
		t.Fatal("release should not be active after usage prune")
	}
}

func TestOpenSQLiteAndRejectsUnsupportedScheme(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	opened, err := Open(ctx, "sqlite://:memory:", nil)
	if err != nil {
		t.Fatalf("Open(sqlite) error = %v", err)
	}
	opened.Close()

	if _, err := Open(ctx, "file:///tmp/forge.db", nil); err == nil {
		t.Fatal("expected unsupported scheme error")
	}

	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = Open(canceledCtx, "postgresql://forge:forge@127.0.0.1:1/forge?sslmode=disable", nil)
	if err == nil {
		t.Fatal("Open(postgresql) error = nil, want canceled connection error")
	}
	if strings.Contains(err.Error(), "unsupported database scheme") {
		t.Fatalf("Open(postgresql) error = %v, want PostgreSQL alias to be recognized", err)
	}
}

func TestSQLiteLeaseLifecycle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, err := NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	defer s.Close()

	leader, err := s.AcquireLease(ctx, "refresh", "holder-1", time.Minute)
	if err != nil {
		t.Fatalf("AcquireLease(holder-1) error = %v", err)
	}
	if !leader {
		t.Fatal("expected first holder to acquire lease")
	}

	leader, err = s.AcquireLease(ctx, "refresh", "holder-2", time.Minute)
	if err != nil {
		t.Fatalf("AcquireLease(holder-2) error = %v", err)
	}
	if leader {
		t.Fatal("expected second holder not to acquire active lease")
	}

	if err := s.ReleaseLease(ctx, "refresh", "holder-1"); err != nil {
		t.Fatalf("ReleaseLease() error = %v", err)
	}

	leader, err = s.AcquireLease(ctx, "refresh", "holder-2", time.Minute)
	if err != nil {
		t.Fatalf("AcquireLease(holder-2 after release) error = %v", err)
	}
	if !leader {
		t.Fatal("expected second holder to acquire released lease")
	}
}

func TestSQLiteLeasePreservesFractionalDuration(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := newSQLiteTestStore(t)
	before := time.Now().UTC()
	acquired, err := s.AcquireLease(ctx, "fractional-lease", "holder", 1500*time.Millisecond)
	after := time.Now().UTC()
	if err != nil || !acquired {
		t.Fatalf("AcquireLease() = %v, %v", acquired, err)
	}

	var leaseUntil sqliteTimestamp
	if err := s.db.QueryRow(`select lease_until from app_leases where name = ?`, "fractional-lease").Scan(&leaseUntil); err != nil {
		t.Fatalf("read lease deadline error = %v", err)
	}
	if !leaseUntil.Valid || leaseUntil.Time.Before(before.Add(1400*time.Millisecond)) || leaseUntil.Time.After(after.Add(1600*time.Millisecond)) {
		t.Fatalf("lease_until = %s, expected about 1.5s after acquisition interval %s..%s", leaseUntil.Time, before, after)
	}
	acquired, err = s.AcquireLease(ctx, "fractional-lease", "other-holder", time.Second)
	if err != nil {
		t.Fatalf("AcquireLease(other holder) error = %v", err)
	}
	if acquired {
		t.Fatal("other holder acquired fractional lease before expiry")
	}
}

func TestSQLiteListUpstreamModules(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, err := NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	defer s.Close()

	module, err := s.UpsertModule(ctx, "puppetlabs", "stdlib")
	if err != nil {
		t.Fatalf("UpsertModule() error = %v", err)
	}
	_, err = s.CreateRelease(ctx, domain.Release{
		ID:              "upstream-1",
		ModuleID:        module.ID,
		Owner:           "puppetlabs",
		Name:            "stdlib",
		Source:          "upstream",
		Version:         "9.0.0",
		FileName:        "puppetlabs-stdlib-9.0.0.tar.gz",
		ContentType:     "application/gzip",
		StoragePath:     "",
		UpstreamSlug:    "puppetlabs-stdlib-9.0.0",
		UpstreamFileURI: "/v3/files/puppetlabs-stdlib-9.0.0.tar.gz",
		Metadata:        map[string]any{},
	})
	if err != nil {
		t.Fatalf("CreateRelease(upstream) error = %v", err)
	}

	modules, err := s.ListUpstreamModules(ctx, 100)
	if err != nil {
		t.Fatalf("ListUpstreamModules() error = %v", err)
	}
	if len(modules) != 1 || modules[0].Owner != "puppetlabs" || modules[0].Name != "stdlib" {
		t.Fatalf("unexpected upstream modules: %#v", modules)
	}
}

func TestSQLiteDeleteMissingObjectsReturnsErrNotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, err := NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	defer s.Close()

	if err := s.DeleteModule(ctx, "missing", "module"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteModule() error = %v, want ErrNotFound", err)
	}
	if err := s.DeleteRelease(ctx, "missing", "module", "1.0.0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteRelease() error = %v, want ErrNotFound", err)
	}
}

func TestSQLiteAccessConfigLoadAndReplace(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	hasher := testAccessTokenHasher(t)
	s, err := NewSQLiteStore("sqlite://:memory:", hasher)
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	defer s.Close()

	configs, err := s.LoadTeamConfigs(ctx)
	if err != nil {
		t.Fatalf("LoadTeamConfigs(empty) error = %v", err)
	}
	if len(configs) != 0 {
		t.Fatalf("expected empty configs, got %#v", configs)
	}

	if err := s.ReplaceTeamConfigs(ctx, []auth.TeamConfig{
		{
			Team:                "teamname",
			ReadTokens:          []string{"read-token"},
			PublishTokens:       []string{"publish-token"},
			PublishOwners:       []string{"teamname"},
			OIDCGroups:          []string{"teamname-devops"},
			OIDCTeamAdminEmails: []string{"owner@example.com"},
			OIDCTeamAdminGroups: []string{"teamname-admins"},
		},
	}); err != nil {
		t.Fatalf("ReplaceTeamConfigs(teamname) error = %v", err)
	}

	configs, err = s.LoadTeamConfigs(ctx)
	if err != nil {
		t.Fatalf("LoadTeamConfigs() error = %v", err)
	}
	if len(configs) != 1 || configs[0].Team != "teamname" {
		t.Fatalf("unexpected configs: %#v", configs)
	}
	if len(configs[0].PublishTokenRecords) != 1 || configs[0].PublishTokenRecords[0].Digest != hasher.Digest("publish-token") {
		t.Fatalf("unexpected publish tokens: %#v", configs[0].PublishTokenRecords)
	}
	if len(configs[0].OIDCGroups) != 1 || configs[0].OIDCGroups[0] != "teamname-devops" {
		t.Fatalf("unexpected oidc groups: %#v", configs[0].OIDCGroups)
	}
	if len(configs[0].OIDCTeamAdminEmails) != 1 || configs[0].OIDCTeamAdminEmails[0] != "owner@example.com" {
		t.Fatalf("unexpected oidc team admin emails: %#v", configs[0].OIDCTeamAdminEmails)
	}
	if len(configs[0].OIDCTeamAdminGroups) != 1 || configs[0].OIDCTeamAdminGroups[0] != "teamname-admins" {
		t.Fatalf("unexpected oidc team admin groups: %#v", configs[0].OIDCTeamAdminGroups)
	}

	if err := s.ReplaceTeamConfigs(ctx, []auth.TeamConfig{
		{
			Team:            "platform-admin",
			OIDCAdminGroups: []string{"forge-admins"},
		},
	}); err != nil {
		t.Fatalf("ReplaceTeamConfigs() error = %v", err)
	}

	configs, err = s.LoadTeamConfigs(ctx)
	if err != nil {
		t.Fatalf("LoadTeamConfigs() after replace error = %v", err)
	}
	if len(configs) != 1 || configs[0].Team != "platform-admin" {
		t.Fatalf("unexpected replaced configs: %#v", configs)
	}
	if len(configs[0].OIDCAdminGroups) != 1 || configs[0].OIDCAdminGroups[0] != "forge-admins" {
		t.Fatalf("unexpected admin groups: %#v", configs[0].OIDCAdminGroups)
	}
}

func TestSQLiteAccessTokensNeverStoreRawCredential(t *testing.T) {
	t.Parallel()

	hasher := testAccessTokenHasher(t)
	s, err := NewSQLiteStore("sqlite://:memory:", hasher)
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	defer s.Close()
	raw := "raw-publish-token-that-must-not-reach-sql"
	if err := s.ReplaceTeamConfigs(context.Background(), []auth.TeamConfig{{Team: "teamname", PublishTokens: []string{raw}}}); err != nil {
		t.Fatalf("ReplaceTeamConfigs() error = %v", err)
	}

	var id, prefix, digest string
	if err := s.db.QueryRow(`select token_id, token_prefix, token_hash from access_tokens`).Scan(&id, &prefix, &digest); err != nil {
		t.Fatalf("select access token metadata error = %v", err)
	}
	if id == "" || prefix == "" || digest != hasher.Digest(raw) {
		t.Fatalf("unexpected stored token metadata id=%q prefix=%q digest=%q", id, prefix, digest)
	}
	if strings.Contains(id+prefix+digest, raw) {
		t.Fatal("access_tokens contains the raw credential")
	}
}

func TestSQLiteMigratesLegacyPlaintextAccessTokens(t *testing.T) {
	t.Parallel()

	databasePath := t.TempDir() + "/legacy-access.db"
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := db.Exec(`
		pragma foreign_keys = on;
		create table access_teams (team text primary key);
		create table access_tokens (
			team text not null references access_teams (team) on delete cascade,
			token_type text not null,
			token text not null,
			primary key (token_type, token)
		);
		insert into access_teams (team) values ('teamname');
		insert into access_tokens (team, token_type, token) values ('teamname', 'read', 'legacy-read-token');
	`); err != nil {
		_ = db.Close()
		t.Fatalf("seed legacy access schema error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy database error = %v", err)
	}

	hasher := testAccessTokenHasher(t)
	s, err := NewSQLiteStore("sqlite://"+databasePath, hasher)
	if err != nil {
		t.Fatalf("NewSQLiteStore() migration error = %v", err)
	}
	defer s.Close()
	configs, err := s.LoadTeamConfigs(context.Background())
	if err != nil {
		t.Fatalf("LoadTeamConfigs() error = %v", err)
	}
	if len(configs) != 1 || len(configs[0].ReadTokenRecords) != 1 || configs[0].ReadTokenRecords[0].Digest != hasher.Digest("legacy-read-token") {
		t.Fatalf("unexpected migrated config: %#v", configs)
	}
	authorizer, err := auth.NewAuthorizerWithTokenHasher(configs, hasher)
	if err != nil {
		t.Fatalf("NewAuthorizerWithTokenHasher() error = %v", err)
	}
	if _, ok := authorizer.AuthenticateToken("legacy-read-token"); !ok {
		t.Fatal("migrated legacy credential no longer authenticates")
	}
}

func TestSQLiteAccessConfigAllowsSharedTeamAdminMappings(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, err := NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	defer s.Close()

	if err := s.ReplaceTeamConfigs(ctx, []auth.TeamConfig{
		{
			Team:                "teamname",
			OIDCTeamAdminEmails: []string{"owner@example.com"},
			OIDCTeamAdminGroups: []string{"platform-owners"},
		},
		{
			Team:                "alpha",
			OIDCTeamAdminEmails: []string{"owner@example.com"},
			OIDCTeamAdminGroups: []string{"platform-owners"},
		},
	}); err != nil {
		t.Fatalf("ReplaceTeamConfigs() error = %v", err)
	}

	configs, err := s.LoadTeamConfigs(ctx)
	if err != nil {
		t.Fatalf("LoadTeamConfigs() error = %v", err)
	}
	for _, team := range []string{"teamname", "alpha"} {
		var cfg *auth.TeamConfig
		for i := range configs {
			if configs[i].Team == team {
				cfg = &configs[i]
				break
			}
		}
		if cfg == nil {
			t.Fatalf("%s config missing: %#v", team, configs)
		}
		if len(cfg.OIDCTeamAdminEmails) != 1 || cfg.OIDCTeamAdminEmails[0] != "owner@example.com" {
			t.Fatalf("unexpected %s team admin emails: %#v", team, cfg.OIDCTeamAdminEmails)
		}
		if len(cfg.OIDCTeamAdminGroups) != 1 || cfg.OIDCTeamAdminGroups[0] != "platform-owners" {
			t.Fatalf("unexpected %s team admin groups: %#v", team, cfg.OIDCTeamAdminGroups)
		}
	}
}

func TestSQLiteListModulesSkipsOrphanModules(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, err := NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	defer s.Close()

	_, err = s.UpsertModule(ctx, "teamname", "orphan")
	if err != nil {
		t.Fatalf("UpsertModule(orphan) error = %v", err)
	}

	module, err := s.UpsertModule(ctx, "teamname", "real")
	if err != nil {
		t.Fatalf("UpsertModule(real) error = %v", err)
	}

	_, err = s.CreateRelease(ctx, domain.Release{
		ID:          "release-1",
		ModuleID:    module.ID,
		Owner:       "teamname",
		Name:        "real",
		Source:      "local",
		Version:     "1.0.0",
		Description: "Real module",
		FileName:    "teamname-real-1.0.0.tar.gz",
		ContentType: "application/gzip",
		SizeBytes:   123,
		SHA256:      "deadbeef",
		StoragePath: "modules/teamname/real/1.0.0/teamname-real-1.0.0.tar.gz",
		Metadata:    map[string]any{},
	})
	if err != nil {
		t.Fatalf("CreateRelease() error = %v", err)
	}

	modules, err := s.ListModules(ctx, 100)
	if err != nil {
		t.Fatalf("ListModules() error = %v", err)
	}

	if len(modules) != 1 {
		t.Fatalf("ListModules() returned %d modules, want 1", len(modules))
	}
	if modules[0].Owner != "teamname" || modules[0].Name != "real" {
		t.Fatalf("unexpected module in list: %#v", modules[0])
	}
}
