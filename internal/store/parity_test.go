package store

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
)

type parityStore interface {
	Store
	DeletedReleaseStore
	ReleaseUsageStore
}

func TestStoreParityLifecycle(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.open(t)
			t.Cleanup(st.Close)

			owner := "parity-" + tc.name
			name := "versions"
			module, err := st.UpsertModule(ctx, owner, name)
			if err != nil {
				t.Fatalf("UpsertModule() error = %v", err)
			}
			for _, version := range []string{"1.0.0", "2.0.0", "1.5.0"} {
				_, err = st.CreateRelease(ctx, NewRelease(
					module.ID,
					owner,
					name,
					version,
					"Parity test module",
					"",
					owner+"-"+name+"-"+version+".tar.gz",
					"application/gzip",
					"",
					"deadbeef",
					"modules/"+owner+"/"+name+"/"+version+"/"+owner+"-"+name+"-"+version+".tar.gz",
					123,
					map[string]any{"version": version},
				))
				if err != nil {
					t.Fatalf("CreateRelease(%s) error = %v", version, err)
				}
			}

			got, err := st.GetModule(ctx, owner, name)
			if err != nil {
				t.Fatalf("GetModule() error = %v", err)
			}
			if got.LatestVersion != "2.0.0" {
				t.Fatalf("LatestVersion after create = %q, want 2.0.0", got.LatestVersion)
			}

			versions, err := st.ListReleases(ctx, owner, name)
			if err != nil {
				t.Fatalf("ListReleases() error = %v", err)
			}
			if gotVersions := moduleVersions(versions); !reflect.DeepEqual(gotVersions, []string{"2.0.0", "1.5.0", "1.0.0"}) {
				t.Fatalf("ListReleases() versions = %#v", gotVersions)
			}

			if err := st.DeleteRelease(ctx, owner, name, "2.0.0"); err != nil {
				t.Fatalf("DeleteRelease(latest) error = %v", err)
			}
			got, err = st.GetModule(ctx, owner, name)
			if err != nil {
				t.Fatalf("GetModule() after deleting latest error = %v", err)
			}
			if got.LatestVersion != "1.5.0" {
				t.Fatalf("LatestVersion after deleting latest = %q, want 1.5.0", got.LatestVersion)
			}
		})
	}
}

func TestStoreParityTombstonesAndUsage(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.open(t)
			t.Cleanup(st.Close)

			owner := "parity-" + tc.name
			name := "stdlib"
			module, err := st.UpsertModule(ctx, owner, name)
			if err != nil {
				t.Fatalf("UpsertModule() error = %v", err)
			}
			for _, version := range []string{"1.0.0", "2.0.0"} {
				_, err = st.CreateRelease(ctx, domain.Release{
					ID:          "release-" + tc.name + "-" + version,
					ModuleID:    module.ID,
					Owner:       owner,
					Name:        name,
					Source:      "upstream",
					Version:     version,
					FileName:    name + "-" + version + ".tar.gz",
					ContentType: "application/gzip",
					Metadata:    map[string]any{},
				})
				if err != nil {
					t.Fatalf("CreateRelease(%s) error = %v", version, err)
				}
			}

			if err := st.MarkReleaseUsed(ctx, owner, name, "1.0.0"); err != nil {
				t.Fatalf("MarkReleaseUsed() error = %v", err)
			}
			active, err := st.IsReleaseActive(ctx, owner, name, "1.0.0", time.Now().Add(-time.Hour))
			if err != nil {
				t.Fatalf("IsReleaseActive() error = %v", err)
			}
			if !active {
				t.Fatal("release should be active after mark")
			}
			activeReleases, err := st.ListActiveReleases(ctx, time.Now().Add(-time.Hour))
			if err != nil {
				t.Fatalf("ListActiveReleases() error = %v", err)
			}
			if !hasReleaseSummary(activeReleases, owner, name, "1.0.0") {
				t.Fatalf("ListActiveReleases() missing active release: %#v", activeReleases)
			}
			scopedActive, err := st.ListActiveReleasesForModules(ctx, time.Now().Add(-time.Hour), []domain.Module{module})
			if err != nil {
				t.Fatalf("ListActiveReleasesForModules() error = %v", err)
			}
			if len(scopedActive) != 1 || !hasReleaseSummary(scopedActive, owner, name, "1.0.0") {
				t.Fatalf("ListActiveReleasesForModules() = %#v", scopedActive)
			}

			if err := st.DeleteRelease(ctx, owner, name, "1.0.0"); err != nil {
				t.Fatalf("DeleteRelease() error = %v", err)
			}
			deleted, err := st.IsReleaseDeleted(ctx, owner, name, "1.0.0", "upstream")
			if err != nil {
				t.Fatalf("IsReleaseDeleted() error = %v", err)
			}
			if !deleted {
				t.Fatal("expected upstream release tombstone")
			}

			if err := st.DeleteModule(ctx, owner, name); err != nil {
				t.Fatalf("DeleteModule() error = %v", err)
			}
			deleted, err = st.IsReleaseDeleted(ctx, owner, name, "1.0.0", "upstream")
			if err != nil {
				t.Fatalf("IsReleaseDeleted() after module delete error = %v", err)
			}
			if deleted {
				t.Fatal("expected module delete to clear release tombstone")
			}
			active, err = st.IsReleaseActive(ctx, owner, name, "1.0.0", time.Now().Add(-time.Hour))
			if err != nil {
				t.Fatalf("IsReleaseActive() after module delete error = %v", err)
			}
			if active {
				t.Fatal("expected module delete to clear release usage")
			}
		})
	}
}

func TestPostgresStoreConcurrentSchemaSetup(t *testing.T) {
	dsn := os.Getenv("PUPPET_FORGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PUPPET_FORGE_TEST_POSTGRES_DSN is not set")
	}

	const workers = 8
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			st, err := NewPostgresStore(context.Background(), dsn)
			if err != nil {
				errs <- err
				return
			}
			st.Close()
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("NewPostgresStore() concurrent schema setup error = %v", err)
	}
}

func TestStoreParityAccessConfigRoundTrip(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.open(t)
			t.Cleanup(st.Close)

			prefix := "parity-" + tc.name + "-"
			want := []auth.TeamConfig{
				{
					Team:                prefix + "teamname",
					ReadTokens:          []string{"read-token"},
					PublishTokens:       []string{"publish-token"},
					PublishOwners:       []string{"teamname", "shared"},
					OIDCGroups:          []string{"teamname-devops"},
					OIDCTeamAdminEmails: []string{"owner@example.com"},
					OIDCTeamAdminGroups: []string{"teamname-admins"},
				},
				{
					Team:         prefix + "secondary",
					OIDCEmails:   []string{"publisher@example.com"},
					OIDCSubjects: []string{"publisher-subject"},
					OIDCDomains:  []string{"example.com"},
				},
			}

			existing, err := st.LoadTeamConfigs(ctx)
			if err != nil {
				t.Fatalf("LoadTeamConfigs(before) error = %v", err)
			}
			withoutParity := withoutParityTeams(existing, prefix)
			t.Cleanup(func() {
				if err := st.ReplaceTeamConfigs(context.Background(), withoutParity); err != nil {
					t.Errorf("cleanup parity access configs error = %v", err)
				}
			})

			configs := append(append([]auth.TeamConfig{}, withoutParity...), want...)
			if err := st.ReplaceTeamConfigs(ctx, configs); err != nil {
				t.Fatalf("ReplaceTeamConfigs() error = %v", err)
			}
			got, err := st.LoadTeamConfigs(ctx)
			if err != nil {
				t.Fatalf("LoadTeamConfigs() error = %v", err)
			}
			got = filterParityTeams(got, prefix)
			normalizeTeamConfigs(got)
			normalizeTeamConfigs(want)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("LoadTeamConfigs() = %#v, want %#v", got, want)
			}
		})
	}
}

func TestStoreParityRejectsReusedAccessTokensWithoutReplacingConfig(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.open(t)
			t.Cleanup(st.Close)

			prefix := "parity-duplicate-" + tc.name + "-"
			existing, err := st.LoadTeamConfigs(ctx)
			if err != nil {
				t.Fatalf("LoadTeamConfigs(before) error = %v", err)
			}
			withoutParity := withoutParityTeams(existing, prefix)
			t.Cleanup(func() {
				if err := st.ReplaceTeamConfigs(context.Background(), withoutParity); err != nil {
					t.Errorf("cleanup parity access configs error = %v", err)
				}
			})

			valid := append(append([]auth.TeamConfig{}, withoutParity...), auth.TeamConfig{
				Team:       prefix + "original",
				ReadTokens: []string{prefix + "read"},
			})
			if err := st.ReplaceTeamConfigs(ctx, valid); err != nil {
				t.Fatalf("ReplaceTeamConfigs(valid) error = %v", err)
			}

			reusedToken := prefix + "reused"
			invalid := append(append([]auth.TeamConfig{}, withoutParity...),
				auth.TeamConfig{Team: prefix + "reader", ReadTokens: []string{reusedToken}},
				auth.TeamConfig{Team: prefix + "publisher", PublishTokens: []string{reusedToken}},
			)
			err = st.ReplaceTeamConfigs(ctx, invalid)
			if err == nil || !strings.Contains(err.Error(), "tokens must be globally unique") {
				t.Fatalf("ReplaceTeamConfigs(invalid) error = %v, want reused-token validation", err)
			}
			if strings.Contains(err.Error(), reusedToken) {
				t.Fatalf("ReplaceTeamConfigs(invalid) leaked token in error: %v", err)
			}

			got, err := st.LoadTeamConfigs(ctx)
			if err != nil {
				t.Fatalf("LoadTeamConfigs(after) error = %v", err)
			}
			got = filterParityTeams(got, prefix)
			if len(got) != 1 || got[0].Team != prefix+"original" {
				t.Fatalf("invalid replacement changed access config: %#v", got)
			}
		})
	}
}

func TestStoreParityFilteredModulePagination(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.open(t)
			t.Cleanup(st.Close)

			prefix := "parity-filter-" + tc.name + "-"
			owners := []string{prefix + "alpha", prefix + "beta"}
			for _, moduleIdentity := range []struct {
				owner string
				name  string
			}{
				{owner: owners[0], name: "service-one"},
				{owner: owners[0], name: "service-two"},
				{owner: owners[1], name: "service-three"},
			} {
				module, err := st.UpsertModule(ctx, moduleIdentity.owner, moduleIdentity.name)
				if err != nil {
					t.Fatalf("UpsertModule(%s/%s) error = %v", moduleIdentity.owner, moduleIdentity.name, err)
				}
				if _, err := st.CreateRelease(ctx, domain.Release{
					ID:          module.ID + ":1.0.0",
					ModuleID:    module.ID,
					Owner:       moduleIdentity.owner,
					Name:        moduleIdentity.name,
					Source:      "local",
					Version:     "1.0.0",
					FileName:    moduleIdentity.owner + "-" + moduleIdentity.name + "-1.0.0.tar.gz",
					ContentType: "application/gzip",
					Metadata:    map[string]any{},
				}); err != nil {
					t.Fatalf("CreateRelease(%s/%s) error = %v", moduleIdentity.owner, moduleIdentity.name, err)
				}
				t.Cleanup(func() {
					if err := st.DeleteModule(context.Background(), moduleIdentity.owner, moduleIdentity.name); err != nil && !errors.Is(err, ErrNotFound) {
						t.Errorf("cleanup module %s/%s error = %v", moduleIdentity.owner, moduleIdentity.name, err)
					}
				})
			}

			first, total, err := st.ListModulesPageFiltered(ctx, []string{owners[0]}, "service", 1, 0)
			if err != nil {
				t.Fatalf("ListModulesPageFiltered(first) error = %v", err)
			}
			if total != 2 || len(first) != 1 || first[0].Owner != owners[0] {
				t.Fatalf("first filtered page = %#v, total = %d", first, total)
			}

			second, total, err := st.ListModulesPageFiltered(ctx, []string{owners[0]}, "service", 1, 1)
			if err != nil {
				t.Fatalf("ListModulesPageFiltered(second) error = %v", err)
			}
			if total != 2 || len(second) != 1 || second[0].Owner != owners[0] || second[0].Name == first[0].Name {
				t.Fatalf("second filtered page = %#v, total = %d", second, total)
			}

			matched, total, err := st.ListModulesPageFiltered(ctx, owners, owners[1]+"/service-three", 10, 0)
			if err != nil {
				t.Fatalf("ListModulesPageFiltered(exact search) error = %v", err)
			}
			if total != 1 || len(matched) != 1 || matched[0].Owner != owners[1] {
				t.Fatalf("exact filtered page = %#v, total = %d", matched, total)
			}
		})
	}
}

type parityStoreCase struct {
	name string
	open func(t *testing.T) parityStore
}

func parityStoreCases(t *testing.T) []parityStoreCase {
	t.Helper()

	cases := []parityStoreCase{
		{
			name: "sqlite",
			open: func(t *testing.T) parityStore {
				t.Helper()
				st, err := NewSQLiteStore("sqlite://:memory:")
				if err != nil {
					t.Fatalf("NewSQLiteStore() error = %v", err)
				}
				return st
			},
		},
	}

	if dsn := os.Getenv("PUPPET_FORGE_TEST_POSTGRES_DSN"); dsn != "" {
		cases = append(cases, parityStoreCase{
			name: "postgres",
			open: func(t *testing.T) parityStore {
				t.Helper()
				st, err := NewPostgresStore(context.Background(), dsn)
				if err != nil {
					t.Fatalf("NewPostgresStore() error = %v", err)
				}
				return st
			},
		})
	}
	return cases
}

func moduleVersions(versions []domain.ModuleVersion) []string {
	got := make([]string, 0, len(versions))
	for _, version := range versions {
		got = append(got, version.Version)
	}
	return got
}

func hasReleaseSummary(releases []ReleaseSummary, owner, name, version string) bool {
	for _, release := range releases {
		if release.Owner == owner && release.Name == name && release.Version == version {
			return true
		}
	}
	return false
}

func filterParityTeams(configs []auth.TeamConfig, prefix string) []auth.TeamConfig {
	next := make([]auth.TeamConfig, 0, len(configs))
	for _, cfg := range configs {
		if strings.HasPrefix(cfg.Team, prefix) {
			next = append(next, cfg)
		}
	}
	return next
}

func withoutParityTeams(configs []auth.TeamConfig, prefix string) []auth.TeamConfig {
	next := make([]auth.TeamConfig, 0, len(configs))
	for _, cfg := range configs {
		if !strings.HasPrefix(cfg.Team, prefix) {
			next = append(next, cfg)
		}
	}
	return next
}

func normalizeTeamConfigs(configs []auth.TeamConfig) {
	sort.Slice(configs, func(i, j int) bool {
		return configs[i].Team < configs[j].Team
	})
	for i := range configs {
		sort.Strings(configs[i].ReadTokens)
		sort.Strings(configs[i].PublishTokens)
		sort.Strings(configs[i].PublishOwners)
		sort.Strings(configs[i].OIDCGroups)
		sort.Strings(configs[i].OIDCTeamAdminEmails)
		sort.Strings(configs[i].OIDCTeamAdminGroups)
		sort.Strings(configs[i].OIDCAdminEmails)
		sort.Strings(configs[i].OIDCAdminSubjects)
		sort.Strings(configs[i].OIDCAdminGroups)
	}
}
