package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"puppet-forge/internal/auth"
	"puppet-forge/internal/domain"
)

func TestSQLiteDeleteLastReleaseRemovesModule(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, err := NewSQLiteStore("sqlite://:memory:")
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
	opened, err := Open(ctx, "sqlite://:memory:")
	if err != nil {
		t.Fatalf("Open(sqlite) error = %v", err)
	}
	opened.Close()

	if _, err := Open(ctx, "file:///tmp/forge.db"); err == nil {
		t.Fatal("expected unsupported scheme error")
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
	s, err := NewSQLiteStore("sqlite://:memory:")
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
			OIDCAdminGroups:     []string{"should-not-be-admin-for-teamname"},
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
	if len(configs[0].PublishTokens) != 1 || configs[0].PublishTokens[0] != "publish-token" {
		t.Fatalf("unexpected publish tokens: %#v", configs[0].PublishTokens)
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
			Team:                "carbon",
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
	for _, team := range []string{"teamname", "carbon"} {
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
