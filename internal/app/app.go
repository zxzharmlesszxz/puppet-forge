package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"cloud.google.com/go/storage"

	"puppet-forge/internal/auth"
	"puppet-forge/internal/config"
	"puppet-forge/internal/httpapi"
	"puppet-forge/internal/observability"
	"puppet-forge/internal/proxy"
	"puppet-forge/internal/service"
	artifactstorage "puppet-forge/internal/storage"
	"puppet-forge/internal/store"
	"puppet-forge/internal/webauth"
)

const defaultGCSHost = "https://storage.googleapis.com"
const upstreamRefreshLeaseName = "upstream-refresh"

type App struct {
	router    http.Handler
	store     store.Store
	gcsClient *storage.Client
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

func New(cfg config.Config) (*App, error) {
	ctx := context.Background()

	moduleStore, err := store.Open(ctx, cfg.DatabaseDSN)
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
	if len(teamAccess) == 0 && cfg.AdminToken == "" {
		moduleStore.Close()
		closeGCSClient(gcsClient)
		return nil, errors.New("ADMIN_TOKEN is required when access config is empty")
	}
	authorizer, err := auth.NewAuthorizer(auth.AccessConfigsWithRuntimeAdmin(teamAccess, cfg.AdminToken))
	if err != nil {
		moduleStore.Close()
		closeGCSClient(gcsClient)
		return nil, err
	}
	var oidcAuth *webauth.OIDCAuth
	if cfg.WebAuthMode == "oidc" {
		oidcAuth, err = webauth.New(ctx, webauth.Config{
			IssuerURL:     cfg.OIDCIssuerURL,
			ClientID:      cfg.OIDCClientID,
			ClientSecret:  cfg.OIDCClientSecret,
			RedirectURL:   cfg.OIDCRedirectURL,
			LogoutURL:     cfg.OIDCLogoutURL,
			CookieSecret:  cfg.OIDCCookieSecret,
			PublicBaseURL: cfg.PublicBaseURL,
		})
		if err != nil {
			moduleStore.Close()
			closeGCSClient(gcsClient)
			return nil, err
		}
	}
	moduleSvc := service.NewModuleService(moduleStore, artifacts, cfg.ArtifactPrefix, forgeProxy)
	backgroundCtx, cancel := context.WithCancel(context.Background())
	observability.RegisterModuleMetrics(backgroundCtx, moduleSvc, cfg.MetricsModuleLimit)
	forgeProxy.SetModuleObserver(func(ctx context.Context, module proxy.UpstreamModule) {
		if err := moduleSvc.IndexUpstreamModule(ctx, module); err != nil {
			slog.Default().Error("upstream module indexing failed", "err", err)
			return
		}
		if err := moduleSvc.MarkUpstreamModuleCurrentReleaseUsed(ctx, module); err != nil {
			slog.Default().Error("upstream module usage marking failed", "err", err)
		}
	})
	router := httpapi.NewRouter(httpapi.RouterConfig{
		Modules:             moduleSvc,
		ForgeProxy:          forgeProxy.Handler(),
		PublicBaseURL:       cfg.PublicBaseURL,
		Authorizer:          authorizer,
		WebAuth:             oidcAuth,
		AdminToken:          cfg.AdminToken,
		PublicModuleAccess:  cfg.PublicModuleAccess,
		ActiveReleaseTTL:    cfg.ActiveReleaseTTL,
		SecurityHSTSEnabled: cfg.SecurityHSTSEnabled,
	},
		httpapi.WithModuleUploadMaxBytes(cfg.ModuleUploadMaxBytes),
	)

	app := &App{
		router:    router,
		store:     moduleStore,
		gcsClient: gcsClient,
		cancel:    cancel,
	}
	app.startUpstreamRefresh(backgroundCtx, moduleSvc, cfg.UpstreamSyncInterval, cfg.UpstreamSyncLimit)

	return app, nil
}

func (a *App) Router() http.Handler {
	return a.router
}

func (a *App) Close() {
	if a.cancel != nil {
		a.cancel()
	}
	a.wg.Wait()
	if a.store != nil {
		a.store.Close()
	}
	closeGCSClient(a.gcsClient)
}

func closeGCSClient(client *storage.Client) {
	if client != nil {
		_ = client.Close()
	}
}

func (a *App) startUpstreamRefresh(ctx context.Context, moduleSvc *service.ModuleService, interval time.Duration, limit int) {
	if interval <= 0 || moduleSvc == nil {
		return
	}

	holder, err := os.Hostname()
	if err != nil || holder == "" {
		holder = fmt.Sprintf("pid-%d", os.Getpid())
	}
	leaseDuration := 2 * interval
	if leaseDuration < 30*time.Second {
		leaseDuration = 30 * time.Second
	}

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				slog.Default().Error("upstream refresh worker panicked", "panic", r)
			}
		}()
		defer func() {
			if a.store != nil {
				_ = a.store.ReleaseLease(context.Background(), upstreamRefreshLeaseName, holder)
			}
		}()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		slog.Default().Info("enabled upstream refresh", "interval", interval, "limit", limit)

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				leader, err := a.store.AcquireLease(ctx, upstreamRefreshLeaseName, holder, leaseDuration)
				if err != nil {
					slog.Default().Error("acquire upstream refresh lease failed", "err", err)
					continue
				}
				if !leader {
					continue
				}
				refreshCtx, cancel := context.WithTimeout(ctx, interval)
				err = moduleSvc.RefreshCachedUpstreamModules(refreshCtx, limit)
				cancel()
				if err != nil {
					slog.Default().Error("refresh cached upstream modules failed", "err", err)
				}
			}
		}
	}()
}

func buildArtifactStorage(ctx context.Context, cfg config.Config) (artifactstorage.ArtifactStorage, *storage.Client, error) {
	switch cfg.ArtifactBackend {
	case "gcs":
		if cfg.ArtifactEndpoint != "" && cfg.ArtifactEndpoint != defaultGCSHost {
			if err := os.Setenv("STORAGE_EMULATOR_HOST", cfg.ArtifactEndpoint); err != nil {
				return nil, nil, fmt.Errorf("set STORAGE_EMULATOR_HOST: %w", err)
			}
		} else {
			_ = os.Unsetenv("STORAGE_EMULATOR_HOST")
		}
		gcsClient, err := storage.NewClient(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("create gcs client: %w", err)
		}
		artifacts := artifactstorage.NewGCSStorage(gcsClient, cfg.ArtifactBucket, cfg.ArtifactProject)
		if err := artifacts.EnsureBucket(ctx); err != nil {
			_ = gcsClient.Close()
			return nil, nil, err
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
