package store

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"puppet-forge/internal/auth"
)

type ReleaseSummary struct {
	Owner     string
	Name      string
	Version   string
	CreatedAt time.Time
}

type Store interface {
	ModuleStore
	AccessStore
	AcquireLease(ctx context.Context, name, holder string, duration time.Duration) (bool, error)
	ReleaseLease(ctx context.Context, name, holder string) error
	Close()
}

type AccessStore interface {
	LoadTeamConfigs(ctx context.Context) ([]auth.TeamConfig, error)
	ReplaceTeamConfigs(ctx context.Context, configs []auth.TeamConfig) error
}

func Open(ctx context.Context, dsn string) (Store, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_DSN: %w", err)
	}

	switch parsed.Scheme {
	case "postgres":
		return NewPostgresStore(ctx, dsn)
	case "sqlite":
		return NewSQLiteStore(dsn)
	default:
		return nil, fmt.Errorf("unsupported database scheme %q in DATABASE_DSN", parsed.Scheme)
	}
}
