package store

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
)

func TestStoreParityNamespaceMigrationRejectsCaseCollisions(t *testing.T) {
	for _, backendCase := range parityStoreCases(t) {
		for _, collision := range []struct{ name, team, owners, remove string }{
			{"primary-extra", "Team", "('Team','Team'),('Team','team')", "team"},
			{"extra-extra", "origin", "('origin','Shared'),('origin','shared')", "shared"},
		} {
			t.Run(backendCase.name+"/"+collision.name, func(t *testing.T) {
				st := backendCase.open(t)
				t.Cleanup(st.Close)
				var execute func(string) error
				var migrate func() error
				switch backend := st.(type) {
				case *SQLiteStore:
					execute = func(query string) error { _, err := backend.db.ExecContext(t.Context(), query); return err }
					migrate = func() error { return migrateSQLiteSchema(backend.db, backend.tokenHasher) }
				case *PostgresStore:
					execute = func(query string) error { _, err := backend.pool.Exec(t.Context(), query); return err }
					migrate = func() error { return backend.ensureOperationalTables(t.Context()) }
				default:
					t.Fatal("unsupported migration backend")
				}
				if err := st.ReplaceTeamConfigs(t.Context(), []auth.TeamConfig{{Team: collision.team, PublishTokens: []string{"migration-token"}, OIDCTeamAdminGroups: []string{"migration-admins"}}}); err != nil {
					t.Fatal(err)
				}
				before, err := st.LoadTeamConfigs(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				for _, query := range []string{
					`create table access_publish_owners (team text not null references access_teams(team) on delete cascade, owner text not null, primary key(team,owner))`,
					fmt.Sprintf(`insert into access_publish_owners(team,owner) values %s`, collision.owners),
					`update schema_metadata set version=8 where id=1`,
				} {
					if err := execute(query); err != nil {
						t.Fatal(err)
					}
				}
				for range 2 {
					if err := migrate(); err == nil || !strings.Contains(err.Error(), "namespace migration conflict") {
						t.Fatalf("case collision = %v", err)
					}
					after, err := st.LoadTeamConfigs(t.Context())
					if err != nil || !reflect.DeepEqual(before, after) {
						t.Fatalf("failed migration changed access: %#v %v", after, err)
					}
				}
				if err := execute(fmt.Sprintf(`delete from access_publish_owners where owner='%s'`, collision.remove)); err != nil {
					t.Fatalf("legacy table was not preserved: %v", err)
				}
				for range 2 {
					if err := migrate(); err != nil {
						t.Fatalf("retry after resolving collision: %v", err)
					}
				}
			})
		}
	}
}

func TestStoreParityMigratesAndRemovesExtraNamespaceTable(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			st := tc.open(t)
			t.Cleanup(st.Close)
			var execute func(string) error
			var migrate func() error
			var legacyTableExists func() (bool, error)
			switch backend := st.(type) {
			case *SQLiteStore:
				execute = func(query string) error { _, err := backend.db.ExecContext(ctx, query); return err }
				migrate = func() error { return migrateSQLiteSchema(backend.db, backend.tokenHasher) }
				legacyTableExists = func() (bool, error) {
					var exists bool
					err := backend.db.QueryRowContext(ctx, `select exists(select 1 from sqlite_master where type='table' and name='access_publish_owners')`).Scan(&exists)
					return exists, err
				}
			case *PostgresStore:
				execute = func(query string) error { _, err := backend.pool.Exec(ctx, query); return err }
				migrate = func() error { return backend.ensureOperationalTables(ctx) }
				legacyTableExists = func() (bool, error) {
					var exists bool
					err := backend.pool.QueryRow(ctx, `select to_regclass('access_publish_owners') is not null`).Scan(&exists)
					return exists, err
				}
			default:
				t.Fatal("unsupported migration backend")
			}
			if exists, err := legacyTableExists(); err != nil || exists {
				t.Fatalf("fresh schema contains legacy table: exists=%v err=%v", exists, err)
			}
			if err := st.ReplaceTeamConfigs(ctx, []auth.TeamConfig{
				{Team: "origin", PublishTokens: []string{"origin-publish"}, OIDCTeamAdminGroups: []string{"origin-admins"}},
				{Team: "shared", OIDCReaderGroups: []string{"shared-readers"}},
			}); err != nil {
				t.Fatal(err)
			}
			before, err := st.LoadTeamConfigs(ctx)
			if err != nil {
				t.Fatal(err)
			}
			module, err := st.UpsertModule(ctx, "archive", "demo")
			if err != nil {
				t.Fatal(err)
			}
			release := NewRelease(module.ID, "archive", "demo", "1.0.0", "", "", "demo.tar.gz", "application/gzip", "md5", "sha", "modules/demo", 1, nil)
			if _, err := st.CreateRelease(ctx, release); err != nil {
				t.Fatal(err)
			}
			for _, query := range []string{
				`create table access_publish_owners (team text not null references access_teams(team) on delete cascade, owner text not null, primary key(team,owner))`,
				`insert into access_publish_owners(team,owner) values ('origin','origin'),('origin','shared'),('origin','archive')`,
				`update schema_metadata set version=8 where id=1`,
			} {
				if err := execute(query); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if err := migrate(); err != nil {
					t.Fatal(err)
				}
				if exists, err := legacyTableExists(); err != nil || exists {
					t.Fatalf("migration retained legacy table: exists=%v err=%v", exists, err)
				}
				after, err := st.LoadTeamConfigs(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if len(after) != 3 {
					t.Fatalf("migrated teams = %#v", after)
				}
				for _, cfg := range after {
					if cfg.Team == "archive" {
						if !reflect.DeepEqual(cfg, auth.TeamConfig{Team: "archive"}) {
							t.Fatalf("new namespace inherited access: %#v", cfg)
						}
						continue
					}
					for _, previous := range before {
						if cfg.Team == previous.Team && !reflect.DeepEqual(cfg, previous) {
							t.Fatalf("migration changed existing team: before=%#v after=%#v", previous, cfg)
						}
					}
				}
				if stored, err := st.GetRelease(ctx, "archive", "demo", "1.0.0"); err != nil || stored.SHA256 != release.SHA256 || stored.StoragePath != release.StoragePath {
					t.Fatalf("migration changed module release: %#v err=%v", stored, err)
				}
			}
		})
	}
}
