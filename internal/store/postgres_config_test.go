package store

import (
	"testing"
	"time"
)

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
