package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
)

func BenchmarkSQLiteCatalogPageWithTenThousandReleases(b *testing.B) {
	st, err := NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	seedSQLiteCatalogBenchmark(b, st, 1000, 10)

	ctx := context.Background()
	b.ResetTimer()
	for range b.N {
		modules, _, err := st.ListModulesPageFiltered(ctx, nil, "module", 100, 0)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := st.CountReleasesForModules(ctx, modules); err != nil {
			b.Fatal(err)
		}
	}
}

func seedSQLiteCatalogBenchmark(tb testing.TB, st *SQLiteStore, moduleCount, releasesPerModule int) {
	tb.Helper()
	tx, err := st.db.Begin()
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for moduleIndex := range moduleCount {
		owner := fmt.Sprintf("team-%03d", moduleIndex%100)
		name := fmt.Sprintf("module-%04d", moduleIndex)
		moduleID := fmt.Sprintf("module-%04d", moduleIndex)
		if _, err := tx.Exec(`insert into modules (id, slug, owner, name, latest_version, created_at, updated_at) values (?, ?, ?, ?, ?, ?, ?)`,
			moduleID, owner+"-"+name, owner, name, fmt.Sprintf("1.0.%d", releasesPerModule-1), now, now); err != nil {
			tb.Fatal(err)
		}
		for releaseIndex := range releasesPerModule {
			version := fmt.Sprintf("1.0.%d", releaseIndex)
			if _, err := tx.Exec(`insert into releases (id, module_id, slug, source, version, file_name, content_type, size_bytes, sha256, storage_path, metadata, created_at) values (?, ?, ?, 'local', ?, ?, 'application/gzip', 1, 'sha', ?, '{}', ?)`,
				fmt.Sprintf("release-%04d-%02d", moduleIndex, releaseIndex), moduleID, owner+"-"+name+"-"+version, version, name+".tar.gz", "modules/"+owner+"/"+name+"/"+version+".tar.gz", now); err != nil {
				tb.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
}

func TestSQLiteCatalogPerformanceFixtureShape(t *testing.T) {
	if testing.Short() {
		t.Skip("performance fixture")
	}
	st, err := NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	seedSQLiteCatalogBenchmark(t, st, 100, 10)
	modules, total, err := st.ListModulesPageFiltered(context.Background(), nil, "module", 100, 0)
	if err != nil || total != 100 || len(modules) != 100 {
		t.Fatalf("fixture catalog = modules:%d total:%d err:%v", len(modules), total, err)
	}
	releases, err := st.ListReleasesForModules(context.Background(), []domain.Module{modules[0]})
	if err != nil || len(releases) != 10 {
		t.Fatalf("fixture releases = %d err:%v", len(releases), err)
	}
	counts, err := st.CountReleasesForModules(context.Background(), []domain.Module{modules[0], modules[1]})
	if err != nil || len(counts) != 2 || counts[0].Count != 10 || counts[1].Count != 10 {
		t.Fatalf("fixture release counts = %#v err:%v", counts, err)
	}
}
