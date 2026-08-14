package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/storage"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/api/option"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/config"
	"github.com/zxzharmlesszxz/puppet-forge/internal/httpapi"
	"github.com/zxzharmlesszxz/puppet-forge/internal/metrics"
	"github.com/zxzharmlesszxz/puppet-forge/internal/observability"
	"github.com/zxzharmlesszxz/puppet-forge/internal/proxy"
	"github.com/zxzharmlesszxz/puppet-forge/internal/service"
	artifactstorage "github.com/zxzharmlesszxz/puppet-forge/internal/storage"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
	"github.com/zxzharmlesszxz/puppet-forge/internal/webauth"
)

const defaultGCSHost = "https://storage.googleapis.com"
const upstreamRefreshLeaseName = "upstream-refresh"
const accessTokenHistoryCleanupLeaseName = "access-token-history-cleanup"
const accessTokenHistoryCleanupInterval = 24 * time.Hour
const deletedReleaseCleanupLeaseName = "deleted-release-cleanup"
const deletedReleaseCleanupInterval = 24 * time.Hour
const releaseUsageCleanupLeaseName = "release-usage-cleanup"
const releaseUsageCleanupInterval = 24 * time.Hour
const sessionStateCleanupLeaseName = "session-state-cleanup"
const sessionStateCleanupInterval = 24 * time.Hour
const sessionStateHistoryTTL = 24 * time.Hour
const rateLimitCleanupLeaseName = "rate-limit-cleanup"
const rateLimitCleanupInterval = time.Hour
const retentionCleanupTimeout = 10 * time.Minute
const artifactDeletionCleanupLeaseName = "artifact-deletion-cleanup"
const artifactDeletionCleanupInterval = time.Minute
const artifactDeletionCleanupBatch = 100
const artifactDeletionCleanupTimeout = 90 * time.Second
const upstreamArtifactCacheCleanupLeaseName = "upstream-artifact-cache-cleanup"
const upstreamArtifactCacheCleanupInterval = 24 * time.Hour
const retentionCleanupRetryInterval = 30 * time.Second
const defaultCloseTimeout = 10 * time.Second
const defaultLeaseReleaseTimeout = 5 * time.Second

type App struct {
	router          http.Handler
	metricsHandler  http.Handler
	store           store.Store
	gcsClient       *storage.Client
	cancel          context.CancelFunc
	wg              sync.WaitGroup
	waitMetrics     func()
	closeTimeout    time.Duration
	leaseTimeout    time.Duration
	leaseHolderOnce sync.Once
	leaseHolder     string
	closeOnce       sync.Once
	closeDone       chan struct{}
}

func New(cfg config.Config) (*App, error) {
	return NewContext(context.Background(), cfg)
}

func NewContext(ctx context.Context, cfg config.Config) (*App, error) {
	if ctx == nil {
		return nil, errors.New("application context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	trustedProxyCIDRs, err := config.ParseTrustedProxyCIDRs(cfg.TrustedProxyCIDRs)
	if err != nil {
		return nil, err
	}

	tokenHasher, err := auth.NewTokenHasher(cfg.AccessTokenPepper)
	if err != nil {
		return nil, err
	}

	moduleStore, err := openStore(ctx, cfg, tokenHasher)
	if err != nil {
		return nil, err
	}

	artifacts, gcsClient, err := buildArtifactStorage(ctx, cfg)
	if err != nil {
		moduleStore.Close()
		return nil, err
	}
	forgeProxy, err := proxy.NewForgeProxy(
		cfg.UpstreamURL,
		cfg.UpstreamProxyJSONCacheTTL,
		cfg.ForgeCacheMaxBodyBytes,
		artifacts,
		"upstream-cache",
		proxy.WithMaxArtifactBytes(cfg.UpstreamArtifactMaxBytes),
		proxy.WithMaxStaleAge(cfg.UpstreamProxyJSONStaleTTL),
		proxy.WithLeaseStore(moduleStore),
	)
	if err != nil {
		moduleStore.Close()
		closeGCSClient(gcsClient)
		return nil, err
	}
	teamAccess, err := moduleStore.LoadTeamConfigs(ctx)
	if err != nil {
		moduleStore.Close()
		closeGCSClient(gcsClient)
		return nil, err
	}
	authorizer, err := auth.NewAuthorizerWithTokenHasher(auth.AccessConfigsWithRuntimeAdmin(teamAccess, cfg.AdminToken), tokenHasher)
	if err != nil {
		moduleStore.Close()
		closeGCSClient(gcsClient)
		return nil, err
	}
	if !authorizer.Enabled() {
		moduleStore.Close()
		closeGCSClient(gcsClient)
		return nil, errors.New("at least one access credential or ADMIN_TOKEN is required")
	}
	var oidcAuth *webauth.OIDCAuth
	if cfg.WebAuthMode == "oidc" {
		oidcAuth, err = webauth.New(ctx, webauth.Config{
			IssuerURL:         cfg.OIDCIssuerURL,
			ClientID:          cfg.OIDCClientID,
			ClientSecret:      cfg.OIDCClientSecret,
			RedirectURL:       cfg.OIDCRedirectURL,
			LogoutURL:         cfg.OIDCLogoutURL,
			CookieSecret:      cfg.OIDCCookieSecret,
			PublicBaseURL:     cfg.PublicBaseURL,
			Scopes:            strings.Fields(cfg.OIDCScopes),
			SigningAlgorithms: strings.Fields(cfg.OIDCSigningAlgorithms),
			StateStore:        moduleStore,
			SessionStore:      moduleStore,
		})
		if err != nil {
			moduleStore.Close()
			closeGCSClient(gcsClient)
			return nil, err
		}
	}
	moduleSvc := service.NewModuleService(moduleStore, artifacts, cfg.ArtifactPrefix, forgeProxy)
	backgroundCtx, cancel := context.WithCancel(ctx)
	moduleRegistry := prometheus.NewRegistry()
	waitMetrics, err := observability.RegisterModuleMetrics(backgroundCtx, moduleSvc, cfg.MetricsModuleLimit, cfg.MetricsRefreshInterval, moduleRegistry)
	if err != nil {
		cancel()
		moduleStore.Close()
		closeGCSClient(gcsClient)
		return nil, fmt.Errorf("register module metrics: %w", err)
	}
	forgeProxy.SetModuleObserver(func(ctx context.Context, module proxy.UpstreamModule, fresh bool) {
		if fresh {
			if err := moduleSvc.IndexUpstreamModule(ctx, module); err != nil {
				slog.Default().Error("upstream module indexing failed", "err", err)
			}
		}
	})
	router, err := httpapi.NewRouter(httpapi.RouterConfig{
		Modules:               moduleSvc,
		ForgeProxy:            forgeProxy.Handler(),
		PublicBaseURL:         cfg.PublicBaseURL,
		AllowedPublicHosts:    splitConfigList(cfg.AllowedPublicHosts),
		TrustedProxyCIDRs:     trustedProxyCIDRs,
		TrustForwardedHeaders: cfg.TrustForwardedHeaders,
		Authorizer:            authorizer,
		TokenHasher:           tokenHasher,
		WebAuth:               oidcAuth,
		AdminToken:            cfg.AdminToken,
		ManageSessionSecret:   cfg.ManageSessionSecret,
		RefreshAccessConfig:   true,
		PublicModuleAccess:    cfg.PublicModuleAccess,
		ActiveReleaseTTL:      cfg.ActiveReleaseTTL,
		SecurityHSTSEnabled:   cfg.SecurityHSTSEnabled,
	},
		httpapi.WithModuleUploadMaxBytes(cfg.ModuleUploadMaxBytes),
	)
	if err != nil {
		cancel()
		waitMetrics()
		moduleStore.Close()
		closeGCSClient(gcsClient)
		return nil, fmt.Errorf("build HTTP router: %w", err)
	}

	app := &App{
		router:         router,
		metricsHandler: observability.MetricsHandler(prometheus.Gatherers{prometheus.DefaultGatherer, moduleRegistry}),
		store:          moduleStore,
		gcsClient:      gcsClient,
		cancel:         cancel,
		waitMetrics:    waitMetrics,
		closeTimeout:   cfg.ShutdownTimeout,
		leaseTimeout:   defaultLeaseReleaseTimeout,
	}
	app.startUpstreamRefresh(backgroundCtx, moduleSvc, cfg.UpstreamSyncInterval, cfg.UpstreamSyncLimit, cfg.UpstreamSyncConcurrency)
	app.startAccessTokenHistoryCleanup(backgroundCtx, moduleSvc, cfg.AccessTokenHistoryTTL)
	app.startDeletedReleaseCleanup(backgroundCtx, moduleSvc, cfg.DeletedReleaseTTL)
	app.startReleaseUsageCleanup(backgroundCtx, moduleSvc, cfg.ActiveReleaseTTL)
	app.startSessionStateCleanup(backgroundCtx, moduleSvc)
	app.startRateLimitCleanup(backgroundCtx, moduleSvc)
	app.startArtifactDeletionCleanup(backgroundCtx, moduleSvc)
	app.startUpstreamArtifactCacheCleanup(backgroundCtx, moduleSvc, cfg.UpstreamArtifactOrphanTTL)

	return app, nil
}

func (a *App) startUpstreamArtifactCacheCleanup(ctx context.Context, moduleSvc *service.ModuleService, ttl time.Duration) {
	if ttl <= 0 || moduleSvc == nil {
		return
	}
	a.startRetentionCleanup(ctx, upstreamArtifactCacheCleanupLeaseName, upstreamArtifactCacheCleanupInterval, "upstream artifact cache", func(ctx context.Context) error {
		startedAt := time.Now()
		result, err := moduleSvc.PruneUpstreamArtifactCache(ctx, time.Now().UTC().Add(-ttl))
		metrics.ObserveUpstreamArtifactCleanup(err, result.Scanned, result.Deleted, result.Failed, time.Since(startedAt))
		if err != nil {
			return err
		}
		if result.Deleted > 0 {
			slog.Default().Info("pruned orphan upstream artifact cache", "scanned", result.Scanned, "deleted", result.Deleted, "failed", result.Failed, "ttl", ttl)
		}
		return nil
	})
}

func ReconcileArtifacts(ctx context.Context, cfg config.Config, repair bool) (service.ReconciliationReport, error) {
	tokenHasher, err := auth.NewTokenHasher(cfg.AccessTokenPepper)
	if err != nil {
		return service.ReconciliationReport{}, err
	}
	moduleStore, err := openStore(ctx, cfg, tokenHasher)
	if err != nil {
		return service.ReconciliationReport{}, err
	}
	defer moduleStore.Close()
	artifacts, gcsClient, err := buildArtifactStorage(ctx, cfg)
	if err != nil {
		return service.ReconciliationReport{}, err
	}
	defer closeGCSClient(gcsClient)
	return service.NewModuleService(moduleStore, artifacts, cfg.ArtifactPrefix, nil).ReconcileArtifacts(ctx, repair)
}

func openStore(ctx context.Context, cfg config.Config, tokenHasher *auth.TokenHasher) (store.Store, error) {
	const maxPostgresPoolConns = int(^uint32(0) >> 1)
	if cfg.DatabaseMaxConns > maxPostgresPoolConns || cfg.DatabaseMinConns > maxPostgresPoolConns {
		return nil, fmt.Errorf("postgres pool size exceeds int32: max=%d min=%d", cfg.DatabaseMaxConns, cfg.DatabaseMinConns)
	}
	return store.OpenWithOptions(ctx, cfg.DatabaseDSN, tokenHasher, store.OpenOptions{
		PostgresPool: &store.PostgresPoolConfig{
			// #nosec G115 -- both values are bounded to MaxInt32 immediately above.
			MaxConns: int32(cfg.DatabaseMaxConns),
			// #nosec G115 -- both values are bounded to MaxInt32 immediately above.
			MinConns:        int32(cfg.DatabaseMinConns),
			MaxConnLifetime: cfg.DatabaseMaxConnLifetime,
			MaxConnIdleTime: cfg.DatabaseMaxConnIdleTime,
		},
	})
}

func splitConfigList(value string) []string {
	return strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' })
}

func (a *App) Router() http.Handler {
	return a.router
}

func (a *App) MetricsHandler() http.Handler {
	return a.metricsHandler
}

func (a *App) Close() error {
	a.closeOnce.Do(func() {
		a.closeDone = make(chan struct{})
		if a.cancel != nil {
			a.cancel()
		}
		go func() {
			a.wg.Wait()
			if a.waitMetrics != nil {
				a.waitMetrics()
			}
			if a.store != nil {
				a.store.Close()
			}
			closeGCSClient(a.gcsClient)
			close(a.closeDone)
		}()
	})
	closeTimeout := a.closeTimeout
	if closeTimeout <= 0 {
		closeTimeout = defaultCloseTimeout
	}
	timer := time.NewTimer(closeTimeout)
	defer timer.Stop()
	select {
	case <-a.closeDone:
		return nil
	case <-timer.C:
		slog.Default().Error("background workers did not stop before shutdown deadline", "timeout", closeTimeout)
		return fmt.Errorf("background workers did not stop within %s", closeTimeout)
	}
}

func (a *App) releaseLease(leaseName, holder string) {
	if a.store == nil {
		return
	}
	timeout := a.leaseTimeout
	if timeout <= 0 {
		timeout = defaultLeaseReleaseTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := a.store.ReleaseLease(ctx, leaseName, holder); err != nil && !errors.Is(err, context.Canceled) {
		slog.Default().Warn("release background worker lease failed", "lease", leaseName, "err", err)
	}
}

func closeGCSClient(client *storage.Client) {
	if client != nil {
		_ = client.Close()
	}
}

func (a *App) startUpstreamRefresh(ctx context.Context, moduleSvc *service.ModuleService, interval time.Duration, limit, concurrency int) {
	if interval <= 0 || moduleSvc == nil {
		return
	}

	holder := a.leaseIdentity()
	leaseDuration := max(2*interval, 30*time.Second)

	a.wg.Go(func() {
		defer a.releaseLease(upstreamRefreshLeaseName, holder)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		slog.Default().Info("enabled upstream refresh", "interval", interval, "limit", limit, "concurrency", concurrency)

		for {
			select {
			case <-ctx.Done():
				slog.Default().Debug("upstream refresh worker stopping", "reason", "context_canceled")
				return
			case <-ticker.C:
				slog.Default().Debug("attempting upstream refresh lease", "lease", upstreamRefreshLeaseName, "duration", leaseDuration)
				leader, err := a.store.AcquireLease(ctx, upstreamRefreshLeaseName, holder, leaseDuration)
				if err != nil {
					slog.Default().Error("acquire upstream refresh lease failed", "err", err)
					continue
				}
				if !leader {
					slog.Default().Debug("upstream refresh skipped", "reason", "lease_held_by_another_replica")
					continue
				}
				slog.Default().Debug("upstream refresh lease acquired", "lease", upstreamRefreshLeaseName)
				refreshCtx, cancel := context.WithTimeout(ctx, interval)
				err = moduleSvc.RefreshCachedUpstreamModules(refreshCtx, limit, concurrency)
				cancel()
				if err != nil {
					slog.Default().Error("refresh cached upstream modules failed", "err", err)
				}
			}
		}
	})
}

func purgeAccessTokenHistory(ctx context.Context, moduleSvc *service.ModuleService, ttl time.Duration) error {
	if ttl <= 0 {
		return nil
	}
	deleted, err := moduleSvc.PurgeAccessTokenHistory(ctx, time.Now().UTC().Add(-ttl))
	if err != nil {
		return fmt.Errorf("purge access token history: %w", err)
	}
	if deleted > 0 {
		slog.Default().Info("purged access token history", "deleted", deleted, "ttl", ttl)
	}
	return nil
}

func (a *App) startAccessTokenHistoryCleanup(ctx context.Context, moduleSvc *service.ModuleService, ttl time.Duration) {
	if ttl <= 0 || moduleSvc == nil {
		return
	}
	a.startRetentionCleanup(ctx, accessTokenHistoryCleanupLeaseName, accessTokenHistoryCleanupInterval, "access token history", func(ctx context.Context) error {
		return purgeAccessTokenHistory(ctx, moduleSvc, ttl)
	})
}

func purgeDeletedReleases(ctx context.Context, moduleSvc *service.ModuleService, ttl time.Duration) error {
	if ttl <= 0 {
		return nil
	}
	deleted, err := moduleSvc.PurgeDeletedReleases(ctx, time.Now().UTC().Add(-ttl))
	if err != nil {
		return fmt.Errorf("purge deleted releases: %w", err)
	}
	if deleted > 0 {
		slog.Default().Info("purged deleted release tombstones", "deleted", deleted, "ttl", ttl)
	}
	return nil
}

func (a *App) startDeletedReleaseCleanup(ctx context.Context, moduleSvc *service.ModuleService, ttl time.Duration) {
	if ttl <= 0 || moduleSvc == nil {
		return
	}
	a.startRetentionCleanup(ctx, deletedReleaseCleanupLeaseName, deletedReleaseCleanupInterval, "deleted release", func(ctx context.Context) error {
		return purgeDeletedReleases(ctx, moduleSvc, ttl)
	})
}

func purgeReleaseUsage(ctx context.Context, moduleSvc *service.ModuleService, ttl time.Duration) error {
	if ttl <= 0 {
		return nil
	}
	if err := moduleSvc.PurgeReleaseUsage(ctx, time.Now().UTC().Add(-ttl)); err != nil {
		return fmt.Errorf("purge release usage: %w", err)
	}
	return nil
}

func (a *App) startReleaseUsageCleanup(ctx context.Context, moduleSvc *service.ModuleService, ttl time.Duration) {
	if ttl <= 0 || moduleSvc == nil {
		return
	}
	a.startRetentionCleanup(ctx, releaseUsageCleanupLeaseName, releaseUsageCleanupInterval, "release usage", func(ctx context.Context) error {
		return purgeReleaseUsage(ctx, moduleSvc, ttl)
	})
}

func (a *App) startSessionStateCleanup(ctx context.Context, moduleSvc *service.ModuleService) {
	a.startRetentionCleanup(ctx, sessionStateCleanupLeaseName, sessionStateCleanupInterval, "session state", func(ctx context.Context) error {
		now := time.Now().UTC()
		result, err := moduleSvc.PurgeSessionState(ctx, now, now.Add(-sessionStateHistoryTTL))
		if err != nil {
			return err
		}
		deleted := result.ManageSessions + result.OIDCStates + result.OIDCSessions
		if deleted > 0 {
			slog.Info("purged session state", "manage_sessions", result.ManageSessions, "oidc_states", result.OIDCStates, "oidc_sessions", result.OIDCSessions)
		}
		return nil
	})
}

func (a *App) startRateLimitCleanup(ctx context.Context, moduleSvc *service.ModuleService) {
	a.startRetentionCleanup(ctx, rateLimitCleanupLeaseName, rateLimitCleanupInterval, "rate limit", func(ctx context.Context) error {
		deleted, err := moduleSvc.PurgeRateLimits(ctx, time.Now().UTC().Add(-rateLimitCleanupInterval))
		if err != nil {
			return err
		}
		if deleted > 0 {
			slog.Info("purged expired rate limits", "deleted", deleted)
		}
		return nil
	})
}

func (a *App) startRetentionCleanup(ctx context.Context, leaseName string, interval time.Duration, description string, cleanup func(context.Context) error) {
	holder := a.leaseIdentity()
	a.wg.Go(func() {
		timer := time.NewTimer(0)
		defer timer.Stop()
		defer a.releaseLease(leaseName, holder)
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				cleanupCtx, cancel := context.WithTimeout(ctx, retentionCleanupTimeout)
				succeeded := false
				leader, err := a.store.AcquireLease(cleanupCtx, leaseName, holder, 2*retentionCleanupTimeout)
				if err != nil {
					slog.Default().Error("acquire "+description+" cleanup lease failed", "err", err)
				} else if leader {
					if err := cleanup(cleanupCtx); err != nil {
						slog.Default().Error(description+" cleanup failed", "err", err)
					} else {
						succeeded = true
					}
				}
				cancel()
				timer.Reset(retentionCleanupDelay(interval, succeeded, holder+":"+leaseName))
			}
		}
	})
}

func retentionCleanupDelay(interval time.Duration, succeeded bool, jitterKey string) time.Duration {
	if succeeded {
		return interval
	}
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(jitterKey))
	jitter := time.Duration(int(hash.Sum32()%11)-5) * time.Second
	return min(interval, retentionCleanupRetryInterval+jitter)
}

func (a *App) startArtifactDeletionCleanup(ctx context.Context, moduleSvc *service.ModuleService) {
	if moduleSvc == nil || a.store == nil {
		return
	}
	holder := a.leaseIdentity()
	a.wg.Go(func() {
		defer a.releaseLease(artifactDeletionCleanupLeaseName, holder)

		ticker := time.NewTicker(artifactDeletionCleanupInterval)
		defer ticker.Stop()
		run := func() {
			runCtx, cancel := context.WithTimeout(ctx, artifactDeletionCleanupTimeout)
			defer cancel()

			pending, err := moduleSvc.CountArtifactDeletions(runCtx)
			if err != nil {
				slog.Default().Error("count pending artifact deletions failed", "err", err)
			} else {
				metrics.SetArtifactDeletionsPending(pending)
			}
			leader, err := a.store.AcquireLease(runCtx, artifactDeletionCleanupLeaseName, holder, 2*artifactDeletionCleanupTimeout)
			if err != nil {
				slog.Default().Error("acquire artifact deletion cleanup lease failed", "err", err)
				return
			}
			if !leader {
				return
			}
			result, err := moduleSvc.ProcessArtifactDeletions(runCtx, artifactDeletionCleanupBatch)
			if err != nil {
				slog.Default().Error("artifact deletion cleanup failed",
					"err", err,
					"attempted", result.Attempted,
					"deleted", result.Deleted,
					"canceled", result.Canceled,
					"failed", result.Failed,
					"pending", result.Pending,
				)
				return
			}
			if result.Attempted > 0 {
				slog.Default().Info("processed artifact deletion queue",
					"attempted", result.Attempted,
					"deleted", result.Deleted,
					"canceled", result.Canceled,
					"pending", result.Pending,
				)
			}
		}

		run()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run()
			}
		}
	})
}

func (a *App) leaseIdentity() string {
	a.leaseHolderOnce.Do(func() {
		a.leaseHolder = newLeaseHolder()
	})
	return a.leaseHolder
}

func newLeaseHolder() string {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "unknown-host"
	}
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return fmt.Sprintf("%s-pid-%d-start-%d", hostname, os.Getpid(), time.Now().UnixNano())
	}
	return fmt.Sprintf("%s-pid-%d-%s", hostname, os.Getpid(), hex.EncodeToString(entropy[:]))
}

func buildArtifactStorage(ctx context.Context, cfg config.Config) (artifactstorage.ArtifactStorage, *storage.Client, error) {
	switch cfg.ArtifactBackend {
	case "gcs":
		gcsClient, err := newGCSClient(ctx, cfg.ArtifactEndpoint, cfg.ArtifactGCSAnonymous)
		if err != nil {
			return nil, nil, fmt.Errorf("create gcs client: %w", err)
		}
		artifacts, err := artifactstorage.NewGCSStorage(gcsClient, cfg.ArtifactBucket, cfg.ArtifactProject, cfg.ArtifactEndpoint)
		if err != nil {
			_ = gcsClient.Close()
			return nil, nil, err
		}
		if cfg.ArtifactEnsureBucket {
			if err := artifacts.EnsureBucket(ctx); err != nil {
				_ = gcsClient.Close()
				return nil, nil, err
			}
		}
		return artifacts, gcsClient, nil
	case "s3":
		artifacts, err := artifactstorage.NewS3Storage(
			ctx,
			cfg.ArtifactEndpoint,
			cfg.ArtifactRegion,
			cfg.ArtifactBucket,
			cfg.ArtifactAccessKeyID,
			cfg.ArtifactSecretAccessKey,
			cfg.ArtifactPathStyle,
		)
		if err != nil {
			return nil, nil, err
		}
		return artifacts, nil, nil
	default:
		return nil, nil, fmt.Errorf("unsupported artifact backend: %s", cfg.ArtifactBackend)
	}
}

func newGCSClient(ctx context.Context, endpoint string, anonymous bool) (*storage.Client, error) {
	if endpoint == "" || endpoint == defaultGCSHost {
		if anonymous {
			return storage.NewClient(ctx, option.WithoutAuthentication())
		}
		return storage.NewClient(ctx)
	}
	apiEndpoint := strings.TrimRight(endpoint, "/") + "/storage/v1/"
	options := []option.ClientOption{option.WithEndpoint(apiEndpoint)}
	if anonymous {
		options = append(options, option.WithoutAuthentication())
	}
	return storage.NewClient(ctx, options...)
}
