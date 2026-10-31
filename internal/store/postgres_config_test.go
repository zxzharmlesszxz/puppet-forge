package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestPostgresLocksDoNotStarveDataPool(t *testing.T) {
	dsn := os.Getenv("PUPPET_FORGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PUPPET_FORGE_TEST_POSTGRES_DSN is not set")
	}
	for _, size := range []int32{1, 2} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			st, err := newPostgresStore(ctx, isolatedPostgresTestDSN(t, dsn), nil, &PostgresPoolConfig{MaxConns: size})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(st.Close)
			unlock, err := st.LockModule(ctx, "teamname", "module")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = unlock() }()
			waiting := make(chan error, size-1)
			for range size - 1 {
				go func() {
					release, err := st.LockModule(ctx, "teamname", "module")
					if err == nil {
						err = release()
					}
					waiting <- err
				}()
			}
			for st.lockPool.Stat().AcquiredConns() < size {
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(time.Millisecond):
				}
			}
			if _, err := st.UpsertModule(ctx, "teamname", "module"); err != nil {
				t.Fatalf("SQL under contended module lock: %v", err)
			}
			if err := unlock(); err != nil {
				t.Fatal(err)
			}
			for range size - 1 {
				if err := <-waiting; err != nil {
					t.Fatal(err)
				}
			}
			accessUnlock, err := st.LockAccessConfig(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = accessUnlock() }()
			if _, err := st.LoadTeamConfigs(ctx); err != nil {
				t.Fatalf("SQL under access lock: %v", err)
			}
		})
	}
}

func TestPostgresPoolConfigAppliesExplicitLimits(t *testing.T) {
	t.Parallel()

	options := &PostgresPoolConfig{
		MaxConns:        12,
		MinConns:        2,
		MaxConnLifetime: 45 * time.Minute,
		MaxConnIdleTime: 10 * time.Minute,
	}
	config, err := postgresPoolConfig("postgres://forge:forge@localhost:5432/forge?sslmode=disable", options)
	if err != nil {
		t.Fatalf("postgresPoolConfig() error = %v", err)
	}
	if config.MaxConns != options.MaxConns || config.MinConns != options.MinConns {
		t.Fatalf("pool sizes = max:%d min:%d", config.MaxConns, config.MinConns)
	}
	if config.MaxConnLifetime != options.MaxConnLifetime || config.MaxConnIdleTime != options.MaxConnIdleTime {
		t.Fatalf("pool lifetimes = lifetime:%s idle:%s", config.MaxConnLifetime, config.MaxConnIdleTime)
	}
}
