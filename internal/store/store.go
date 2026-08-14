package store

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
)

type ReleaseSummary struct {
	Owner     string
	Name      string
	Version   string
	CreatedAt time.Time
}

type ModuleReleaseSummary struct {
	Owner     string
	Name      string
	Version   string
	CreatedAt time.Time
}

type ArtifactReleaseRecord struct {
	Owner       string
	Name        string
	Version     string
	StoragePath string
	SHA256      string
	SizeBytes   int64
}

type ManageSession struct {
	SessionHash    string
	CredentialHash string
	CredentialID   string
	AuthMethod     string
	CSRFSecret     string
	CreatedAt      time.Time
	ExpiresAt      time.Time
	LastSeenAt     time.Time
	RevokedAt      *time.Time
}

type OIDCState struct {
	StateHash    string
	Nonce        string
	PKCEVerifier string
	NextPath     string
	CreatedAt    time.Time
	ExpiresAt    time.Time
}

type OIDCSession struct {
	SessionHash string
	Subject     string
	Email       string
	Name        string
	Groups      []string
	CSRFSecret  string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	LastSeenAt  time.Time
	RevokedAt   *time.Time
}

type SessionCleanupResult struct {
	ManageSessions int64
	OIDCStates     int64
	OIDCSessions   int64
}

type Store interface {
	ModuleStore
	AccessStore
	ManageSessionStore
	OIDCStateStore
	OIDCSessionStore
	SessionMaintenanceStore
	RateLimitStore
	AcquireLease(ctx context.Context, name, holder string, duration time.Duration) (bool, error)
	ReleaseLease(ctx context.Context, name, holder string) error
	Close()
}

type RateLimitStore interface {
	ConsumeRateLimit(ctx context.Context, key string, limit int, window time.Duration, now time.Time) (bool, error)
	PurgeRateLimits(ctx context.Context, before time.Time) (int64, error)
}

type SessionMaintenanceStore interface {
	PurgeSessionState(ctx context.Context, now, historyBefore time.Time) (SessionCleanupResult, error)
}

type ManageSessionStore interface {
	CreateManageSession(ctx context.Context, session ManageSession) error
	GetManageSession(ctx context.Context, sessionHash string, now time.Time) (ManageSession, error)
	RevokeManageSession(ctx context.Context, sessionHash string, revokedAt time.Time) error
}

type OIDCStateStore interface {
	CreateOIDCState(ctx context.Context, state OIDCState) error
	ConsumeOIDCState(ctx context.Context, stateHash string, now time.Time) (OIDCState, error)
}

type OIDCSessionStore interface {
	CreateOIDCSession(ctx context.Context, session OIDCSession) error
	GetOIDCSession(ctx context.Context, sessionHash string, now time.Time) (OIDCSession, error)
	RevokeOIDCSession(ctx context.Context, sessionHash string, revokedAt time.Time) error
}

type AccessStore interface {
	LockAccessConfig(ctx context.Context) (AccessConfigUnlock, error)
	LoadTeamConfigs(ctx context.Context) ([]auth.TeamConfig, error)
	ReplaceTeamConfigs(ctx context.Context, configs []auth.TeamConfig) error
	IsAccessTokenActive(ctx context.Context, tokenID string, now time.Time) (bool, error)
	MarkAccessTokenUsed(ctx context.Context, tokenID string, usedAt time.Time) error
	PurgeAccessTokenHistory(ctx context.Context, cutoff time.Time) (int64, error)
}

type AccessConfigUnlock func() error

type ModuleReleaseBatchStore interface {
	ListReleasesForModules(ctx context.Context, modules []domain.Module) ([]ModuleReleaseSummary, error)
}

type ArtifactReleaseStore interface {
	ListArtifactReleases(ctx context.Context) ([]ArtifactReleaseRecord, error)
}

type PostgresPoolConfig struct {
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
}

type OpenOptions struct {
	PostgresPool *PostgresPoolConfig
}

func normalizeReleaseMetadata(release *domain.Release) {
	if release.Metadata == nil {
		release.Metadata = map[string]any{}
	}
}

func Open(ctx context.Context, dsn string, tokenHasher *auth.TokenHasher) (Store, error) {
	return OpenWithOptions(ctx, dsn, tokenHasher, OpenOptions{})
}

func OpenWithOptions(ctx context.Context, dsn string, tokenHasher *auth.TokenHasher, options OpenOptions) (Store, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_DSN: %w", err)
	}

	switch parsed.Scheme {
	case "postgres":
		return newPostgresStore(ctx, dsn, tokenHasher, options.PostgresPool)
	case "sqlite":
		return NewSQLiteStore(dsn, tokenHasher)
	default:
		return nil, fmt.Errorf("unsupported database scheme %q in DATABASE_DSN", parsed.Scheme)
	}
}
